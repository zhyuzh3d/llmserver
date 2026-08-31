package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zhyuzh3d/llmserver/internal/config"
)

func TestStandardModelsUsesBearerKeyAndAcceptsDataShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Fatalf("authorization header was not set")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-b"},{"id":"model-a","name":"Model A"}]}`))
	}))
	defer server.Close()

	models, err := Models(context.Background(), config.ProviderConfig{
		ID: "api", Type: "openai_responses", BaseURL: server.URL,
		APIKey: config.NewSecret("upstream-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "model-a" || models[0].DisplayName != "Model A" || models[1].ID != "model-b" {
		t.Fatalf("models = %#v", models)
	}
}

func TestModelsEndpointPreservesVersionedBasePath(t *testing.T) {
	endpoint, err := modelsEndpoint("https://example.test/proxy/v1/", "")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://example.test/proxy/v1/models" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestStandardModelsSupportsExplicitURLAndAPIKeyHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog" || r.URL.Query().Get("api-version") != "v2" {
			t.Fatalf("request URL = %s", r.URL.String())
		}
		if r.Header.Get("api-key") != "secret" || r.Header.Get("Authorization") != "" {
			t.Fatal("custom API key header was not used")
		}
		_, _ = w.Write([]byte(`{"models":[{"id":"custom"}]}`))
	}))
	defer server.Close()
	models, err := Models(context.Background(), config.ProviderConfig{
		ID: "api", Type: "openai_responses", BaseURL: server.URL, ModelsURL: server.URL + "/catalog?api-version=v2",
		APIKey: config.NewSecret("secret"), APIKeyHeader: "api-key", APIKeyPrefix: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "custom" {
		t.Fatalf("models = %#v", models)
	}
}
