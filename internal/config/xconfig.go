package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type XConfigResult struct {
	Provider    ProviderConfig
	Deployments []DeploymentConfig
}

type xconfigDocument struct {
	LLM xconfigLLM `yaml:"llm"`
}

type xconfigLLM struct {
	Provider          string                     `yaml:"provider"`
	Models            map[string]xconfigModel    `yaml:"models"`
	CognitiveResource xconfigCognitiveResource   `yaml:"cognitive_resource"`
	Providers         map[string]xconfigProvider `yaml:"providers"`
	Credentials       xconfigCredentials         `yaml:"credentials"`
}

type xconfigModel struct {
	ID                        string   `yaml:"id"`
	InputUSDPerMillion        float64  `yaml:"input_usd_per_million"`
	OutputUSDPerMillion       float64  `yaml:"output_usd_per_million"`
	SupportedReasoningEfforts []string `yaml:"supported_reasoning_efforts"`
}

type xconfigCognitiveResource struct {
	PriceTableVersion string `yaml:"price_table_version"`
}

type xconfigProvider struct {
	Name               string `yaml:"name"`
	BaseURL            string `yaml:"base_url"`
	WireAPI            string `yaml:"wire_api"`
	RequiresOpenAIAuth bool   `yaml:"requires_openai_auth"`
}

type xconfigCredentials struct {
	Environment map[string]string `yaml:"environment"`
}

func LoadXConfig(path, providerID string) (*XConfigResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read xconfig: %w", err)
	}
	var document xconfigDocument
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode xconfig llm section: %w", err)
	}

	providerName := document.LLM.Provider
	provider, exists := document.LLM.Providers[providerName]
	if !exists {
		return nil, fmt.Errorf("xconfig llm provider %q is not configured", providerName)
	}
	if providerID == "" {
		providerID = "api-" + strings.ToLower(providerName)
	}
	if provider.WireAPI != "responses" {
		return nil, fmt.Errorf("xconfig provider %q uses unsupported wire_api %q", providerName, provider.WireAPI)
	}

	credential := document.LLM.Credentials.Environment["OPENAI_API_KEY"]
	result := &XConfigResult{
		Provider: ProviderConfig{
			ID:          providerID,
			Type:        "openai_responses",
			DisplayName: provider.Name,
			BaseURL:     strings.TrimRight(provider.BaseURL, "/"),
			WireAPI:     provider.WireAPI,
			APIKey:      NewSecret(credential),
		},
	}

	priceVersion := document.LLM.CognitiveResource.PriceTableVersion
	if priceVersion == "" {
		priceVersion = "xconfig"
	}
	aliases := make([]string, 0, len(document.LLM.Models))
	for alias := range document.LLM.Models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		model := document.LLM.Models[alias]
		if model.ID == "" {
			return nil, fmt.Errorf("xconfig model %q has no id", alias)
		}
		result.Deployments = append(result.Deployments, DeploymentConfig{
			ID:                       alias,
			ProviderID:               providerID,
			UpstreamModel:            model.ID,
			SupportedReasoningEffort: append([]string(nil), model.SupportedReasoningEfforts...),
			Price: PriceConfig{
				Revision:         fmt.Sprintf("%s-%s", priceVersion, alias),
				Currency:         "USD",
				InputPerMillion:  formatRate(model.InputUSDPerMillion),
				OutputPerMillion: formatRate(model.OutputUSDPerMillion),
				Source:           "catalog_default",
			},
			Enabled:             true,
			HardBudgetSupported: true,
		})
	}
	return result, nil
}

func formatRate(value float64) string {
	return fmt.Sprintf("%.6f", value)
}
