package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhyuzh3d/llmserver/internal/config"
	"github.com/zhyuzh3d/llmserver/internal/provider"
)

func TestNonStreamingRequestIsSanitizedAndUsageIsRead(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Errorf("authorization was not set at provider boundary")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "upstream-model" {
			t.Errorf("model = %#v", body["model"])
		}
		if _, exists := body["llmserver"]; exists {
			t.Error("private llmserver extension leaked upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_up","object":"response","status":"completed","model":"upstream-model","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"cost":{"currency":"CNY","input":"0.01","output":"0.02","total":"0.03"}}}`)
	}))
	defer upstream.Close()

	adapter, err := New("api", upstream.URL+"/gateway", config.NewSecret("upstream-secret"), upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	events, err := adapter.Start(context.Background(), provider.Request{
		UpstreamModel: "upstream-model",
		RawRequest:    json.RawMessage(`{"model":"public-model","input":"hello","llmserver":{"budget":{"max_charge":"1"}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	final := completedEvent(t, events)
	if !final.Usage.InputTokens.Present || final.Usage.InputTokens.Value != 7 || !final.Usage.OutputTokens.Present || final.Usage.OutputTokens.Value != 3 {
		t.Fatalf("usage = %#v", final.Usage)
	}
	if final.OutputText != "ok" {
		t.Fatalf("output text = %q", final.OutputText)
	}
	if len(final.Costs) != 1 || final.Costs[0].Unit != "CNY" || final.Costs[0].Total != "0.030000000" {
		t.Fatalf("costs = %#v", final.Costs)
	}
}

func TestStreamingResponseProducesDeltasAndCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_up\",\"model\":\"upstream-model\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	adapter, err := New("api", upstream.URL, config.NewSecret("secret"), upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	events, err := adapter.Start(context.Background(), provider.Request{UpstreamModel: "upstream-model", RawRequest: json.RawMessage(`{"model":"public","input":"hello","stream":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	var final *provider.Final
	for event := range events {
		switch event.Type {
		case provider.EventOutputTextDelta:
			deltas.WriteString(event.Delta)
		case provider.EventCompleted:
			final = event.Final
		case provider.EventFailed:
			t.Fatalf("stream failed: %v", event.Err)
		}
	}
	if deltas.String() != "hello" || final == nil || final.OutputText != "hello" {
		t.Fatalf("deltas=%q final=%#v", deltas.String(), final)
	}
}

func TestProviderErrorBodyIsNotReflected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprint(w, `{"error":"sensitive upstream detail"}`)
	}))
	defer upstream.Close()
	adapter, err := New("api", upstream.URL, config.NewSecret("secret"), upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Start(context.Background(), provider.Request{UpstreamModel: "model", RawRequest: json.RawMessage(`{"model":"public","input":"hello"}`)})
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func completedEvent(t *testing.T, events <-chan provider.Event) *provider.Final {
	t.Helper()
	for event := range events {
		if event.Type == provider.EventFailed {
			t.Fatalf("provider failed: %v", event.Err)
		}
		if event.Type == provider.EventCompleted {
			return event.Final
		}
	}
	t.Fatal("provider did not complete")
	return nil
}
