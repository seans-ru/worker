package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	BackendURL             string
	RegSecret              string
	WorkerName             string
	QBitURL                string
	QBitAPIKey             string
	QBitDownloads          string
	StagingDir             string
	S3AccessKey            string
	S3SecretKey            string
	S3Endpoint             string
	S3Bucket               string
	S3Region               string
	ClaimInterval          time.Duration
	MaxConcurrentTasks     int
	HeartbeatInterval      time.Duration
	DownloadStallTimeout   time.Duration
	TorrentMetadataTimeout time.Duration
	TaskMaxDuration        time.Duration
	DrainOnShutdown        bool
	DrainTimeout           time.Duration
	StateDir               string
	// StreamSegmentSize is the progressive S3 segment size for streaming-while-downloading.
	// 0 means default (8 MiB).
	StreamSegmentSize int64
	// TranscodeEnabled builds an HLS quality ladder from the primary video (ABR playback).
	TranscodeEnabled bool
	// FfmpegPath is the ffmpeg binary used for the ladder (default "ffmpeg").
	FfmpegPath string
	// TranscodeEncoder: "libx264" (default) or "h264_nvenc".
	TranscodeEncoder string
	// TranscodeSegmentSec is the HLS segment duration (default 4).
	TranscodeSegmentSec int
}

func Load() (*Config, error) {
	cfg := &Config{
		BackendURL:             os.Getenv("BACKEND_URL"),
		RegSecret:              os.Getenv("WORKER_REG_SECRET"),
		WorkerName:             envOr("WORKER_NAME", hostname()),
		QBitURL:                envOr("QBIT_URL", "http://qbittorrent:8080"),
		QBitAPIKey:             os.Getenv("QBIT_API_KEY"),
		QBitDownloads:          envOr("QBIT_DOWNLOADS", "/downloads"),
		StagingDir:             envOr("STAGING_DIR", "/staging"),
		S3AccessKey:            os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:            os.Getenv("S3_SECRET_KEY"),
		S3Endpoint:             envOr("S3_ENDPOINT", "https://s3.buckets.ru"),
		S3Bucket:               envOr("S3_BUCKET", "seans"),
		S3Region:               envOr("S3_REGION", "ru-1"),
		ClaimInterval:          envDuration("CLAIM_INTERVAL_SEC", 10*time.Second),
		MaxConcurrentTasks:     envInt("MAX_CONCURRENT_TASKS", 1),
		HeartbeatInterval:      envDuration("HEARTBEAT_INTERVAL_SEC", 60*time.Second),
		DownloadStallTimeout:   envDuration("DOWNLOAD_STALL_TIMEOUT_MIN", 30*time.Minute),
		TorrentMetadataTimeout: envDuration("TORRENT_METADATA_TIMEOUT", 5*time.Minute),
		TaskMaxDuration:        envDurationHours("TASK_MAX_HOURS", 12),
		DrainOnShutdown:        envBool("DRAIN_ON_SHUTDOWN", false),
		DrainTimeout:           envDuration("DRAIN_TIMEOUT_SEC", 3600*time.Second),
		StateDir:               envOr("STATE_DIR", "/state"),
		StreamSegmentSize:      envInt64("STREAM_SEGMENT_SIZE_MB", 8) * 1024 * 1024,
		TranscodeEnabled:       envBool("TRANSCODE_ENABLED", false),
		FfmpegPath:             envOr("FFMPEG_PATH", "ffmpeg"),
		TranscodeEncoder:       envOr("TRANSCODE_ENCODER", "libx264"),
		TranscodeSegmentSec:    envInt("TRANSCODE_SEGMENT_SEC", 4),
	}

	if cfg.BackendURL == "" {
		return nil, fmt.Errorf("BACKEND_URL is required")
	}
	if cfg.RegSecret == "" {
		return nil, fmt.Errorf("WORKER_REG_SECRET is required")
	}
	if cfg.QBitAPIKey == "" {
		return nil, fmt.Errorf("QBIT_API_KEY is required")
	}
	if cfg.S3AccessKey == "" {
		return nil, fmt.Errorf("S3_ACCESS_KEY is required")
	}
	if cfg.S3SecretKey == "" {
		return nil, fmt.Errorf("S3_SECRET_KEY is required")
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envInt64(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return time.Duration(n) * time.Second
}

func envDurationHours(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return time.Duration(n) * time.Hour
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "worker-unknown"
	}
	return h
}
