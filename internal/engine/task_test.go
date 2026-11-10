package engine

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Geardung/seans-worker/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestIsActiveState(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"downloading", true},
		{"forcedDL", true},
		{"stalledDL", true},
		{"checkingDL", true},
		{"queuedDL", true},
		{"metaDL", true},
		{"stoppedUP", false},
		{"pausedUP", false},
		{"uploading", false},
		{"stalledUP", false},
		{"unknown", false},
	}

	for _, tt := range tests {
		if got := isActiveState(tt.state); got != tt.want {
			t.Errorf("isActiveState(%q) = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestRelativePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/Show/S01/E01.mkv", "Show/S01/E01.mkv"},
		{"Show/S01/E01.mkv", "Show/S01/E01.mkv"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := relativePath(tt.input); got != tt.want {
			t.Errorf("relativePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTaskError(t *testing.T) {
	err := &TaskError{Reason: "stalled", Permanent: true}
	if err.Error() != "stalled" {
		t.Errorf("expected stalled, got %s", err.Error())
	}
}

func TestConfigLoad_Defaults(t *testing.T) {
	t.Setenv("BACKEND_URL", "http://localhost:9999")
	t.Setenv("WORKER_REG_SECRET", "test-secret")
	t.Setenv("QBIT_API_KEY", "qbt_testkey123")
	t.Setenv("S3_ACCESS_KEY", "AKIAKEY")
	t.Setenv("S3_SECRET_KEY", "secret")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.BackendURL != "http://localhost:9999" {
		t.Errorf("expected backend URL, got %s", cfg.BackendURL)
	}
	if cfg.RegSecret != "test-secret" {
		t.Errorf("expected reg secret, got %s", cfg.RegSecret)
	}
	if cfg.ClaimInterval != 10*time.Second {
		t.Errorf("expected 10s claim interval, got %v", cfg.ClaimInterval)
	}
	if cfg.HeartbeatInterval != 60*time.Second {
		t.Errorf("expected 60s heartbeat, got %v", cfg.HeartbeatInterval)
	}
	if cfg.MaxConcurrentTasks != 1 {
		t.Errorf("expected 1 max tasks, got %d", cfg.MaxConcurrentTasks)
	}
}

func TestConfigLoad_MissingRequired(t *testing.T) {
	t.Setenv("BACKEND_URL", "")
	t.Setenv("WORKER_REG_SECRET", "")
	t.Setenv("QBIT_API_KEY", "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error for missing required config")
	}
}
