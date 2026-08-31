package workbuddy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
const acpDrainTimeout = 8 * time.Second
const defaultWarmupTimeout = 30 * time.Second
const workerReplenishRetry = 5 * time.Second

const systemPrompt = "You are serving one text-only language model request. Never use tools, files, shell, network, MCP, plugins, GUI, subagents, or background tasks. Return only the answer requested by the user."
const warmupPrompt = "Reply only OK."

type Config struct {
	ProviderID       string
	Executable       string
	ExpectedVersion  string
	ExtraArgs        []string
	MaxConcurrency   int
	DefaultReasoning string
	WarmupEnabled    bool
	WarmupModel      string
	WarmupTimeout    time.Duration
}

// Adapter uses WorkBuddy's ACP stdio transport, the same long-lived integration
// path intended for IDE and Web clients. Every public request creates a fresh
// ACP session and temporary working directory; model and reasoning are set on
// that session, so workers can be reused without sharing conversation state.
type Adapter struct {
	config   Config
	target   int
	slot     chan struct{}
	mu       sync.Mutex
	idle     []*acpWorker
	live     int
	creating int
	ready    chan struct{}
	retired  bool
}

func New(config Config) (*Adapter, error) {
	if config.ProviderID == "" || !filepath.IsAbs(config.Executable) {
		return nil, errors.New("WorkBuddy provider id and absolute executable are required")
	}
	output, err := exec.Command(config.Executable, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("probe WorkBuddy executable: %w", err)
	}
	version := strings.TrimSpace(string(output))
	if config.ExpectedVersion != "" && !strings.HasPrefix(version, config.ExpectedVersion) {
		return nil, fmt.Errorf("WorkBuddy version %q does not match expected prefix %q", version, config.ExpectedVersion)
	}
	if config.MaxConcurrency == 0 {
		config.MaxConcurrency = 2
	}
	if config.WarmupEnabled && strings.TrimSpace(config.WarmupModel) == "" {
		return nil, errors.New("WorkBuddy warmup model is required when warmup is enabled")
	}
	if config.WarmupTimeout <= 0 {
		config.WarmupTimeout = defaultWarmupTimeout
	}
	target := min(config.MaxConcurrency, 2)
	adapter := &Adapter{
		config: config,
		target: target,
		slot:   make(chan struct{}, config.MaxConcurrency),
		ready:  make(chan struct{}),
	}
	// Initialize the common two-lane pool in parallel. When enabled, each lane
	// performs one tiny real generation so Node startup, authentication, TLS and
	// the upstream model route are warm before the public listener is ready.
	type workerResult struct {
		worker *acpWorker
		err    error
	}
	results := make(chan workerResult, target)
	for range target {
		go func() {
			worker, workerErr := prepareACPWorker(config)
			results <- workerResult{worker: worker, err: workerErr}
		}()
	}
	workers := make([]*acpWorker, 0, target)
	var firstErr error
	for range target {
		result := <-results
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		workers = append(workers, result.worker)
	}
	if firstErr != nil {
		for _, worker := range workers {
			worker.close()
		}
		return nil, firstErr
	}
	adapter.idle = workers
	adapter.live = len(workers)
	return adapter, nil
}

func (a *Adapter) ID() string { return a.config.ProviderID }

func (a *Adapter) Start(ctx context.Context, request provider.Request) (<-chan provider.Event, error) {
	select {
	case a.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	effort := request.ReasoningEffort
	if effort == "" {
		effort = a.config.DefaultReasoning
	}
	worker, err := a.acquireWorker(ctx)
	if err != nil {
		<-a.slot
		return nil, err
	}
	events := make(chan provider.Event)
	go func() {
		defer close(events)
		defer func() { <-a.slot }()
		reusable := worker.run(ctx, request, effort, events)
		a.releaseWorker(worker, reusable)
	}()
	return events, nil
}

func (a *Adapter) Retire() {
	a.mu.Lock()
	a.retired = true
	idle := a.idle
	a.idle = nil
	a.live -= len(idle)
	a.notifyLocked()
	a.mu.Unlock()
	for _, worker := range idle {
		worker.close()
	}
}

func (a *Adapter) acquireWorker(ctx context.Context) (*acpWorker, error) {
	for {
		a.mu.Lock()
		if a.retired {
			a.mu.Unlock()
			return nil, errors.New("WorkBuddy adapter is retired")
		}
		for len(a.idle) > 0 {
			last := len(a.idle) - 1
			worker := a.idle[last]
			a.idle = a.idle[:last]
			if !worker.closed.Load() {
				a.mu.Unlock()
				return worker, nil
			}
			a.live--
		}
		if a.live+a.creating < cap(a.slot) {
			a.creating++
			a.mu.Unlock()
			worker, err := prepareACPWorker(a.config)
			a.mu.Lock()
			a.creating--
			if err == nil && !a.retired {
				a.live++
				a.notifyLocked()
				a.mu.Unlock()
				return worker, nil
			}
			retired := a.retired
			a.notifyLocked()
			a.scheduleReplenishLocked()
			a.mu.Unlock()
			if worker != nil {
				worker.close()
			}
			if retired {
				return nil, errors.New("WorkBuddy adapter is retired")
			}
			return nil, err
		}
		ready := a.ready
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ready:
		}
	}
}

