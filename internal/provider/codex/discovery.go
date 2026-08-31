package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"
)

type DiscoveredModel struct {
	ID                        string   `json:"id"`
	DisplayName               string   `json:"display_name"`
	Description               string   `json:"description,omitempty"`
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts,omitempty"`
	Hidden                    bool     `json:"hidden"`
}

func DiscoverModels(ctx context.Context, config Config) ([]DiscoveredModel, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := append([]string{}, config.ExtraArgs...)
	args = append(args, "app-server", "--stdio")
	command := exec.CommandContext(probeCtx, config.Executable, args...)
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
		"params": map[string]any{"clientInfo": map[string]string{"name": "llmserver-admin", "version": "0.2.0"}},
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
	if err := encoder.Encode(map[string]any{"id": 2, "method": "model/list", "params": map[string]any{"limit": 100}}); err != nil {
		return nil, err
	}
	raw, err := waitRPCResult(scanner, 2)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
			Hidden      bool   `json:"hidden"`
			Efforts     []struct {
				Value string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode Codex model list: %w", err)
	}
	models := make([]DiscoveredModel, 0, len(result.Data))
	for _, item := range result.Data {
		if item.ID == "" || item.Hidden {
			continue
		}
		model := DiscoveredModel{ID: item.ID, DisplayName: item.DisplayName, Description: item.Description, Hidden: item.Hidden}
		for _, effort := range item.Efforts {
			model.SupportedReasoningEfforts = append(model.SupportedReasoningEfforts, effort.Value)
		}
		models = append(models, model)
	}
	return models, nil
}
