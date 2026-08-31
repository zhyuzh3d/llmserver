package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/config"
	codexadapter "github.com/zhyuzh3d/llmserver/internal/provider/codex"
	workbuddyadapter "github.com/zhyuzh3d/llmserver/internal/provider/workbuddy"
)

type Model struct {
	ID                        string   `json:"id"`
	DisplayName               string   `json:"display_name"`
	Description               string   `json:"description,omitempty"`
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts,omitempty"`
	CreditMultiplier          string   `json:"credit_multiplier,omitempty"`
}

func Models(ctx context.Context, provider config.ProviderConfig) ([]Model, error) {
	var models []Model
	var err error
	switch provider.Type {
	case "openai_responses":
		models, err = standardModels(ctx, provider)
	case "codex_exec":
		var discovered []codexadapter.DiscoveredModel
		discovered, err = codexadapter.DiscoverModels(ctx, codexadapter.Config{
			ProviderID: provider.ID, Executable: provider.Executable,
			ExpectedVersion: provider.ExpectedVersion, ExtraArgs: provider.ExtraArgs,
		})
		for _, item := range discovered {
			models = append(models, Model{ID: item.ID, DisplayName: item.DisplayName, Description: item.Description, SupportedReasoningEfforts: item.SupportedReasoningEfforts})
		}
	case "workbuddy_exec":
		var discovered []workbuddyadapter.DiscoveredModel
		discovered, err = workbuddyadapter.DiscoverModels(ctx, workbuddyadapter.Config{
			ProviderID: provider.ID, Executable: provider.Executable,
			ExpectedVersion: provider.ExpectedVersion, ExtraArgs: provider.ExtraArgs,
		})
		for _, item := range discovered {
			models = append(models, Model{ID: item.ID, DisplayName: item.DisplayName, CreditMultiplier: item.CreditMultiplier})
		}
	default:
		err = fmt.Errorf("provider type %q does not support discovery", provider.Type)
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func standardModels(ctx context.Context, provider config.ProviderConfig) ([]Model, error) {
	if provider.APIKey.IsEmpty() {
		return nil, errors.New("provider API key is not configured")
	}
	endpoint, err := modelsEndpoint(provider.BaseURL, provider.ModelsURL)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	authHeader, authPrefix, err := authentication(provider.APIKeyHeader, provider.APIKeyPrefix)
	if err != nil {
		return nil, err
	}
	request.Header.Set(authHeader, authValue(authPrefix, provider.APIKey.Reveal()))
	request.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, errors.New("provider model discovery request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil, fmt.Errorf("provider model discovery returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, errors.New("provider model discovery response was invalid")
	}
	items := payload.Data
	if len(items) == 0 {
		items = payload.Models
	}
	models := make([]Model, 0, len(items))
	for _, item := range items {
		if item.ID == "" {
			continue
		}
		name := item.Name
		if name == "" {
			name = item.ID
		}
		models = append(models, Model{ID: item.ID, DisplayName: name})
	}
	return models, nil
}

func modelsEndpoint(baseURL, explicitURL string) (string, error) {
	if strings.TrimSpace(explicitURL) != "" {
		parsed, err := url.Parse(strings.TrimSpace(explicitURL))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return "", errors.New("invalid provider models URL")
		}
		parsed.Fragment = ""
		return parsed.String(), nil
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid provider base URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(path, "/v1/models") || strings.HasSuffix(path, "/models"):
	case strings.HasSuffix(path, "/v1"):
		path += "/models"
	default:
		path += "/v1/models"
	}
	parsed.Path = path
	parsed.Fragment = ""
	return parsed.String(), nil
}

func authentication(header, prefix string) (string, string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		header = "Authorization"
	}
	if strings.ContainsAny(header, "\r\n:") {
		return "", "", errors.New("provider API key header is invalid")
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "Bearer"
	} else if strings.EqualFold(prefix, "none") {
		prefix = ""
	}
	if strings.ContainsAny(prefix, "\r\n") {
		return "", "", errors.New("provider API key prefix is invalid")
	}
	return header, prefix, nil
}

func authValue(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + " " + key
}
