package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/gateway"
	"github.com/zhyuzh3d/llmserver/internal/pricing"
	_ "modernc.org/sqlite"
)

type SQLite struct {
	db *sql.DB
}

func Open(path string) (*SQLite, error) {
	if path == "" {
		return nil, errors.New("SQLite state path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	// One connection makes in-memory databases deterministic and serializes the
	// short reservation/settlement transactions. WAL still permits readers.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &SQLite{db: db}
	if err := store.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) initialize(ctx context.Context) error {
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
	} {
		if _, err := s.db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("configure SQLite: %w", err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
		return fmt.Errorf("migrate SQLite schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite migration: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE runs SET status = 'failed', settlement_status = 'unconfirmed', error_code = 'recovered_incomplete_run', updated_at = ?
		WHERE status IN ('accepted', 'running')`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("recover incomplete runs: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA optimize"); err != nil {
		return fmt.Errorf("optimize SQLite: %w", err)
	}
	return nil
}

func (s *SQLite) Reserve(ctx context.Context, reservation gateway.RunReservation) (gateway.StoredRun, bool, error) {
	if reservation.RunID == "" || reservation.ClientID == "" || reservation.DeploymentID == "" || reservation.Fingerprint == "" {
		return gateway.StoredRun{}, false, errors.New("incomplete run reservation")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return gateway.StoredRun{}, false, err
	}
	defer tx.Rollback()
	if reservation.IdempotencyKey != "" {
		record, found, lookupErr := lookupIdempotency(ctx, tx, reservation.ClientID, reservation.IdempotencyKey)
		if lookupErr != nil {
			return gateway.StoredRun{}, false, lookupErr
		}
		if found {
			if err := tx.Commit(); err != nil {
				return gateway.StoredRun{}, false, err
			}
			return record, false, nil
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO runs(id, client_id, deployment_id, status, settlement_status, request_fingerprint, created_at, updated_at)
		VALUES(?, ?, ?, 'accepted', 'pending', ?, ?, ?)`,
		reservation.RunID, reservation.ClientID, reservation.DeploymentID, reservation.Fingerprint, now, now)
	if err != nil {
		return gateway.StoredRun{}, false, err
	}
	if reservation.IdempotencyKey != "" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO idempotency_keys(client_id, idempotency_key, run_id, request_fingerprint, created_at)
			VALUES(?, ?, ?, ?, ?)`,
			reservation.ClientID, reservation.IdempotencyKey, reservation.RunID, reservation.Fingerprint, now)
		if err != nil {
			return gateway.StoredRun{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return gateway.StoredRun{}, false, err
	}
	return gateway.StoredRun{RunID: reservation.RunID, Status: "accepted", SettlementState: "pending", Fingerprint: reservation.Fingerprint}, true, nil
}

func lookupIdempotency(ctx context.Context, tx *sql.Tx, clientID, key string) (gateway.StoredRun, bool, error) {
	var record gateway.StoredRun
	var responseJSON, billingJSON []byte
	err := tx.QueryRowContext(ctx, `
		SELECT r.id, r.status, r.settlement_status, i.request_fingerprint,
		       COALESCE(r.response_json, X''), COALESCE(r.billing_json, X'')
		FROM idempotency_keys i JOIN runs r ON r.id = i.run_id
		WHERE i.client_id = ? AND i.idempotency_key = ?`, clientID, key).
		Scan(&record.RunID, &record.Status, &record.SettlementState, &record.Fingerprint, &responseJSON, &billingJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return gateway.StoredRun{}, false, nil
	}
	if err != nil {
		return gateway.StoredRun{}, false, err
	}
	record.ResponseJSON = responseJSON
	record.BillingJSON = billingJSON
	return record, true, nil
}

func (s *SQLite) MarkRunning(ctx context.Context, runID string) error {
	return s.updateState(ctx, runID, "running", "pending", "")
}

func (s *SQLite) MarkFailed(ctx context.Context, runID, settlementStatus, errorCode string) error {
	if settlementStatus != "unconfirmed" && settlementStatus != "failed" && settlementStatus != "not_chargeable" {
		return errors.New("invalid failed settlement status")
	}
	return s.updateState(ctx, runID, "failed", settlementStatus, errorCode)
}

func (s *SQLite) updateState(ctx context.Context, runID, status, settlementStatus, errorCode string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, settlement_status = ?, error_code = ?, updated_at = ?
		WHERE id = ? AND status IN ('accepted', 'running')`,
		status, settlementStatus, nullable(errorCode), time.Now().UTC().Format(time.RFC3339Nano), runID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("run %q is not mutable", runID)
	}
	return nil
}

func (s *SQLite) Complete(ctx context.Context, completion gateway.RunCompletion) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO run_usage(run_id, input_tokens, output_tokens, input_source, output_source, estimator_version, input_characters, output_characters)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		completion.RunID, completion.InputTokens, completion.OutputTokens, completion.InputSource, completion.OutputSource,
		nullable(completion.Estimator), completion.InputChars, completion.OutputChars)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO run_charges(run_id, price_version, currency, input_per_million, output_per_million, input_charge, output_charge, total_charge, budget_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		completion.RunID, completion.PriceVersion, completion.Currency, completion.InputUnitPrice, completion.OutputUnitPrice,
		completion.InputCharge, completion.OutputCharge, completion.TotalCharge, nullableBytes(completion.BudgetJSON))
	if err != nil {
		return err
	}
	if len(completion.QuotaJSON) > 0 && string(completion.QuotaJSON) != "null" && string(completion.QuotaJSON) != "[]" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO run_quota_observations(run_id, phase, payload_json, observed_at)
			VALUES(?, 'completed', ?, ?)`, completion.RunID, completion.QuotaJSON, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	if len(completion.UpstreamCostJSON) > 0 && string(completion.UpstreamCostJSON) != "null" && string(completion.UpstreamCostJSON) != "[]" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO upstream_cost_records(run_id, provider_id, payload_json, observed_at)
			VALUES(?, ?, ?, ?)`, completion.RunID, completion.ProviderID, completion.UpstreamCostJSON, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = 'completed', settlement_status = 'confirmed', response_json = ?, billing_json = ?, updated_at = ?
		WHERE id = ? AND status IN ('accepted', 'running')`,
		completion.ResponseJSON, completion.BillingJSON, time.Now().UTC().Format(time.RFC3339Nano), completion.RunID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("run %q is not completable", completion.RunID)
	}
	return tx.Commit()
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableBytes(value []byte) any {
	if len(value) == 0 || string(value) == "null" {
		return nil
	}
	return value
}

type UsageSummary struct {
	Deployments []DeploymentUsage `json:"deployments"`
	Actual      []ActualUsage     `json:"actual"`
	Quotas      []QuotaUsage      `json:"quotas"`
}

type DeploymentUsage struct {
	DeploymentID string `json:"deployment_id"`
	Runs         int64  `json:"runs"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Currency     string `json:"currency"`
	PublicTotal  string `json:"public_total"`
}

