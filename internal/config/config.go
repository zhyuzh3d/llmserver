package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultListen = "127.0.0.1:4815"
const defaultStatePath = "llmserver.db"

type Config struct {
	Server        ServerConfig       `yaml:"server"`
	XConfigImport XConfigImport      `yaml:"xconfig_import"`
	Clients       []ClientConfig     `yaml:"clients"`
	ManualPrices  []ManualPrice      `yaml:"manual_prices"`
	Providers     []ProviderConfig   `yaml:"providers"`
	Deployments   []DeploymentConfig `yaml:"deployments"`
}

type ServerConfig struct {
	Listen    string `yaml:"listen"`
	StatePath string `yaml:"state_path"`
}

type XConfigImport struct {
	Enabled    bool   `yaml:"enabled"`
	Path       string `yaml:"path"`
	ProviderID string `yaml:"provider_id"`
}

type ClientConfig struct {
	ID                       string   `yaml:"id"`
	TokenEnv                 string   `yaml:"token_env"`
	AllowedDeployments       []string `yaml:"allowed_deployments"`
	IncludeQuotaObservations bool     `yaml:"include_quota_observations"`
}

type ManualPrice struct {
	DeploymentID     string `yaml:"deployment_id"`
	Revision         string `yaml:"revision"`
	Currency         string `yaml:"currency"`
	InputPerMillion  string `yaml:"input_per_million"`
	OutputPerMillion string `yaml:"output_per_million"`
}

type ProviderConfig struct {
	ID              string   `yaml:"id"`
	Type            string   `yaml:"type"`
	DisplayName     string   `yaml:"display_name"`
	BaseURL         string   `yaml:"base_url"`
	WireAPI         string   `yaml:"wire_api"`
	APIKeyEnv       string   `yaml:"api_key_env"`
	Executable      string   `yaml:"executable"`
	ExpectedVersion string   `yaml:"expected_version"`
	ExtraArgs       []string `yaml:"extra_args"`
	ObserveQuota    bool     `yaml:"observe_quota"`
	APIKey          Secret   `yaml:"-"`
}

type DeploymentConfig struct {
	ID                       string      `yaml:"id"`
	ProviderID               string      `yaml:"provider_id"`
	UpstreamModel            string      `yaml:"upstream_model"`
	SupportedReasoningEffort []string    `yaml:"supported_reasoning_efforts"`
	Price                    PriceConfig `yaml:"price"`
	HardBudgetSupported      bool        `yaml:"hard_budget_supported"`
	Enabled                  bool        `yaml:"enabled"`
}

type PriceConfig struct {
	Revision         string `yaml:"revision"`
	Currency         string `yaml:"currency"`
	InputPerMillion  string `yaml:"input_per_million"`
	OutputPerMillion string `yaml:"output_per_million"`
	Source           string `yaml:"source"`
}

// Secret intentionally has no String or Marshal method. It must only be opened
// at the provider boundary and never included in formatted config diagnostics.
type Secret struct {
	value string
}

func NewSecret(value string) Secret { return Secret{value: value} }

func (s Secret) Reveal() string { return s.value }

func (s Secret) IsEmpty() bool { return s.value == "" }

func (s Secret) String() string { return "[REDACTED]" }

func (s Secret) GoString() string { return "config.Secret{[REDACTED]}" }

func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal("[REDACTED]") }

