package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadXConfigImportsOnlyLLMAndKeepsSecretOpaque(t *testing.T) {
	dir := t.TempDir()
	xconfigPath := filepath.Join(dir, "xconfig.yaml")
	xconfig := `
social_accounts:
  example:
    password: never-read-this
llm:
  provider: OpenAI
  models:
    terra:
      id: gpt-test
      input_usd_per_million: 2
      output_usd_per_million: 12
      supported_reasoning_efforts: [low, medium]
  cognitive_resource:
    price_table_version: test-v1
  providers:
    OpenAI:
      name: OpenAI
      base_url: https://example.test/v1
      wire_api: responses
      requires_openai_auth: true
  credentials:
    environment:
      OPENAI_API_KEY: top-secret-key
`
	if err := os.WriteFile(xconfigPath, []byte(xconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := LoadXConfig(xconfigPath, "api-test")
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Provider.APIKey.Reveal(); got != "top-secret-key" {
		t.Fatalf("credential mismatch")
	}
	if len(result.Deployments) != 1 || result.Deployments[0].ID != "terra" {
		t.Fatalf("unexpected deployments: %#v", result.Deployments)
	}
	if strings.Contains(result.Provider.DisplayName, "top-secret-key") {
		t.Fatal("secret leaked into display data")
	}
	formatted := fmt.Sprintf("%v %#v", result.Provider.APIKey, result.Provider.APIKey)
	encoded, err := json.Marshal(result.Provider.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(formatted+string(encoded), "top-secret-key") {
		t.Fatal("secret leaked through formatting or JSON")
	}
}

func TestLoadAppliesManualPriceWithHighestPriority(t *testing.T) {
	dir := t.TempDir()
	xconfigPath := filepath.Join(dir, "xconfig.yaml")
	serverPath := filepath.Join(dir, "llmserver.yaml")
	writeFile(t, xconfigPath, `
llm:
  provider: OpenAI
  models:
    terra:
      id: gpt-test
      input_usd_per_million: 2
      output_usd_per_million: 12
  cognitive_resource:
    price_table_version: test-v1
  providers:
    OpenAI:
      name: OpenAI
      base_url: https://example.test
      wire_api: responses
  credentials:
    environment:
      OPENAI_API_KEY: secret
`)
	writeFile(t, serverPath, `
xconfig_import:
  enabled: true
  path: xconfig.yaml
  provider_id: api-test
clients:
  - id: test-client
    token_env: TEST_TOKEN
    allowed_deployments: [terra]
manual_prices:
  - deployment_id: terra
    revision: manual-v2
    currency: USD
    input_per_million: "3.000000"
    output_per_million: "15.000000"
`)

	cfg, err := Load(serverPath)
	if err != nil {
		t.Fatal(err)
	}
	price := cfg.Deployments[0].Price
	if price.Source != "manual_override" || price.Revision != "manual-v2" || price.InputPerMillion != "3.000000" {
		t.Fatalf("manual price was not applied: %#v", price)
	}
}

func TestLoadExplicitAPIProviderReadsCredentialFromEnvironment(t *testing.T) {
	dir := t.TempDir()
	serverPath := filepath.Join(dir, "llmserver.yaml")
	t.Setenv("TEST_UPSTREAM_API_KEY", "environment-only-secret")
	writeFile(t, serverPath, `
providers:
  - id: api-test
    type: openai_responses
    base_url: https://example.test/v1
    wire_api: responses
    api_key_env: TEST_UPSTREAM_API_KEY
deployments:
  - id: test-model
    provider_id: api-test
    upstream_model: upstream-test-model
    enabled: true
    hard_budget_supported: true
    price:
      revision: test-price-v1
      currency: USD
      input_per_million: "1"
      output_per_million: "2"
clients:
  - id: device
    token_env: TEST_CLIENT_TOKEN
    allowed_deployments: [test-model]
`)
	cfg, err := Load(serverPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers[0].APIKey.Reveal(); got != "environment-only-secret" {
		t.Fatal("provider did not resolve the environment credential")
	}
	encoded, err := json.Marshal(cfg.Providers[0].APIKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "environment-only-secret") {
		t.Fatal("resolved secret leaked through JSON")
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
