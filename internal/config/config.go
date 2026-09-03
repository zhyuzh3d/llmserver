package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhyuzh3d/llmserver/internal/pricing"
	"gopkg.in/yaml.v3"
)

const (
	defaultListen      = "127.0.0.1:4815"
	defaultAdminListen = "127.0.0.1:4816"
	defaultStatePath   = "llmserver.db"
)

// Config contains only non-secret settings and is safe to keep in Git.
type Config struct {
	Version     int                `yaml:"version" json:"version"`
	Server      ServerConfig       `yaml:"server" json:"server"`
	Clients     []ClientConfig     `yaml:"clients" json:"clients"`
	Providers   []ProviderConfig   `yaml:"providers" json:"providers"`
	Deployments []DeploymentConfig `yaml:"deployments" json:"deployments"`
}

type ServerConfig struct {
	Listen      string `yaml:"listen" json:"listen"`
	AdminListen string `yaml:"admin_listen" json:"admin_listen"`
	StatePath   string `yaml:"state_path" json:"state_path"`
}

type ClientConfig struct {
	ID                 string   `yaml:"id" json:"id"`
	AllowedDeployments []string `yaml:"allowed_deployments" json:"allowed_deployments"`
	DailyLimitUSD      string   `yaml:"daily_limit_usd,omitempty" json:"daily_limit_usd,omitempty"`
	Token              Secret   `yaml:"-" json:"-"`
}