type ActualUsage struct {
	ProviderID   string `json:"provider_id"`
	DeploymentID string `json:"deployment_id"`
	Unit         string `json:"unit"`
	Source       string `json:"source"`
	Total        string `json:"total"`
}

type QuotaUsage struct {
	DeploymentID string          `json:"deployment_id"`
	ObservedAt   string          `json:"observed_at"`
	Observations json.RawMessage `json:"observations"`
}

type UsageReportFilter struct {
	Since                time.Time
	Until                time.Time
	GroupBy              string
	ProviderID           string
	ClientID             string
	ProviderByDeployment map[string]string
}

type UsageReport struct {
	Since   string       `json:"since"`
	Until   string       `json:"until"`
	GroupBy string       `json:"group_by"`
	Groups  []UsageGroup `json:"groups"`
}

type UsageGroup struct {
	ID           string          `json:"id"`
	Runs         int64           `json:"runs"`
	InputTokens  int64           `json:"input_tokens"`
	OutputTokens int64           `json:"output_tokens"`
	PublicTotals []UnitTotal     `json:"public_totals"`
	ActualTotals []ActualTotal   `json:"actual_totals"`
	QuotaTotals  []QuotaTotal    `json:"quota_totals"`
	Models       []ModelUsageRow `json:"models"`
}