func (a *Adapter) releaseWorker(worker *acpWorker, reusable bool) {
	if worker == nil {
		return
	}
	a.mu.Lock()
	if reusable && !a.retired && !worker.closed.Load() && len(a.idle) < a.config.MaxConcurrency {
		a.idle = append(a.idle, worker)
		a.notifyLocked()
		a.mu.Unlock()
		return
	}
	a.live--
	a.notifyLocked()
	a.scheduleReplenishLocked()
	a.mu.Unlock()
	worker.close()
}

func (a *Adapter) notifyLocked() {
	close(a.ready)
	a.ready = make(chan struct{})
}

func (a *Adapter) scheduleReplenishLocked() {
	for !a.retired && a.live+a.creating < a.target {
		a.creating++
		go a.replenishWorker()
	}
}

func (a *Adapter) replenishWorker() {
	worker, err := prepareACPWorker(a.config)
	a.mu.Lock()
	a.creating--
	if err == nil && !a.retired {
		a.live++
		a.idle = append(a.idle, worker)
		liveWorkers := a.live
		a.notifyLocked()
		a.mu.Unlock()
		slog.Info("WorkBuddy worker pool replenished", "provider", a.config.ProviderID, "live_workers", liveWorkers)
		return
	}
	retired := a.retired
	a.notifyLocked()
	a.mu.Unlock()
	if worker != nil {
		worker.close()
	}
	if err != nil && !retired {
		slog.Warn("WorkBuddy worker replenishment failed", "provider", a.config.ProviderID, "error", err)
		time.AfterFunc(workerReplenishRetry, func() {
			a.mu.Lock()
			a.scheduleReplenishLocked()
			a.mu.Unlock()
		})
	}
}

type acpWorker struct {
	command    *exec.Cmd
	stdin      io.WriteCloser
	encoder    *json.Encoder
	scanner    *bufio.Scanner
	runtimeDir string
	nextID     int64
	closed     atomic.Bool
	closeOnce  sync.Once
}

func prepareACPWorker(config Config) (*acpWorker, error) {
	worker, err := newACPWorker(config)
	if err != nil {
		return nil, err
	}
	if !config.WarmupEnabled {
		return worker, nil
	}
	startedAt := time.Now()
	final, err := warmACPWorker(worker, config)
	if err == nil {
		slog.Info("WorkBuddy worker warmed",
			"provider", config.ProviderID,
			"model", config.WarmupModel,
			"duration_ms", time.Since(startedAt).Milliseconds(),
			"input_tokens", final.Usage.InputTokens.Value,
			"output_tokens", final.Usage.OutputTokens.Value,
		)
		return worker, nil
	}
	// A failed warm-up must not make launchd repeatedly restart the whole
	// service and spend more credits. Replace the tainted/cancelled process with
	// a clean persistent worker and let the provider remain available.
	worker.close()
	slog.Warn("WorkBuddy worker warmup failed; keeping an un-warmed worker",
		"provider", config.ProviderID,
		"model", config.WarmupModel,
		"duration_ms", time.Since(startedAt).Milliseconds(),
		"error", err,
	)
	return newACPWorker(config)
}

