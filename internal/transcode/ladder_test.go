package transcode

import "testing"

func TestDefaultLadderValid(t *testing.T) {
	ladder := DefaultLadder()
	if len(ladder) == 0 {
		t.Fatal("empty ladder")
	}
	for _, r := range ladder {
		if err := r.Validate(); err != nil {
			t.Fatalf("rung %s: %v", r.Name, err)
		}
	}
}

func TestRungValidate(t *testing.T) {
	if err := (Rung{}).Validate(); err == nil {
		t.Fatal("empty rung must not validate")
	}
	if err := (Rung{Name: "x", Height: 0, VideoBitrate: "1k", MaxRate: "1k", BufSize: "1k"}).Validate(); err == nil {
		t.Fatal("zero height must not validate")
	}
}

func TestCapLadder(t *testing.T) {
	ladder := DefaultLadder()

	capped := CapLadder(ladder, 720)
	if len(capped) != 3 {
		t.Fatalf("cap 720: got %d rungs, want 3", len(capped))
	}
	for _, r := range capped {
		if r.Height > 720 {
			t.Fatalf("rung %s taller than cap", r.Name)
		}
	}

	one := CapLadder(ladder, 100)
	if len(one) != 1 || one[0].Height != 240 {
		t.Fatalf("cap below ladder must keep lowest rung, got %v", one)
	}

	if got := CapLadder(ladder, 0); len(got) != len(ladder) {
		t.Fatalf("cap 0 must keep all rungs")
	}
}
