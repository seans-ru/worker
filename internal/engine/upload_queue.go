package engine

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Geardung/seans-worker/internal/media"
	"github.com/Geardung/seans-worker/internal/qbit"
	"github.com/Geardung/seans-worker/internal/s3uploader"
)

// uploadItem is one file in the S3 upload queue.
type uploadItem struct {
	FileIndex int
	RelPath   string
	SizeBytes int64
	GroupRank int // 0 = first episode / movie; -1 = unranked leftovers
	Level     int // qBittorrent-style priority (media.FilePrio*)
	LocalPath string
}

func levelLabel(level int) string {
	switch level {
	case media.FilePrioMaximal:
		return "critical"
	case media.FilePrioHigh:
		return "high"
	default:
		return "normal"
	}
}

// buildUploadItems orders selected files for the upload queue:
// ranked groups first (first episode / movie first), and within a group
// sidecar media (external audio, subtitles) before videos (primary first).
// Selected files that belong to no ranked group go last.
//
// Files are uploaded strictly one at a time in this order (see uploadQueue).
func buildUploadItems(files []qbit.TorrentFile, cat *media.MediaCatalog, selected map[int]bool, stagingRoot string) []uploadItem {
	byIndex := make(map[int]qbit.TorrentFile, len(files))
	for _, f := range files {
		byIndex[f.Index] = f
	}

	newItem := func(idx, rank, level int, seen map[int]bool) (uploadItem, bool) {
		if !selected[idx] || seen[idx] {
			return uploadItem{}, false
		}
		f, ok := byIndex[idx]
		if !ok {
			return uploadItem{}, false
		}
		seen[idx] = true
		return uploadItem{
			FileIndex: idx,
			RelPath:   f.Name,
			SizeBytes: f.Size,
			GroupRank: rank,
			Level:     level,
			LocalPath: filepath.Join(stagingRoot, filepath.FromSlash(f.Name)),
		}, true
	}

	prio := cat.DownloadPriorities()
	items := make([]uploadItem, 0, len(selected))
	seen := make(map[int]bool, len(selected))

	// Ranks count only groups that actually contribute selected files, so the
	// first downloaded episode is critical even when earlier episodes were
	// filtered out by task file_paths.
	rank := -1
	for _, g := range cat.RankedGroups() {
		var order []int
		for _, a := range g.ExternalAudio {
			order = append(order, a.Index)
		}
		for _, s := range g.Subtitles {
			order = append(order, s.Index)
		}
		if g.Primary != nil {
			order = append(order, g.Primary.Index)
		}
		for _, v := range g.Videos {
			order = append(order, v.Index)
		}
		contributes := false
		for _, idx := range order {
			if selected[idx] {
				contributes = true
				break
			}
		}
		if !contributes {
			continue
		}
		rank++
		level := media.GroupLevel(rank)
		for _, idx := range order {
			if it, ok := newItem(idx, rank, level, seen); ok {
				items = append(items, it)
			}
		}
	}

	var rest []int
	for idx := range selected {
		if !seen[idx] {
			rest = append(rest, idx)
		}
	}
	sort.Ints(rest)
	for _, idx := range rest {
		if it, ok := newItem(idx, -1, prio[idx], seen); ok {
			items = append(items, it)
		}
	}
	return items
}

// uploadQueue uploads files to S3 strictly one at a time ("по очереди"),
// in the order of the item list, and only once a file is fully downloaded.
// It runs in the background from the download phase on, so small media files
// go up in parallel with progressive segment uploads.
type uploadQueue struct {
	uploader   *s3uploader.Uploader
	destPrefix string // e.g. "users/{task_id}/"
	logFile    string
	logger     *slog.Logger
	state      *TaskState

	items []uploadItem
	done  []bool
	ready map[int]bool

	mu            sync.Mutex
	uploadedBytes int64
	uploadedItems int
	startedAt     time.Time
	finishedAt    time.Time

	wake chan struct{}
	// onUploaded, if set, is called after each successfully uploaded item.
	onUploaded func(uploadItem)
}

func newUploadQueue(up *s3uploader.Uploader, destPrefix, logFile string, items []uploadItem, state *TaskState, logger *slog.Logger) *uploadQueue {
	if logger == nil {
		logger = slog.Default()
	}
	return &uploadQueue{
		uploader:   up,
		destPrefix: destPrefix,
		logFile:    logFile,
		logger:     logger,
		state:      state,
		items:      items,
		done:       make([]bool, len(items)),
		ready:      make(map[int]bool, len(items)),
		wake:       make(chan struct{}, 1),
	}
}

// SetReady marks files as fully downloaded so their queue items may upload.
func (q *uploadQueue) SetReady(fileIndices []int) {
	q.mu.Lock()
	changed := false
	for _, idx := range fileIndices {
		if !q.ready[idx] {
			q.ready[idx] = true
			changed = true
		}
	}
	q.mu.Unlock()
	if changed {
		select {
		case q.wake <- struct{}{}:
		default:
		}
	}
}

