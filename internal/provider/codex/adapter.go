package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/pricing"
	"github.com/zhyuzh3d/llmserver/internal/provider"
)

const maxEventBytes = 8 << 20
const maxDirectErrorBody = 64 << 10
const directEndpoint = "https://chatgpt.com/backend-api/codex/responses"

type Config struct {
	ProviderID       string
	Executable       string
	ExpectedVersion  string
	ExtraArgs        []string
	MaxConcurrency   int
	DefaultReasoning string
	ServiceTier      string
	// DisableDirect is used by deterministic adapter tests. Production keeps
	// the low-overhead Responses transport enabled.
	DisableDirect  bool
	DirectEndpoint string
	DirectClient   *http.Client
}

// Adapter keeps a small pool of initialized App Server processes. Each worker
// handles only one ephemeral thread at a time, so requests never share history,
// while process startup, authentication and transport connections stay warm.
type Adapter struct {
	config             Config
	slot               chan struct{}
	mu                 sync.Mutex
	idle               []*appServerWorker
	retired            bool
	direct             *http.Client
	directEndpoint     string
	authMu             sync.Mutex
	auth               cachedAuth
	directBlockedUntil atomic.Int64
}

func New(config Config) (*Adapter, error) {
	if config.ProviderID == "" || !filepath.IsAbs(config.Executable) {
		return nil, errors.New("Codex provider id and absolute executable are required")
	}
	output, err := exec.Command(config.Executable, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("probe Codex executable: %w", err)
	}
	version := strings.TrimSpace(string(output))
	if config.ExpectedVersion != "" && !strings.HasPrefix(version, config.ExpectedVersion) {
		return nil, fmt.Errorf("Codex version %q does not match expected prefix %q", version, config.ExpectedVersion)
	}
	if config.MaxConcurrency == 0 {
		config.MaxConcurrency = 2
	}
	endpoint := config.DirectEndpoint
	if endpoint == "" {
		endpoint = directEndpoint
	}
	client := config.DirectClient
	if client == nil {
		client = newDirectHTTPClient()
	}
	return &Adapter{
		config:         config,
		slot:           make(chan struct{}, config.MaxConcurrency),
		direct:         client,
		directEndpoint: endpoint,
	}, nil
}

func (a *Adapter) ID() string { return a.config.ProviderID }

