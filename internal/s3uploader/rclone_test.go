package s3uploader

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateConfig(t *testing.T) {
	stateDir := t.TempDir()

	u, err := New(stateDir, "AKIAKEY", "secret", "https://s3.buckets.ru", "seans", "ru-1", false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(stateDir, "rclone.conf"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	conf := string(data)

	for _, want := range []string{
		"[seans]",
		"endpoint = https://s3.buckets.ru",
		"region = ru-1",
		"access_key_id = AKIAKEY",
		"secret_access_key = secret",
		"force_path_style = true",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("rclone.conf missing %q\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "regru") {
		t.Errorf("rclone.conf still references old provider\n%s", conf)
	}

	if u.bucket != "seans" {
		t.Errorf("bucket = %q, want seans", u.bucket)
	}
}

func TestGenerateConfigMock(t *testing.T) {
	stateDir := t.TempDir()

	u, err := New(stateDir, "", "", "", "seans", "", true, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(stateDir, "rclone.conf"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "type = local") {
		t.Errorf("mock config should be local backend, got:\n%s", data)
	}
	_ = u
}
