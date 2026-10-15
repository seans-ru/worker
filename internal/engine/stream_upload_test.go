package engine

import (
	"testing"

	"github.com/Geardung/seans-worker/internal/stream"
)

func TestToPieceStates(t *testing.T) {
	got := toPieceStates([]int{0, 1, 2, 2})
	want := []stream.PieceState{stream.PieceMissing, stream.PieceDownloading, stream.PieceDownloaded, stream.PieceDownloaded}
	if len(got) != len(want) {
		t.Fatalf("len=%d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%d want %d", i, got[i], want[i])
		}
	}
}

func TestProgressFromSeekLimit(t *testing.T) {
	layout := stream.FileLayout{
		Index: 0, Path: "movie.mkv", Size: 100,
		Offset: 0, PieceStart: 0, PieceEnd: 3, PieceSize: 32,
	}
	// Only first two pieces downloaded -> contiguous 64, not complete
	fp := stream.BuildProgress(layout, []stream.PieceState{
		stream.PieceDownloaded, stream.PieceDownloaded, stream.PieceMissing, stream.PieceMissing,
	}, 32, map[int]string{})

	sp := progressFrom(fp, layout)
	if sp.ContiguousBytes != 64 {
		t.Errorf("contiguous=%d want 64", sp.ContiguousBytes)
	}
	if sp.Complete {
		t.Error("should not be complete")
	}
	if sp.SizeBytes != 100 {
		t.Errorf("size=%d", sp.SizeBytes)
	}
	// Critical: clients must only seek within [0, ContiguousBytes)
	if sp.ContiguousBytes > sp.SizeBytes {
		t.Error("contiguous exceeds file size")
	}
}

func TestStreamFileKey(t *testing.T) {
	su := &streamUploader{destPrefix: "users/tsk_001/"}
	if got := su.segmentKey(3, 12); got != "users/tsk_001/stream/f0003/seg_00012.bin" {
		t.Errorf("segmentKey=%s", got)
	}
	if got := su.indexKey(3); got != "users/tsk_001/stream/f0003/index.json" {
		t.Errorf("indexKey=%s", got)
	}
}
