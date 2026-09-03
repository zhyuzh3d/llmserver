package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/provider"
	"github.com/zhyuzh3d/llmserver/internal/toolcall"
)

func TestAdapterUsesMinimalDirectResponsesRequest(t *testing.T) {
	executable := fakeCodex(t, `
if [ "$1" = "--version" ]; then echo 'codex-cli test-1'; exit 0; fi
exit 8
`)
	authPath := filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"local-access","account_id":"local-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer local-access" || request.Header.Get("ChatGPT-Account-ID") != "local-account" {
			t.Error("direct request did not use the current Codex login")
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		tools, ok := body["tools"].([]any)
		if !ok || len(tools) != 0 || body["model"] != "gpt-test" {
			t.Errorf("unexpected direct request: %#v", body)
		}
		reasoning, ok := body["reasoning"].(map[string]any)
		if !ok || reasoning["effort"] != "none" {
			t.Errorf("explicit none reasoning was not forwarded: %#v", body["reasoning"])
		}
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"DIRECT_OK\"}\n\n"))
		_, _ = response.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-test\",\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}}\n\n"))
	}))
	defer server.Close()
	adapter, err := New(Config{
		ProviderID: "codex", Executable: executable, ExpectedVersion: "codex-cli test-",
		DefaultReasoning: "low", ServiceTier: "priority", DirectEndpoint: server.URL, DirectClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Retire)
	events, err := adapter.Start(context.Background(), provider.Request{UpstreamModel: "gpt-test", CanonicalInput: "hello", ReasoningEffort: "none"})
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
	if text != "DIRECT_OK" || final == nil || final.Usage.InputTokens.Value != 12 || final.Usage.OutputTokens.Value != 3 {
		t.Fatalf("text=%q final=%#v", text, final)
	}
}

func TestAdapterUsesNativeDirectFunctionCalling(t *testing.T) {
	executable := fakeCodex(t, `
if [ "$1" = "--version" ]; then echo 'codex-cli test-1'; exit 0; fi
exit 8
`)
	authPath := filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"local-access","account_id":"local-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		tools, _ := body["tools"].([]any)
		input, _ := body["input"].([]any)
		include, _ := body["include"].([]any)
		if len(tools) != 1 || len(input) != 1 || body["tool_choice"] != "required" || body["parallel_tool_calls"] != false {
			t.Fatalf("unexpected function request: %#v", body)
		}
		if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
			t.Fatalf("stateless reasoning continuation was not requested: %#v", include)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"id\\\":7}\"}}\n\n"))
		_, _ = response.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-test\",\"output\":[],\"usage\":{\"input_tokens\":20,\"output_tokens\":5}}}\n\n"))
	}))
	defer server.Close()
	contract, err := toolcall.Parse(json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"],"additionalProperties":false},"strict":true}]`), json.RawMessage(`"required"`), nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{
		ProviderID: "codex", Executable: executable, ExpectedVersion: "codex-cli test-",
		DirectEndpoint: server.URL, DirectClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Retire)
	events, err := adapter.Start(context.Background(), provider.Request{
		UpstreamModel: "gpt-test", Instructions: "Use facts", Input: json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"lookup 7"}]}]`), ToolCall: contract,
	})
	if err != nil {
		t.Fatal(err)
	}
	var final *provider.Final
	for event := range events {
		if event.Type == provider.EventFailed {
			t.Fatal(event.Err)
		}
		if event.Type == provider.EventCompleted {
			final = event.Final
		}
	}
	if final == nil || final.Response == nil {
		t.Fatalf("Codex function response was lost: %#v", final)
	}
	if err := contract.ValidateResponse(final.Response); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteReasoningEffortsAddsNoneOnlyToVerifiedModels(t *testing.T) {
	tests := []struct {
		model    string
		reported []string
		want     string
	}{
		{model: "gpt-5.6-luna", reported: []string{"low", "medium"}, want: "none,low,medium"},
		{model: "gpt-5.6-terra", reported: []string{"low", "none", "high"}, want: "none,low,high"},
		{model: "gpt-5.6-sol", reported: []string{"low", "xhigh", "ultra"}, want: "none,low,xhigh,ultra"},
		{model: "gpt-future", reported: []string{"low", "high"}, want: "low,high"},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			got := strings.Join(completeReasoningEfforts(test.model, test.reported), ",")
			if got != test.want {
				t.Fatalf("efforts=%q want=%q", got, test.want)
			}
		})
	}
}

func TestEffectiveReasoningEffortPreservesExplicitNone(t *testing.T) {
	if got := effectiveReasoningEffort("none", "low"); got != "none" {
		t.Fatalf("explicit effort=%q", got)
	}
	if got := effectiveReasoningEffort("", "low"); got != "low" {
		t.Fatalf("default effort=%q", got)
	}
}

func TestAdapterMapsCodexUsageAndStripsServiceSecretsFromEnvironment(t *testing.T) {
	executable := fakeCodex(t, `
