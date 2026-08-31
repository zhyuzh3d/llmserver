package runtimecfg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/zhyuzh3d/llmserver/internal/auth"
	"github.com/zhyuzh3d/llmserver/internal/config"
	"github.com/zhyuzh3d/llmserver/internal/gateway"
	"github.com/zhyuzh3d/llmserver/internal/provider"
	codexadapter "github.com/zhyuzh3d/llmserver/internal/provider/codex"
	"github.com/zhyuzh3d/llmserver/internal/provider/mock"
	openaiadapter "github.com/zhyuzh3d/llmserver/internal/provider/openai"
	workbuddyadapter "github.com/zhyuzh3d/llmserver/internal/provider/workbuddy"
)

type Snapshot struct {
	Config        *config.Config
	Secrets       *config.SecretConfig
	Authenticator *auth.Authenticator
	Gateway       *gateway.Service
}

type Manager struct {
	publicPath   string
	secretPath   string
	repository   gateway.RunRepository
	mockResponse string
	mu           sync.Mutex
	current      atomic.Pointer[Snapshot]
}

type SecretStatus struct {
	ClientTokens    map[string]bool `json:"client_tokens"`
	ProviderAPIKeys map[string]bool `json:"provider_api_keys"`
}

type Update struct {
	Config             config.Config     `json:"config"`
	ClientTokenUpdates map[string]string `json:"client_token_updates"`
	ClientTokenRenames map[string]string `json:"client_token_renames"`
	ProviderKeyUpdates map[string]string `json:"provider_key_updates"`
}

func New(publicPath, secretPath string, repository gateway.RunRepository, mockResponse string) (*Manager, error) {
	manager := &Manager{publicPath: publicPath, secretPath: secretPath, repository: repository, mockResponse: mockResponse}
	if err := manager.Reload(context.Background()); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Current() (*auth.Authenticator, *gateway.Service) {
	snapshot := m.current.Load()
	if snapshot == nil {
		return nil, nil
	}
	return snapshot.Authenticator, snapshot.Gateway
}

func (m *Manager) Snapshot() *Snapshot { return m.current.Load() }

func (m *Manager) SecretStatus() SecretStatus {
	snapshot := m.current.Load()
	status := SecretStatus{ClientTokens: map[string]bool{}, ProviderAPIKeys: map[string]bool{}}
	if snapshot == nil || snapshot.Secrets == nil {
		return status
	}
	for id, value := range snapshot.Secrets.ClientTokens {
		status.ClientTokens[id] = value != ""
	}
	for id, value := range snapshot.Secrets.ProviderAPIKeys {
		status.ProviderAPIKeys[id] = value != ""
	}
	return status
}

func (m *Manager) RevealSecrets() config.SecretConfig {
	snapshot := m.current.Load()
	if snapshot == nil || snapshot.Secrets == nil {
		return config.SecretConfig{Version: 1, ClientTokens: map[string]string{}, ProviderAPIKeys: map[string]string{}}
	}
	return cloneSecrets(*snapshot.Secrets)
}

func (m *Manager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, secrets, err := config.LoadFiles(m.publicPath, m.secretPath)
	if err != nil {
		return err
	}
	snapshot, err := m.build(ctx, cfg, secrets)
	if err != nil {
		return err
	}
	m.current.Store(snapshot)
	return nil
}

func (m *Manager) Update(ctx context.Context, update Update) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.current.Load()
	if current == nil {
		return errors.New("runtime is not initialized")
	}
	secrets := cloneSecrets(*current.Secrets)
	for newID, oldID := range update.ClientTokenRenames {
		if newID != "" && oldID != "" && secrets.ClientTokens[newID] == "" {
			secrets.ClientTokens[newID] = secrets.ClientTokens[oldID]
		}
	}
	for id, value := range update.ClientTokenUpdates {
		if value != "" {
			secrets.ClientTokens[id] = value
		}
	}
	for id, value := range update.ProviderKeyUpdates {
		if value != "" {
			secrets.ProviderAPIKeys[id] = value
		}
	}
	pruneSecrets(&secrets, update.Config)
	resolved, err := config.Resolve(update.Config, secrets)
	if err != nil {
		return err
	}
	snapshot, err := m.build(ctx, resolved, &secrets)
	if err != nil {
		return err
	}
	if err := config.SaveFiles(m.publicPath, m.secretPath, update.Config, secrets); err != nil {
		return err
	}
	m.current.Store(snapshot)
	return nil
}

