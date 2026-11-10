package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Geardung/seans-worker/internal/media"
	"github.com/Geardung/seans-worker/internal/qbit"
)

func TestBuildUploadItemsOrder(t *testing.T) {
	files := []qbit.TorrentFile{
		{Index: 0, Name: "Show/Season 1/Show.S01E01.mkv", Size: 1000},
		{Index: 1, Name: "Show/Season 1/Show.S01E01.rus.srt", Size: 10},
		{Index: 2, Name: "Show/Season 1/Show.S01E02.mkv", Size: 1000},
		{Index: 3, Name: "Show/Season 1/Show.S01E03.mkv", Size: 1000},
		{Index: 4, Name: "Show/Season 1/Show.S01E03.rus.dub.ac3", Size: 100},
	}
	paths := make([]string, len(files))
	sizes := make([]int64, len(files))
	for i, f := range files {
		paths[i] = f.Name
		sizes[i] = f.Size
	}
	cat := media.Classify(paths, sizes, nil)
	selected := map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true}

	items := buildUploadItems(files, cat, selected, "/staging/tsk_001")

	wantOrder := []int{1, 0, 2, 4, 3} // ep1 media, ep1 video, ep2 video, ep3 media, ep3 video
	wantLevels := []int{
		media.FilePrioMaximal, media.FilePrioMaximal,
		media.FilePrioHigh,
		media.FilePrioNormal, media.FilePrioNormal,
	}
	wantRanks := []int{0, 0, 1, 2, 2}

	if len(items) != len(wantOrder) {
		t.Fatalf("items=%d want %d", len(items), len(wantOrder))
	}
	for i, it := range items {
		if it.FileIndex != wantOrder[i] {
			t.Errorf("item %d: fileIndex=%d want %d (%s)", i, it.FileIndex, wantOrder[i], it.RelPath)
		}
		if it.Level != wantLevels[i] {
			t.Errorf("item %d (%s): level=%d want %d", i, it.RelPath, it.Level, wantLevels[i])
		}
		if it.GroupRank != wantRanks[i] {
			t.Errorf("item %d (%s): rank=%d want %d", i, it.RelPath, it.GroupRank, wantRanks[i])
		}
	}
	if !strings.HasSuffix(items[0].LocalPath, filepath.FromSlash("Show/Season 1/Show.S01E01.rus.srt")) {
		t.Errorf("local path = %q", items[0].LocalPath)
	}
	if !strings.Contains(items[0].LocalPath, "tsk_001") {
		t.Errorf("local path = %q, want under staging root", items[0].LocalPath)
	}
}

func TestBuildUploadItemsUnselectedSkipped(t *testing.T) {
	files := []qbit.TorrentFile{
		{Index: 0, Name: "Show.S01E01.mkv", Size: 1000},
		{Index: 1, Name: "Show.S01E02.mkv", Size: 1000},
	}
	cat := media.Classify([]string{files[0].Name, files[1].Name}, []int64{1000, 1000}, nil)
	selected := map[int]bool{1: true}

	items := buildUploadItems(files, cat, selected, "/staging/x")
	if len(items) != 1 || items[0].FileIndex != 1 {
		t.Fatalf("items=%v want only file 1", items)
	}
	if items[0].Level != media.FilePrioMaximal {
		t.Errorf("level=%d want maximal (first selected group)", items[0].Level)
	}
}

func TestUploadQueuePickNextSkipsNotReady(t *testing.T) {
	items := []uploadItem{
		{FileIndex: 0, RelPath: "a.mkv", SizeBytes: 10},
		{FileIndex: 1, RelPath: "b.srt", SizeBytes: 10},
	}
	q := newUploadQueue(nil, "users/tsk/", "", items, &TaskState{}, testLogger())
	q.SetReady([]int{1}) // first item is not downloaded yet

	got, pos := q.pickNext()
	if got == nil || got.FileIndex != 1 || pos != 2 {
		t.Fatalf("pickNext=%+v pos=%d want file 1 at pos 2", got, pos)
	}

	q.done[1] = true
	if got, _ := q.pickNext(); got != nil {
		t.Fatalf("pickNext=%+v want nil (file 0 not ready)", got)
	}

	q.SetReady([]int{0})
	got, _ = q.pickNext()
	if got == nil || got.FileIndex != 0 {
		t.Fatalf("pickNext=%+v want file 0", got)
	}
}
