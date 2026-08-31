package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

CREATE TABLE IF NOT EXISTS upstream_cost_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES runs(id),
    provider_id TEXT NOT NULL,
    payload_json BLOB NOT NULL,
    observed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS upstream_cost_records_provider_time_idx ON upstream_cost_records(provider_id, observed_at);

CREATE TABLE IF NOT EXISTS compatibility_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    adapter_id TEXT NOT NULL,
    fixture_version TEXT NOT NULL,
    passed INTEGER NOT NULL,
    result_json BLOB NOT NULL,
    created_at TEXT NOT NULL
);
`