// MarkAllReady marks every queued item ready (used when the torrent is done).
func (q *uploadQueue) MarkAllReady() {
	idxs := make([]int, 0, len(q.items))
	for _, it := range q.items {
		idxs = append(idxs, it.FileIndex)
	}
	q.SetReady(idxs)
}

func (q *uploadQueue) totalBytes() int64 {
	var n int64
	for _, it := range q.items {
		n += it.SizeBytes
	}
	return n
}

func (q *uploadQueue) finished() bool {
	for _, d := range q.done {
		if !d {
			return false
		}
	}
	return true
}

// pickNext returns the next not-done item in priority order whose file is
// already fully downloaded. Not-yet-downloaded items do not block later ones.
func (q *uploadQueue) pickNext() (*uploadItem, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.done[i] || !q.ready[q.items[i].FileIndex] {
			continue
		}
		return &q.items[i], i + 1
	}
	return nil, 0
}

// run drains the queue. Returns when all items are uploaded or ctx is done.
func (q *uploadQueue) run(ctx context.Context) error {
	q.logger.Info("upload: queue ready", "items", len(q.items), "total_bytes", q.totalBytes())
	for {
		item, pos := q.pickNext()
		if item == nil {
			if q.finished() {
				q.logger.Info("upload: queue drained",
					"items", len(q.items),
					"bytes", q.totalBytes(),
					"took_sec", q.Seconds(),
				)
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-q.wake:
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if err := q.uploadOne(ctx, *item, pos); err != nil {
			return err
		}
		if q.onUploaded != nil {
			q.onUploaded(*item)
		}
	}
}

func (q *uploadQueue) uploadOne(ctx context.Context, item uploadItem, pos int) error {
	q.mu.Lock()
	if q.startedAt.IsZero() {
		q.startedAt = time.Now()
	}
	base := q.uploadedBytes
	total := q.totalBytes()
	q.mu.Unlock()

	q.logger.Info("upload: start",
		"item", fmt.Sprintf("%d/%d", pos, len(q.items)),
		"path", item.RelPath,
		"size_bytes", item.SizeBytes,
		"priority", levelLabel(item.Level),
		"source", item.LocalPath,
	)

	destKey := q.destPrefix + filepath.ToSlash(item.RelPath)
	cb := func(st s3uploader.TransferStats) {
		transferred := base + st.TransferredBytes
		q.state.Message = fmt.Sprintf("uploading %d/%d: %s (%.1f%%)", pos, len(q.items), item.RelPath, st.Percent)
		if q.state.Stage == StageUploading {
			if total > 0 {
				q.state.ProgressPct = float64(transferred) / float64(total) * 100
			}
			q.state.SpeedMbps = float64(st.SpeedBps) / (1024 * 1024)
			if st.SpeedBps > 0 && total > transferred {
				q.state.ETAMin = int(math.Ceil(float64(total-transferred) / float64(st.SpeedBps) / 60))
			}
		}
		q.logger.Info("upload: progress",
			"path", item.RelPath,
			"pct", fmt.Sprintf("%.1f", st.Percent),
			"transferred", fmt.Sprintf("%d/%d", transferred, total),
			"speed_mibs", fmt.Sprintf("%.2f", float64(st.SpeedBps)/(1024*1024)),
			"eta_sec", st.ETASeconds,
		)
	}

	start := time.Now()
	const maxAttempts = 3
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = q.uploader.UploadFileWithStats(ctx, item.LocalPath, destKey, q.logFile, cb)
		if err == nil {
			break
		}
		q.logger.Warn("upload: attempt failed",
			"path", item.RelPath, "attempt", attempt, "max", maxAttempts, "error", err)
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 5 * time.Second):
			}
		}
	}
	if err != nil {
		return fmt.Errorf("upload %s: %w", item.RelPath, err)
	}

	took := time.Since(start)
	q.mu.Lock()
	q.uploadedBytes += item.SizeBytes
	q.uploadedItems++
	for i := range q.items {
		if q.items[i].FileIndex == item.FileIndex {
			q.done[i] = true
			break
		}
	}
	upItems := q.uploadedItems
	if q.uploadedItems == len(q.items) {
		q.finishedAt = time.Now()
	}
	q.mu.Unlock()

	avgBps := float64(item.SizeBytes) / math.Max(took.Seconds(), 0.001)
	q.logger.Info("upload: done",
		"path", item.RelPath,
		"took_sec", int(took.Seconds()),
		"avg_speed_mibs", fmt.Sprintf("%.2f", avgBps/(1024*1024)),
		"uploaded_items", fmt.Sprintf("%d/%d", upItems, len(q.items)),
	)
	return nil
}

// Seconds returns how long the queue ran (first item start -> last item done).
func (q *uploadQueue) Seconds() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.startedAt.IsZero() {
		return 0
	}
	end := q.finishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return int(end.Sub(q.startedAt).Seconds())
}
