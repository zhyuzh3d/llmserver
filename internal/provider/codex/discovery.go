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
		for _, effort := range item.Efforts {
			model.SupportedReasoningEfforts = append(model.SupportedReasoningEfforts, effort.Value)
		}
		models = append(models, model)
	}
	return models, nil
}