func (a *Adapter) Start(ctx context.Context, request provider.Request) (<-chan provider.Event, error) {
	select {
	case a.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if !a.config.DisableDirect && time.Now().UnixNano() >= a.directBlockedUntil.Load() {
		response, fallback, directErr := a.startDirect(ctx, request)
		if directErr == nil {
			events := make(chan provider.Event)
			go func() {
				defer close(events)
				defer func() { <-a.slot }()
				defer response.Body.Close()
				consumeDirectStream(ctx, response.Body, request.UpstreamModel, events)
			}()
			return events, nil
		}
		if !fallback {
			slog.Warn("Codex direct Responses request failed", "provider", a.config.ProviderID, "error", directErr)
			<-a.slot
			return nil, directErr
		}
		slog.Info("Codex direct Responses route unavailable; using App Server fallback", "provider", a.config.ProviderID, "error", directErr)
	}

	worker, err := a.acquireWorker()
	if err != nil {
		slog.Warn("Codex App Server fallback failed", "provider", a.config.ProviderID, "error", err)
		<-a.slot
		return nil, err
	}
	events := make(chan provider.Event)
	go func() {
		defer close(events)
		defer func() { <-a.slot }()
		reusable := worker.run(ctx, a.config, request, events)
		a.releaseWorker(worker, reusable)
	}()
	return events, nil
}

// Retire stops accepting new work and closes idle workers. An in-flight worker
// finishes its current request, then closes instead of returning to the pool.
func (a *Adapter) Retire() {
	a.mu.Lock()
	a.retired = true
	idle := a.idle
	a.idle = nil
	a.mu.Unlock()
	for _, worker := range idle {
		worker.close()
	}
	if a.direct != nil {
		a.direct.CloseIdleConnections()
	}
}

func (a *Adapter) acquireWorker() (*appServerWorker, error) {
	a.mu.Lock()
	if a.retired {
		a.mu.Unlock()
		return nil, errors.New("Codex adapter is retired")
	}
	for len(a.idle) > 0 {
		last := len(a.idle) - 1
		worker := a.idle[last]
		a.idle = a.idle[:last]
		if !worker.closed.Load() {
			a.mu.Unlock()
			return worker, nil
		}
	}
	a.mu.Unlock()
	return newAppServerWorker(a.config)
}

func (a *Adapter) releaseWorker(worker *appServerWorker, reusable bool) {
	if worker == nil {
		return
	}
	a.mu.Lock()
	if reusable && !a.retired && !worker.closed.Load() {
		a.idle = append(a.idle, worker)
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	worker.close()
}

type cachedAuth struct {
	modTime   time.Time
	access    string
	accountID string
}

func (a *Adapter) startDirect(ctx context.Context, request provider.Request) (*http.Response, bool, error) {
	auth, err := a.loadAuth()
	if err != nil {
		a.directBlockedUntil.Store(time.Now().Add(time.Minute).UnixNano())
		return nil, true, err
	}
	effort := effectiveReasoningEffort(request.ReasoningEffort, a.config.DefaultReasoning)
	body := map[string]any{
		"model":               request.UpstreamModel,
		"instructions":        modelOnlyInstructions,
		"input":               []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": request.CanonicalInput}}}},
		"tools":               []any{},
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"store":               false,
		"stream":              true,
		"include":             []string{},
		"prompt_cache_key":    "llmserver-codex-text-only-v2",
	}
	if effort != "" {
		body["reasoning"] = map[string]string{"effort": effort}
	}
	if a.config.ServiceTier != "" {
		body["service_tier"] = a.config.ServiceTier
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, false, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.directEndpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, false, fmt.Errorf("create Codex Responses request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+auth.access)
	httpRequest.Header.Set("ChatGPT-Account-ID", auth.accountID)
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", "llmserver/0.3 codex-text-adapter")
	response, err := a.direct.Do(httpRequest)
	if err != nil {
		// A transport error is ambiguous: the upstream may already have begun
		// generating. Do not retry through App Server and risk duplicate usage.
		return nil, false, fmt.Errorf("send Codex Responses request: %w", err)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, false, nil
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDirectErrorBody))
	switch response.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		// Authentication refresh and backend contract drift are delegated to
		// the official App Server fallback. Suppress repeated failed probes for
		// a short interval while preserving automatic recovery.
		a.directBlockedUntil.Store(time.Now().Add(time.Minute).UnixNano())
		return nil, true, fmt.Errorf("Codex Responses returned HTTP %d", response.StatusCode)
	default:
		return nil, false, fmt.Errorf("Codex Responses returned HTTP %d", response.StatusCode)
	}
}

func (a *Adapter) loadAuth() (cachedAuth, error) {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	path, err := codexAuthPath()
	if err != nil {
		return cachedAuth{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return cachedAuth{}, fmt.Errorf("stat current Codex credentials: %w", err)
	}
	if a.auth.access != "" && info.ModTime().Equal(a.auth.modTime) {
		return a.auth, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cachedAuth{}, fmt.Errorf("read current Codex credentials: %w", err)
	}
	var document struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &document) != nil || document.Tokens.AccessToken == "" || document.Tokens.AccountID == "" {
		return cachedAuth{}, errors.New("current Codex credentials are incomplete")
	}
	a.auth = cachedAuth{modTime: info.ModTime(), access: document.Tokens.AccessToken, accountID: document.Tokens.AccountID}
	return a.auth, nil
}

func codexAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	return filepath.Join(codexHome, "auth.json"), nil
}

func newDirectHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 8
	transport.IdleConnTimeout = 5 * time.Minute
	if proxyURL := macOSSystemHTTPSProxy(); proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: transport}
}