type ModelUsageRow struct {
	DeploymentID string        `json:"deployment_id"`
	ProviderID   string        `json:"provider_id"`
	Runs         int64         `json:"runs"`
	InputTokens  int64         `json:"input_tokens"`
	OutputTokens int64         `json:"output_tokens"`
	PublicTotals []UnitTotal   `json:"public_totals"`
	ActualTotals []ActualTotal `json:"actual_totals"`
	QuotaTotals  []QuotaTotal  `json:"quota_totals"`
}

type UnitTotal struct {
	Unit  string `json:"unit"`
	Total string `json:"total"`
}

type ActualTotal struct {
	ProviderID string `json:"provider_id"`
	Unit       string `json:"unit"`
	Source     string `json:"source"`
	Total      string `json:"total"`
}

type QuotaTotal struct {
	ProviderID            string   `json:"provider_id"`
	DeploymentID          string   `json:"deployment_id,omitempty"`
	LimitID               string   `json:"limit_id"`
	Unit                  string   `json:"unit"`
	Delta                 float64  `json:"delta"`
	LatestBefore          *float64 `json:"latest_before,omitempty"`
	LatestAfter           *float64 `json:"latest_after,omitempty"`
	WindowDurationMinutes *int64   `json:"window_duration_minutes,omitempty"`
	ResetsAt              *int64   `json:"resets_at,omitempty"`
	Observations          int64    `json:"observations"`
	Status                string   `json:"status"`
}

