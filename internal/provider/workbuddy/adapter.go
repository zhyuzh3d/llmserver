package workbuddy

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
	"strings"
	"syscall"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/pricing"
	"github.com/zhyuzh3d/llmserver/internal/provider"
)

const maxEventBytes = 8 << 20

const systemPrompt = "You are serving one text-only language model request. Never use tools, files, shell, network, MCP, plugins, GUI, subagents, or background tasks. Return only the answer requested by the user."

type Config struct {
	ProviderID      string
	Executable      string
	ExpectedVersion string
	ExtraArgs       []string
}

type Adapter struct {
	config Config
	slot   chan struct{}
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
	return &Adapter{config: config, slot: make(chan struct{}, 1)}, nil
}

func (a *Adapter) ID() string { return a.config.ProviderID }

func (a *Adapter) Start(ctx context.Context, request provider.Request) (<-chan provider.Event, error) {
	select {
	case a.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	runDir, err := os.MkdirTemp("", "llmserver-workbuddy-run-")
	if err != nil {
		<-a.slot
		return nil, fmt.Errorf("create WorkBuddy run directory: %w", err)
	}
	args := append([]string{}, a.config.ExtraArgs...)
	args = append(args,
		"-p",
		"--output-format", "stream-json",
		"--input-format", "text",
		"--include-partial-messages",
		"--tools", "",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--setting-sources", "",
		"--permission-mode", "dontAsk",
		"--max-turns", "1",
	)
	if request.RunID != "" {
		args = append(args, "--session-id", request.RunID)
	}
	args = append(args, "--model", request.UpstreamModel, "--system-prompt", systemPrompt)
	command := exec.CommandContext(ctx, a.config.Executable, args...)
	command.Dir = runDir
	command.Stdin = strings.NewReader(request.CanonicalInput)
	command.Stderr = io.Discard
	command.Env = minimalEnvironment(runDir)
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
		return nil, fmt.Errorf("open WorkBuddy stdout: %w", err)
	}
	if err := command.Start(); err != nil {
		os.RemoveAll(runDir)
		<-a.slot
		return nil, fmt.Errorf("start WorkBuddy: %w", err)
	}

	events := make(chan provider.Event)
	go func() {
		defer close(events)
		defer func() { <-a.slot }()
		defer os.RemoveAll(runDir)
		consume(ctx, stdout, command, request.UpstreamModel, runDir, request.RunID, events)
	}()
	return events, nil
}