func warmACPWorker(worker *acpWorker, config Config) (*provider.Final, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.WarmupTimeout)
	defer cancel()
	events := make(chan provider.Event)
	done := make(chan bool, 1)
	go func() {
		reusable := worker.run(ctx, provider.Request{
			RunID:          "workbuddy-internal-warmup",
			UpstreamModel:  config.WarmupModel,
			CanonicalInput: warmupPrompt,
		}, config.DefaultReasoning, events)
		close(events)
		done <- reusable
	}()
	var final *provider.Final
	var failure error
	for event := range events {
		switch event.Type {
		case provider.EventCompleted:
			final = event.Final
		case provider.EventFailed:
			failure = event.Err
		}
	}
	reusable := <-done
	if ctx.Err() != nil {
		return nil, fmt.Errorf("warmup timed out: %w", ctx.Err())
	}
	if failure != nil {
		return nil, failure
	}
	if !reusable || final == nil {
		return nil, errors.New("warmup did not complete with reusable worker")
	}
	return final, nil
}

func newACPWorker(config Config) (*acpWorker, error) {
	runtimeDir, err := os.MkdirTemp("", "llmserver-workbuddy-worker-")
	if err != nil {
		return nil, fmt.Errorf("create WorkBuddy worker directory: %w", err)
	}
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("secure WorkBuddy worker directory: %w", err)
	}
	args := append([]string{}, config.ExtraArgs...)
	args = append(args,
		"--acp",
		"--acp-transport", "stdio",
		"--no-session-persistence",
		"--tools", "",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--setting-sources", "",
		"--permission-mode", "dontAsk",
		"--max-turns", "1",
		"--system-prompt", systemPrompt,
	)
	command := exec.CommandContext(context.Background(), config.Executable, args...)
	command.Dir = runtimeDir
	command.Stderr = io.Discard
	command.Env = minimalEnvironment(runtimeDir)
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
		return nil, fmt.Errorf("open WorkBuddy ACP stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		stdin.Close()
		os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("open WorkBuddy ACP stdout: %w", err)
	}
	if err := command.Start(); err != nil {
		stdin.Close()
		os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("start WorkBuddy ACP: %w", err)
	}
	worker := &acpWorker{
		command: command, stdin: stdin, encoder: json.NewEncoder(stdin),
		scanner: bufio.NewScanner(stdout), runtimeDir: runtimeDir,
	}
	worker.scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	initializeID := worker.rpcID()
	if err := worker.writeRPC(initializeID, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientInfo":         map[string]string{"name": "llmserver", "version": "0.3.0"},
		"clientCapabilities": map[string]any{},
	}); err != nil {
		worker.close()
		return nil, fmt.Errorf("initialize WorkBuddy ACP: %w", err)
	}
	if _, err := worker.waitRPCResult(initializeID); err != nil {
		worker.close()
		return nil, fmt.Errorf("initialize WorkBuddy ACP: %w", err)
	}
	return worker, nil
}

