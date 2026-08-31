package runtimecfg

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

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
	Adapters      map[string]provider.Adapter
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
	ClientTokens     map[string]bool   `json:"client_tokens"`
	ProviderAPIKeys  map[string]bool   `json:"provider_api_keys"`
	ClientTokenHints map[string]string `json:"client_token_hints"`
	ProviderKeyHints map[string]string `json:"provider_api_key_hints"`
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
	status := SecretStatus{ClientTokens: map[string]bool{}, ProviderAPIKeys: map[string]bool{}, ClientTokenHints: map[string]string{}, ProviderKeyHints: map[string]string{}}
	if snapshot == nil || snapshot.Secrets == nil {
		return status
	}
	for id, value := range snapshot.Secrets.ClientTokens {
		status.ClientTokens[id] = value != ""
		status.ClientTokenHints[id] = maskSecret(value)
	}
	for id, value := range snapshot.Secrets.ProviderAPIKeys {
		status.ProviderAPIKeys[id] = value != ""
		status.ProviderKeyHints[id] = maskSecret(value)
	}
	return status
}

func maskSecret(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		if len(value) <= 4 {
			return "••••"
		}
		return "••••" + value[len(value)-4:]
	}
	return value[:4] + "••••••••" + value[len(value)-4:]
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
	current := m.current.Load()
	snapshot, err := m.build(ctx, cfg, secrets, current)
	if err != nil {
		return err
	}
	old := m.current.Swap(snapshot)
	retireSnapshotExcept(old, snapshot)
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
	snapshot, err := m.build(ctx, resolved, &secrets, current)
	if err != nil {
		return err
	}
	if err := config.SaveFiles(m.publicPath, m.secretPath, update.Config, secrets); err != nil {
		retireSnapshotExcept(snapshot, current)
		return err
	}
	old := m.current.Swap(snapshot)
	retireSnapshotExcept(old, snapshot)
	return nil
}

// Close retires the current runtime's persistent local provider workers.
func (m *Manager) Close() {
	retireSnapshotExcept(m.current.Swap(nil), nil)
}

func retireSnapshotExcept(snapshot, keep *Snapshot) {
	if snapshot == nil {
		return
	}
	for id, adapter := range snapshot.Adapters {
		if keep != nil && sameAdapter(adapter, keep.Adapters[id]) {
			continue
		}
		if retirable, ok := adapter.(provider.Retirable); ok {
			retirable.Retire()
		}
	}
}

func sameAdapter(left, right provider.Adapter) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	return leftValue.Type() == rightValue.Type() && leftValue.Kind() == reflect.Pointer && leftValue.Pointer() == rightValue.Pointer()
}

