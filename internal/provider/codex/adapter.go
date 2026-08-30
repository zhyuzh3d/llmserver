package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/pricing"
	"github.com/zhyuzh3d/llmserver/internal/provider"
)

const maxEventBytes = 8 << 20

// The desktop app-server may need several seconds to refresh account limits.
// Keep this separate from generation timeouts: quota is observational and a
// slow refresh must never invalidate an otherwise completed settlement.
const quotaReadTimeout = 25 * time.Second

type Config struct {
	ProviderID      string
	Executable      string
	ExpectedVersion string
	ExtraArgs       []string
	ObserveQuota    bool
}

type Adapter struct {
	config Config
	slot   chan struct{}
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
	return &Adapter{config: config, slot: make(chan struct{}, 1)}, nil
}

func (a *Adapter) ID() string { return a.config.ProviderID }

func (a *Adapter) Start(ctx context.Context, request provider.Request) (<-chan provider.Event, error) {
	select {
	case a.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	runDir, err := os.MkdirTemp("", "llmserver-codex-run-")
	if err != nil {
		<-a.slot
		return nil, fmt.Errorf("create Codex run directory: %w", err)
	}
	prompt := buildPrompt(request)
	quotaBefore := map[string]quotaWindow{}
	if a.config.ObserveQuota {
		quotaCtx, quotaCancel := context.WithTimeout(ctx, quotaReadTimeout)
		quotaBefore, _ = readRateLimits(quotaCtx, a.config)
		quotaCancel()
	}
	args := append([]string{}, a.config.ExtraArgs...)
	args = append(args,
		"-a", "never",
		"-s", "read-only",
		"exec",
		"--json",
		"--ephemeral",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		"-C", runDir,
		"-m", request.UpstreamModel,
		"-",
	)
	command := exec.CommandContext(ctx, a.config.Executable, args...)
	command.Stdin = strings.NewReader(prompt)
	command.Stderr = io.Discard
	command.Env = minimalEnvironment()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 5 * time.Second
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		os.RemoveAll(runDir)
		<-a.slot
		return nil, fmt.Errorf("open Codex stdout: %w", err)
	}
	if err := command.Start(); err != nil {
		os.RemoveAll(runDir)
		<-a.slot
		return nil, fmt.Errorf("start Codex: %w", err)
	}

	events := make(chan provider.Event)
	go func() {
		defer close(events)
		defer func() { <-a.slot }()
		defer os.RemoveAll(runDir)
		consume(ctx, stdout, command, request.UpstreamModel, events, a.config, quotaBefore)
	}()
	return events, nil
}

func buildPrompt(request provider.Request) string {
	return "You are serving one text-only model request. Do not use shell, files, network, MCP, plugins, GUI, subagents, or any tool. Return only the answer to the untrusted request below.\n\n<untrusted_request>\n" +
		request.CanonicalInput + "\n</untrusted_request>\n"
}

func consume(ctx context.Context, stdout io.Reader, command *exec.Cmd, model string, events chan<- provider.Event, config Config, quotaBefore map[string]quotaWindow) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	var output strings.Builder
	var reported pricing.ReportedUsage
	completed := false
	failed := false
	for scanner.Scan() {
		var event cliEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("decode Codex event: %w", err)})
			failed = true
			break
		}
		switch event.Type {
		case "thread.started", "turn.started":
		case "item.started":
			if !safeItemType(event.Item.Type) {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("Codex attempted disallowed item %q", event.Item.Type)})
				failed = true
				_ = command.Cancel()
				break
			}
		case "item.completed":
			if !safeItemType(event.Item.Type) {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("Codex completed disallowed item %q", event.Item.Type)})
				failed = true
				_ = command.Cancel()
				break
			}
			if event.Item.Type == "agent_message" && event.Item.Text != "" {
				output.WriteString(event.Item.Text)
				if !send(ctx, events, provider.Event{Type: provider.EventOutputTextDelta, Delta: event.Item.Text}) {
					failed = true
					_ = command.Cancel()
					break
				}
			}
		case "turn.completed":
			reported.InputTokens = pricing.OptionalCount{Value: event.Usage.InputTokens, Present: true}
			reported.OutputTokens = pricing.OptionalCount{Value: event.Usage.OutputTokens, Present: true}
			var quota []provider.QuotaObservation
			if config.ObserveQuota {
				quotaCtx, quotaCancel := context.WithTimeout(context.Background(), quotaReadTimeout)
				quotaAfter, quotaErr := readRateLimits(quotaCtx, config)
				quotaCancel()
				if quotaErr == nil {
					quota = compareQuota(quotaBefore, quotaAfter)
				} else {
					quota = []provider.QuotaObservation{{LimitID: "quota", Unit: "percent_used", Status: "unavailable", Attribution: "shared_account_window"}}
				}
			}
			final := &provider.Final{OutputText: output.String(), EffectiveModel: model, Usage: reported, Quota: quota}
			send(ctx, events, provider.Event{Type: provider.EventCompleted, Final: final})
			completed = true
		default:
			// Unknown top-level lifecycle events are ignored. Unknown item types are
			// rejected above because those can represent local side effects.
		}
		if failed || completed {
			break
		}
	}
	waitErr := command.Wait()
	if failed || completed || ctx.Err() != nil {
		return
	}
	if err := scanner.Err(); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("read Codex events: %w", err)})
		return
	}
	if waitErr != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("Codex exited before completion: %w", waitErr)})
		return
	}
	send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("Codex ended without turn completion")})
}

