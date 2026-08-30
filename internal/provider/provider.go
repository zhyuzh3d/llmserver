package provider

import (
	"context"
	"encoding/json"

	"github.com/zhyuzh3d/llmserver/internal/pricing"
)

type Request struct {
	RunID           string
	DeploymentID    string
	UpstreamModel   string
	Instructions    string
	Input           json.RawMessage
	CanonicalInput  string
	MaxOutputTokens *int64
	RawRequest      json.RawMessage
}

type EventType string

const (
	EventOutputTextDelta EventType = "output_text.delta"
	EventCompleted       EventType = "provider.completed"
	EventFailed          EventType = "provider.failed"
)

type Event struct {
	Type  EventType
	Delta string
	Final *Final
	Err   error
}

type Final struct {
	Response       map[string]any
	OutputText     string
	EffectiveModel string
	Usage          pricing.ReportedUsage
	Quota          []QuotaObservation
}

type QuotaObservation struct {
	LimitID               string   `json:"limit_id"`
	Unit                  string   `json:"unit"`
	Before                *float64 `json:"before,omitempty"`
	After                 *float64 `json:"after,omitempty"`
	Delta                 *float64 `json:"delta,omitempty"`
	WindowDurationMinutes *int64   `json:"window_duration_minutes,omitempty"`
	ResetsAt              *int64   `json:"resets_at,omitempty"`
	Status                string   `json:"status"`
	Attribution           string   `json:"attribution"`
}

type Adapter interface {
	ID() string
	Start(context.Context, Request) (<-chan Event, error)
}