func (w *acpWorker) run(ctx context.Context, request provider.Request, effort string, events chan<- provider.Event) bool {
	runDir, err := os.MkdirTemp(w.runtimeDir, "run-")
	if err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("create WorkBuddy run directory: %w", err)})
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

	sessionRPCID := w.rpcID()
	if err := w.writeRPC(sessionRPCID, "session/new", map[string]any{
		"cwd": runDir, "mcpServers": []any{},
	}); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("start WorkBuddy session: %w", err)})
		return false
	}
	sessionRaw, err := w.waitRPCResult(sessionRPCID)
	if err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("start WorkBuddy session: %w", err)})
		return false
	}
	var sessionResult struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(sessionRaw, &sessionResult); err != nil || sessionResult.SessionID == "" {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("WorkBuddy ACP returned an invalid session")})
		return false
	}

	modelRPCID := w.rpcID()
	if err := w.writeRPC(modelRPCID, "session/set_model", map[string]any{
		"sessionId": sessionResult.SessionID, "modelId": request.UpstreamModel,
	}); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("select WorkBuddy model: %w", err)})
		return false
	}
	if _, err := w.waitRPCResult(modelRPCID); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("select WorkBuddy model: %w", err)})
		return false
	}
	if effort != "" {
		effortRPCID := w.rpcID()
		if err := w.writeRPC(effortRPCID, "session/set_config_option", map[string]any{
			"sessionId": sessionResult.SessionID,
			"configId":  "thought_level",
			"value":     effort,
		}); err != nil {
			send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("set WorkBuddy reasoning effort: %w", err)})
			return false
		}
		if _, err := w.waitRPCResult(effortRPCID); err != nil {
			send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("set WorkBuddy reasoning effort: %w", err)})
			return false
		}
	}

	promptRPCID := w.rpcID()
	if err := w.writeRPC(promptRPCID, "session/prompt", map[string]any{
		"sessionId": sessionResult.SessionID,
		"prompt":    []map[string]string{{"type": "text", "text": request.CanonicalInput}},
	}); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("start WorkBuddy prompt: %w", err)})
		return false
	}

	var output strings.Builder
	reported := pricing.ReportedUsage{}
	usagePresent := false
	credit := ""
	effectiveModel := request.UpstreamModel
	modelDone := false
	completionSent := false
	var drainTimer *time.Timer
	defer func() {
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()

	complete := func() bool {
		if completionSent {
			return true
		}
		var costs []provider.CostObservation
		if credit != "" {
			if parsed, parseErr := pricing.ParseDecimal(credit); parseErr == nil && parsed.Nanos() >= 0 {
				costs = []provider.CostObservation{{Unit: "POINTS", Total: parsed.String()}}
			}
		}
		stopCancellationWatch()
		final := &provider.Final{OutputText: output.String(), EffectiveModel: effectiveModel, Usage: reported, Costs: costs}
		if !send(ctx, events, provider.Event{Type: provider.EventCompleted, Final: final}) {
			return false
		}
		completionSent = true
		drainTimer = time.AfterFunc(acpDrainTimeout, w.close)
		// ACP waits up to five seconds for a UI conversation title after the
		// model and usage are already complete. Cancel only that remaining
		// session work so the runtime can return to the pool immediately.
		_ = w.writeNotification("session/cancel", map[string]any{"sessionId": sessionResult.SessionID})
		return true
	}

	for {
		raw, err := w.scanMessage()
		if err != nil {
			if completionSent {
				return false
			}
			if ctx.Err() == nil {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("read WorkBuddy ACP events: %w", err)})
			}
			return false
		}
		var envelope acpEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			if completionSent {
				return false
			}
			send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("decode WorkBuddy ACP event: %w", err)})
			return false
		}
		if envelope.ID == promptRPCID {
			if completionSent {
				return !w.closed.Load()
			}
			if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
				if !completionSent {
					send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("WorkBuddy prompt failed")})
				}
				return false
			}
			var result struct {
				StopReason string `json:"stopReason"`
			}
			if json.Unmarshal(envelope.Result, &result) != nil || result.StopReason != "end_turn" {
				if !completionSent {
					send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("WorkBuddy prompt did not complete")})
				}
				return false
			}
			if !completionSent && !complete() {
				return false
			}
			return !w.closed.Load()
		}
		if envelope.Method != "session/update" {
			continue
		}
		var params acpSessionUpdateParams
		if json.Unmarshal(envelope.Params, &params) != nil || params.SessionID != sessionResult.SessionID {
			continue
		}
		if params.Update.Meta.ResponseModelID != "" {
			effectiveModel = params.Update.Meta.ResponseModelID
		}
		switch params.Update.SessionUpdate {
		case "agent_message_chunk":
			if params.Update.Content.Type != "text" {
				if !completionSent {
					send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("WorkBuddy attempted disallowed content %q", params.Update.Content.Type)})
				}
				return false
			}
			if params.Update.Content.Text != "" {
				output.WriteString(params.Update.Content.Text)
				if !completionSent && !send(ctx, events, provider.Event{Type: provider.EventOutputTextDelta, Delta: params.Update.Content.Text}) {
					return false
				}
			}
		case "agent_thought_chunk", "session_info_update", "usage_update":
			if params.Update.Meta.AgentPhase != nil && params.Update.Meta.AgentPhase.Phase == "model_done" {
				modelDone = true
			}
			if params.Update.Meta.Usage != nil {
				reported.InputTokens = pricing.OptionalCount{Value: params.Update.Meta.Usage.PromptTokens, Present: true}
				reported.OutputTokens = pricing.OptionalCount{Value: params.Update.Meta.Usage.CompletionTokens, Present: true}
				usagePresent = true
				credit = decimalText(params.Update.Meta.Usage.Credit)
			}
		case "tool_call", "tool_call_update", "interruption_request":
			if !completionSent {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("WorkBuddy attempted disallowed action %q", params.Update.SessionUpdate)})
			}
			return false
		default:
			// Model, mode, title, command catalog and other UI-only updates are
			// intentionally ignored.
		}
		if !completionSent && modelDone && usagePresent && !complete() {
			return false
		}
	}
}

