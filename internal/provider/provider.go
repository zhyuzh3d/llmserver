package provider

import (
	"context"
	"encoding/json"

	"github.com/zhyuzh3d/llmserver/internal/pricing"
	"github.com/zhyuzh3d/llmserver/internal/toolcall"
)

type Request struct {
	RunID           string
	DeploymentID    string
	UpstreamModel   string
	Instructions    string
	Input           json.RawMessage
	CanonicalInput  string
	MaxOutputTokens *int64
	ReasoningEffort string
	RawRequest      json.RawMessage
	ToolCall        *toolcall.Request
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
	Costs          []CostObservation
}

type CostObservation struct {
	Unit   string
	Input  string
	Output string
	Total  string
}

type Adapter interface {
	ID() string
	Start(context.Context, Request) (<-chan Event, error)
}

// Retirable is implemented by adapters that own persistent local resources.
// Retirement rejects new work, closes idle resources immediately and lets
// requests already in flight finish before their resources are closed.
type Retirable interface {
	Retire()
}
