package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/zhyuzh3d/llmserver/internal/auth"
	"github.com/zhyuzh3d/llmserver/internal/config"
	"github.com/zhyuzh3d/llmserver/internal/pricing"
	"github.com/zhyuzh3d/llmserver/internal/provider"
	"github.com/zhyuzh3d/llmserver/internal/provider/mock"
)

func TestRunUsesReportedInputAndEstimatedOutput(t *testing.T) {
	reportedInput := int64(2000)
	service := newTestService(t, &mock.Adapter{
		ProviderID:    "mock",
		ResponseText:  "你好abcd",
		ReportedInput: &reportedInput,
	})
	client := auth.NewClient("device", "secret", []string{"terra"})
	events, _, err := service.Start(context.Background(), client, Request{
		Model:          "terra",
		CanonicalInput: "ignored",
	})
	if err != nil {
		t.Fatal(err)
	}

	var completed *Event
	for event := range events {
		if event.Type == EventRunCompleted {
			copy := event
			completed = &copy
		}
	}
	if completed == nil || completed.Billing == nil {
		t.Fatal("run did not complete with billing")
	}
	if completed.Billing.Usage.InputTokens != 2000 || completed.Billing.Usage.OutputTokens != 3 {
		t.Fatalf("unexpected usage: %#v", completed.Billing.Usage)
	}
	if completed.Billing.Charges.Total != "0.004036000" {
		t.Fatalf("unexpected charge: %#v", completed.Billing.Charges)
	}
	if completed.Billing.PriceVersion == "manual-price-name" {
		t.Fatal("public price version leaked internal revision")
	}
}

func TestRunRejectsUnauthorizedDeploymentBeforeProviderStart(t *testing.T) {
	adapter := &countingAdapter{Adapter: mock.Adapter{ProviderID: "mock", ResponseText: "ok"}}
	service := newTestService(t, adapter)
	client := auth.NewClient("device", "secret", []string{"sol"})
	_, _, err := service.Start(context.Background(), client, Request{Model: "terra"})
	if err == nil {
		t.Fatal("unauthorized request unexpectedly started")
	}
	if adapter.starts != 0 {
		t.Fatalf("provider started %d times", adapter.starts)
	}
}

func TestHardBudgetRejectsBeforeProviderStart(t *testing.T) {
	adapter := &countingAdapter{Adapter: mock.Adapter{ProviderID: "mock", ResponseText: "ok"}}
	service := newTestService(t, adapter)
	client := auth.NewClient("device", "secret", []string{"terra"})
	maxOutput := int64(300)
	_, _, err := service.Start(context.Background(), client, Request{
		Model:           "terra",
		CanonicalInput:  "你好",
		MaxOutputTokens: &maxOutput,
		Budget: &BudgetRequest{
			Mode:      "hard",
			Currency:  "USD",
			MaxCharge: "0.000001",
		},
	})
	if err == nil {
		t.Fatal("over-budget request unexpectedly started")
	}
	if adapter.starts != 0 {
		t.Fatalf("provider started %d times", adapter.starts)
	}
}

func TestDailyQuotaRejectsBeforeProviderStart(t *testing.T) {
	adapter := &countingAdapter{Adapter: mock.Adapter{ProviderID: "mock", ResponseText: "ok"}}
	repository := &quotaRejectRepository{}
	service, err := NewService([]config.DeploymentConfig{{
		ID: "terra", ProviderID: "mock", UpstreamModel: "mock-terra", Enabled: true,
		Price: config.PriceConfig{Revision: "price", Currency: "USD", InputPerMillion: "2", OutputPerMillion: "12"},
	}}, []provider.Adapter{adapter}, WithRunRepository(repository))
	if err != nil {
		t.Fatal(err)
	}
	client := auth.NewClient("device", "secret", []string{"terra"})
	client.DailyLimitUSD = "1.00"
	_, _, err = service.Start(context.Background(), client, Request{Model: "terra", CanonicalInput: "hello"})
	var apiError *Error
	if !errors.As(err, &apiError) || apiError.Status != 402 || apiError.Code != "daily_quota_exceeded" {
		t.Fatalf("daily quota error = %#v", err)
	}
	if adapter.starts != 0 {
		t.Fatalf("provider started %d times", adapter.starts)
	}
	if repository.reservation.DailyLimitUSD != "1.00" || repository.reservation.DayStart.IsZero() {
		t.Fatalf("quota reservation = %#v", repository.reservation)
	}
}

func TestPublicResponseDoesNotLeakProviderInjectedConfiguration(t *testing.T) {
	response := publicResponse("req_abc", "luna", map[string]any{
		"id":               "upstream-id",
		"status":           "completed",
		"instructions":     "private provider prompt",
		"tools":            []any{map[string]any{"type": "private_tool"}},
		"prompt_cache_key": "private-routing-key",
		"output":           []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
	}, "ok", pricing.BillableUsage{InputTokens: 1, OutputTokens: 1})
	for _, forbidden := range []string{"instructions", "tools", "prompt_cache_key"} {
		if _, exists := response[forbidden]; exists {
			t.Fatalf("public response leaked %s", forbidden)
		}
	}
	if response["id"] != "resp_abc" || response["model"] != "luna" {
		t.Fatalf("public identity was not normalized: %#v", response)
	}
}

type countingAdapter struct {
	mock.Adapter
	starts int
}

type quotaRejectRepository struct {
	reservation RunReservation
}

func (r *quotaRejectRepository) Reserve(_ context.Context, reservation RunReservation) (StoredRun, bool, error) {
	r.reservation = reservation
	return StoredRun{}, false, ErrDailyQuotaExceeded
}
func (*quotaRejectRepository) MarkRunning(context.Context, string) error                { return nil }
func (*quotaRejectRepository) Complete(context.Context, RunCompletion) error            { return nil }
func (*quotaRejectRepository) MarkFailed(context.Context, string, string, string) error { return nil }

func (a *countingAdapter) Start(ctx context.Context, request provider.Request) (<-chan provider.Event, error) {
	a.starts++
	return a.Adapter.Start(ctx, request)
}

func newTestService(t *testing.T, adapter provider.Adapter) *Service {
	t.Helper()
	service, err := NewService([]config.DeploymentConfig{{
		ID:            "terra",
		ProviderID:    "mock",
		UpstreamModel: "mock-terra",
		Price: config.PriceConfig{
			Revision:         "manual-price-name",
			Currency:         "USD",
			InputPerMillion:  "2",
			OutputPerMillion: "12",
		},
		Enabled: true,
	}}, []provider.Adapter{adapter})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
