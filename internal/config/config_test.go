package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFilesMergesSecretsWithoutMakingThemMarshallable(t *testing.T) {
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "config.yaml")
	secretPath := filepath.Join(dir, "xconfig.yaml")
	writeFile(t, publicPath, testPublicConfig("127.0.0.1:4816"))
	writeFile(t, secretPath, `
version: 1
client_tokens:
  device: device-secret-value
provider_api_keys:
  api: upstream-secret-value
`)
	cfg, secrets, err := LoadFiles(publicPath, secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Clients[0].Token.Reveal() != "device-secret-value" || cfg.Providers[0].APIKey.Reveal() != "upstream-secret-value" {
		t.Fatal("secrets were not merged")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	formatted := fmt.Sprintf("%v %#v", cfg.Clients[0].Token, cfg.Providers[0].APIKey)
	if strings.Contains(string(encoded)+formatted, "secret-value") {
		t.Fatal("secret leaked through config serialization")
	}
	if secrets.ClientTokens["device"] == "" {
		t.Fatal("secret source was not returned")
	}
}

func TestSaveFilesSeparatesPublicAndSecretValues(t *testing.T) {
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "configs", "config.yaml")
	secretPath := filepath.Join(dir, "xconfigs", "llmserver", "xconfig.yaml")
	cfg := Config{
		Version:     1,
		Server:      ServerConfig{Listen: "127.0.0.1:4815", AdminListen: "127.0.0.1:4816", StatePath: filepath.Join(dir, "state.db")},
		Clients:     []ClientConfig{{ID: "device", AllowedDeployments: []string{"model"}, Token: NewSecret("must-not-be-public")}},
		Providers:   []ProviderConfig{{ID: "api", Type: "openai_responses", BaseURL: "https://example.test/v1", WireAPI: "responses", APIKey: NewSecret("must-not-be-public")}},
		Deployments: []DeploymentConfig{{ID: "model", ProviderID: "api", UpstreamModel: "upstream", Enabled: true, Price: PriceConfig{Revision: "v1", Currency: "USD", InputPerMillion: "1", OutputPerMillion: "2"}}},
	}
	secrets := SecretConfig{Version: 1, ClientTokens: map[string]string{"device": "device-secret"}, ProviderAPIKeys: map[string]string{"api": "api-secret"}}
	if err := SaveFiles(publicPath, secretPath, cfg, secrets); err != nil {
		t.Fatal(err)
	}
	publicRaw, _ := os.ReadFile(publicPath)
	secretRaw, _ := os.ReadFile(secretPath)
	if strings.Contains(string(publicRaw), "secret") || !strings.Contains(string(secretRaw), "device-secret") {
		t.Fatalf("configuration separation failed")
	}
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode = %o", info.Mode().Perm())
	}
}

func TestAdminListenMustRemainLoopback(t *testing.T) {
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "config.yaml")
	secretPath := filepath.Join(dir, "xconfig.yaml")
	writeFile(t, publicPath, testPublicConfig("0.0.0.0:4816"))
	writeFile(t, secretPath, "version: 1\nclient_tokens: {device: token}\nprovider_api_keys: {api: key}\n")
	if _, _, err := LoadFiles(publicPath, secretPath); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback admin address was accepted: %v", err)
	}
}

func TestBootstrapCreatesDurableClientTokenWithoutInventingProviderKey(t *testing.T) {
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "configs", "config.yaml")
	secretPath := filepath.Join(dir, "xconfigs", "llmserver", "xconfig.yaml")
	writeFile(t, publicPath, testPublicConfig("127.0.0.1:4816"))
	changed, err := BootstrapSecretFile(publicPath, secretPath)
	if err != nil || !changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	_, first, err := LoadFiles(publicPath, secretPath)
	if err != nil {
		t.Fatal(err)
	}
	token := first.ClientTokens["device"]
	if len(token) != 64 || first.ProviderAPIKeys["api"] != "" {
		t.Fatalf("unexpected bootstrap result: token length=%d provider key present=%t", len(token), first.ProviderAPIKeys["api"] != "")
	}
	changed, err = BootstrapSecretFile(publicPath, secretPath)
	if err != nil || changed {
		t.Fatalf("second bootstrap changed=%t err=%v", changed, err)
	}
	_, second, err := LoadFiles(publicPath, secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if second.ClientTokens["device"] != token {
		t.Fatal("bootstrap rotated an existing client token")
	}
}

func testPublicConfig(adminListen string) string {
	return `
version: 1
server:
  listen: 127.0.0.1:4815
  admin_listen: ` + adminListen + `
  state_path: state.db
clients:
  - id: device
    allowed_deployments: [model]
providers:
  - id: api
    type: openai_responses
    base_url: https://example.test/v1
    wire_api: responses
    default_public_price: {revision: default, currency: USD, input_per_million: "1", output_per_million: "2"}
deployments:
  - id: model
    provider_id: api
    upstream_model: upstream
    enabled: true
    hard_budget_supported: true
    price: {revision: v1, currency: USD, input_per_million: "1", output_per_million: "2"}
`
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
