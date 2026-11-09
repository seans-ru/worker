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
	states := []stream.PieceState{
		stream.PieceDownloaded, stream.PieceDownloaded, stream.PieceMissing, stream.PieceMissing,
	}

	// Nothing uploaded yet: nothing is seekable on S3 even though 64 bytes
	// are downloaded locally.
	fp := stream.BuildProgress(layout, states, 32, map[int]string{})
	sp := progressFrom(fp, layout)
	if sp.ContiguousBytes != 0 {
		t.Errorf("contiguous=%d want 0 (no segments on S3 yet)", sp.ContiguousBytes)
	}
	if sp.Complete {
		t.Error("should not be complete")
	}
	if sp.SizeBytes != 100 {
		t.Errorf("size=%d want 100", sp.SizeBytes)
	}

	// First two segments uploaded -> seekable prefix 64.
	fp = stream.BuildProgress(layout, states, 32, map[int]string{0: "k0", 1: "k1"})
	sp = progressFrom(fp, layout)
	if sp.ContiguousBytes != 64 {
		t.Errorf("contiguous=%d want 64", sp.ContiguousBytes)
	}
	if sp.Complete {
		t.Error("should not be complete")
	}
	// Critical: clients must only seek within [0, ContiguousBytes)
	if sp.ContiguousBytes > sp.SizeBytes {
		t.Error("contiguous exceeds file size")
	}
	if !sp.StreamReady {
		t.Error("uploaded prefix should make the stream ready")
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