func (s Secret) MarshalYAML() (any, error) { return "[REDACTED]", nil }

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read llmserver config: %w", err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode llmserver config: %w", err)
	}
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = defaultListen
	}
	if cfg.Server.StatePath == "" {
		cfg.Server.StatePath = defaultStatePath
	}
	if cfg.Server.StatePath != ":memory:" && !filepath.IsAbs(cfg.Server.StatePath) {
		cfg.Server.StatePath = filepath.Join(filepath.Dir(path), cfg.Server.StatePath)
	}

	if cfg.XConfigImport.Enabled {
		importPath := cfg.XConfigImport.Path
		if importPath == "" {
			return nil, errors.New("xconfig_import.path is required when import is enabled")
		}
		if !filepath.IsAbs(importPath) {
			importPath = filepath.Join(filepath.Dir(path), importPath)
		}
		imported, err := LoadXConfig(importPath, cfg.XConfigImport.ProviderID)
		if err != nil {
			return nil, err
		}
		cfg.Providers = append(cfg.Providers, imported.Provider)
		cfg.Deployments = append(cfg.Deployments, imported.Deployments...)
	}
	if err := resolveProviderSecrets(&cfg); err != nil {
		return nil, err
	}

	if err := applyManualPrices(&cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func resolveProviderSecrets(cfg *Config) error {
	for i := range cfg.Providers {
		provider := &cfg.Providers[i]
		if provider.Type != "openai_responses" || !provider.APIKey.IsEmpty() {
			continue
		}
		if provider.APIKeyEnv == "" {
			return fmt.Errorf("provider %q api_key_env is required", provider.ID)
		}
		value := os.Getenv(provider.APIKeyEnv)
		if value == "" {
			return fmt.Errorf("provider %q credential environment variable %q is empty", provider.ID, provider.APIKeyEnv)
		}
		provider.APIKey = NewSecret(value)
	}
	return nil
}

func (c *Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		return fmt.Errorf("invalid server.listen: %w", err)
	}

	providerIDs := make(map[string]struct{}, len(c.Providers))
	for _, provider := range c.Providers {
		if provider.ID == "" {
			return errors.New("provider id is required")
		}
		if _, exists := providerIDs[provider.ID]; exists {
			return fmt.Errorf("duplicate provider id %q", provider.ID)
		}
		providerIDs[provider.ID] = struct{}{}
		switch provider.Type {
		case "openai_responses":
			if provider.APIKey.IsEmpty() {
				return fmt.Errorf("provider %q credential is empty", provider.ID)
			}
		case "codex_exec", "workbuddy_exec":
			if !filepath.IsAbs(provider.Executable) {
				return fmt.Errorf("provider %q executable must be an absolute path", provider.ID)
			}
		default:
			return fmt.Errorf("provider %q has unsupported type %q", provider.ID, provider.Type)
		}
	}

	deploymentIDs := make(map[string]struct{}, len(c.Deployments))
	for _, deployment := range c.Deployments {
		if deployment.ID == "" || deployment.UpstreamModel == "" {
			return errors.New("deployment id and upstream model are required")
		}
		if _, exists := deploymentIDs[deployment.ID]; exists {
			return fmt.Errorf("duplicate deployment id %q", deployment.ID)
		}
		deploymentIDs[deployment.ID] = struct{}{}
		if _, exists := providerIDs[deployment.ProviderID]; !exists {
			return fmt.Errorf("deployment %q references unknown provider %q", deployment.ID, deployment.ProviderID)
		}
		if deployment.Price.Revision == "" || deployment.Price.Currency == "" || deployment.Price.InputPerMillion == "" || deployment.Price.OutputPerMillion == "" {
			return fmt.Errorf("deployment %q requires a complete price revision", deployment.ID)
		}
	}

	clientIDs := make(map[string]struct{}, len(c.Clients))
	for _, client := range c.Clients {
		if client.ID == "" || client.TokenEnv == "" {
			return errors.New("client id and token_env are required")
		}
		if _, exists := clientIDs[client.ID]; exists {
			return fmt.Errorf("duplicate client id %q", client.ID)
		}
		clientIDs[client.ID] = struct{}{}
		for _, deploymentID := range client.AllowedDeployments {
			if _, exists := deploymentIDs[deploymentID]; !exists {
				return fmt.Errorf("client %q allows unknown deployment %q", client.ID, deploymentID)
			}
		}
	}
	return nil
}

func applyManualPrices(cfg *Config) error {
	prices := make(map[string]ManualPrice, len(cfg.ManualPrices))
	for _, price := range cfg.ManualPrices {
		if price.DeploymentID == "" || price.Revision == "" || price.Currency == "" || price.InputPerMillion == "" || price.OutputPerMillion == "" {
			return errors.New("manual price requires deployment_id, revision, currency, input_per_million, and output_per_million")
		}
		if _, exists := prices[price.DeploymentID]; exists {
			return fmt.Errorf("duplicate manual price for deployment %q", price.DeploymentID)
		}
		prices[price.DeploymentID] = price
	}

	for i := range cfg.Deployments {
		price, exists := prices[cfg.Deployments[i].ID]
		if !exists {
			continue
		}
		cfg.Deployments[i].Price = PriceConfig{
			Revision:         price.Revision,
			Currency:         price.Currency,
			InputPerMillion:  price.InputPerMillion,
			OutputPerMillion: price.OutputPerMillion,
			Source:           "manual_override",
		}
		delete(prices, cfg.Deployments[i].ID)
	}
	for deploymentID := range prices {
		return fmt.Errorf("manual price references unknown deployment %q", deploymentID)
	}
	return nil
}
