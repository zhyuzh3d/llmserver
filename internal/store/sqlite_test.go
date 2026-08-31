package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/gateway"
)

func TestCompletedRunSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reservation := gateway.RunReservation{RunID: "req_one", ClientID: "device", DeploymentID: "terra", IdempotencyKey: "operation", Fingerprint: "fingerprint"}
	if _, created, err := repository.Reserve(context.Background(), reservation); err != nil || !created {
		t.Fatalf("reserve created=%t err=%v", created, err)
	}
	if err := repository.MarkRunning(context.Background(), reservation.RunID); err != nil {
		t.Fatal(err)
	}
	completion := gateway.RunCompletion{
		RunID: reservation.RunID, ResponseJSON: []byte(`{"id":"resp_one"}`), BillingJSON: []byte(`{"request_id":"req_one"}`),
		InputTokens: 2, OutputTokens: 1, InputSource: "estimated_v1", OutputSource: "provider_reported", Estimator: "text_estimator_v1",
		InputChars: 5, PriceVersion: "price_public", Currency: "USD", InputUnitPrice: "2.000000000", OutputUnitPrice: "12.000000000",
		InputCharge: "0.000004000", OutputCharge: "0.000012000", TotalCharge: "0.000016000",
	}
	if err := repository.Complete(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	record, created, err := reopened.Reserve(context.Background(), gateway.RunReservation{
		RunID: "req_two", ClientID: "device", DeploymentID: "terra", IdempotencyKey: "operation", Fingerprint: "fingerprint",
	})
	if err != nil || created {
		t.Fatalf("retry created=%t err=%v", created, err)
	}
	if record.RunID != "req_one" || record.Status != "completed" || record.SettlementState != "confirmed" {
		t.Fatalf("stored record = %#v", record)
	}
}

func TestSameIdempotencyKeyIsScopedByClient(t *testing.T) {
	repository, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	for _, reservation := range []gateway.RunReservation{
		{RunID: "req_one", ClientID: "device-one", DeploymentID: "terra", IdempotencyKey: "same", Fingerprint: "one"},
		{RunID: "req_two", ClientID: "device-two", DeploymentID: "terra", IdempotencyKey: "same", Fingerprint: "two"},
	} {
		if _, created, err := repository.Reserve(context.Background(), reservation); err != nil || !created {
			t.Fatalf("reserve %#v created=%t err=%v", reservation, created, err)
		}
	}
}

func TestUsageSummarySeparatesPublicChargeAndActualEstimate(t *testing.T) {
	repository, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	reservation := gateway.RunReservation{RunID: "req_usage", ClientID: "device", DeploymentID: "model", Fingerprint: "fingerprint"}
	if _, created, err := repository.Reserve(context.Background(), reservation); err != nil || !created {
		t.Fatalf("reserve created=%t err=%v", created, err)
	}
	if err := repository.MarkRunning(context.Background(), reservation.RunID); err != nil {
		t.Fatal(err)
	}
	completion := gateway.RunCompletion{
		RunID: reservation.RunID, ProviderID: "api", ResponseJSON: []byte(`{"id":"resp_usage"}`), BillingJSON: []byte(`{"request_id":"req_usage"}`),
		InputTokens: 10, OutputTokens: 5, InputSource: "provider_reported", OutputSource: "estimated_v1",
		PriceVersion: "public", Currency: "USD", InputUnitPrice: "2.000000000", OutputUnitPrice: "12.000000000",
		InputCharge: "0.000020000", OutputCharge: "0.000060000", TotalCharge: "0.000080000",
		UpstreamCostJSON: []byte(`[{"unit":"CNY","source":"configured_estimate","total":"0.000040000"}]`),
	}
	if err := repository.Complete(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	summary, err := repository.UsageSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Deployments) != 1 || summary.Deployments[0].PublicTotal != "0.000080000" {
		t.Fatalf("public summary = %#v", summary.Deployments)
	}
	if len(summary.Actual) != 1 || summary.Actual[0].Unit != "CNY" || summary.Actual[0].Total != "0.000040000" || summary.Actual[0].Source != "configured_estimate" {
		t.Fatalf("actual summary = %#v", summary.Actual)
	}
	report, err := repository.UsageReport(context.Background(), UsageReportFilter{
		Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Minute), GroupBy: "provider",
		ProviderByDeployment: map[string]string{"model": "api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Groups) != 1 || report.Groups[0].ID != "api" || report.Groups[0].Runs != 1 || len(report.Groups[0].Models) != 1 {
		t.Fatalf("provider report = %#v", report.Groups)
	}
	if report.Groups[0].PublicTotals[0].Total != "0.000080000" || report.Groups[0].ActualTotals[0].Total != "0.000040000" {
		t.Fatalf("provider totals = %#v %#v", report.Groups[0].PublicTotals, report.Groups[0].ActualTotals)
	}
	clientReport, err := repository.UsageReport(context.Background(), UsageReportFilter{
		Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Minute), GroupBy: "client", ClientID: "device",
		ProviderByDeployment: map[string]string{"model": "api"},
	})
	if err != nil || len(clientReport.Groups) != 1 || clientReport.Groups[0].ID != "device" {
		t.Fatalf("client report = %#v err=%v", clientReport.Groups, err)
	}
}
