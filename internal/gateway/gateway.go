package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/auth"
	"github.com/zhyuzh3d/llmserver/internal/config"
	"github.com/zhyuzh3d/llmserver/internal/pricing"
	"github.com/zhyuzh3d/llmserver/internal/provider"
)

type Service struct {
	deployments map[string]Deployment
	providers   map[string]provider.Adapter
	repository  RunRepository
}

type Deployment struct {
	ID                  string
	ProviderID          string
	UpstreamModel       string
	Price               pricing.PriceRevision
	ActualPrice         *pricing.PriceRevision
	ActualPoints        *pricing.PriceRevision
	Enabled             bool
	HardBudgetSupported bool
}

type Model struct {
	ID string `json:"id"`
}

type Request struct {
	Model           string
	Instructions    string
	Input           json.RawMessage
	CanonicalInput  string
	MaxOutputTokens *int64
	RawRequest      json.RawMessage
	Budget          *BudgetRequest
	IdempotencyKey  string
}

type BudgetRequest struct {
	Mode      pricing.BudgetMode
	Currency  string
	MaxCharge string
}

type EventType string

const (
	EventOutputTextDelta  EventType = "output_text.delta"
	EventBillingCompleted EventType = "billing.completed"
	EventRunCompleted     EventType = "run.completed"
	EventRunFailed        EventType = "run.failed"
)

type Event struct {
	Type     EventType
	Delta    string
	Response map[string]any
	Billing  *Billing
	Error    *Error
}

type Billing struct {
	RequestID         string                      `json:"request_id"`
	SettlementStatus  string                      `json:"settlement_status"`
	PriceVersion      string                      `json:"price_version"`
	Currency          string                      `json:"currency"`
	Usage             BillingUsage                `json:"usage"`
	UnitPrices        BillingUnitPrices           `json:"unit_prices"`
	Charges           BillingCharges              `json:"charges"`
	Budget            *BillingBudget              `json:"budget,omitempty"`
	QuotaObservations []provider.QuotaObservation `json:"quota_observations,omitempty"`
}

type BillingUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type BillingUnitPrices struct {
	InputPerMillion  string `json:"input_per_million"`
	OutputPerMillion string `json:"output_per_million"`
}

type BillingCharges struct {
	Input  string `json:"input"`
	Output string `json:"output"`
	Total  string `json:"total"`
}

type BillingBudget struct {
	Mode      string `json:"mode"`
	MaxCharge string `json:"max_charge"`
	Exceeded  bool   `json:"exceeded"`
}

type Error struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *Error) Error() string { return e.Message }

func NewService(deployments []config.DeploymentConfig, adapters []provider.Adapter, options ...Option) (*Service, error) {
	service := &Service{
		deployments: make(map[string]Deployment, len(deployments)),
		providers:   make(map[string]provider.Adapter, len(adapters)),
	}
	for _, option := range options {
		option(service)
	}
	for _, adapter := range adapters {
		if _, exists := service.providers[adapter.ID()]; exists {
			return nil, fmt.Errorf("duplicate provider adapter %q", adapter.ID())
		}
		service.providers[adapter.ID()] = adapter
	}
	for _, item := range deployments {
		inputRate, err := pricing.ParseDecimal(item.Price.InputPerMillion)
		if err != nil {
			return nil, fmt.Errorf("deployment %q input price: %w", item.ID, err)
		}
		outputRate, err := pricing.ParseDecimal(item.Price.OutputPerMillion)
		if err != nil {
			return nil, fmt.Errorf("deployment %q output price: %w", item.ID, err)
		}
		if _, exists := service.providers[item.ProviderID]; !exists {
			return nil, fmt.Errorf("deployment %q has no provider adapter %q", item.ID, item.ProviderID)
		}
		deployment := Deployment{
			ID:            item.ID,
			ProviderID:    item.ProviderID,
			UpstreamModel: item.UpstreamModel,
			Price: pricing.PriceRevision{
				ID:               item.Price.Revision,
				Currency:         item.Price.Currency,
				InputPerMillion:  inputRate,
				OutputPerMillion: outputRate,
			},
			Enabled:             item.Enabled,
			HardBudgetSupported: item.HardBudgetSupported,
		}
		if item.ActualPrice != nil {
			actualInput, parseErr := pricing.ParseDecimal(item.ActualPrice.InputPerMillion)
			if parseErr != nil {
				return nil, fmt.Errorf("deployment %q actual input price: %w", item.ID, parseErr)
			}
			actualOutput, parseErr := pricing.ParseDecimal(item.ActualPrice.OutputPerMillion)
			if parseErr != nil {
				return nil, fmt.Errorf("deployment %q actual output price: %w", item.ID, parseErr)
			}
			deployment.ActualPrice = &pricing.PriceRevision{ID: item.ActualPrice.Revision, Currency: item.ActualPrice.Currency, InputPerMillion: actualInput, OutputPerMillion: actualOutput}
		}
		if item.ActualPoints != nil {
			pointsInput, parseErr := pricing.ParseDecimal(item.ActualPoints.InputPerMillion)
			if parseErr != nil {
				return nil, fmt.Errorf("deployment %q actual input points: %w", item.ID, parseErr)
			}
			pointsOutput, parseErr := pricing.ParseDecimal(item.ActualPoints.OutputPerMillion)
			if parseErr != nil {
				return nil, fmt.Errorf("deployment %q actual output points: %w", item.ID, parseErr)
			}
			deployment.ActualPoints = &pricing.PriceRevision{ID: item.ID + "-points", Currency: "POINTS", InputPerMillion: pointsInput, OutputPerMillion: pointsOutput}
		}
		service.deployments[item.ID] = deployment
	}
	return service, nil
}

