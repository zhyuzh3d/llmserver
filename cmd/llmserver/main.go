package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zhyuzh3d/llmserver/internal/api"
	"github.com/zhyuzh3d/llmserver/internal/auth"
	"github.com/zhyuzh3d/llmserver/internal/config"
	"github.com/zhyuzh3d/llmserver/internal/gateway"
	"github.com/zhyuzh3d/llmserver/internal/provider"
	codexadapter "github.com/zhyuzh3d/llmserver/internal/provider/codex"
	"github.com/zhyuzh3d/llmserver/internal/provider/mock"
	openaiadapter "github.com/zhyuzh3d/llmserver/internal/provider/openai"
	workbuddyadapter "github.com/zhyuzh3d/llmserver/internal/provider/workbuddy"
	"github.com/zhyuzh3d/llmserver/internal/store"
)

func main() {
	configPath := flag.String("config", "llmserver.yaml", "path to llmserver YAML configuration")
	mockResponse := flag.String("mock-response", "", "use an offline mock provider with this response text")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load configuration", err)
	}
	authenticator, err := auth.FromConfig(cfg.Clients)
	if err != nil {
		fatal("configure client authentication", err)
	}
	adapters := make([]provider.Adapter, 0, len(cfg.Providers))
	for _, providerConfig := range cfg.Providers {
		if *mockResponse != "" {
			adapters = append(adapters, &mock.Adapter{ProviderID: providerConfig.ID, ResponseText: *mockResponse})
			continue
		}
		switch providerConfig.Type {
		case "openai_responses":
			adapter, adapterErr := openaiadapter.New(providerConfig.ID, providerConfig.BaseURL, providerConfig.APIKey, nil)
			if adapterErr != nil {
				fatal("configure provider "+providerConfig.ID, adapterErr)
			}
			adapters = append(adapters, adapter)
		case "codex_exec":
			adapter, adapterErr := codexadapter.New(codexadapter.Config{
				ProviderID:      providerConfig.ID,
				Executable:      providerConfig.Executable,
				ExpectedVersion: providerConfig.ExpectedVersion,
				ExtraArgs:       providerConfig.ExtraArgs,
				ObserveQuota:    providerConfig.ObserveQuota,
			})
			if adapterErr != nil {
				fatal("configure provider "+providerConfig.ID, adapterErr)
			}
			adapters = append(adapters, adapter)
		case "workbuddy_exec":
			adapter, adapterErr := workbuddyadapter.New(workbuddyadapter.Config{
				ProviderID:      providerConfig.ID,
				Executable:      providerConfig.Executable,
				ExpectedVersion: providerConfig.ExpectedVersion,
				ExtraArgs:       providerConfig.ExtraArgs,
			})
			if adapterErr != nil {
				fatal("configure provider "+providerConfig.ID, adapterErr)
			}
			adapters = append(adapters, adapter)
		default:
			fatal("configure provider "+providerConfig.ID, &unsupportedProviderError{providerType: providerConfig.Type})
		}
	}
	runStore, err := store.Open(cfg.Server.StatePath)
	if err != nil {
		fatal("open state database", err)
	}
	defer runStore.Close()
	gatewayService, err := gateway.NewService(cfg.Deployments, adapters, gateway.WithRunRepository(runStore))
	if err != nil {
		fatal("configure gateway", err)
	}
	server := api.NewServer(authenticator, gatewayService)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	mode := "standard-api"
	if *mockResponse != "" {
		mode = "offline-mock"
	}
	slog.Info("llmserver starting", "listen", cfg.Server.Listen, "mode", mode)
	if err := api.RunHTTPServer(ctx, cfg.Server.Listen, server.Handler()); err != nil {
		fatal("serve", err)
	}
}

type unsupportedProviderError struct{ providerType string }

func (e *unsupportedProviderError) Error() string {
	return "unsupported provider type " + e.providerType
}

func fatal(message string, err error) {
	slog.Error(message, "error", err)
	os.Exit(1)
}