func (m *Manager) build(ctx context.Context, cfg *config.Config, secrets *config.SecretConfig) (*Snapshot, error) {
	authenticator, err := auth.FromConfig(cfg.Clients)
	if err != nil {
		return nil, err
	}
	adapters := make([]provider.Adapter, 0, len(cfg.Providers))
	for _, providerConfig := range cfg.Providers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if m.mockResponse != "" {
			adapters = append(adapters, &mock.Adapter{ProviderID: providerConfig.ID, ResponseText: m.mockResponse})
			continue
		}
		switch providerConfig.Type {
		case "openai_responses":
			if providerConfig.APIKey.IsEmpty() {
				adapters = append(adapters, unavailableAdapter{id: providerConfig.ID, reason: "provider API key is not configured"})
				continue
			}
			adapter, adapterErr := openaiadapter.New(providerConfig.ID, providerConfig.BaseURL, providerConfig.APIKey, nil)
			if adapterErr != nil {
				return nil, fmt.Errorf("configure provider %s: %w", providerConfig.ID, adapterErr)
			}
			adapters = append(adapters, adapter)
		case "codex_exec":
			adapter, adapterErr := codexadapter.New(codexadapter.Config{
				ProviderID: providerConfig.ID, Executable: providerConfig.Executable,
				ExpectedVersion: providerConfig.ExpectedVersion, ExtraArgs: providerConfig.ExtraArgs,
				ObserveQuota: providerConfig.ObserveQuota,
			})
			if adapterErr != nil {
				return nil, fmt.Errorf("configure provider %s: %w", providerConfig.ID, adapterErr)
			}
			adapters = append(adapters, adapter)
		case "workbuddy_exec":
			adapter, adapterErr := workbuddyadapter.New(workbuddyadapter.Config{
				ProviderID: providerConfig.ID, Executable: providerConfig.Executable,
				ExpectedVersion: providerConfig.ExpectedVersion, ExtraArgs: providerConfig.ExtraArgs,
			})
			if adapterErr != nil {
				return nil, fmt.Errorf("configure provider %s: %w", providerConfig.ID, adapterErr)
			}
			adapters = append(adapters, adapter)
		default:
			return nil, fmt.Errorf("unsupported provider type %q", providerConfig.Type)
		}
	}
	gatewayService, err := gateway.NewService(cfg.Deployments, adapters, gateway.WithRunRepository(m.repository))
	if err != nil {
		return nil, err
	}
	return &Snapshot{Config: cfg, Secrets: secrets, Authenticator: authenticator, Gateway: gatewayService}, nil
}

type unavailableAdapter struct {
	id     string
	reason string
}

func (a unavailableAdapter) ID() string { return a.id }

func (a unavailableAdapter) Start(context.Context, provider.Request) (<-chan provider.Event, error) {
	return nil, errors.New(a.reason)
}

func cloneSecrets(source config.SecretConfig) config.SecretConfig {
	clone := config.SecretConfig{Version: source.Version, ClientTokens: map[string]string{}, ProviderAPIKeys: map[string]string{}}
	for id, value := range source.ClientTokens {
		clone.ClientTokens[id] = value
	}
	for id, value := range source.ProviderAPIKeys {
		clone.ProviderAPIKeys[id] = value
	}
	return clone
}

func pruneSecrets(secrets *config.SecretConfig, cfg config.Config) {
	clients := make(map[string]struct{}, len(cfg.Clients))
	for _, client := range cfg.Clients {
		clients[client.ID] = struct{}{}
	}
	for id := range secrets.ClientTokens {
		if _, exists := clients[id]; !exists {
			delete(secrets.ClientTokens, id)
		}
	}
	providers := make(map[string]struct{}, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		providers[provider.ID] = struct{}{}
	}
	for id := range secrets.ProviderAPIKeys {
		if _, exists := providers[id]; !exists {
			delete(secrets.ProviderAPIKeys, id)
		}
	}
}
