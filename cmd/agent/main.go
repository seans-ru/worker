package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Geardung/seans-worker/internal/backend"
	"github.com/Geardung/seans-worker/internal/config"
	"github.com/Geardung/seans-worker/internal/engine"
	"github.com/Geardung/seans-worker/internal/qbit"
	"github.com/Geardung/seans-worker/internal/s3uploader"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	logger.Info("seans worker starting", "version", version)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config error", "error", err)
		os.Exit(1)
	}

	// Init qBittorrent client (API key auth, no login needed)
	qc, err := qbit.New(cfg.QBitURL, cfg.QBitAPIKey, logger)
	if err != nil {
		logger.Error("qbit client error", "error", err)
		os.Exit(1)
	}
	ctx := context.Background()

	// Init rclone uploader
	uploader, err := s3uploader.New(cfg.StateDir, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Endpoint, cfg.S3Bucket, cfg.S3Region, cfg.MockS3, logger)
	if err != nil {
		logger.Error("uploader init error", "error", err)
		os.Exit(1)
	}

	// Init backend client
	bc := backend.New(cfg.BackendURL, cfg.RegSecret, logger)

	// Register with backend (retry loop)
	for {
		err := bc.Register(ctx, cfg.WorkerName)
		if err != nil {
			logger.Error("registration failed, retrying in 10s", "error", err)
			time.Sleep(10 * time.Second)
			continue
		}
		break
	}

	// Create and run engine
	eng := engine.New(cfg, bc, qc, uploader, logger)

	// Graceful shutdown on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	runCtx, runCancel := context.WithCancel(context.Background())
	go func() {
		<-sigCh
		logger.Info("received shutdown signal")
		eng.Shutdown(runCtx)
		runCancel()
	}()

	logger.Info("worker ready",
		"heartbeat_sec", int(cfg.HeartbeatInterval.Seconds()),
		"staging_dir", cfg.StagingDir,
		"mock_s3", cfg.MockS3,
	)

	eng.Run(runCtx)
	fmt.Println("worker exited")
}