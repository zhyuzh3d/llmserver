package workbuddy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/provider"
)

func TestAdapterWarmsAndReplenishesPersistentWorkers(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "warmups")
	executable := fakeWorkBuddy(t, fmt.Sprintf(`
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}' ;;
    *'"method":"session/new"'*) echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-warm"}}' ;;
    *'"method":"session/set_model"'*)
      case "$line" in *'"modelId":"hy4-preview"'*) ;; *) exit 6 ;; esac
      echo '{"jsonrpc":"2.0","id":3,"result":{}}'
      ;;
    *'"method":"session/set_config_option"'*) echo '{"jsonrpc":"2.0","id":4,"result":{}}' ;;
    *'"method":"session/prompt"'*)
      printf 'warm\n' >> %q
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-warm","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"OK"}}}}'
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-warm","update":{"sessionUpdate":"session_info_update","_meta":{"codebuddy.ai/agentPhase":{"phase":"model_done"}}}}}'
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-warm","update":{"sessionUpdate":"usage_update","_meta":{"usage":{"prompt_tokens":4,"completion_tokens":2,"credit":0.01}}}}}'
      echo '{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}'
      ;;
  esac
done
`, marker))
	adapter, err := New(Config{
		ProviderID: "workbuddy", Executable: executable, MaxConcurrency: 2,
		DefaultReasoning: "high", WarmupEnabled: true, WarmupModel: "hy4-preview", WarmupTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Retire)
	waitForWarmups(t, marker, 2)

	worker, err := adapter.acquireWorker(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adapter.releaseWorker(worker, false)
	waitForWarmups(t, marker, 3)

	deadline := time.Now().Add(2 * time.Second)
	for {
		adapter.mu.Lock()
		live, idle := adapter.live, len(adapter.idle)
		adapter.mu.Unlock()
		if live == 2 && idle == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool did not recover: live=%d idle=%d", live, idle)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForWarmups(t *testing.T, marker string, wanted int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		contents, _ := os.ReadFile(marker)
		if strings.Count(string(contents), "warm\n") >= wanted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("warmups=%q wanted=%d", contents, wanted)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAdapterMapsACPOutputUsageAndStripsServiceSecrets(t *testing.T) {
	executable := fakeWorkBuddy(t, `
if [ -n "$LLMSERVER_CLIENT_TOKEN" ]; then exit 9; fi
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}' ;;
    *'"method":"session/new"'*) echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-test"}}' ;;
    *'"method":"session/set_model"'*)
      case "$line" in *'"modelId":"hy4-preview"'*) ;; *) exit 6 ;; esac
      echo '{"jsonrpc":"2.0","id":3,"result":{}}'
      ;;
    *'"method":"session/set_config_option"'*)
      case "$line" in *'"configId":"thought_level"'*'"value":"low"'*) ;; *) exit 7 ;; esac
      echo '{"jsonrpc":"2.0","id":4,"result":{}}'
      ;;
    *'"method":"session/prompt"'*)
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"WORKBUDDY_OK"},"_meta":{"codebuddy.ai/responseModelId":"hy4-preview"}}}}'
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-test","update":{"sessionUpdate":"session_info_update","_meta":{"codebuddy.ai/agentPhase":{"phase":"model_done"}}}}}'
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-test","update":{"sessionUpdate":"usage_update","_meta":{"usage":{"prompt_tokens":6281,"completion_tokens":79}}}}}'
      echo '{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}'
      ;;
  esac
done
`)
	t.Setenv("LLMSERVER_CLIENT_TOKEN", "must-not-reach-workbuddy")
	adapter, err := New(Config{ProviderID: "workbuddy", Executable: executable, ExpectedVersion: "2.115.0", DefaultReasoning: "low"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Retire)
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
	if len(final.Costs) != 0 {
		t.Fatalf("missing ACP credit must remain unknown: %#v", final.Costs)
	}
}

func TestAdapterStartsStatelessContextFreeACPWorker(t *testing.T) {
	executable := fakeWorkBuddy(t, `
case " $* " in *" --no-session-persistence "*) ;; *) exit 10 ;; esac
if [ "$CODEBUDDY_DISABLE_AUTO_MEMORY" != "1" ]; then exit 11; fi
if [ "$CODEBUDDY_CODE_DISABLE_AUTO_MEMORY" != "1" ]; then exit 12; fi
if [ "$CODEBUDDY_DISABLE_SYSTEM_REMINDER_MD" != "1" ]; then exit 13; fi
if [ "$CODEBUDDY_CODE_DISABLE_TERMINAL_TITLE" != "1" ]; then exit 14; fi
if [ "$CODEBUDDY_CODE_DISABLE_SESSION_SUMMARY" != "1" ]; then exit 15; fi
if [ "$DISABLE_TELEMETRY" != "1" ]; then exit 16; fi
if [ "$DISABLE_ERROR_REPORTING" != "1" ]; then exit 17; fi
if [ "$DISABLE_AUTOUPDATER" != "1" ]; then exit 18; fi
if [ "$CODEBUDDY_DISABLE_HOT_RELOAD" != "1" ]; then exit 19; fi
if [ "$CODEBUDDY_SKIP_BUILTIN_MARKETPLACE" != "1" ]; then exit 20; fi
if [ "$CODEBUDDY_DISABLE_CRON" != "1" ]; then exit 21; fi
if [ "$CODEBUDDY_MAIN_AGENT_ENABLED" != "0" ]; then exit 22; fi
if [ "$CODEBUDDY_REPL_ENABLED" != "0" ]; then exit 23; fi
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}' ;;
  esac
done
`)
	adapter, err := New(Config{ProviderID: "workbuddy", Executable: executable, MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	adapter.Retire()
}

func TestAdapterRecordsCreditAlreadyReturnedByPrompt(t *testing.T) {
	executable := fakeWorkBuddy(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}' ;;
    *'"method":"session/new"'*) echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-credit"}}' ;;
    *'"method":"session/set_model"'*) echo '{"jsonrpc":"2.0","id":3,"result":{}}' ;;
    *'"method":"session/prompt"'*)
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-credit","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"OK"}}}}'
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-credit","update":{"sessionUpdate":"session_info_update","_meta":{"codebuddy.ai/agentPhase":{"phase":"model_done"}}}}}'
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-credit","update":{"sessionUpdate":"usage_update","_meta":{"usage":{"prompt_tokens":10,"completion_tokens":2,"credit":1.25}}}}}'
      echo '{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn"}}'
      ;;
  esac
