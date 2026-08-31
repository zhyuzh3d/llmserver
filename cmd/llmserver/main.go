package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zhyuzh3d/llmserver/internal/admin"
	"github.com/zhyuzh3d/llmserver/internal/api"
	"github.com/zhyuzh3d/llmserver/internal/config"
	"github.com/zhyuzh3d/llmserver/internal/runtimecfg"
	"github.com/zhyuzh3d/llmserver/internal/store"
)

func main() {
	publicPath := flag.String("config", "configs/config.yaml", "path to the non-secret configuration")
	secretPath := flag.String("xconfig", "../xconfigs/llmserver/xconfig.yaml", "path to the secret xconfig")
	flag.Parse()

	created, err := config.BootstrapSecretFile(*publicPath, *secretPath)
	if err != nil {
		fatal("bootstrap secret xconfig", err)
	}
	if created {
		slog.Info("secret xconfig initialized", "path", *secretPath)
	}
	cfg, _, err := config.LoadFiles(*publicPath, *secretPath)
	if err != nil {
		fatal("load configuration", err)
	}
	runStore, err := store.Open(config.ResolveStatePath(*publicPath, cfg.Server.StatePath))
	if err != nil {
		fatal("open state database", err)
	}
	defer runStore.Close()
	manager, err := runtimecfg.New(*publicPath, *secretPath, runStore, "")
	if err != nil {
		fatal("configure runtime", err)
	}
	defer manager.Close()
	apiServer := api.NewDynamicServer(manager.Current)
	adminServer := admin.New(manager, runStore)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- api.RunHTTPServer(ctx, cfg.Server.Listen, apiServer.Handler()) }()
	go func() { errCh <- api.RunHTTPServer(ctx, cfg.Server.AdminListen, adminServer.Handler()) }()
	slog.Info("llmserver starting", "api", cfg.Server.Listen, "admin", "http://"+cfg.Server.AdminListen+"/admin/")
	if err := <-errCh; err != nil {
		cancel()
		fatal("serve", err)
	}
}

func fatal(message string, err error) {
	slog.Error(message, "error", err)
	os.Exit(1)
}