if [ "$1" = "--version" ]; then
  echo 'codex-cli test-1'
  exit 0
fi
if [ -n "$LLMSERVER_CLIENT_TOKEN" ]; then
  exit 9
fi
if [ "$1" != "app-server" ] || [ "$2" != "--stdio" ]; then
  exit 8
fi
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"id":1,"result":{}}' ;;
    *'"method":"thread/start"'*) echo '{"id":2,"result":{"model":"gpt-test","thread":{"id":"thread"}}}' ;;
    *'"method":"turn/start"'*)
      case "$line" in *'"effort":"low"'*) ;; *) exit 7 ;; esac
      case "$line" in *'"serviceTierForTurn":"priority"'*) ;; *) exit 6 ;; esac
      echo '{"id":3,"result":{"turn":{"id":"turn","items":[],"status":"inProgress"}}}'
      echo '{"method":"item/started","params":{"threadId":"thread","turnId":"turn","startedAtMs":1,"item":{"id":"message","type":"agentMessage","text":""}}}'
      echo '{"method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn","itemId":"message","delta":"CODEX_OK"}}'
      echo '{"method":"item/completed","params":{"threadId":"thread","turnId":"turn","completedAtMs":2,"item":{"id":"message","type":"agentMessage","text":"CODEX_OK"}}}'
      echo '{"method":"thread/tokenUsage/updated","params":{"threadId":"thread","turnId":"turn","tokenUsage":{"last":{"inputTokens":12735,"cachedInputTokens":0,"outputTokens":7,"reasoningOutputTokens":0,"totalTokens":12742},"total":{"inputTokens":12735,"cachedInputTokens":0,"outputTokens":7,"reasoningOutputTokens":0,"totalTokens":12742}}}}'
      echo '{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","items":[],"status":"completed"}}}'
      ;;
  esac
done
`)
	t.Setenv("LLMSERVER_CLIENT_TOKEN", "must-not-reach-codex")
	adapter, err := New(Config{ProviderID: "codex", Executable: executable, ExpectedVersion: "codex-cli test-", DefaultReasoning: "low", ServiceTier: "priority", DisableDirect: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Retire)
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
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"id":1,"result":{}}' ;;
    *'"method":"thread/start"'*) echo '{"id":2,"result":{"model":"gpt-test","thread":{"id":"thread"}}}' ;;
    *'"method":"turn/start"'*)
      echo '{"id":3,"result":{"turn":{"id":"turn","items":[],"status":"inProgress"}}}'
      echo '{"method":"item/started","params":{"threadId":"thread","turnId":"turn","startedAtMs":1,"item":{"id":"command","type":"commandExecution","command":"pwd","commandActions":[],"cwd":"/tmp","status":"inProgress"}}}'
      ;;
  esac
done
`)
	adapter, err := New(Config{ProviderID: "codex", Executable: executable, DisableDirect: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Retire)
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

func TestCancellationClosesCodexRunWithoutWaitingForChild(t *testing.T) {
	executable := fakeCodex(t, `
if [ "$1" = "--version" ]; then echo 'codex-cli test-1'; exit 0; fi
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"id":1,"result":{}}' ;;
    *'"method":"thread/start"'*) echo '{"id":2,"result":{"model":"gpt-test","thread":{"id":"thread"}}}' ;;
    *'"method":"turn/start"'*) echo '{"id":3,"result":{"turn":{"id":"turn","items":[],"status":"inProgress"}}}'; sleep 30 ;;
  esac
done
`)
	adapter, err := New(Config{ProviderID: "codex", Executable: executable, DisableDirect: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Retire)
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
	directory := t.TempDir()
	codexHome := filepath.Join(directory, "codex-home")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(directory, "codex-fake")
	preamble := `
if [ "$1" != "--version" ]; then
  while [ "$#" -gt 0 ] && [ "$1" != "app-server" ]; do shift; done
fi
`
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+preamble+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
