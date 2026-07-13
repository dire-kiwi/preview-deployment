package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/api"
	"github.com/dire-kiwi/preview-deployment/internal/config"
	"github.com/dire-kiwi/preview-deployment/internal/docker"
	"github.com/dire-kiwi/preview-deployment/internal/orchestrator"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		runHealthcheck()
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	dockerClient := docker.New(cfg.DockerSocket)
	connectCtx, connectCancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = dockerClient.Connect(connectCtx)
	connectCancel()
	if err != nil {
		logger.Error("cannot connect to Docker", "socket", cfg.DockerSocket, "error", err)
		os.Exit(1)
	}

	service, err := orchestrator.New(dockerClient, cfg, logger)
	if err != nil {
		logger.Error("cannot initialize orchestrator", "error", err)
		os.Exit(1)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := service.CleanupOrphanPayloads(cleanupCtx); err != nil {
		logger.Warn("could not clean up orphan runtime payloads", "error", err)
	}
	cleanupCancel()
	hibernationCtx, hibernationCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := service.ReconcileHibernation(hibernationCtx); err != nil {
		logger.Warn("could not initialize preview hibernation state", "error", err)
	}
	hibernationCancel()
	httpAPI := api.New(
		service,
		dockerClient,
		logger,
		cfg.MaxUploadBytes,
		cfg.MaxBinaryBytes,
		cfg.APIToken,
		api.WithDashboardControls(cfg.DashboardToken, cfg.DashboardOrigin),
	)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpAPI.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	signals, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	go service.RunHibernation(signals)

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("orchestrator listening",
			"address", cfg.ListenAddr,
			"preview_domain", cfg.PreviewDomain,
			"docker_network", cfg.DockerNetwork,
			"preview_idle_timeout", cfg.PreviewIdleTimeout,
		)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-signals.Done():
		logger.Info("shutting down orchestrator")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			_ = server.Close()
		}
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			stopSignals()
			logger.Error("HTTP server stopped", "error", err)
			os.Exit(1)
		}
	}
}

func runHealthcheck() {
	address := os.Getenv("HEALTHCHECK_URL")
	if address == "" {
		address = "http://127.0.0.1:8080/healthz"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(address)
	if err != nil {
		os.Exit(1)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}
