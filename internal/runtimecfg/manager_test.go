package runtimecfg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhyuzh3d/llmserver/internal/config"
)

func TestUpdateMovesClientTokenAndSwapsRuntime(t *testing.T) {
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "configs", "config.yaml")
	secretPath := filepath.Join(dir, "xconfigs", "llmserver", "xconfig.yaml")
	writeManagerFile(t, publicPath, `
version: 1
server: {listen: "127.0.0.1:4815", admin_listen: "127.0.0.1:4816", state_path: ":memory:"}
clients:
  - {id: old-device, allowed_deployments: [model]}
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
    price: {revision: public-v1, currency: USD, input_per_million: "1", output_per_million: "2"}
`)
	writeManagerFile(t, secretPath, "version: 1\nclient_tokens: {old-device: client-secret}\nprovider_api_keys: {api: provider-secret}\n")

	manager, err := New(publicPath, secretPath, nil, "mock response")
	if err != nil {
		t.Fatal(err)
	}
	updated := *manager.Snapshot().Config
	updated.Clients = append([]config.ClientConfig(nil), updated.Clients...)
	updated.Clients[0].ID = "new-device"
	if err := manager.Update(context.Background(), Update{Config: updated, ClientTokenRenames: map[string]string{"new-device": "old-device"}}); err != nil {
		t.Fatal(err)
	}
	status := manager.SecretStatus()
	if !status.ClientTokens["new-device"] || status.ClientTokens["old-device"] {
		t.Fatalf("secret status = %#v", status.ClientTokens)
	}
	if manager.Snapshot().Config.Clients[0].ID != "new-device" {
		t.Fatal("runtime snapshot was not swapped")
	}
	publicRaw, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicRaw), "client-secret") || strings.Contains(string(publicRaw), "provider-secret") {
		t.Fatal("secret leaked into public config")
	}
}

func writeManagerFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