func macOSSystemHTTPSProxy() *url.URL {
	if _, err := os.Stat("/usr/sbin/scutil"); err != nil {
		return nil
	}
	output, err := exec.Command("/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return nil
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(parts) == 2 {
			values[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	if values["HTTPSEnable"] != "1" || values["HTTPSProxy"] == "" || values["HTTPSPort"] == "" {
		return nil
	}
	proxyURL, err := url.Parse("http://" + values["HTTPSProxy"] + ":" + values["HTTPSPort"])
	if err != nil {
		return nil
	}
	return proxyURL
}

func consumeDirectStream(ctx context.Context, body io.Reader, requestedModel string, events chan<- provider.Event) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	var dataLines []string
	completed := false
	flush := func() bool {
		if len(dataLines) == 0 {
			return true
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if data == "[DONE]" {
			return true
		}
		var event map[string]any
		decoder := json.NewDecoder(strings.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&event); err != nil {
			send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("decode Codex Responses event: %w", err)})
			return false
		}
		eventType, _ := event["type"].(string)
		switch eventType {
		case "response.output_text.delta":
			delta, _ := event["delta"].(string)
			return delta == "" || send(ctx, events, provider.Event{Type: provider.EventOutputTextDelta, Delta: delta})
		case "response.completed":
			response, ok := event["response"].(map[string]any)
			if !ok {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("Codex completion has no response")})
				return false
			}
			model, _ := response["model"].(string)
			if model == "" {
				model = requestedModel
			}
			usage, _ := response["usage"].(map[string]any)
			inputTokens, inputPresent := directInteger(usage["input_tokens"])
			outputTokens, outputPresent := directInteger(usage["output_tokens"])
			final := &provider.Final{
				EffectiveModel: model,
				Usage: pricing.ReportedUsage{
					InputTokens:  pricing.OptionalCount{Value: inputTokens, Present: inputPresent},
					OutputTokens: pricing.OptionalCount{Value: outputTokens, Present: outputPresent},
				},
			}
			completed = true
			return send(ctx, events, provider.Event{Type: provider.EventCompleted, Final: final})
		case "error", "response.failed", "response.incomplete":
			send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("Codex Responses event %s", eventType)})
			return false
		default:
			return true
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !flush() || completed {
				return
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(dataLines) > 0 && !completed {
		if !flush() || completed {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("read Codex Responses stream: %w", err)})
		return
	}
	if !completed {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("Codex Responses stream ended before completion")})
	}
}

func directInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	return parsed, err == nil && parsed >= 0
}

type appServerWorker struct {
	command    *exec.Cmd
	stdin      io.WriteCloser
	encoder    *json.Encoder
	scanner    *bufio.Scanner
	runtimeDir string
	nextID     int64
	pending    []json.RawMessage
	closed     atomic.Bool
	closeOnce  sync.Once
}

func newAppServerWorker(config Config) (*appServerWorker, error) {
	runtimeDir, err := os.MkdirTemp("", "llmserver-codex-worker-")
	if err != nil {
		return nil, fmt.Errorf("create Codex worker directory: %w", err)
	}
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("secure Codex worker directory: %w", err)
	}
	codexHome := filepath.Join(runtimeDir, "home")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("create isolated Codex home: %w", err)
	}
	if err := copyCodexRuntimeFiles(codexHome); err != nil {
		os.RemoveAll(runtimeDir)
		return nil, err
	}

	args := append([]string{}, config.ExtraArgs...)
	args = append(args,
		"--enable", "respect_system_proxy",
		"--disable", "apps",
		"--disable", "plugins",
		"--disable", "hooks",
		"--disable", "browser_use",
		"--disable", "computer_use",
		"--disable", "image_generation",
		"--disable", "skill_search",
		"app-server", "--stdio",
	)
	command := exec.CommandContext(context.Background(), config.Executable, args...)
	command.Dir = runtimeDir
	command.Stderr = io.Discard
	command.Env = minimalEnvironment(codexHome)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 5 * time.Second
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("open Codex stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		stdin.Close()
		os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("open Codex stdout: %w", err)
	}
	if err := command.Start(); err != nil {
		stdin.Close()
		os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	worker := &appServerWorker{
		command: command, stdin: stdin, encoder: json.NewEncoder(stdin),
		scanner: bufio.NewScanner(stdout), runtimeDir: runtimeDir,
	}
	worker.scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	initializeID := worker.rpcID()
	if err := worker.writeRPC(initializeID, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "llmserver", "version": "0.3.0"},
	}); err != nil {
		worker.close()
		return nil, fmt.Errorf("initialize Codex app-server: %w", err)
	}
	if _, err := worker.waitRPCResult(initializeID); err != nil {
		worker.close()
		return nil, fmt.Errorf("initialize Codex app-server: %w", err)
	}
	if err := worker.encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		worker.close()
		return nil, fmt.Errorf("acknowledge Codex app-server: %w", err)
	}
	return worker, nil
}