func (s *SQLite) UsageSummary(ctx context.Context) (UsageSummary, error) {
	var summary UsageSummary
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.deployment_id, ru.input_tokens, ru.output_tokens, rc.currency, rc.total_charge
		FROM runs r JOIN run_usage ru ON ru.run_id = r.id JOIN run_charges rc ON rc.run_id = r.id
		WHERE r.status = 'completed' AND r.settlement_status = 'confirmed'
		ORDER BY r.created_at`)
	if err != nil {
		return summary, err
	}
	deploymentIndex := map[string]int{}
	for rows.Next() {
		var deploymentID, currency, totalText string
		var inputTokens, outputTokens int64
		if err := rows.Scan(&deploymentID, &inputTokens, &outputTokens, &currency, &totalText); err != nil {
			rows.Close()
			return summary, err
		}
		key := deploymentID + "\x00" + currency
		index, exists := deploymentIndex[key]
		if !exists {
			index = len(summary.Deployments)
			deploymentIndex[key] = index
			summary.Deployments = append(summary.Deployments, DeploymentUsage{DeploymentID: deploymentID, Currency: currency, PublicTotal: "0.000000000"})
		}
		item := &summary.Deployments[index]
		item.Runs++
		item.InputTokens += inputTokens
		item.OutputTokens += outputTokens
		current, parseErr := pricing.ParseDecimal(item.PublicTotal)
		if parseErr != nil {
			rows.Close()
			return summary, parseErr
		}
		value, parseErr := pricing.ParseDecimal(totalText)
		if parseErr != nil {
			rows.Close()
			return summary, parseErr
		}
		sum, addErr := current.Add(value)
		if addErr != nil {
			rows.Close()
			return summary, addErr
		}
		item.PublicTotal = sum.String()
	}
	if err := rows.Close(); err != nil {
		return summary, err
	}

	actualRows, err := s.db.QueryContext(ctx, `
		SELECT u.provider_id, r.deployment_id, u.payload_json
		FROM upstream_cost_records u JOIN runs r ON r.id = u.run_id
		WHERE r.status = 'completed' ORDER BY u.observed_at`)
	if err != nil {
		return summary, err
	}
	type record struct {
		Unit   string `json:"unit"`
		Source string `json:"source"`
		Total  string `json:"total"`
	}
	actualIndex := map[string]int{}
	for actualRows.Next() {
		var providerID, deploymentID string
		var payload []byte
		if err := actualRows.Scan(&providerID, &deploymentID, &payload); err != nil {
			actualRows.Close()
			return summary, err
		}
		var records []record
		if err := json.Unmarshal(payload, &records); err != nil {
			continue
		}
		for _, row := range records {
			key := providerID + "\x00" + deploymentID + "\x00" + row.Unit + "\x00" + row.Source
			index, exists := actualIndex[key]
			if !exists {
				index = len(summary.Actual)
				actualIndex[key] = index
				summary.Actual = append(summary.Actual, ActualUsage{ProviderID: providerID, DeploymentID: deploymentID, Unit: row.Unit, Source: row.Source, Total: "0.000000000"})
			}
			current, parseErr := pricing.ParseDecimal(summary.Actual[index].Total)
			if parseErr != nil {
				continue
			}
			value, parseErr := pricing.ParseDecimal(row.Total)
			if parseErr != nil {
				continue
			}
			sum, addErr := current.Add(value)
			if addErr == nil {
				summary.Actual[index].Total = sum.String()
			}
		}
	}
	if err := actualRows.Close(); err != nil {
		return summary, err
	}

	quotaRows, err := s.db.QueryContext(ctx, `
		SELECT r.deployment_id, q.observed_at, q.payload_json
		FROM run_quota_observations q JOIN runs r ON r.id = q.run_id
		ORDER BY q.observed_at DESC LIMIT 50`)
	if err != nil {
		return summary, err
	}
	for quotaRows.Next() {
		var item QuotaUsage
		var payload []byte
		if err := quotaRows.Scan(&item.DeploymentID, &item.ObservedAt, &payload); err != nil {
			quotaRows.Close()
			return summary, err
		}
		item.Observations = append(json.RawMessage(nil), payload...)
		summary.Quotas = append(summary.Quotas, item)
	}
	if err := quotaRows.Close(); err != nil {
		return summary, err
	}
	return summary, nil
}

type reportBucket struct {
	id           string
	runIDs       map[string]struct{}
	inputTokens  int64
	outputTokens int64
	public       map[string]pricing.Decimal
	actual       map[string]pricing.Decimal
	quota        map[string]*QuotaTotal
	models       map[string]*reportModel
}

type reportModel struct {
	providerID   string
	runIDs       map[string]struct{}
	inputTokens  int64
	outputTokens int64
	public       map[string]pricing.Decimal
	actual       map[string]pricing.Decimal
	quota        map[string]*QuotaTotal
}

func (s *SQLite) UsageReport(ctx context.Context, filter UsageReportFilter) (UsageReport, error) {
	if filter.GroupBy != "provider" && filter.GroupBy != "client" {
		return UsageReport{}, errors.New("usage report group_by must be provider or client")
	}
	if filter.Until.IsZero() {
		filter.Until = time.Now().UTC()
	}
	if filter.Since.IsZero() || !filter.Since.Before(filter.Until) {
		return UsageReport{}, errors.New("usage report requires a valid time window")
	}
	report := UsageReport{Since: filter.Since.UTC().Format(time.RFC3339Nano), Until: filter.Until.UTC().Format(time.RFC3339Nano), GroupBy: filter.GroupBy}
	buckets := map[string]*reportBucket{}

	publicRows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.client_id, r.deployment_id, ru.input_tokens, ru.output_tokens, rc.currency, rc.total_charge
		FROM runs r JOIN run_usage ru ON ru.run_id = r.id JOIN run_charges rc ON rc.run_id = r.id
		WHERE r.status = 'completed' AND r.settlement_status = 'confirmed' AND r.created_at >= ? AND r.created_at < ?
		ORDER BY r.created_at`, report.Since, report.Until)
	if err != nil {
		return report, err
	}
	for publicRows.Next() {
		var runID, clientID, deploymentID, currency, total string
		var inputTokens, outputTokens int64
		if err := publicRows.Scan(&runID, &clientID, &deploymentID, &inputTokens, &outputTokens, &currency, &total); err != nil {
			publicRows.Close()
			return report, err
		}
		providerID := filter.ProviderByDeployment[deploymentID]
		if providerID == "" {
			providerID = "unknown"
		}
		if !reportRunMatches(filter, clientID, providerID) {
			continue
		}
		groupID := providerID
		if filter.GroupBy == "client" {
			groupID = clientID
		}
		bucket := ensureReportBucket(buckets, groupID)
		model := ensureReportModel(bucket, deploymentID, providerID)
		bucket.runIDs[runID] = struct{}{}
		model.runIDs[runID] = struct{}{}
		bucket.inputTokens += inputTokens
		bucket.outputTokens += outputTokens
		model.inputTokens += inputTokens
		model.outputTokens += outputTokens
		if err := addReportDecimal(bucket.public, currency, total); err != nil {
			publicRows.Close()
			return report, err
		}
		if err := addReportDecimal(model.public, currency, total); err != nil {
			publicRows.Close()
			return report, err
		}
	}
	if err := publicRows.Err(); err != nil {
		publicRows.Close()
		return report, err
	}
	if err := publicRows.Close(); err != nil {
		return report, err
	}

	actualRows, err := s.db.QueryContext(ctx, `
		SELECT r.client_id, r.deployment_id, u.provider_id, u.payload_json
		FROM upstream_cost_records u JOIN runs r ON r.id = u.run_id
		WHERE r.status = 'completed' AND u.observed_at >= ? AND u.observed_at < ?
		ORDER BY u.observed_at`, report.Since, report.Until)
	if err != nil {
		return report, err
	}
	type actualRecord struct {
		Unit   string `json:"unit"`
		Source string `json:"source"`
		Total  string `json:"total"`
	}
	for actualRows.Next() {
		var clientID, deploymentID, providerID string
		var payload []byte
		if err := actualRows.Scan(&clientID, &deploymentID, &providerID, &payload); err != nil {
			actualRows.Close()
			return report, err
		}
		if !reportRunMatches(filter, clientID, providerID) {
			continue
		}
		groupID := providerID
		if filter.GroupBy == "client" {
			groupID = clientID
		}
		bucket := ensureReportBucket(buckets, groupID)
		model := ensureReportModel(bucket, deploymentID, providerID)
		var records []actualRecord
		if err := json.Unmarshal(payload, &records); err != nil {
			continue
		}
		for _, record := range records {
			key := providerID + "\x00" + record.Unit + "\x00" + record.Source
			if err := addReportDecimal(bucket.actual, key, record.Total); err != nil {
				continue
			}
			if err := addReportDecimal(model.actual, key, record.Total); err != nil {
				continue
			}
		}
	}
	if err := actualRows.Err(); err != nil {
		actualRows.Close()
		return report, err
	}
	if err := actualRows.Close(); err != nil {
		return report, err
	}

	quotaRows, err := s.db.QueryContext(ctx, `
		SELECT r.client_id, r.deployment_id, q.payload_json
		FROM run_quota_observations q JOIN runs r ON r.id = q.run_id
		WHERE r.status = 'completed' AND q.observed_at >= ? AND q.observed_at < ?
		ORDER BY q.observed_at`, report.Since, report.Until)
	if err != nil {
		return report, err
	}
	type quotaObservation struct {
		LimitID               string   `json:"limit_id"`
		Unit                  string   `json:"unit"`
		Before                *float64 `json:"before"`
		After                 *float64 `json:"after"`
		Delta                 *float64 `json:"delta"`
		WindowDurationMinutes *int64   `json:"window_duration_minutes"`
		ResetsAt              *int64   `json:"resets_at"`
		Status                string   `json:"status"`
	}
	for quotaRows.Next() {
		var clientID, deploymentID string
		var payload []byte
		if err := quotaRows.Scan(&clientID, &deploymentID, &payload); err != nil {
			quotaRows.Close()
			return report, err
		}
		providerID := filter.ProviderByDeployment[deploymentID]
		if providerID == "" {
			providerID = "unknown"
		}
		if !reportRunMatches(filter, clientID, providerID) {
			continue
		}
		groupID := providerID
		if filter.GroupBy == "client" {
			groupID = clientID
		}
		bucket := ensureReportBucket(buckets, groupID)
		model := ensureReportModel(bucket, deploymentID, providerID)
		var observations []quotaObservation
		if err := json.Unmarshal(payload, &observations); err != nil {
			continue
		}
		for _, observation := range observations {
			addQuota(bucket.quota, providerID, "", observation.LimitID, observation.Unit, observation.Status, observation.Before, observation.After, observation.Delta, observation.WindowDurationMinutes, observation.ResetsAt)
			addQuota(model.quota, providerID, deploymentID, observation.LimitID, observation.Unit, observation.Status, observation.Before, observation.After, observation.Delta, observation.WindowDurationMinutes, observation.ResetsAt)
		}
	}
	if err := quotaRows.Err(); err != nil {
		quotaRows.Close()
		return report, err
	}
	if err := quotaRows.Close(); err != nil {
		return report, err
	}

	report.Groups = renderReportBuckets(buckets)
	return report, nil
}