func (m *Manager) build(ctx context.Context, cfg *config.Config, secrets *config.SecretConfig, reuse *Snapshot) (*Snapshot, error) {
	authenticator, err := auth.FromConfig(cfg.Clients)
	if err != nil {
		return nil, err
	}
	adapters := make([]provider.Adapter, 0, len(cfg.Providers))
	adapterByID := make(map[string]provider.Adapter, len(cfg.Providers))
	newAdapters := make([]provider.Adapter, 0, len(cfg.Providers))
	retireBuiltAdapters := func() {
		for _, adapter := range newAdapters {
			if retirable, ok := adapter.(provider.Retirable); ok {
				retirable.Retire()
			}
		}
	}
	for _, providerConfig := range cfg.Providers {
		if err := ctx.Err(); err != nil {
			retireBuiltAdapters()
			return nil, err
		}
		if reusable := reusableAdapter(reuse, providerConfig); reusable != nil {
			adapters = append(adapters, reusable)
			adapterByID[providerConfig.ID] = reusable
			continue
		}
		var built provider.Adapter
		if m.mockResponse != "" {
			built = &mock.Adapter{ProviderID: providerConfig.ID, ResponseText: m.mockResponse}
			adapters = append(adapters, built)
			adapterByID[providerConfig.ID] = built
			newAdapters = append(newAdapters, built)
			continue
		}
		switch providerConfig.Type {
		case "openai_responses":
			if providerConfig.APIKey.IsEmpty() {
				built = unavailableAdapter{id: providerConfig.ID, reason: "provider API key is not configured"}
				adapters = append(adapters, built)
				adapterByID[providerConfig.ID] = built
				continue
			}
			adapter, adapterErr := openaiadapter.NewConfigured(openaiadapter.Config{
				ProviderID: providerConfig.ID, BaseURL: providerConfig.BaseURL, ResponsesURL: providerConfig.ResponsesURL,
				APIKey: providerConfig.APIKey, APIKeyHeader: providerConfig.APIKeyHeader, APIKeyPrefix: providerConfig.APIKeyPrefix,
			}, nil)
			if adapterErr != nil {
				retireBuiltAdapters()
				return nil, fmt.Errorf("configure provider %s: %w", providerConfig.ID, adapterErr)
			}
			built = adapter
		case "codex_exec":
			adapter, adapterErr := codexadapter.New(codexadapter.Config{
				ProviderID: providerConfig.ID, Executable: providerConfig.Executable,
				ExpectedVersion: providerConfig.ExpectedVersion, ExtraArgs: providerConfig.ExtraArgs,
				MaxConcurrency:   providerConfig.MaxConcurrency,
				DefaultReasoning: providerConfig.DefaultReasoning, ServiceTier: providerConfig.ServiceTier,
			})
			if adapterErr != nil {
				retireBuiltAdapters()
				return nil, fmt.Errorf("configure provider %s: %w", providerConfig.ID, adapterErr)
			}
			built = adapter
		case "workbuddy_exec":
			adapter, adapterErr := workbuddyadapter.New(workbuddyadapter.Config{
				ProviderID: providerConfig.ID, Executable: providerConfig.Executable,
				ExpectedVersion: providerConfig.ExpectedVersion, ExtraArgs: providerConfig.ExtraArgs,
				MaxConcurrency:   providerConfig.MaxConcurrency,
				DefaultReasoning: providerConfig.DefaultReasoning,
				WarmupEnabled:    providerConfig.WarmupEnabled, WarmupModel: providerConfig.WarmupModel,
				WarmupTimeout: time.Duration(providerConfig.WarmupTimeoutSecs) * time.Second,
			})
			if adapterErr != nil {
				retireBuiltAdapters()
				return nil, fmt.Errorf("configure provider %s: %w", providerConfig.ID, adapterErr)
			}
			built = adapter
		default:
			retireBuiltAdapters()
			return nil, fmt.Errorf("unsupported provider type %q", providerConfig.Type)
		}
		adapters = append(adapters, built)
		adapterByID[providerConfig.ID] = built
		newAdapters = append(newAdapters, built)
	}
	gatewayService, err := gateway.NewService(cfg.Deployments, adapters, gateway.WithRunRepository(m.repository))
	if err != nil {
		retireBuiltAdapters()
		return nil, err
	}
	return &Snapshot{Config: cfg, Secrets: secrets, Authenticator: authenticator, Gateway: gatewayService, Adapters: adapterByID}, nil
}

func reusableAdapter(snapshot *Snapshot, candidate config.ProviderConfig) provider.Adapter {
	if snapshot == nil {
		return nil
	}
	for _, existing := range snapshot.Config.Providers {
		if sameProviderRuntime(existing, candidate) {
			return snapshot.Adapters[candidate.ID]
		}
	}
	return nil
}

func sameProviderRuntime(left, right config.ProviderConfig) bool {
	if left.ID != right.ID || left.Type != right.Type {
		return false
	}
	switch left.Type {
	case "openai_responses":
		return left.BaseURL == right.BaseURL && left.ResponsesURL == right.ResponsesURL &&
			left.APIKeyHeader == right.APIKeyHeader && left.APIKeyPrefix == right.APIKeyPrefix &&
			left.WireAPI == right.WireAPI && left.APIKey.Reveal() == right.APIKey.Reveal()
	case "codex_exec":
		return left.Executable == right.Executable && left.ExpectedVersion == right.ExpectedVersion &&
			slices.Equal(left.ExtraArgs, right.ExtraArgs) && left.MaxConcurrency == right.MaxConcurrency &&
			left.DefaultReasoning == right.DefaultReasoning && left.ServiceTier == right.ServiceTier
	case "workbuddy_exec":
		return left.Executable == right.Executable && left.ExpectedVersion == right.ExpectedVersion &&
			slices.Equal(left.ExtraArgs, right.ExtraArgs) && left.MaxConcurrency == right.MaxConcurrency &&
			left.DefaultReasoning == right.DefaultReasoning && left.WarmupEnabled == right.WarmupEnabled &&
			left.WarmupModel == right.WarmupModel && left.WarmupTimeoutSecs == right.WarmupTimeoutSecs
	default:
		return false
	}
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