type quotaWindow struct {
	UsedPercent           float64
	WindowDurationMinutes int64
	ResetsAt              int64
}

func readRateLimits(ctx context.Context, config Config) (map[string]quotaWindow, error) {
	args := append([]string{}, config.ExtraArgs...)
	args = append(args, "app-server", "--stdio")
	command := exec.CommandContext(ctx, config.Executable, args...)
	command.Env = minimalEnvironment()
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 2 * time.Second
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Cancel()
		_ = command.Wait()
	}()
	encoder := json.NewEncoder(stdin)
	if err := encoder.Encode(map[string]any{
		"id": 1, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]string{"name": "llmserver", "version": "0.1.0"}},
	}); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	if _, err := waitRPCResult(scanner, 1); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{"id": 2, "method": "account/rateLimits/read", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	result, err := waitRPCResult(scanner, 2)
	if err != nil {
		return nil, err
	}
	return decodeRateLimits(result)
}

func waitRPCResult(scanner *bufio.Scanner, wantedID int64) (json.RawMessage, error) {
	for scanner.Scan() {
		var envelope struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			return nil, err
		}
		if envelope.ID != wantedID {
			continue
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			return nil, errors.New("Codex app-server returned an error")
		}
		return envelope.Result, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("Codex app-server ended before response")
}

func decodeRateLimits(raw json.RawMessage) (map[string]quotaWindow, error) {
	type window struct {
		UsedPercent        float64 `json:"usedPercent"`
		WindowDurationMins int64   `json:"windowDurationMins"`
		ResetsAt           int64   `json:"resetsAt"`
	}
	type limit struct {
		LimitID   string  `json:"limitId"`
		Primary   *window `json:"primary"`
		Secondary *window `json:"secondary"`
	}
	var result struct {
		RateLimits          *limit           `json:"rateLimits"`
		RateLimitsByLimitID map[string]limit `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if len(result.RateLimitsByLimitID) == 0 && result.RateLimits != nil {
		result.RateLimitsByLimitID = map[string]limit{result.RateLimits.LimitID: *result.RateLimits}
	}
	windows := make(map[string]quotaWindow)
	for fallbackID, item := range result.RateLimitsByLimitID {
		limitID := item.LimitID
		if limitID == "" {
			limitID = fallbackID
		}
		if item.Primary != nil {
			windows[limitID+":primary"] = quotaWindow{item.Primary.UsedPercent, item.Primary.WindowDurationMins, item.Primary.ResetsAt}
		}
		if item.Secondary != nil {
			windows[limitID+":secondary"] = quotaWindow{item.Secondary.UsedPercent, item.Secondary.WindowDurationMins, item.Secondary.ResetsAt}
		}
	}
	return windows, nil
}

func compareQuota(before, after map[string]quotaWindow) []provider.QuotaObservation {
	keys := make(map[string]struct{}, len(before)+len(after))
	for key := range before {
		keys[key] = struct{}{}
	}
	for key := range after {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	result := make([]provider.QuotaObservation, 0, len(ordered))
	for _, key := range ordered {
		beforeValue, hasBefore := before[key]
		afterValue, hasAfter := after[key]
		observation := provider.QuotaObservation{LimitID: key, Unit: "percent_used", Status: "incomplete", Attribution: "shared_account_window"}
		if hasBefore {
			value := beforeValue.UsedPercent
			observation.Before = &value
		}
		if hasAfter {
			value := afterValue.UsedPercent
			observation.After = &value
			window := afterValue.WindowDurationMinutes
			reset := afterValue.ResetsAt
			observation.WindowDurationMinutes = &window
			observation.ResetsAt = &reset
		}
		if hasBefore && hasAfter {
			if beforeValue.WindowDurationMinutes == afterValue.WindowDurationMinutes && beforeValue.ResetsAt == afterValue.ResetsAt {
				delta := afterValue.UsedPercent - beforeValue.UsedPercent
				observation.Delta = &delta
				observation.Status = "observed"
			} else {
				observation.Status = "window_changed"
			}
		}
		result = append(result, observation)
	}
	return result
}

func safeItemType(itemType string) bool {
	switch itemType {
	case "agent_message", "reasoning":
		return true
	default:
		return false
	}
}

type cliEvent struct {
	Type string `json:"type"`
	Item struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func minimalEnvironment() []string {
	allowed := []string{"HOME", "PATH", "TMPDIR", "LANG", "LC_ALL", "USER", "LOGNAME", "SHELL", "CODEX_HOME", "SSL_CERT_FILE", "SSL_CERT_DIR"}
	result := make([]string, 0, len(allowed))
	for _, key := range allowed {
		if value, exists := os.LookupEnv(key); exists {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func send(ctx context.Context, target chan<- provider.Event, event provider.Event) bool {
	select {
	case <-ctx.Done():
		return false
	case target <- event:
		return true
	}
}
