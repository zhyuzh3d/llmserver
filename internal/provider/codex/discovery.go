package codex

import (
	"context"
	"encoding/json"
	"fmt"
)

type DiscoveredModel struct {
	ID                        string   `json:"id"`
	DisplayName               string   `json:"display_name"`
	Description               string   `json:"description,omitempty"`
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts,omitempty"`
	Hidden                    bool     `json:"hidden"`
}

func DiscoverModels(ctx context.Context, config Config) ([]DiscoveredModel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	worker, err := newAppServerWorker(config)
	if err != nil {
		return nil, err
	}
	defer worker.close()
	requestID := worker.rpcID()
	if err := worker.writeRPC(requestID, "model/list", map[string]any{"limit": 100}); err != nil {
		return nil, err
	}
	raw, err := worker.waitRPCResult(requestID)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
			Hidden      bool   `json:"hidden"`
			Efforts     []struct {
				Value string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode Codex model list: %w", err)
	}
	models := make([]DiscoveredModel, 0, len(result.Data))
	for _, item := range result.Data {
		if item.ID == "" || item.Hidden {
			continue
		}
		model := DiscoveredModel{ID: item.ID, DisplayName: item.DisplayName, Description: item.Description, Hidden: item.Hidden}
		var reportedEfforts []string
		for _, effort := range item.Efforts {
			reportedEfforts = append(reportedEfforts, effort.Value)
		}
		model.SupportedReasoningEfforts = completeReasoningEfforts(item.ID, reportedEfforts)
		models = append(models, model)
	}
	return models, nil
}

// Codex model/list currently omits "none" for these GPT-5.6 models even
// though both the subscription Responses endpoint and App Server turn/start
// accept it. Keep discovery useful for admin-created deployments without
// extending the override to models that have not been verified.
func completeReasoningEfforts(modelID string, reported []string) []string {
	verifiedNone := modelID == "gpt-5.6-luna" || modelID == "gpt-5.6-terra" || modelID == "gpt-5.6-sol"
	efforts := make([]string, 0, len(reported)+1)
	seen := make(map[string]struct{}, len(reported)+1)
	appendEffort := func(effort string) {
		if effort == "" {
			return
		}
		if _, exists := seen[effort]; exists {
			return
		}
		seen[effort] = struct{}{}
		efforts = append(efforts, effort)
	}
	if verifiedNone {
		appendEffort("none")
	}
	for _, effort := range reported {
		appendEffort(effort)
	}
	return efforts
}
