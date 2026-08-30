package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/provider"
)

func TestAdapterMapsCodexUsageAndStripsServiceSecretsFromEnvironment(t *testing.T) {
	executable := fakeCodex(t, `
if [ "$1" = "--version" ]; then
  echo 'codex-cli test-1'
  exit 0
fi
if [ -n "$LLMSERVER_CLIENT_TOKEN" ]; then
  echo '{"type":"item.completed","item":{"type":"command_execution"}}'
  exit 0
fi
echo '{"type":"thread.started","thread_id":"thread"}'
echo '{"type":"turn.started"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"CODEX_OK"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":12735,"output_tokens":7}}'
`)
	t.Setenv("LLMSERVER_CLIENT_TOKEN", "must-not-reach-codex")
	adapter, err := New(Config{ProviderID: "codex", Executable: executable, ExpectedVersion: "codex-cli test-"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := adapter.Start(context.Background(), provider.Request{UpstreamModel: "gpt-test", CanonicalInput: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var final *provider.Final
	for event := range events {
		if event.Type == provider.EventFailed {
			t.Fatalf("adapter failed: %v", event.Err)
		}
		if event.Type == provider.EventOutputTextDelta {
			text += event.Delta
		}
		if event.Type == provider.EventCompleted {
			final = event.Final
		}
	}
	if text != "CODEX_OK" || final == nil {
		t.Fatalf("text=%q final=%#v", text, final)
	}
	if final.Usage.InputTokens.Value != 12735 || final.Usage.OutputTokens.Value != 7 {
		t.Fatalf("usage=%#v", final.Usage)
	}
}

func TestAdapterFailsClosedOnLocalToolItem(t *testing.T) {
	executable := fakeCodex(t, `
if [ "$1" = "--version" ]; then
  echo 'codex-cli test-1'
  exit 0
fi
echo '{"type":"turn.started"}'
echo '{"type":"item.started","item":{"type":"command_execution"}}'
`)
	adapter, err := New(Config{ProviderID: "codex", Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	events, err := adapter.Start(context.Background(), provider.Request{UpstreamModel: "gpt-test", CanonicalInput: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var failure error
	for event := range events {
		if event.Type == provider.EventFailed {
			failure = event.Err
		}
	}
	if failure == nil || !strings.Contains(failure.Error(), "disallowed") {
		t.Fatalf("failure=%v", failure)
	}
}

func TestCompareQuotaKeepsIndependentWindowsAndDetectsReset(t *testing.T) {
	before := map[string]quotaWindow{
		"codex:primary":   {UsedPercent: 6, WindowDurationMinutes: 10080, ResetsAt: 100},
		"spark:secondary": {UsedPercent: 20, WindowDurationMinutes: 10080, ResetsAt: 200},
	}
	after := map[string]quotaWindow{
		"codex:primary":   {UsedPercent: 7, WindowDurationMinutes: 10080, ResetsAt: 100},
		"spark:secondary": {UsedPercent: 1, WindowDurationMinutes: 10080, ResetsAt: 300},
	}
	observations := compareQuota(before, after)
	if len(observations) != 2 || observations[0].Delta == nil || *observations[0].Delta != 1 {
		t.Fatalf("observations=%#v", observations)
	}
	if observations[1].Status != "window_changed" || observations[1].Delta != nil {
		t.Fatalf("reset window was compared: %#v", observations[1])
	}
}

func TestCancellationClosesCodexRunWithoutWaitingForChild(t *testing.T) {
	executable := fakeCodex(t, `
if [ "$1" = "--version" ]; then echo 'codex-cli test-1'; exit 0; fi
echo '{"type":"turn.started"}'
sleep 30
`)
	adapter, err := New(Config{ProviderID: "codex", Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events, err := adapter.Start(ctx, provider.Request{UpstreamModel: "gpt-test", CanonicalInput: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		for range events {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Codex process group did not stop")
	}
}

func fakeCodex(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex-fake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
