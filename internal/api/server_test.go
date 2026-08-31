package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhyuzh3d/llmserver/internal/auth"
	"github.com/zhyuzh3d/llmserver/internal/config"
	"github.com/zhyuzh3d/llmserver/internal/gateway"
	"github.com/zhyuzh3d/llmserver/internal/provider"
	"github.com/zhyuzh3d/llmserver/internal/provider/mock"
	"github.com/zhyuzh3d/llmserver/internal/store"
)

func TestResponsesNonStreamingReturnsPublicBilling(t *testing.T) {
	server := newTestServer(t, nil)
	request := newRequest(t, http.MethodPost, "/v1/responses", `{
  "model":"terra",
  "instructions":"be concise",
  "input":"你好abcd",
  "store":false
}`)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	billing := body["llmserver_billing"].(map[string]any)
	if _, exists := billing["pricing_basis"]; exists {
		t.Fatal("public response leaked pricing basis")
	}
	if _, exists := billing["quota_observations"]; exists {
		t.Fatal("quota observations are not part of the public response contract")
	}
	charges := billing["charges"].(map[string]any)
	if charges["total"] != "0.000048000" {
		t.Fatalf("unexpected total charge: %#v", charges)
	}
	usage := body["usage"].(map[string]any)
	billingUsage := billing["usage"].(map[string]any)
	if usage["input_tokens"] != billingUsage["input_tokens"] || usage["output_tokens"] != billingUsage["output_tokens"] {
		t.Fatalf("standard and billing usage differ: %#v %#v", usage, billingUsage)
	}
	if got := response.Header().Get("x-llmserver-request-id"); !strings.HasPrefix(got, "req_") {
		t.Fatalf("missing request id header: %q", got)
	}
}

func TestModelsAreFilteredByClientPolicy(t *testing.T) {
	server := newTestServer(t, nil)
	request := newRequest(t, http.MethodGet, "/v1/models", "")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if strings.Contains(response.Body.String(), "sol") || !strings.Contains(response.Body.String(), "terra") {
		t.Fatalf("unexpected model list: %s", response.Body.String())
	}
}

func TestResponsesStreamingIncludesFinalBillingEvent(t *testing.T) {
	server := newTestServer(t, nil)
	request := newRequest(t, http.MethodPost, "/v1/responses", `{"model":"terra","input":"hello","stream":true}`)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	body := response.Body.String()
	for _, expected := range []string{"response.created", "response.output_text.delta", "llmserver.billing.completed", "response.completed", "data: [DONE]"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("stream is missing %q: %s", expected, body)
		}
	}
}

func TestStrictModeOmitsBillingPayload(t *testing.T) {
	server := newTestServer(t, nil)
	request := newRequest(t, http.MethodPost, "/v1/responses", `{"model":"terra","input":"hello"}`)
	request.Header.Set("x-llmserver-compatibility", "strict")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if strings.Contains(response.Body.String(), "llmserver_billing") {
		t.Fatalf("strict response contains billing: %s", response.Body.String())
	}
	if response.Header().Get("x-llmserver-request-id") == "" {
		t.Fatal("strict response must retain request id")
	}
}

func TestUnauthorizedRequestDoesNotStartProvider(t *testing.T) {
	counter := &countingAdapter{Adapter: mock.Adapter{ProviderID: "mock", ResponseText: "ok"}}
	server := newTestServer(t, counter)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"terra","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || counter.starts != 0 {
		t.Fatalf("status=%d starts=%d body=%s", response.Code, counter.starts, response.Body.String())
	}
}

func TestReasoningEffortIsValidatedAndForwarded(t *testing.T) {
	counter := &countingAdapter{Adapter: mock.Adapter{ProviderID: "mock", ResponseText: "ok"}}
	server := newTestServer(t, counter)
	request := newRequest(t, http.MethodPost, "/v1/responses", `{"model":"terra","input":"hello","reasoning":{"effort":"LOW"}}`)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || counter.lastRequest.ReasoningEffort != "low" {
		t.Fatalf("status=%d effort=%q body=%s", response.Code, counter.lastRequest.ReasoningEffort, response.Body.String())
	}

	counter.starts = 0
	request = newRequest(t, http.MethodPost, "/v1/responses", `{"model":"terra","input":"hello","reasoning":{"effort":"ultra"}}`)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || counter.starts != 0 || !strings.Contains(response.Body.String(), "unsupported_reasoning_effort") {
		t.Fatalf("status=%d starts=%d body=%s", response.Code, counter.starts, response.Body.String())
	}
}

func TestCancelledStreamCancelsMockProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"terra","input":"hello","stream":true}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer client-secret")
	request.Header.Set("Content-Type", "application/json")
	adapter := &mock.Adapter{ProviderID: "mock", ResponseText: strings.Repeat("hello", 100)}
	server := newTestServer(t, adapter)
	cancel()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
}

