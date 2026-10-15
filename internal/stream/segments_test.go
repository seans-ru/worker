package stream

import (
	"testing"
)

func TestBuildFileLayouts(t *testing.T) {
	// 3 files, piece size 16:
	// file0 size 20 -> pieces 0-1 (offset 0)
	// file1 size 10 -> piece 1 (offset 20, piece 1 covers 16-32)
	// file2 size 50 -> pieces 2-5 (offset 30)
	files := []FileEntry{
		{0, "a.mkv", 20},
		{1, "b.ac3", 10},
		{2, "c.mkv", 50},
	}
	layouts := BuildFileLayouts(files, 16)
	if len(layouts) != 3 {
		t.Fatalf("layouts=%d", len(layouts))
	}
	if layouts[0].PieceStart != 0 || layouts[0].PieceEnd != 1 {
		t.Errorf("file0 pieces=%d-%d", layouts[0].PieceStart, layouts[0].PieceEnd)
	}
	if layouts[1].Offset != 20 || layouts[1].PieceStart != 1 || layouts[1].PieceEnd != 1 {
		t.Errorf("file1=%+v", layouts[1])
	}
	if layouts[2].Offset != 30 || layouts[2].PieceStart != 1 || layouts[2].PieceEnd != 5 {
		// 30/16 = 1, (30+50-1)/16 = 79/16 = 4 -> wait 30+49=79, 79/16=4
		t.Logf("file2=%+v", layouts[2])
		if layouts[2].PieceStart != 1 {
			t.Errorf("file2 start piece=%d", layouts[2].PieceStart)
		}
		if layouts[2].PieceEnd != 4 {
			t.Errorf("file2 end piece=%d want 4", layouts[2].PieceEnd)
		}
	}
}

func TestContiguousBytes(t *testing.T) {
	l := FileLayout{
		Index: 0, Path: "a.bin", Size: 100,
		Offset: 0, PieceStart: 0, PieceEnd: 3, PieceSize: 32,
	}
	// pieces 0..3 cover 0-128, file 0-100

	states := []PieceState{PieceDownloaded, PieceDownloaded, PieceMissing, PieceDownloaded}
	// pieces 0,1 = 64 bytes contiguous
	if got := ContiguousBytes(l, states); got != 64 {
		t.Errorf("contiguous=%d want 64", got)
	}

	// all downloaded -> full size
	all := []PieceState{PieceDownloaded, PieceDownloaded, PieceDownloaded, PieceDownloaded}
	if got := ContiguousBytes(l, all); got != 100 {
		t.Errorf("full contiguous=%d want 100", got)
	}

	// only last piece -> 0 contiguous (hole at start)
	hole := []PieceState{PieceMissing, PieceMissing, PieceMissing, PieceDownloaded}
	if got := ContiguousBytes(l, hole); got != 0 {
		t.Errorf("hole contiguous=%d want 0", got)
	}

	// piece downloading is not available
	partial := []PieceState{PieceDownloaded, PieceDownloading, PieceDownloaded, PieceDownloaded}
	if got := ContiguousBytes(l, partial); got != 32 {
		t.Errorf("partial contiguous=%d want 32", got)
	}
}

func TestContiguousBytesOffsetFile(t *testing.T) {
	// file starts mid-piece at offset 10, size 50, piece size 16
	// pieces: 0 (0-16), 1 (16-32), 2 (32-48), 3 (48-64), 4 (64-80)
	// file range abs 10-60
	l := FileLayout{
		Index: 1, Path: "b.bin", Size: 50,
		Offset: 10, PieceStart: 0, PieceEnd: 3, PieceSize: 16,
	}
	states := []PieceState{PieceDownloaded, PieceDownloaded, PieceDownloaded, PieceMissing}
	// pieces 0-2 cover abs 0-48, file bytes 0 to 48-10=38
	if got := ContiguousBytes(l, states); got != 38 {
		t.Errorf("contiguous=%d want 38", got)
	}
}

func TestDownloadedBytes(t *testing.T) {
	l := FileLayout{
		Index: 0, Path: "a.bin", Size: 100,
		Offset: 0, PieceStart: 0, PieceEnd: 3, PieceSize: 32,
	}
	states := []PieceState{PieceDownloaded, PieceMissing, PieceDownloaded, PieceDownloaded}
	// piece0=32 + piece2=32 + piece3=32 but file only 100 so piece3 contributes 4
	// 32+32+32=96, piece3 is bytes 96-128 -> file 96-100 = 4. Total 32+32+4=68? wait
	// piece2 is 64-96 = 32 bytes. piece3 96-128 intersect 0-100 = 96-100 = 4.
	// total = 32 + 32 + 4 = 68
	if got := DownloadedBytes(l, states); got != 68 {
		t.Errorf("downloaded=%d want 68", got)
	}
}

func TestSegmentAvailableAndProgress(t *testing.T) {
	l := FileLayout{
		Index: 0, Path: "movie.mkv", Size: 25,
		Offset: 0, PieceStart: 0, PieceEnd: 2, PieceSize: 10,
	}
	// segments of 10: [0-10), [10-20), [20-25)
	states := []PieceState{PieceDownloaded, PieceDownloaded, PieceMissing}
	segs := SplitSegments(25, 10)
	if len(segs) != 3 {
		t.Fatalf("segs=%d", len(segs))
	}
	if !SegmentAvailable(l, segs[0], states) || !SegmentAvailable(l, segs[1], states) {
		t.Error("first two segments should be available")
	}
	if SegmentAvailable(l, segs[2], states) {
		t.Error("third segment should not be available")
	}

	uploaded := map[int]string{0: "seg/0"}
	fp := BuildProgress(l, states, 10, uploaded)
	if fp.ContiguousBytes != 20 {
		t.Errorf("contiguous=%d", fp.ContiguousBytes)
	}
	if fp.PercentSeekable != 80 {
		t.Errorf("pctSeek=%v", fp.PercentSeekable)
	}
	if fp.Complete {
		t.Error("should not be complete")
	}
	next := NextUploadable(fp)
	if len(next) != 1 || next[0].Index != 1 {
		t.Errorf("next=%v want only seg 1", next)
	}
}

func TestSplitSegmentsEdge(t *testing.T) {
	if SplitSegments(0, 10) != nil {
		t.Error("empty size")
	}
	if SplitSegments(10, 0) != nil {
		t.Error("zero segment")
	}
	segs := SplitSegments(10, 10)
	if len(segs) != 1 || segs[0].End != 10 {
		t.Errorf("segs=%v", segs)
	}
}