func (s *Service) Models(client *auth.Client) []Model {
	if client == nil {
		return nil
	}
	models := make([]Model, 0, len(s.deployments))
	for _, deployment := range s.deployments {
		if deployment.Enabled && client.Allows(deployment.ID) {
			models = append(models, Model{ID: deployment.ID})
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

func (s *Service) Ready() bool {
	for _, deployment := range s.deployments {
		if deployment.Enabled {
			if _, ok := s.providers[deployment.ProviderID]; ok {
				return true
			}
		}
	}
	return false
}

func (s *Service) Start(ctx context.Context, client *auth.Client, request Request) (<-chan Event, string, error) {
	deployment, exists := s.deployments[request.Model]
	if !exists || !deployment.Enabled {
		return nil, "", &Error{Status: http.StatusNotFound, Code: "unknown_model_deployment", Message: "model deployment is not available"}
	}
	if client == nil || !client.Allows(deployment.ID) {
		return nil, "", &Error{Status: http.StatusForbidden, Code: "model_not_allowed", Message: "client is not allowed to use this model"}
	}
	runID, err := newRunID()
	if err != nil {
		return nil, "", &Error{Status: http.StatusInternalServerError, Code: "internal_error", Message: "could not create request id"}
	}
	fingerprint, err := requestFingerprint(request)
	if err != nil {
		return nil, "", &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "request could not be canonicalized"}
	}

	var parsedBudget *pricing.Budget
	if request.Budget != nil {
		maxCharge, parseErr := pricing.ParseDecimal(request.Budget.MaxCharge)
		if parseErr != nil {
			return nil, "", &Error{Status: http.StatusBadRequest, Code: "invalid_budget", Message: "budget max_charge is invalid"}
		}
		parsedBudget = &pricing.Budget{Mode: request.Budget.Mode, Currency: request.Budget.Currency, MaxCharge: maxCharge}
		if request.Budget.Mode == pricing.BudgetHard {
			if !deployment.HardBudgetSupported {
				return nil, "", &Error{Status: http.StatusUnprocessableEntity, Code: "hard_budget_not_enforceable", Message: "hard budget is not supported by this deployment"}
			}
			if request.MaxOutputTokens == nil {
				return nil, "", &Error{Status: http.StatusUnprocessableEntity, Code: "hard_budget_not_enforceable", Message: "hard budget requires max_output_tokens"}
			}
			inputEstimate := pricing.EstimateTextV1(request.CanonicalInput)
			evaluation, evalErr := pricing.EvaluateHardBudget(deployment.Price, inputEstimate.Tokens, *request.MaxOutputTokens, *parsedBudget)
			if evalErr != nil {
				return nil, "", &Error{Status: http.StatusBadRequest, Code: "invalid_budget", Message: evalErr.Error()}
			}
			if !evaluation.Allowed {
				return nil, "", &Error{Status: http.StatusPaymentRequired, Code: evaluation.Reason, Message: "request maximum charge exceeds budget"}
			}
		} else if request.Budget.Mode != pricing.BudgetSoft {
			return nil, "", &Error{Status: http.StatusBadRequest, Code: "invalid_budget", Message: "budget mode must be hard or soft"}
		}
	}

	if s.repository != nil {
		record, created, reserveErr := s.repository.Reserve(ctx, RunReservation{
			RunID:          runID,
			ClientID:       client.ID,
			DeploymentID:   deployment.ID,
			IdempotencyKey: request.IdempotencyKey,
			Fingerprint:    fingerprint,
		})
		if reserveErr != nil {
			return nil, "", &Error{Status: http.StatusInternalServerError, Code: "persistence_failed", Message: "request could not be persisted"}
		}
		if !created {
			if record.Fingerprint != fingerprint {
				return nil, record.RunID, &Error{Status: http.StatusConflict, Code: "idempotency_key_reused", Message: "idempotency key was already used for a different request", RequestID: record.RunID}
			}
			if record.Status == "completed" && record.SettlementState == "confirmed" {
				events, storedErr := storedCompletion(record)
				if storedErr != nil {
					return nil, record.RunID, &Error{Status: http.StatusInternalServerError, Code: "persistence_corrupt", Message: "stored response could not be restored", RequestID: record.RunID}
				}
				return events, record.RunID, nil
			}
			return nil, record.RunID, &Error{Status: http.StatusConflict, Code: "idempotency_in_progress", Message: "the original request has not completed successfully", RequestID: record.RunID}
		}
	}

	adapter := s.providers[deployment.ProviderID]
	providerCtx, cancelProvider := context.WithCancel(ctx)
	providerEvents, err := adapter.Start(providerCtx, provider.Request{
		RunID:           runID,
		DeploymentID:    deployment.ID,
		UpstreamModel:   deployment.UpstreamModel,
		Instructions:    request.Instructions,
		Input:           request.Input,
		CanonicalInput:  request.CanonicalInput,
		MaxOutputTokens: request.MaxOutputTokens,
		RawRequest:      request.RawRequest,
	})
	if err != nil {
		cancelProvider()
		if s.repository != nil {
			_ = s.repository.MarkFailed(ctx, runID, "unconfirmed", "provider_start_failed")
		}
		return nil, "", &Error{Status: http.StatusBadGateway, Code: "provider_start_failed", Message: "provider could not start the request"}
	}
	if s.repository != nil {
		if err := s.repository.MarkRunning(ctx, runID); err != nil {
			cancelProvider()
			s.markFailed(runID, "failed", "persistence_failed")
			return nil, runID, &Error{Status: http.StatusInternalServerError, Code: "persistence_failed", Message: "request state could not be persisted", RequestID: runID}
		}
	}

	events := make(chan Event)
	go s.consume(providerCtx, cancelProvider, events, providerEvents, runID, deployment, request, parsedBudget, client.IncludeQuotaObservations)
	return events, runID, nil
}

func (s *Service) consume(ctx context.Context, cancel context.CancelFunc, events chan<- Event, providerEvents <-chan provider.Event, runID string, deployment Deployment, request Request, budget *pricing.Budget, includeQuota bool) {
	defer close(events)
	defer cancel()
	var output strings.Builder
	for {
		select {
		case <-ctx.Done():
			s.markFailed(runID, "unconfirmed", "request_cancelled")
			return
		case event, ok := <-providerEvents:
			if !ok {
				return
			}
			switch event.Type {
			case provider.EventOutputTextDelta:
				output.WriteString(event.Delta)
				if !send(ctx, events, Event{Type: EventOutputTextDelta, Delta: event.Delta}) {
					return
				}
			case provider.EventCompleted:
				if event.Final == nil {
					return
				}
				outputText := event.Final.OutputText
				if outputText == "" {
					outputText = output.String()
				}
				usage, err := pricing.ResolveUsage(event.Final.Usage, request.CanonicalInput, outputText)
				if err != nil {
					return
				}
				settlement, err := pricing.Calculate(deployment.Price, usage)
				if err != nil {
					return
				}
				billing := newBilling(runID, deployment.Price, settlement, budget)
				actualRecords := calculateActualRecords(deployment, usage, event.Final.Costs)
				if includeQuota && len(event.Final.Quota) > 0 {
					billing.QuotaObservations = append([]provider.QuotaObservation(nil), event.Final.Quota...)
				}
				response := publicResponse(runID, deployment.ID, event.Final.Response, outputText, usage)
				if s.repository != nil {
					responseJSON, responseErr := json.Marshal(response)
					billingJSON, billingErr := json.Marshal(billing)
					budgetJSON, budgetErr := json.Marshal(billing.Budget)
					quotaJSON, quotaErr := json.Marshal(event.Final.Quota)
					actualJSON, actualErr := json.Marshal(actualRecords)
					if responseErr != nil || billingErr != nil || budgetErr != nil || quotaErr != nil || actualErr != nil {
						s.markFailed(runID, "failed", "settlement_encode_failed")
						send(ctx, events, Event{Type: EventRunFailed, Error: &Error{Status: http.StatusInternalServerError, Code: "settlement_failed", Message: "settlement could not be persisted", RequestID: runID}})
						return
					}
					inputChars, outputChars := int64(0), int64(0)
					estimator := ""
					if usage.InputEstimate != nil {
						inputChars = usage.InputEstimate.Characters
						estimator = usage.InputEstimate.Version
					}
					if usage.OutputEstimate != nil {
						outputChars = usage.OutputEstimate.Characters
						estimator = usage.OutputEstimate.Version
					}
					persistCtx, cancelPersist := context.WithTimeout(context.Background(), 5*time.Second)
					persistErr := s.repository.Complete(persistCtx, RunCompletion{
						RunID: runID, ResponseJSON: responseJSON, BillingJSON: billingJSON,
						InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
						InputSource: string(usage.InputSource), OutputSource: string(usage.OutputSource),
						Estimator: estimator, InputChars: inputChars, OutputChars: outputChars,
						PriceVersion: billing.PriceVersion, Currency: billing.Currency,
						InputUnitPrice: billing.UnitPrices.InputPerMillion, OutputUnitPrice: billing.UnitPrices.OutputPerMillion,
						InputCharge: billing.Charges.Input, OutputCharge: billing.Charges.Output, TotalCharge: billing.Charges.Total,
						BudgetJSON:       budgetJSON,
						QuotaJSON:        quotaJSON,
						ProviderID:       deployment.ProviderID,
						UpstreamCostJSON: actualJSON,
					})
					cancelPersist()
					if persistErr != nil {
						s.markFailed(runID, "failed", "settlement_persist_failed")
						send(ctx, events, Event{Type: EventRunFailed, Error: &Error{Status: http.StatusInternalServerError, Code: "settlement_failed", Message: "settlement could not be persisted", RequestID: runID}})
						return
					}
				}
				if !send(ctx, events, Event{Type: EventBillingCompleted, Billing: &billing}) {
					return
				}
				send(ctx, events, Event{Type: EventRunCompleted, Response: response, Billing: &billing})
				return
			case provider.EventFailed:
				s.markFailed(runID, "unconfirmed", "provider_stream_failed")
				send(ctx, events, Event{Type: EventRunFailed, Error: &Error{
					Status:  http.StatusBadGateway,
					Code:    "provider_stream_failed",
					Message: "provider stream failed before completion",
				}})
				return
			}
		}
	}
}

type actualRecord struct {
	Unit         string `json:"unit"`
	Source       string `json:"source"`
	InputCharge  string `json:"input"`
	OutputCharge string `json:"output"`
	TotalCharge  string `json:"total"`
}

func calculateActualRecords(deployment Deployment, usage pricing.BillableUsage, reported []provider.CostObservation) []actualRecord {
	records := make([]actualRecord, 0, 2)
	reportedUnits := make(map[string]struct{}, len(reported))
	for _, item := range reported {
		unit := strings.ToUpper(strings.TrimSpace(item.Unit))
		if unit == "" && deployment.ActualPrice != nil {
			unit = deployment.ActualPrice.Currency
		}
		if unit == "" {
			unit = "UNSPECIFIED"
		}
		total, err := pricing.ParseDecimal(item.Total)
		if err != nil || total.Nanos() < 0 {
			continue
		}
		input := ""
		if value, parseErr := pricing.ParseDecimal(item.Input); parseErr == nil && value.Nanos() >= 0 {
			input = value.String()
		}
		output := ""
		if value, parseErr := pricing.ParseDecimal(item.Output); parseErr == nil && value.Nanos() >= 0 {
			output = value.String()
		}
		records = append(records, actualRecord{Unit: unit, Source: "provider_reported", InputCharge: input, OutputCharge: output, TotalCharge: total.String()})
		reportedUnits[unit] = struct{}{}
	}
	if deployment.ActualPrice != nil {
		_, alreadyReported := reportedUnits[strings.ToUpper(deployment.ActualPrice.Currency)]
		if settlement, err := pricing.Calculate(*deployment.ActualPrice, usage); err == nil && !alreadyReported {
			records = append(records, actualRecord{Unit: deployment.ActualPrice.Currency, Source: "configured_estimate", InputCharge: settlement.InputCharge.String(), OutputCharge: settlement.OutputCharge.String(), TotalCharge: settlement.TotalCharge.String()})
		}
	}
	if deployment.ActualPoints != nil {
		_, alreadyReported := reportedUnits["POINTS"]
		if settlement, err := pricing.Calculate(*deployment.ActualPoints, usage); err == nil && !alreadyReported {
			records = append(records, actualRecord{Unit: "POINTS", Source: "configured_estimate", InputCharge: settlement.InputCharge.String(), OutputCharge: settlement.OutputCharge.String(), TotalCharge: settlement.TotalCharge.String()})
		}
	}
	return records
}

func (s *Service) markFailed(runID, settlementStatus, code string) {
	if s.repository == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.repository.MarkFailed(ctx, runID, settlementStatus, code)
}

func requestFingerprint(request Request) (string, error) {
	var value any
	if len(request.RawRequest) == 0 {
		value = map[string]any{
			"model":             request.Model,
			"instructions":      request.Instructions,
			"canonical_input":   request.CanonicalInput,
			"max_output_tokens": request.MaxOutputTokens,
		}
	} else if err := json.Unmarshal(request.RawRequest, &value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(append([]byte(request.Model+"\n"), canonical...))
	return hex.EncodeToString(hash[:]), nil
}

func newBilling(runID string, price pricing.PriceRevision, settlement pricing.Settlement, budget *pricing.Budget) Billing {
	billing := Billing{
		RequestID:        runID,
		SettlementStatus: "confirmed",
		PriceVersion:     publicPriceVersion(price.ID),
		Currency:         price.Currency,
		Usage: BillingUsage{
			InputTokens:  settlement.Usage.InputTokens,
			OutputTokens: settlement.Usage.OutputTokens,
		},
		UnitPrices: BillingUnitPrices{
			InputPerMillion:  price.InputPerMillion.String(),
			OutputPerMillion: price.OutputPerMillion.String(),
		},
		Charges: BillingCharges{
			Input:  settlement.InputCharge.String(),
			Output: settlement.OutputCharge.String(),
			Total:  settlement.TotalCharge.String(),
		},
	}
	if budget != nil {
		billing.Budget = &BillingBudget{
			Mode:      string(budget.Mode),
			MaxCharge: budget.MaxCharge.String(),
			Exceeded:  settlement.TotalCharge.Nanos() > budget.MaxCharge.Nanos(),
		}
	}
	return billing
}

func minimalResponse(runID, model, outputText string, usage pricing.BillableUsage) map[string]any {
	return map[string]any{
		"id":     strings.Replace(runID, "req_", "resp_", 1),
		"object": "response",
		"status": "completed",
		"model":  model,
		"output": []any{map[string]any{
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": outputText,
			}},
		}},
		"usage": map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
			"total_tokens":  usage.InputTokens + usage.OutputTokens,
		},
	}
}

