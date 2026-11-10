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

	u, err := New(stateDir, "AKIAKEY", "secret", "https://s3.buckets.ru", "seans", "ru-1", slog.New(slog.DiscardHandler))
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
		"type = s3",
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
	if strings.Contains(conf, "type = local") || strings.Contains(conf, "regru") {
		t.Errorf("rclone.conf contains unexpected backend\n%s", conf)
	}

	if u.bucket != "seans" {
		t.Errorf("bucket = %q, want seans", u.bucket)
	}
}

func TestRemotePath(t *testing.T) {
	u, err := New(t.TempDir(), "AK", "SK", "https://s3.buckets.ru", "seans", "ru-1", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	p := u.remotePath("users/tsk/stream/f0000/seg_00000.bin")
	if p != "seans:seans/users/tsk/stream/f0000/seg_00000.bin" {
		t.Errorf("remote path=%s", p)
	}
	// Backslashes in keys must not leak into object keys.
	p2 := u.remotePath(`users\tst\seg.bin`)
	if p2 != "seans:seans/users/tst/seg.bin" {
		t.Errorf("normalized path=%s", p2)
	}
}

func TestParseTransferStats(t *testing.T) {
	line := `2026/10/30 11:40:00 INFO  : Transferred:   1.234 GiB / 2 GiB, 60%, 10.5 MiB/s, ETA 1m20s`
	st, ok := ParseTransferStats(line)
	if !ok {
		t.Fatalf("failed to parse stats line")
	}
	if st.Percent != 60 {
		t.Errorf("percent=%v want 60", st.Percent)
	}
	if st.TotalBytes != 2<<30 {
		t.Errorf("total=%d want %d", st.TotalBytes, 2<<30)
	}
	if st.TransferredBytes <= 0 || st.TransferredBytes >= 2<<30 {
		t.Errorf("transferred=%d", st.TransferredBytes)
	}
	if st.SpeedBps != 10<<20+512<<10 {
		t.Errorf("speed=%d want %d", st.SpeedBps, 10<<20+512<<10)
	}
	if st.ETASeconds != 80 {
		t.Errorf("eta=%d want 80", st.ETASeconds)
	}

	if _, ok := ParseTransferStats("Checks: 3 / 3, 100%"); ok {
		t.Error("non-stats line should not parse")
	}
}

func TestParseRcloneETA(t *testing.T) {
	cases := map[string]int64{
		"45s":    45,
		"1m20s":  80,
		"2h3m":   7380,
		"1h2m3s": 3723,
		"0s":     0,
	}
	for in, want := range cases {
		if got := parseRcloneETA(in); got != want {
			t.Errorf("parseRcloneETA(%q)=%d want %d", in, got, want)
		}
	}
}