func copyCodexRuntimeFiles(targetHome string) error {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate current Codex credentials: %w", err)
	}
	sourceHome := os.Getenv("CODEX_HOME")
	if sourceHome == "" {
		sourceHome = filepath.Join(userHome, ".codex")
	}
	authSource := filepath.Join(sourceHome, "auth.json")
	if err := copyFile(authSource, filepath.Join(targetHome, "auth.json"), 0o600, true); err != nil {
		return fmt.Errorf("prepare isolated Codex credentials: %w", err)
	}
	// A current model cache avoids an unnecessary catalog refresh on worker
	// startup. It contains no credential and is optional.
	_ = copyFile(filepath.Join(sourceHome, "models_cache.json"), filepath.Join(targetHome, "models_cache.json"), 0o600, false)
	return nil
}

func copyFile(source, target string, mode os.FileMode, required bool) error {
	input, err := os.Open(source)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

const modelOnlyInstructions = "You are serving one text-only model request. Never use shell, files, network, MCP, plugins, GUI, subagents, or any tool. Return only the answer requested by the user."

func (w *appServerWorker) run(ctx context.Context, config Config, request provider.Request, events chan<- provider.Event) bool {
	runDir, err := os.MkdirTemp(w.runtimeDir, "run-")
	if err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("create Codex run directory: %w", err)})
		return false
	}
	defer os.RemoveAll(runDir)
	watchDone := make(chan struct{})
	var stopWatch sync.Once
	stopCancellationWatch := func() { stopWatch.Do(func() { close(watchDone) }) }
	defer stopCancellationWatch()
	go func() {
		select {
		case <-ctx.Done():
			w.close()
		case <-watchDone:
		}
	}()

	threadRPCID := w.rpcID()
	if err := w.writeRPC(threadRPCID, "thread/start", map[string]any{
		"approvalPolicy":   "never",
		"baseInstructions": modelOnlyInstructions,
		"cwd":              runDir,
		"ephemeral":        true,
		"model":            request.UpstreamModel,
		"sandbox":          "read-only",
	}); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("start Codex thread: %w", err)})
		return false
	}
	threadRaw, err := w.waitRPCResult(threadRPCID)
	if err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("start Codex thread: %w", err)})
		return false
	}
	var threadResult struct {
		Model  string `json:"model"`
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(threadRaw, &threadResult); err != nil || threadResult.Thread.ID == "" {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("Codex app-server returned an invalid thread")})
		return false
	}

	turnParams := map[string]any{
		"threadId": threadResult.Thread.ID,
		"input":    []map[string]string{{"type": "text", "text": request.CanonicalInput}},
	}
	effort := effectiveReasoningEffort(request.ReasoningEffort, config.DefaultReasoning)
	if effort != "" {
		turnParams["effort"] = effort
	}
	if config.ServiceTier != "" {
		turnParams["serviceTierForTurn"] = config.ServiceTier
	}
	turnRPCID := w.rpcID()
	if err := w.writeRPC(turnRPCID, "turn/start", turnParams); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("start Codex turn: %w", err)})
		return false
	}
	if _, err := w.waitRPCResult(turnRPCID); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("start Codex turn: %w", err)})
		return false
	}

	var output strings.Builder
	var reported pricing.ReportedUsage
	usagePresent := false
	for {
		raw, err := w.nextMessage()
		if err != nil {
			if ctx.Err() == nil {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("read Codex events: %w", err)})
			}
			return false
		}
		var event appServerEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("decode Codex event: %w", err)})
			return false
		}
		switch event.Method {
		case "item/started", "item/completed":
			var params itemLifecycleParams
			if json.Unmarshal(event.Params, &params) != nil || (params.ThreadID != "" && params.ThreadID != threadResult.Thread.ID) {
				continue
			}
			if !safeItemType(params.Item.Type) {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("Codex attempted disallowed item %q", params.Item.Type)})
				return false
			}
			if event.Method == "item/completed" && params.Item.Type == "agentMessage" && output.Len() == 0 && params.Item.Text != "" {
				output.WriteString(params.Item.Text)
				if !send(ctx, events, provider.Event{Type: provider.EventOutputTextDelta, Delta: params.Item.Text}) {
					return false
				}
			}
		case "item/agentMessage/delta":
			var params struct {
				ThreadID string `json:"threadId"`
				Delta    string `json:"delta"`
			}
			if json.Unmarshal(event.Params, &params) != nil || params.Delta == "" || (params.ThreadID != "" && params.ThreadID != threadResult.Thread.ID) {
				continue
			}
			output.WriteString(params.Delta)
			if !send(ctx, events, provider.Event{Type: provider.EventOutputTextDelta, Delta: params.Delta}) {
				return false
			}
		case "thread/tokenUsage/updated":
			var params tokenUsageParams
			if json.Unmarshal(event.Params, &params) == nil && (params.ThreadID == "" || params.ThreadID == threadResult.Thread.ID) {
				reported.InputTokens = pricing.OptionalCount{Value: params.TokenUsage.Last.InputTokens, Present: true}
				reported.OutputTokens = pricing.OptionalCount{Value: params.TokenUsage.Last.OutputTokens, Present: true}
				usagePresent = true
			}
		case "turn/completed":
			var params turnCompletedParams
			if json.Unmarshal(event.Params, &params) != nil || (params.ThreadID != "" && params.ThreadID != threadResult.Thread.ID) {
				continue
			}
			if params.Turn.Status != "completed" {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("Codex turn failed")})
				return false
			}
			if !usagePresent {
				reported = pricing.ReportedUsage{}
			}
			effectiveModel := threadResult.Model
			if effectiveModel == "" {
				effectiveModel = request.UpstreamModel
			}
			stopCancellationWatch()
			final := &provider.Final{OutputText: output.String(), EffectiveModel: effectiveModel, Usage: reported}
			return send(ctx, events, provider.Event{Type: provider.EventCompleted, Final: final}) && !w.closed.Load()
		default:
			// Account, rate-limit, reasoning and status notifications are not
			// part of the public response and require no extra request.
		}
	}
}