func reportRunMatches(filter UsageReportFilter, clientID, providerID string) bool {
	return (filter.ClientID == "" || filter.ClientID == clientID) && (filter.ProviderID == "" || filter.ProviderID == providerID)
}

func ensureReportBucket(buckets map[string]*reportBucket, id string) *reportBucket {
	if bucket := buckets[id]; bucket != nil {
		return bucket
	}
	bucket := &reportBucket{id: id, runIDs: map[string]struct{}{}, public: map[string]pricing.Decimal{}, actual: map[string]pricing.Decimal{}, quota: map[string]*QuotaTotal{}, models: map[string]*reportModel{}}
	buckets[id] = bucket
	return bucket
}

func ensureReportModel(bucket *reportBucket, deploymentID, providerID string) *reportModel {
	if model := bucket.models[deploymentID]; model != nil {
		return model
	}
	model := &reportModel{providerID: providerID, runIDs: map[string]struct{}{}, public: map[string]pricing.Decimal{}, actual: map[string]pricing.Decimal{}, quota: map[string]*QuotaTotal{}}
	bucket.models[deploymentID] = model
	return model
}

func addReportDecimal(target map[string]pricing.Decimal, key, raw string) error {
	value, err := pricing.ParseDecimal(raw)
	if err != nil {
		return err
	}
	current, exists := target[key]
	if !exists {
		current, _ = pricing.ParseDecimal("0")
	}
	sum, err := current.Add(value)
	if err != nil {
		return err
	}
	target[key] = sum
	return nil
}