type acpEnvelope struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type acpSessionUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
		Content       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Meta struct {
			ResponseModelID string `json:"codebuddy.ai/responseModelId"`
			AgentPhase      *struct {
				Phase string `json:"phase"`
			} `json:"codebuddy.ai/agentPhase"`
			Usage *struct {
				PromptTokens     int64           `json:"prompt_tokens"`
				CompletionTokens int64           `json:"completion_tokens"`
				Credit           json.RawMessage `json:"credit"`
			} `json:"usage"`
		} `json:"_meta"`
	} `json:"update"`
}

func decimalText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return strings.Trim(string(raw), `"`)
}

func (w *acpWorker) rpcID() int64 {
	w.nextID++
	return w.nextID
}

func (w *acpWorker) writeRPC(id int64, method string, params any) error {
	if w.closed.Load() {
		return errors.New("WorkBuddy ACP is closed")
	}
	return w.encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func (w *acpWorker) writeNotification(method string, params any) error {
	if w.closed.Load() {
		return errors.New("WorkBuddy ACP is closed")
	}
	return w.encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (w *acpWorker) waitRPCResult(wantedID int64) (json.RawMessage, error) {
	for {
		raw, err := w.scanMessage()
		if err != nil {
			return nil, err
		}
		var envelope acpEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, err
		}
		if envelope.ID != wantedID {
			continue
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			return nil, errors.New("WorkBuddy ACP returned an error")
		}
		return envelope.Result, nil
	}
}

func (w *acpWorker) scanMessage() (json.RawMessage, error) {
	if !w.scanner.Scan() {
		if err := w.scanner.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("WorkBuddy ACP ended before response")
	}
	line := w.scanner.Bytes()
	if index := bytesIndexByte(line, '{'); index > 0 {
		line = line[index:]
	}
	return append(json.RawMessage(nil), line...), nil
}

func bytesIndexByte(value []byte, target byte) int {
	for index, current := range value {
		if current == target {
			return index
		}
	}
	return -1
}

func (w *acpWorker) close() {
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

func minimalEnvironment(runDir string) []string {
	allowed := []string{"HOME", "PATH", "TMPDIR", "LANG", "LC_ALL", "USER", "LOGNAME", "SHELL", "SSL_CERT_FILE", "SSL_CERT_DIR"}
	result := make([]string, 0, len(allowed)+20)
	for _, key := range allowed {
		if value, exists := os.LookupEnv(key); exists {
			result = append(result, key+"="+value)
		}
	}
	return append(result,
		"CODEBUDDY_CODE_DISABLE_BACKGROUND_TASKS=1",
		"CODEBUDDY_CODE_ENABLE_TELEMETRY=0",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_AUTOUPDATER=1",
		"CODEBUDDY_DISABLE_HOT_RELOAD=1",
		"CODEBUDDY_SKIP_BUILTIN_MARKETPLACE=1",
		"CODEBUDDY_DISABLE_CRON=1",
		"CODEBUDDY_MAIN_AGENT_ENABLED=0",
		"CODEBUDDY_REPL_ENABLED=0",
		"CODEBUDDY_DISABLE_AUTO_MEMORY=1",
		"CODEBUDDY_CODE_DISABLE_AUTO_MEMORY=1",
		"CODEBUDDY_DISABLE_SYSTEM_REMINDER_MD=1",
		"CODEBUDDY_CODE_DISABLE_TERMINAL_TITLE=1",
		"CODEBUDDY_CODE_DISABLE_SESSION_SUMMARY=1",
		"CODEBUDDY_DISABLE_WORKFLOWS=1",
		"CODEBUDDY_PROMPT_SUGGESTION_DISABLED=1",
		"CODEBUDDY_CODE_DEBUG_LOGS_DIR="+runDir,
		"CODEBUDDY_SESSION_LOG="+filepath.Join(runDir, "session.log"),
	)
}

func send(ctx context.Context, target chan<- provider.Event, event provider.Event) bool {
	select {
	case <-ctx.Done():
		return false
	case target <- event:
		return true
	}
}