func consume(ctx context.Context, stdout io.Reader, command *exec.Cmd, requestedModel, runDir, runID string, events chan<- provider.Event) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	var streamed strings.Builder
	var finalText string
	effectiveModel := requestedModel
	completed := false
	failed := false
	for scanner.Scan() {
		var event cliEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("decode WorkBuddy event: %w", err)})
			failed = true
			break
		}
		switch event.Type {
		case "stream_event":
			if event.Event.Type == "content_block_start" && !safeContentType(event.Event.ContentBlock.Type) {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("WorkBuddy attempted disallowed content %q", event.Event.ContentBlock.Type)})
				failed = true
				_ = command.Cancel()
				break
			}
			if event.Event.Type == "content_block_delta" && event.Event.Delta.Type == "text_delta" && event.Event.Delta.Text != "" {
				streamed.WriteString(event.Event.Delta.Text)
				if !send(ctx, events, provider.Event{Type: provider.EventOutputTextDelta, Delta: event.Event.Delta.Text}) {
					failed = true
					_ = command.Cancel()
					break
				}
			}
		case "assistant":
			if event.Message.Model != "" {
				effectiveModel = event.Message.Model
			}
			for _, content := range event.Message.Content {
				if !safeContentType(content.Type) {
					send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("WorkBuddy attempted disallowed content %q", content.Type)})
					failed = true
					_ = command.Cancel()
					break
				}
				if content.Type == "text" && content.Text != "" {
					finalText += content.Text
				}
			}
		case "result":
			if event.IsError || event.Subtype != "success" {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("WorkBuddy run failed")})
				failed = true
				break
			}
			if event.Result != "" {
				finalText = event.Result
			}
			if streamed.Len() == 0 && finalText != "" {
				if !send(ctx, events, provider.Event{Type: provider.EventOutputTextDelta, Delta: finalText}) {
					failed = true
					break
				}
			}
			usage := pricing.ReportedUsage{
				InputTokens:  pricing.OptionalCount{Value: event.Usage.InputTokens, Present: true},
				OutputTokens: pricing.OptionalCount{Value: event.Usage.OutputTokens, Present: true},
			}
			var costs []provider.CostObservation
			if credit, found := readSessionCredit(ctx, runDir, runID); found {
				costs = []provider.CostObservation{{Unit: "POINTS", Total: credit}}
			} else if cost, err := pricing.ParseDecimal(event.TotalCostUSD.String()); err == nil && cost.Nanos() >= 0 {
				costs = []provider.CostObservation{{Unit: "USD", Total: cost.String()}}
			}
			final := &provider.Final{OutputText: finalText, EffectiveModel: effectiveModel, Usage: usage, Costs: costs}
			send(ctx, events, provider.Event{Type: provider.EventCompleted, Final: final})
			completed = true
		case "system", "file-history-snapshot":
		default:
			// Unknown lifecycle records carry no local action by themselves. Tool
			// content is rejected in assistant and raw stream events above.
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
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("read WorkBuddy events: %w", err)})
		return
	}
	if waitErr != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("WorkBuddy exited before completion: %w", waitErr)})
		return
	}
	send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("WorkBuddy ended without result")})
}

func readSessionCredit(ctx context.Context, runDir, runID string) (string, bool) {
	if runDir == "" || runID == "" || filepath.Base(runID) != runID {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	resolvedDir, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		resolvedDir = filepath.Clean(runDir)
	}
	projectKey := strings.Trim(strings.ReplaceAll(resolvedDir, string(filepath.Separator), "-"), "-")
	sessionPath := filepath.Join(home, ".codebuddy", "projects", projectKey, runID+".jsonl")
	for attempt := 0; attempt < 8; attempt++ {
		if credit, found := readSessionCreditFile(sessionPath); found {
			return credit, true
		}
		timer := time.NewTimer(40 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", false
		case <-timer.C:
		}
	}
	return "", false
}

func readSessionCreditFile(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 32<<20))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var latest string
	for scanner.Scan() {
		var record struct {
			ProviderData struct {
				RawUsage struct {
					Credit json.Number `json:"credit"`
				} `json:"rawUsage"`
			} `json:"providerData"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.ProviderData.RawUsage.Credit == "" {
			continue
		}
		credit, parseErr := pricing.ParseDecimal(record.ProviderData.RawUsage.Credit.String())
		if parseErr == nil && credit.Nanos() >= 0 {
			latest = credit.String()
		}
	}
	return latest, latest != ""
}

func safeContentType(contentType string) bool {
	switch contentType {
	case "", "text", "thinking":
		return true
	default:
		return false
	}
}

type cliEvent struct {
	Type         string      `json:"type"`
	Subtype      string      `json:"subtype"`
	IsError      bool        `json:"is_error"`
	Result       string      `json:"result"`
	TotalCostUSD json.Number `json:"total_cost_usd"`
	Message      struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Event struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func minimalEnvironment(runDir string) []string {
	allowed := []string{"HOME", "PATH", "TMPDIR", "LANG", "LC_ALL", "USER", "LOGNAME", "SHELL", "SSL_CERT_FILE", "SSL_CERT_DIR"}
	result := make([]string, 0, len(allowed)+4)
	for _, key := range allowed {
		if value, exists := os.LookupEnv(key); exists {
			result = append(result, key+"="+value)
		}
	}
	return append(result,
		"CODEBUDDY_CODE_DISABLE_BACKGROUND_TASKS=1",
		"CODEBUDDY_CODE_ENABLE_TELEMETRY=0",
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