func addQuota(target map[string]*QuotaTotal, providerID, deploymentID, limitID, unit, status string, before, after, delta *float64, windowDurationMinutes, resetsAt *int64) {
	key := providerID + "\x00" + deploymentID + "\x00" + limitID + "\x00" + unit
	item := target[key]
	if item == nil {
		item = &QuotaTotal{ProviderID: providerID, DeploymentID: deploymentID, LimitID: limitID, Unit: unit, Status: status}
		target[key] = item
	}
	item.Observations++
	item.Status = status
	item.LatestBefore = before
	item.LatestAfter = after
	item.WindowDurationMinutes = windowDurationMinutes
	item.ResetsAt = resetsAt
	if status == "observed" && delta != nil {
		item.Delta += *delta
	}
}

func renderReportBuckets(buckets map[string]*reportBucket) []UsageGroup {
	ids := make([]string, 0, len(buckets))
	for id := range buckets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	groups := make([]UsageGroup, 0, len(ids))
	for _, id := range ids {
		bucket := buckets[id]
		group := UsageGroup{ID: id, Runs: int64(len(bucket.runIDs)), InputTokens: bucket.inputTokens, OutputTokens: bucket.outputTokens, PublicTotals: renderUnitTotals(bucket.public), ActualTotals: renderActualTotals(bucket.actual), QuotaTotals: renderQuotaTotals(bucket.quota)}
		modelIDs := make([]string, 0, len(bucket.models))
		for modelID := range bucket.models {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, modelID := range modelIDs {
			model := bucket.models[modelID]
			group.Models = append(group.Models, ModelUsageRow{DeploymentID: modelID, ProviderID: model.providerID, Runs: int64(len(model.runIDs)), InputTokens: model.inputTokens, OutputTokens: model.outputTokens, PublicTotals: renderUnitTotals(model.public), ActualTotals: renderActualTotals(model.actual), QuotaTotals: renderQuotaTotals(model.quota)})
		}
		groups = append(groups, group)
	}
	return groups
}

func renderUnitTotals(values map[string]pricing.Decimal) []UnitTotal {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]UnitTotal, 0, len(keys))
	for _, key := range keys {
		result = append(result, UnitTotal{Unit: key, Total: values[key].String()})
	}
	return result
}