type ProviderConfig struct {
	ID                 string      `yaml:"id" json:"id"`
	Type               string      `yaml:"type" json:"type"`
	DisplayName        string      `yaml:"display_name" json:"display_name"`
	BaseURL            string      `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	ModelsURL          string      `yaml:"models_url,omitempty" json:"models_url,omitempty"`
	ResponsesURL       string      `yaml:"responses_url,omitempty" json:"responses_url,omitempty"`
	APIKeyHeader       string      `yaml:"api_key_header,omitempty" json:"api_key_header,omitempty"`
	APIKeyPrefix       string      `yaml:"api_key_prefix,omitempty" json:"api_key_prefix,omitempty"`
	WireAPI            string      `yaml:"wire_api,omitempty" json:"wire_api,omitempty"`
	Executable         string      `yaml:"executable,omitempty" json:"executable,omitempty"`
	ExpectedVersion    string      `yaml:"expected_version,omitempty" json:"expected_version,omitempty"`
	ExtraArgs          []string    `yaml:"extra_args,omitempty" json:"extra_args,omitempty"`
	MaxConcurrency     int         `yaml:"max_concurrency,omitempty" json:"max_concurrency,omitempty"`
	DefaultReasoning   string      `yaml:"default_reasoning_effort,omitempty" json:"default_reasoning_effort,omitempty"`
	ServiceTier        string      `yaml:"service_tier,omitempty" json:"service_tier,omitempty"`
	WarmupEnabled      bool        `yaml:"warmup_enabled,omitempty" json:"warmup_enabled,omitempty"`
	WarmupModel        string      `yaml:"warmup_model,omitempty" json:"warmup_model,omitempty"`
	WarmupTimeoutSecs  int         `yaml:"warmup_timeout_seconds,omitempty" json:"warmup_timeout_seconds,omitempty"`
	DefaultPublicPrice PriceConfig `yaml:"default_public_price" json:"default_public_price"`
	APIKey             Secret      `yaml:"-" json:"-"`
}

type DeploymentConfig struct {
	ID                       string          `yaml:"id" json:"id"`
	ProviderID               string          `yaml:"provider_id" json:"provider_id"`
	UpstreamModel            string          `yaml:"upstream_model" json:"upstream_model"`
	SupportedReasoningEffort []string        `yaml:"supported_reasoning_efforts,omitempty" json:"supported_reasoning_efforts,omitempty"`
	FunctionCalling          string          `yaml:"function_calling,omitempty" json:"function_calling,omitempty"`
	Price                    PriceConfig     `yaml:"price" json:"price"`
	ActualPrice              *PriceConfig    `yaml:"actual_price,omitempty" json:"actual_price,omitempty"`
	ActualPoints             *UnitRateConfig `yaml:"actual_points,omitempty" json:"actual_points,omitempty"`
	HardBudgetSupported      bool            `yaml:"hard_budget_supported" json:"hard_budget_supported"`
	Enabled                  bool            `yaml:"enabled" json:"enabled"`
}

type PriceConfig struct {
	Revision         string `yaml:"revision" json:"revision"`
	Currency         string `yaml:"currency" json:"currency"`
	InputPerMillion  string `yaml:"input_per_million" json:"input_per_million"`
	OutputPerMillion string `yaml:"output_per_million" json:"output_per_million"`
	Source           string `yaml:"source,omitempty" json:"source,omitempty"`
}

type UnitRateConfig struct {
	InputPerMillion  string `yaml:"input_per_million" json:"input_per_million"`
	OutputPerMillion string `yaml:"output_per_million" json:"output_per_million"`
	Source           string `yaml:"source,omitempty" json:"source,omitempty"`
}

// SecretConfig is the only on-disk configuration allowed to contain credentials.
// It lives outside the Git repository and is always written with mode 0600.
type SecretConfig struct {
	Version         int               `yaml:"version" json:"version"`
	ClientTokens    map[string]string `yaml:"client_tokens" json:"client_tokens"`
	ProviderAPIKeys map[string]string `yaml:"provider_api_keys" json:"provider_api_keys"`
}

// Secret intentionally cannot reveal itself through formatting or marshalling.
type Secret struct{ value string }

func NewSecret(value string) Secret { return Secret{value: value} }
func (s Secret) Reveal() string     { return s.value }
func (s Secret) IsEmpty() bool      { return s.value == "" }
func (s Secret) String() string     { return "[REDACTED]" }
func (s Secret) GoString() string   { return "config.Secret{[REDACTED]}" }
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED]")
}
func (s Secret) MarshalYAML() (any, error) { return "[REDACTED]", nil }

func LoadFiles(publicPath, secretPath string) (*Config, *SecretConfig, error) {
	cfg, err := LoadPublicFile(publicPath)
	if err != nil {
		return nil, nil, err
	}

	secretRaw, err := os.ReadFile(secretPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read secret xconfig: %w", err)
	}
	var secrets SecretConfig
	secretDecoder := yaml.NewDecoder(strings.NewReader(string(secretRaw)))
	secretDecoder.KnownFields(true)
	if err := secretDecoder.Decode(&secrets); err != nil {
		return nil, nil, fmt.Errorf("decode secret xconfig: %w", err)
	}
	mergeSecrets(cfg, &secrets)
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	return cfg, &secrets, nil
}

func LoadPublicFile(publicPath string) (*Config, error) {
	publicRaw, err := os.ReadFile(publicPath)
	if err != nil {
		return nil, fmt.Errorf("read public config: %w", err)
	}
	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(publicRaw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode public config: %w", err)
	}
	applyDefaults(&cfg)
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = defaultListen
	}
	if cfg.Server.AdminListen == "" {
		cfg.Server.AdminListen = defaultAdminListen
	}
	if cfg.Server.StatePath == "" {
		cfg.Server.StatePath = defaultStatePath
	}
}

func ResolveStatePath(publicPath, configuredPath string) string {
	if configuredPath == ":memory:" || filepath.IsAbs(configuredPath) {
		return configuredPath
	}
	return filepath.Clean(filepath.Join(filepath.Dir(publicPath), configuredPath))
}

func mergeSecrets(cfg *Config, secrets *SecretConfig) {
	for i := range cfg.Clients {
		cfg.Clients[i].Token = NewSecret(secrets.ClientTokens[cfg.Clients[i].ID])
	}
	for i := range cfg.Providers {
		cfg.Providers[i].APIKey = NewSecret(secrets.ProviderAPIKeys[cfg.Providers[i].ID])
	}
}

func Resolve(publicConfig Config, secrets SecretConfig) (*Config, error) {
	resolved := publicConfig
	applyDefaults(&resolved)
	mergeSecrets(&resolved, &secrets)
	if err := resolved.Validate(); err != nil {
		return nil, err
	}
	return &resolved, nil
}

func SaveFiles(publicPath, secretPath string, cfg Config, secrets SecretConfig) error {
	publicCopy := cfg
	for i := range publicCopy.Clients {
		publicCopy.Clients[i].Token = Secret{}
	}
	for i := range publicCopy.Providers {
		publicCopy.Providers[i].APIKey = Secret{}
	}
	publicRaw, err := yaml.Marshal(publicCopy)
	if err != nil {
		return fmt.Errorf("encode public config: %w", err)
	}
	if secrets.Version == 0 {
		secrets.Version = 1
	}
	if secrets.ClientTokens == nil {
		secrets.ClientTokens = map[string]string{}
	}
	if secrets.ProviderAPIKeys == nil {
		secrets.ProviderAPIKeys = map[string]string{}
	}
	secretRaw, err := marshalSecrets(secrets)
	if err != nil {
		return fmt.Errorf("encode secret xconfig: %w", err)
	}
	if err := writeAtomic(publicPath, publicRaw, 0o644, 0o755); err != nil {
		return fmt.Errorf("save public config: %w", err)
	}
	if err := writeAtomic(secretPath, secretRaw, 0o600, 0o700); err != nil {
		return fmt.Errorf("save secret xconfig: %w", err)
	}
	return nil
}

func marshalSecrets(secrets SecretConfig) ([]byte, error) {
	return yaml.Marshal(secrets)
}

func writeAtomic(path string, contents []byte, mode, dirMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".llmserver-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func (c *Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		return fmt.Errorf("invalid server.listen: %w", err)
	}
	adminHost, _, err := net.SplitHostPort(c.Server.AdminListen)
	if err != nil {
		return fmt.Errorf("invalid server.admin_listen: %w", err)
	}
	if !isLoopbackHost(adminHost) {
		return errors.New("server.admin_listen must use a loopback address")
	}

	providerIDs := make(map[string]struct{}, len(c.Providers))
	providersByID := make(map[string]ProviderConfig, len(c.Providers))
	for _, provider := range c.Providers {
		if provider.ID == "" {
			return errors.New("provider id is required")
		}
		if _, exists := providerIDs[provider.ID]; exists {
			return fmt.Errorf("duplicate provider id %q", provider.ID)
		}
		providerIDs[provider.ID] = struct{}{}
		providersByID[provider.ID] = provider
		switch provider.Type {
		case "openai_responses":
			if provider.BaseURL == "" {
				return fmt.Errorf("provider %q requires base_url", provider.ID)
			}
			if err := validateAPIProvider(provider); err != nil {
				return fmt.Errorf("provider %q: %w", provider.ID, err)
			}
		case "codex_exec", "workbuddy_exec":
			if !filepath.IsAbs(provider.Executable) {
				return fmt.Errorf("provider %q executable must be an absolute path", provider.ID)
			}
			if provider.MaxConcurrency != 0 && (provider.MaxConcurrency < 1 || provider.MaxConcurrency > 16) {
				return fmt.Errorf("provider %q max_concurrency must be between 1 and 16", provider.ID)
			}
			if provider.DefaultReasoning != "" && !validReasoningEffort(provider.DefaultReasoning) {
				return fmt.Errorf("provider %q has invalid default_reasoning_effort", provider.ID)
			}
			if provider.Type != "codex_exec" && provider.ServiceTier != "" {
				return fmt.Errorf("provider %q service_tier is only supported by codex_exec", provider.ID)
			}
			if provider.Type == "codex_exec" && provider.ServiceTier != "" && provider.ServiceTier != "priority" {
				return fmt.Errorf("provider %q has invalid service_tier", provider.ID)
			}
			if provider.Type != "workbuddy_exec" && (provider.WarmupEnabled || provider.WarmupModel != "" || provider.WarmupTimeoutSecs != 0) {
				return fmt.Errorf("provider %q warmup settings are only supported by workbuddy_exec", provider.ID)
			}
			if provider.WarmupEnabled && strings.TrimSpace(provider.WarmupModel) == "" {
				return fmt.Errorf("provider %q warmup_model is required when warmup is enabled", provider.ID)
			}
			if provider.WarmupTimeoutSecs != 0 && (provider.WarmupTimeoutSecs < 5 || provider.WarmupTimeoutSecs > 300) {
				return fmt.Errorf("provider %q warmup_timeout_seconds must be between 5 and 300", provider.ID)
			}
		default:
			return fmt.Errorf("provider %q has unsupported type %q", provider.ID, provider.Type)
		}
		if err := validatePrice(provider.ID+" default", provider.DefaultPublicPrice); err != nil {
			return err
		}
	}

	deploymentIDs := make(map[string]struct{}, len(c.Deployments))
	deploymentCurrencies := make(map[string]string, len(c.Deployments))
	for _, deployment := range c.Deployments {
		if deployment.ID == "" || deployment.UpstreamModel == "" {
			return errors.New("deployment id and upstream model are required")
		}
		if _, exists := deploymentIDs[deployment.ID]; exists {
			return fmt.Errorf("duplicate deployment id %q", deployment.ID)
		}
		deploymentIDs[deployment.ID] = struct{}{}
		deploymentCurrencies[deployment.ID] = strings.ToUpper(strings.TrimSpace(deployment.Price.Currency))
		if _, exists := providerIDs[deployment.ProviderID]; !exists {
			return fmt.Errorf("deployment %q references unknown provider %q", deployment.ID, deployment.ProviderID)
		}
		provider := providersByID[deployment.ProviderID]
		switch deployment.FunctionCalling {
		case "", "unsupported", "native", "emulated":
		default:
			return fmt.Errorf("deployment %q function_calling must be native, emulated, or unsupported", deployment.ID)
		}
		if deployment.FunctionCalling == "native" && provider.Type == "workbuddy_exec" {
			return fmt.Errorf("deployment %q cannot enable native function calling on workbuddy_exec", deployment.ID)
		}
		if provider.DefaultReasoning != "" && len(deployment.SupportedReasoningEffort) > 0 && !containsString(deployment.SupportedReasoningEffort, provider.DefaultReasoning) {
			return fmt.Errorf("provider %q default_reasoning_effort %q is not supported by deployment %q", provider.ID, provider.DefaultReasoning, deployment.ID)
		}
		if err := validatePrice(deployment.ID, deployment.Price); err != nil {
			return err
		}
		if deployment.ActualPrice != nil {
			if err := validatePrice(deployment.ID+" actual", *deployment.ActualPrice); err != nil {
				return err
			}
		}
		if deployment.ActualPoints != nil {
			if err := validateRate(deployment.ID+" actual points input", deployment.ActualPoints.InputPerMillion); err != nil {
				return err
			}
			if err := validateRate(deployment.ID+" actual points output", deployment.ActualPoints.OutputPerMillion); err != nil {
				return err
			}
		}
	}

	clientIDs := make(map[string]struct{}, len(c.Clients))
	for _, client := range c.Clients {
		if client.ID == "" || client.Token.IsEmpty() {
			return errors.New("client id and matching client_tokens secret are required")
		}
		if _, exists := clientIDs[client.ID]; exists {
			return fmt.Errorf("duplicate client id %q", client.ID)
		}
		clientIDs[client.ID] = struct{}{}
		if client.DailyLimitUSD != "" {
			limit, parseErr := pricing.ParseDecimal(client.DailyLimitUSD)
			if parseErr != nil || limit.IsZero() {
				return fmt.Errorf("client %q daily_limit_usd must be a positive decimal", client.ID)
			}
		}
		for _, deploymentID := range client.AllowedDeployments {
			if _, exists := deploymentIDs[deploymentID]; !exists {
				return fmt.Errorf("client %q allows unknown deployment %q", client.ID, deploymentID)
			}
			if client.DailyLimitUSD != "" && deploymentCurrencies[deploymentID] != "USD" {
				return fmt.Errorf("client %q daily USD limit cannot include non-USD deployment %q", client.ID, deploymentID)
			}
		}
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validReasoningEffort(value string) bool {
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	default:
		return false
	}
}

func validateAPIProvider(provider ProviderConfig) error {
	if provider.WireAPI != "" && provider.WireAPI != "responses" {
		return fmt.Errorf("unsupported wire_api %q", provider.WireAPI)
	}
	for label, raw := range map[string]string{"base_url": provider.BaseURL, "models_url": provider.ModelsURL, "responses_url": provider.ResponsesURL} {
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("%s must be an absolute HTTP URL", label)
		}
	}
	if strings.ContainsAny(provider.APIKeyHeader, "\r\n:") || strings.ContainsAny(provider.APIKeyPrefix, "\r\n") {
		return errors.New("API key header or prefix is invalid")
	}
	return nil
}

func validatePrice(label string, price PriceConfig) error {
	if price.Revision == "" || price.Currency == "" || price.InputPerMillion == "" || price.OutputPerMillion == "" {
		return fmt.Errorf("deployment %q requires a complete price revision", label)
	}
	if err := validateRate(label+" input", price.InputPerMillion); err != nil {
		return err
	}
	return validateRate(label+" output", price.OutputPerMillion)
}

func validateRate(label, raw string) error {
	value, err := pricing.ParseDecimal(raw)
	if err != nil {
		return fmt.Errorf("%s rate is invalid: %w", label, err)
	}
	if value.Nanos() < 0 {
		return fmt.Errorf("%s rate cannot be negative", label)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