func effectiveReasoningEffort(requested, fallback string) string {
	if requested != "" {
		return requested
	}
	return fallback
}

func (w *appServerWorker) rpcID() int64 {
	w.nextID++
	return w.nextID
}

func (w *appServerWorker) writeRPC(id int64, method string, params any) error {
	if w.closed.Load() {
		return errors.New("Codex app-server is closed")
	}
	return w.encoder.Encode(map[string]any{"id": id, "method": method, "params": params})
}

func (w *appServerWorker) waitRPCResult(wantedID int64) (json.RawMessage, error) {
	for {
		raw, err := w.scanMessage()
		if err != nil {
			return nil, err
		}
		var envelope struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, err
		}
		if envelope.ID != wantedID {
			w.pending = append(w.pending, raw)
			continue
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			return nil, errors.New("Codex app-server returned an error")
		}
		return envelope.Result, nil
	}
}

func (w *appServerWorker) nextMessage() (json.RawMessage, error) {
	if len(w.pending) > 0 {
		raw := w.pending[0]
		w.pending = w.pending[1:]
		return raw, nil
	}
	return w.scanMessage()
}

func (w *appServerWorker) scanMessage() (json.RawMessage, error) {
	if !w.scanner.Scan() {
		if err := w.scanner.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("Codex app-server ended before response")
	}
	return append(json.RawMessage(nil), w.scanner.Bytes()...), nil
}

func (w *appServerWorker) close() {
	w.closeOnce.Do(func() {
		w.closed.Store(true)
		if w.stdin != nil {
			_ = w.stdin.Close()
		}
		if w.command != nil {
			_ = w.command.Cancel()
			_ = w.command.Wait()
		}
		_ = os.RemoveAll(w.runtimeDir)
	})
}

func safeItemType(itemType string) bool {
	switch itemType {
	case "userMessage", "agentMessage", "reasoning":
		return true
	default:
		return false
	}
}

type appServerEvent struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type itemLifecycleParams struct {
	ThreadID string `json:"threadId"`
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

type tokenUsageParams struct {
	ThreadID   string `json:"threadId"`
	TokenUsage struct {
		Last struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"last"`
	} `json:"tokenUsage"`
}

type turnCompletedParams struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		Status string `json:"status"`
	} `json:"turn"`
}

func minimalEnvironment(codexHome string) []string {
	allowed := []string{"HOME", "PATH", "TMPDIR", "LANG", "LC_ALL", "USER", "LOGNAME", "SHELL", "SSL_CERT_FILE", "SSL_CERT_DIR"}
	result := make([]string, 0, len(allowed)+1)
	for _, key := range allowed {
		if value, exists := os.LookupEnv(key); exists {
			result = append(result, key+"="+value)
		}
	}
	return append(result, "CODEX_HOME="+codexHome)
}

func send(ctx context.Context, target chan<- provider.Event, event provider.Event) bool {
	select {
	case <-ctx.Done():
		return false
	case target <- event:
		return true
	}
}