func renderActualTotals(values map[string]pricing.Decimal) []ActualTotal {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]ActualTotal, 0, len(keys))
	for _, key := range keys {
		parts := strings.Split(key, "\x00")
		if len(parts) != 3 {
			continue
		}
		result = append(result, ActualTotal{ProviderID: parts[0], Unit: parts[1], Source: parts[2], Total: values[key].String()})
	}
	return result
}

func renderQuotaTotals(values map[string]*QuotaTotal) []QuotaTotal {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]QuotaTotal, 0, len(keys))
	for _, key := range keys {
		result = append(result, *values[key])
	}
	return result
}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);
INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES(1, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY,
    client_id TEXT NOT NULL,
    deployment_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('accepted', 'running', 'completed', 'failed')),
    settlement_status TEXT NOT NULL CHECK(settlement_status IN ('pending', 'confirmed', 'unconfirmed', 'not_chargeable', 'failed')),
    request_fingerprint TEXT NOT NULL,
    response_json BLOB,
    billing_json BLOB,
    error_code TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS runs_client_created_idx ON runs(client_id, created_at);
CREATE INDEX IF NOT EXISTS runs_deployment_status_idx ON runs(deployment_id, status);
CREATE INDEX IF NOT EXISTS runs_status_created_idx ON runs(status, created_at);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    client_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    run_id TEXT NOT NULL UNIQUE REFERENCES runs(id),
    request_fingerprint TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(client_id, idempotency_key)
);

CREATE TABLE IF NOT EXISTS run_usage (
    run_id TEXT PRIMARY KEY REFERENCES runs(id),
    input_tokens INTEGER NOT NULL CHECK(input_tokens >= 0),
    output_tokens INTEGER NOT NULL CHECK(output_tokens >= 0),
    input_source TEXT NOT NULL,
    output_source TEXT NOT NULL,
    estimator_version TEXT,
    input_characters INTEGER NOT NULL DEFAULT 0,
    output_characters INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS run_charges (
    run_id TEXT PRIMARY KEY REFERENCES runs(id),
    price_version TEXT NOT NULL,
    currency TEXT NOT NULL,
    input_per_million TEXT NOT NULL,
    output_per_million TEXT NOT NULL,
    input_charge TEXT NOT NULL,
    output_charge TEXT NOT NULL,
    total_charge TEXT NOT NULL,
    budget_json BLOB
);

CREATE TABLE IF NOT EXISTS run_quota_observations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES runs(id),
    phase TEXT NOT NULL,
    payload_json BLOB NOT NULL,
    observed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS run_quota_observations_run_time_idx ON run_quota_observations(run_id, observed_at);
CREATE INDEX IF NOT EXISTS run_quota_observations_time_idx ON run_quota_observations(observed_at);

CREATE TABLE IF NOT EXISTS upstream_cost_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES runs(id),
    provider_id TEXT NOT NULL,
    payload_json BLOB NOT NULL,
    observed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS upstream_cost_records_provider_time_idx ON upstream_cost_records(provider_id, observed_at);
CREATE INDEX IF NOT EXISTS upstream_cost_records_time_idx ON upstream_cost_records(observed_at);

CREATE TABLE IF NOT EXISTS compatibility_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    adapter_id TEXT NOT NULL,
    fixture_version TEXT NOT NULL,
    passed INTEGER NOT NULL,
    result_json BLOB NOT NULL,
    created_at TEXT NOT NULL
);
`
