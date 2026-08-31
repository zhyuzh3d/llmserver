package workbuddy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/provider"
)

func TestAdapterMapsWorkBuddyResultAndUsage(t *testing.T) {
	executable := fakeWorkBuddy(t, `
if [ "$1" = "--version" ]; then
  echo '2.115.0-test'
  exit 0
fi
if [ -n "$LLMSERVER_CLIENT_TOKEN" ]; then
  echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"leaked-environment"}]}}'
  exit 0
fi
echo '{"type":"system","subtype":"init"}'
echo '{"type":"assistant","message":{"model":"hy4-preview","content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"WORKBUDDY_OK"}]}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"WORKBUDDY_OK","total_cost_usd":999,"usage":{"input_tokens":6281,"output_tokens":79}}'
`)
	t.Setenv("LLMSERVER_CLIENT_TOKEN", "must-not-reach-workbuddy")
	adapter, err := New(Config{ProviderID: "workbuddy", Executable: executable, ExpectedVersion: "2.115.0"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := adapter.Start(context.Background(), provider.Request{UpstreamModel: "hy4-preview", CanonicalInput: "hello"})
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
	if text != "WORKBUDDY_OK" || final == nil {
		t.Fatalf("text=%q final=%#v", text, final)
	}
	if final.Usage.InputTokens.Value != 6281 || final.Usage.OutputTokens.Value != 79 {
		t.Fatalf("usage=%#v", final.Usage)
	}
	if len(final.Costs) != 1 || final.Costs[0].Unit != "USD" || final.Costs[0].Total != "999.000000000" {
		t.Fatalf("costs=%#v", final.Costs)
	}
}

func TestAdapterFailsClosedOnToolUse(t *testing.T) {
	executable := fakeWorkBuddy(t, `
if [ "$1" = "--version" ]; then echo '2.115.0'; exit 0; fi
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}'
`)
	adapter, err := New(Config{ProviderID: "workbuddy", Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	events, err := adapter.Start(context.Background(), provider.Request{UpstreamModel: "auto", CanonicalInput: "hello"})
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

func TestCancellationClosesWorkBuddyRunWithoutWaitingForChild(t *testing.T) {
	executable := fakeWorkBuddy(t, `
if [ "$1" = "--version" ]; then echo '2.115.0'; exit 0; fi
echo '{"type":"system","subtype":"init"}'
sleep 30
`)
	adapter, err := New(Config{ProviderID: "workbuddy", Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events, err := adapter.Start(ctx, provider.Request{UpstreamModel: "hy4-preview", CanonicalInput: "hello"})
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
		t.Fatal("cancelled WorkBuddy process group did not stop")
	}
}

func fakeWorkBuddy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workbuddy-fake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