done
`)
	adapter, err := New(Config{ProviderID: "workbuddy", Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Retire)
	events, err := adapter.Start(context.Background(), provider.Request{UpstreamModel: "hy4-preview", CanonicalInput: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var final *provider.Final
	for event := range events {
		if event.Type == provider.EventCompleted {
			final = event.Final
		}
	}
	if final == nil || len(final.Costs) != 1 || final.Costs[0].Unit != "POINTS" || final.Costs[0].Total != "1.250000000" {
		t.Fatalf("final = %#v", final)
	}
}

func TestAdapterFailsClosedOnACPToolCall(t *testing.T) {
	executable := fakeWorkBuddy(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{}}' ;;
    *'"method":"session/new"'*) echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-tool"}}' ;;
    *'"method":"session/set_model"'*) echo '{"jsonrpc":"2.0","id":3,"result":{}}' ;;
    *'"method":"session/prompt"'*) echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-tool","update":{"sessionUpdate":"tool_call"}}}' ;;
  esac
done
`)
	adapter, err := New(Config{ProviderID: "workbuddy", Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Retire)
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

func TestCancellationClosesWorkBuddyACPWithoutWaitingForChild(t *testing.T) {
	executable := fakeWorkBuddy(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{}}' ;;
    *'"method":"session/new"'*) echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-cancel"}}' ;;
    *'"method":"session/set_model"'*) echo '{"jsonrpc":"2.0","id":3,"result":{}}' ;;
    *'"method":"session/prompt"'*) sleep 30 ;;
  esac
done
`)
	adapter, err := New(Config{ProviderID: "workbuddy", Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Retire)
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
		t.Fatal("cancelled WorkBuddy ACP process group did not stop")
	}
}

func fakeWorkBuddy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workbuddy-fake")
	preamble := `
if [ "$1" = "--version" ]; then
  echo '2.115.0-test'
  exit 0
fi
`
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+preamble+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
