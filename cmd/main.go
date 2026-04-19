package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	cs "github.com/ccvass/swarmex/swarmex-cluster-scaler"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	configPath := os.Getenv("CLUSTER_SCALER_CONFIG")
	if configPath == "" {
		configPath = "/etc/swarmex/cluster-scaler.yaml"
	}

	cfg, err := cs.LoadConfig(configPath)
	if err != nil {
		logger.Error("failed to load config", "path", configPath, "error", err)
		os.Exit(1)
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Error("docker client failed", "error", err)
		os.Exit(1)
	}
	defer cli.Close()

	// Register configured providers
	var providers []cs.Provider
	if cfg.Providers.AWS != nil {
		providers = append(providers, &cs.AWSProvider{Config: cfg.Providers.AWS})
		logger.Info("provider registered", "name", "aws", "region", cfg.Providers.AWS.Region)
	}
	if cfg.Providers.GCP != nil {
		providers = append(providers, &cs.GCPProvider{Config: cfg.Providers.GCP})
		logger.Info("provider registered", "name", "gcp", "zone", cfg.Providers.GCP.Zone)
	}
	if cfg.Providers.Azure != nil {
		providers = append(providers, &cs.AzureProvider{Config: cfg.Providers.Azure})
		logger.Info("provider registered", "name", "azure", "location", cfg.Providers.Azure.Location)
	}
	if cfg.Providers.DigitalOcean != nil {
		providers = append(providers, &cs.DOProvider{Config: cfg.Providers.DigitalOcean})
		logger.Info("provider registered", "name", "digitalocean", "region", cfg.Providers.DigitalOcean.Region)
	}

	if len(providers) == 0 {
		logger.Error("no providers configured")
		os.Exit(1)
	}

	dbPath := os.Getenv("CLUSTER_SCALER_DB")
	if dbPath == "" { dbPath = "/data/cluster-scaler.db" }
	ctrl, err := cs.New(cli, cfg, providers, dbPath, logger)
	defer ctrl.Close()
	if err != nil { logger.Error("failed to init", "error", err); os.Exit(1) }

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
		logger.Info("health endpoint", "addr", ":8080")
		http.ListenAndServe(":8080", nil)
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	interval, _ := time.ParseDuration(cfg.EvalInterval)
	if interval == 0 {
		interval = 30 * time.Second
	}

	logger.Info("swarmex-cluster-scaler starting",
		"providers", len(providers), "min", cfg.MinNodes, "max", cfg.MaxNodes,
		"scale_up", cfg.ScaleUpCPU, "scale_down", cfg.ScaleDownCPU)

	go ctrl.RunLoop(ctx, interval)
	<-ctx.Done()
	logger.Info("shutdown complete")
}
