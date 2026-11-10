package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestExecuteRealFfmpeg runs the full plan against a generated test clip.
// Skipped when ffmpeg is not on PATH.
func TestExecuteRealFfmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	gen := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=12",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate clip: %v: %s", err, out)
	}

	ladder := []Rung{{Name: "240p", Height: 240, VideoBitrate: "200k", MaxRate: "250k", BufSize: "400k"}}
	outRoot := filepath.Join(dir, "out")
	plan, err := BuildPlan(src, outRoot, ladder, Options{SegmentSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(plan.MasterPath); err != nil {
		t.Fatalf("master playlist missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outRoot, "hls", "240p", PlaylistName)); err != nil {
		t.Fatalf("rung playlist missing: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(outRoot, "hls", "240p"))
	if err != nil {
		t.Fatal(err)
	}
	segs := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".ts" {
			segs++
		}
	}
	if segs == 0 {
		t.Fatal("no ts segments produced")
	}
}