func publicResponse(runID, model string, upstream map[string]any, outputText string, usage pricing.BillableUsage) map[string]any {
	if upstream == nil {
		return minimalResponse(runID, model, outputText, usage)
	}
	response := map[string]any{
		"id":     strings.Replace(runID, "req_", "resp_", 1),
		"object": "response",
		"status": "completed",
		"model":  model,
	}
	// Some compatible providers echo injected prompts, tools, routing keys, or
	// account metadata. Only contract fields are allowed across this boundary.
	for _, key := range []string{"created_at", "completed_at", "error", "incomplete_details", "output"} {
		if value, exists := upstream[key]; exists {
			response[key] = value
		}
	}
	if status, ok := upstream["status"].(string); ok && status != "" {
		response["status"] = status
	}
	if _, exists := response["output"]; !exists {
		response["output"] = minimalResponse(runID, model, outputText, usage)["output"]
	}
	return response
}

func publicPriceVersion(revision string) string {
	hash := sha256.Sum256([]byte(revision))
	return "price_" + hex.EncodeToString(hash[:8])
}

func newRunID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "req_" + hex.EncodeToString(value), nil
}

func send(ctx context.Context, target chan<- Event, event Event) bool {
	select {
	case <-ctx.Done():
		return false
	case target <- event:
		return true
	}
}

func AsError(err error) *Error {
	var gatewayError *Error
	if errors.As(err, &gatewayError) {
		return gatewayError
	}
	return &Error{Status: http.StatusInternalServerError, Code: "internal_error", Message: "internal server error"}
}