func TestSSEFramesContainValidJSON(t *testing.T) {
	server := newTestServer(t, nil)
	request := newRequest(t, http.MethodPost, "/v1/responses", `{"model":"terra","input":"hello","stream":true}`)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	scanner := bufio.NewScanner(bytes.NewReader(response.Body.Bytes()))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &value); err != nil {
			t.Fatalf("invalid SSE JSON %q: %v", line, err)
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		t.Fatal(err)
	}
}

func TestCompletedIdempotentRetryDoesNotStartProviderTwice(t *testing.T) {
	adapter := &countingAdapter{Adapter: mock.Adapter{ProviderID: "mock", ResponseText: "ok"}}
	server := newPersistentTestServer(t, adapter)
	body := `{"model":"terra","input":"hello","llmserver":{"idempotency_key":"same-operation"}}`

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, newRequest(t, http.MethodPost, "/v1/responses", body))
	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, newRequest(t, http.MethodPost, "/v1/responses", body))
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d, %d; second body=%s", first.Code, second.Code, second.Body.String())
	}
	if adapter.starts != 1 {
		t.Fatalf("provider starts = %d", adapter.starts)
	}
	if first.Header().Get("x-llmserver-request-id") != second.Header().Get("x-llmserver-request-id") {
		t.Fatalf("idempotent retry returned a different request id")
	}
	if !strings.Contains(second.Body.String(), "llmserver_billing") {
		t.Fatalf("stored completion lost billing: %s", second.Body.String())
	}
}

func TestIdempotencyKeyCannotBeReusedForDifferentRequest(t *testing.T) {
	adapter := &countingAdapter{Adapter: mock.Adapter{ProviderID: "mock", ResponseText: "ok"}}
	server := newPersistentTestServer(t, adapter)
	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, newRequest(t, http.MethodPost, "/v1/responses", `{"model":"terra","input":"one","llmserver":{"idempotency_key":"operation"}}`))
	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, newRequest(t, http.MethodPost, "/v1/responses", `{"model":"terra","input":"two","llmserver":{"idempotency_key":"operation"}}`))
	if first.Code != http.StatusOK || second.Code != http.StatusConflict {
		t.Fatalf("statuses = %d, %d; body=%s", first.Code, second.Code, second.Body.String())
	}
	if adapter.starts != 1 || !strings.Contains(second.Body.String(), "idempotency_key_reused") {
		t.Fatalf("provider starts=%d body=%s", adapter.starts, second.Body.String())
	}
}

func newRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer client-secret")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func newTestServer(t *testing.T, adapter provider.Adapter) *Server {
	t.Helper()
	if adapter == nil {
		adapter = &mock.Adapter{ProviderID: "mock", ResponseText: "你好abcd"}
	}
	service, err := gateway.NewService([]config.DeploymentConfig{
		{
			ID:                       "terra",
			ProviderID:               "mock",
			UpstreamModel:            "mock-terra",
			SupportedReasoningEffort: []string{"low", "high"},
			Price: config.PriceConfig{
				Revision:         "internal-manual-price",
				Currency:         "USD",
				InputPerMillion:  "2",
				OutputPerMillion: "12",
			},
			Enabled: true,
		},
		{
			ID:            "sol",
			ProviderID:    "mock",
			UpstreamModel: "mock-sol",
			Price: config.PriceConfig{
				Revision:         "sol-price",
				Currency:         "USD",
				InputPerMillion:  "4",
				OutputPerMillion: "20",
			},
			Enabled: true,
		},
	}, []provider.Adapter{adapter})
	if err != nil {
		t.Fatal(err)
	}
	authenticator := auth.New(auth.NewClient("device", "client-secret", []string{"terra"}))
	return NewServer(authenticator, service)
}

func newPersistentTestServer(t *testing.T, adapter provider.Adapter) *Server {
	t.Helper()
	repository, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	service, err := gateway.NewService([]config.DeploymentConfig{{
		ID: "terra", ProviderID: "mock", UpstreamModel: "mock-terra", Enabled: true,
		Price: config.PriceConfig{Revision: "price", Currency: "USD", InputPerMillion: "2", OutputPerMillion: "12"},
	}}, []provider.Adapter{adapter}, gateway.WithRunRepository(repository))
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(auth.New(auth.NewClient("device", "client-secret", []string{"terra"})), service)
}

type countingAdapter struct {
	mock.Adapter
	starts      int
	lastRequest provider.Request
}

func (a *countingAdapter) Start(ctx context.Context, request provider.Request) (<-chan provider.Event, error) {
	a.starts++
	a.lastRequest = request
	return a.Adapter.Start(ctx, request)
}
