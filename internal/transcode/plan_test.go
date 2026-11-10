package transcode

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPlan(t *testing.T) {
	ladder := DefaultLadder()
	plan, err := BuildPlan("/media/in.mkv", "/work/out", ladder, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Src != "/media/in.mkv" || plan.OutRoot != "/work/out" {
		t.Fatalf("plan src/outRoot: %+v", plan)
	}
	if len(plan.Steps) != len(ladder) {
		t.Fatalf("steps = %d, want %d", len(plan.Steps), len(ladder))
	}
	for i, s := range plan.Steps {
		if s.Rung.Name != ladder[i].Name {
			t.Fatalf("step %d rung = %s", i, s.Rung.Name)
		}
		wantDir := filepath.Join("/work/out", "hls", ladder[i].Name)
		if s.OutDir != wantDir {
			t.Fatalf("step %d outDir = %s, want %s", i, s.OutDir, wantDir)
		}
		if !strings.Contains(strings.Join(s.Args, " "), wantDir) {
			t.Fatalf("step %d args do not use outDir: %v", i, s.Args)
		}
	}
	if plan.MasterPath != filepath.Join("/work/out", "hls", "master.m3u8") {
		t.Fatalf("master path %q", plan.MasterPath)
	}
	for _, r := range ladder {
		if !strings.Contains(plan.Master, r.Name+"/index.m3u8") {
			t.Fatalf("master playlist misses rung %s:\n%s", r.Name, plan.Master)
		}
	}
}

func TestBuildPlanRejectsBadInput(t *testing.T) {
	if _, err := BuildPlan("", "/out", DefaultLadder(), Options{}); err == nil {
		t.Fatal("empty src must fail")
	}
	if _, err := BuildPlan("/in", "/out", nil, Options{}); err == nil {
		t.Fatal("empty ladder must fail")
	}
}
