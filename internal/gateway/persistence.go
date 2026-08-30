package gateway

import (
	"context"
	"encoding/json"
)

type RunRepository interface {
	Reserve(context.Context, RunReservation) (StoredRun, bool, error)
	MarkRunning(context.Context, string) error
	Complete(context.Context, RunCompletion) error
	MarkFailed(context.Context, string, string, string) error
}

type RunReservation struct {
	RunID          string
	ClientID       string
	DeploymentID   string
	IdempotencyKey string
	Fingerprint    string
}

type StoredRun struct {
	RunID           string
	Status          string
	SettlementState string
	Fingerprint     string
	ResponseJSON    []byte
	BillingJSON     []byte
}

type RunCompletion struct {
	RunID           string
	ResponseJSON    []byte
	BillingJSON     []byte
	InputTokens     int64
	OutputTokens    int64
	InputSource     string
	OutputSource    string
	Estimator       string
	InputChars      int64
	OutputChars     int64
	PriceVersion    string
	Currency        string
	InputUnitPrice  string
	OutputUnitPrice string
	InputCharge     string
	OutputCharge    string
	TotalCharge     string
	BudgetJSON      []byte
	QuotaJSON       []byte
}

type Option func(*Service)

func WithRunRepository(repository RunRepository) Option {
	return func(service *Service) { service.repository = repository }
}

func storedCompletion(record StoredRun) (<-chan Event, error) {
	var response map[string]any
	var billing Billing
	if err := json.Unmarshal(record.ResponseJSON, &response); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(record.BillingJSON, &billing); err != nil {
		return nil, err
	}
	events := make(chan Event, 2)
	events <- Event{Type: EventBillingCompleted, Billing: &billing}
	events <- Event{Type: EventRunCompleted, Response: response, Billing: &billing}
	close(events)
	return events, nil
}
