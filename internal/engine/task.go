package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Geardung/seans-worker/internal/backend"
	"github.com/Geardung/seans-worker/internal/config"
	"github.com/Geardung/seans-worker/internal/media"
	"github.com/Geardung/seans-worker/internal/qbit"
	"github.com/Geardung/seans-worker/internal/s3uploader"
	"github.com/Geardung/seans-worker/internal/stream"
)

type Stage string

const (
	StagePreparing   Stage = "preparing"
	StageDownloading Stage = "downloading"
	StageUploading   Stage = "uploading"
	StageDone        Stage = "done"
)

type TaskState struct {
	TaskID          string
	Stage           Stage
	ProgressPct     float64
	SpeedMbps       float64
	ETAMin          int
	Message         string
	TorrentHash     string
	Files           []qbit.TorrentFile
	StartedAt       time.Time
	DownloadStart   time.Time
	UploadStart     time.Time
	DownloadSeconds int
	UploadSeconds   int

	// Stream holds progressive download/seek window for the primary video file.
	Stream *backend.StreamProgress
	// StreamFileIndex is the torrent file index of the primary streamable video.
	StreamFileIndex int
	// SegmentSize for progressive S3 upload (0 = default).
	SegmentSize int64
	// Selected holds torrent file indices the task will download/upload
	// (honors claim file_paths; empty selection with a non-empty filter fails).
	Selected map[int]bool
}

type taskRunner struct {
	cfg      *config.Config
	bc       *backend.Client
	qc       *qbit.Client
	uploader *s3uploader.Uploader
	task     *backend.Task
	state    *TaskState
	logger   *slog.Logger

	// items is the ordered S3 upload queue content (built in prepare).
	items []uploadItem
	// queue uploads files one at a time, starting in the download phase.
	queue *uploadQueue
	// queueErr / segErr receive the results of the background workers.
	queueErr chan error
	segErr   chan error
	// filmOnS3 is closed when the primary video is fully uploaded to S3.
	filmOnS3 chan struct{}
	filmOnce sync.Once
}

func newTaskRunner(cfg *config.Config, bc *backend.Client, qc *qbit.Client, up *s3uploader.Uploader, task *backend.Task, state *TaskState, logger *slog.Logger) *taskRunner {
	return &taskRunner{
		cfg:      cfg,
		bc:       bc,
		qc:       qc,
		uploader: up,
		task:     task,
		state:    state,
		logger:   logger.With("task_id", task.TaskID),
	}
}

func (tr *taskRunner) run(ctx context.Context) {
	// Per-task context so background workers (upload queue, segment loop)
	// stop when the task ends.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tr.logger.Info("task started", "stage", "preparing")

	if err := tr.prepare(ctx); err != nil {
		tr.failTask(ctx, err)
		return
	}
	if err := tr.download(ctx); err != nil {
		tr.failTask(ctx, err)
		return
	}
	if err := tr.upload(ctx); err != nil {
		tr.failTask(ctx, err)
		return
	}
	tr.complete(ctx)
}

func (tr *taskRunner) prepare(ctx context.Context) error {
	tr.state.Stage = StagePreparing
	tr.state.Message = "preparing task environment"

	taskID := tr.task.TaskID
	stagingPath := filepath.Join(tr.cfg.StagingDir, taskID)
	savePath := filepath.Join(tr.cfg.QBitDownloads, taskID)

	os.RemoveAll(stagingPath)

	existing, err := tr.qc.GetTorrentBySavePath(ctx, "seans", savePath)
	if err != nil {
		return fmt.Errorf("check existing torrents: %w", err)
	}
	if existing != nil {
		tr.logger.Info("removing leftover torrent", "hash", existing.Hash)
		if delErr := tr.qc.DeleteTorrent(ctx, existing.Hash, true); delErr != nil {
			tr.logger.Warn("failed to delete leftover torrent", "error", delErr)
		}
	}

	if err := tr.qc.AddTorrentMagnet(ctx, tr.task.Magnet, savePath, "seans"); err != nil {
		return fmt.Errorf("add magnet: %w", err)
	}

	torrent, err := tr.waitForTorrent(ctx, savePath)
	if err != nil {
		return err
	}
	tr.state.TorrentHash = torrent.Hash

	if torrent.State == "metaDL" {
		tr.logger.Info("waiting for torrent metadata", "timeout", tr.cfg.TorrentMetadataTimeout)
		if err := tr.waitForMetadata(ctx, torrent.Hash); err != nil {
			return err
		}
	}

	files, err := tr.qc.GetFiles(ctx, torrent.Hash)
	if err != nil {
		return fmt.Errorf("get files: %w", err)
	}
	tr.state.Files = files

	// Classify media (series episodes, external audio, subtitles) and pick stream target.
	paths := make([]string, len(files))
	sizes := make([]int64, len(files))
	for i, f := range files {
		paths[i] = f.Name
		sizes[i] = f.Size
	}
	cat := media.Classify(paths, sizes, tr.task.FilePaths)
	selected, primary := cat.SelectDownloadPriorities(false)

	// Non-empty file_paths must match at least one torrent file; otherwise the
	// task cannot honor the request (do not fall back to the full torrent).
	if media.HasPathFilter(tr.task.FilePaths) && len(selected) == 0 {
		return &TaskError{
			Reason:    "no files matched task.file_paths",
			Permanent: true,
		}
	}
	tr.state.Selected = make(map[int]bool, len(selected))
	for _, idx := range selected {
		tr.state.Selected[idx] = true
	}

	// Upload queue: selected files go up to S3 one at a time, in episode
	// priority order (first episode first, media before video in each group).
	tr.items = buildUploadItems(files, cat, tr.state.Selected, stagingPath)

	// Download priorities by episode order: first episode (or movie) is
	// critical, second is high, the rest are normal; samples stay off.
	prioMap := cat.DownloadPriorities()
	byPrio := make(map[int][]int)
	for idx, p := range prioMap {
		byPrio[p] = append(byPrio[p], idx)
	}
	for _, level := range []int{media.FilePrioMaximal, media.FilePrioHigh, media.FilePrioNormal, media.FilePrioSkip} {
		idxs := byPrio[level]
		if len(idxs) == 0 {
			continue
		}
		if err := tr.qc.SetFilePriority(ctx, torrent.Hash, idxs, level); err != nil {
			tr.logger.Warn("set file priority failed", "priority", level, "error", err)
		}
	}
	tr.logger.Info("download priorities set",
		"critical", len(byPrio[media.FilePrioMaximal]),
		"high", len(byPrio[media.FilePrioHigh]),
		"normal", len(byPrio[media.FilePrioNormal]),
		"skip", len(byPrio[media.FilePrioSkip]),
	)

	// Sequential download so contiguous prefix grows — required for seekable streaming.
	if err := tr.qc.EnableSequentialDownload(ctx, torrent.Hash); err != nil {
		tr.logger.Warn("enable sequential download failed", "error", err)
	}
	if err := tr.qc.EnableFirstLastPiecePrio(ctx, torrent.Hash); err != nil {
		tr.logger.Warn("enable first/last piece prio failed", "error", err)
	}

	// Pick primary video for progressive stream upload.
	if len(primary) > 0 {
		tr.state.StreamFileIndex = primary[0]
	} else if g := cat.FirstPlayable(); g != nil && g.Primary != nil {
		tr.state.StreamFileIndex = g.Primary.Index
	} else {
		// Fallback: largest video file.
		best := -1
		var bestSize int64
		for _, f := range files {
			m := media.ParsePath(f.Name, f.Size, f.Index)
			if m.Kind == media.KindVideo && f.Size > bestSize {
				best, bestSize = f.Index, f.Size
			}
		}
		tr.state.StreamFileIndex = best
	}
	tr.state.SegmentSize = tr.cfg.StreamSegmentSize

	// Send manifest to backend and get upload slots. Only report files this
	// task will actually download/upload (claim file_paths / media selection).
	manifestFiles := make([]backend.ManifestFile, 0, len(selected))
	for _, f := range files {
		if !tr.state.Selected[f.Index] {
			continue
		}
		m := media.ParsePath(f.Name, f.Size, f.Index)
		kind := string(m.Kind)
		lang := string(m.Language)
		isPrimary := m.Index == tr.state.StreamFileIndex && m.Kind == media.KindVideo
		mf := backend.ManifestFile{
			Path:       f.Name,
			SizeBytes:  f.Size,
			Kind:       kind,
			Season:     m.Season,
			Episode:    m.Episode,
			EpisodeEnd: m.EpisodeEnd,
			Language:   lang,
			Flags:      m.Flags,
			Primary:    isPrimary,
		}
		manifestFiles = append(manifestFiles, mf)
	}

	catalog := map[string]any{
		"groups": describeGroups(cat),
	}
	if _, err := tr.bc.Manifest(ctx, tr.task.TaskID, manifestFiles, catalog); err != nil {
		return fmt.Errorf("send manifest: %w", err)
	}

	tr.logger.Info("preparing complete",
		"files", len(files),
		"stream_file_index", tr.state.StreamFileIndex,
		"groups", len(cat.Groups),
		"upload_items", len(tr.items),
	)
	return nil
}

func describeGroups(cat *media.MediaCatalog) []map[string]any {
	out := make([]map[string]any, 0, len(cat.Groups))
	for _, g := range cat.Groups {
		item := map[string]any{
			"season":  g.Key.Season,
			"episode": g.Key.Episode,
			"year":    g.Key.MovieYear,
		}
		if g.Primary != nil {
			item["primary_path"] = g.Primary.Path
			item["primary_size"] = g.Primary.Size
			item["primary_index"] = g.Primary.Index
		}
		var audios []map[string]any
		for _, a := range g.ExternalAudio {
			audios = append(audios, map[string]any{
				"path": a.Path, "language": string(a.Language), "flags": a.Flags,
			})
		}
		var subs []map[string]any
		for _, s := range g.Subtitles {
			subs = append(subs, map[string]any{
				"path": s.Path, "language": string(s.Language),
			})
		}
		item["external_audio"] = audios
		item["subtitles"] = subs
		out = append(out, item)
	}
	return out
}

func (tr *taskRunner) waitForTorrent(ctx context.Context, savePath string) (*qbit.TorrentInfo, error) {
	deadline := time.After(2 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("torrent did not appear within 2 minutes")
		case <-ticker.C:
			torrent, err := tr.qc.GetTorrentBySavePath(ctx, "seans", savePath)
			if err != nil {
				return nil, err
			}
			if torrent != nil {
				return torrent, nil
			}
		}
	}
}

func (tr *taskRunner) waitForMetadata(ctx context.Context, hash string) error {
	deadline := time.After(tr.cfg.TorrentMetadataTimeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return &TaskError{Reason: "metadata timeout: torrent files not available", Permanent: false}
		case <-ticker.C:
			files, err := tr.qc.GetFiles(ctx, hash)
			if err != nil {
				continue
			}
			if len(files) > 0 {
				return nil
			}
		}
	}
}

// signalFilmOnS3 stops the segment loop (the film is fully on S3 now).
func (tr *taskRunner) signalFilmOnS3() {
	tr.filmOnce.Do(func() {
		if tr.filmOnS3 != nil {
			close(tr.filmOnS3)
		}
	})
}

func (tr *taskRunner) download(ctx context.Context) error {
	tr.state.Stage = StageDownloading
	tr.state.DownloadStart = time.Now()
	tr.state.Message = "downloading torrent"
	tr.logger.Info("download started",
		"stream_file_index", tr.state.StreamFileIndex,
		"segment_size", tr.cfg.StreamSegmentSize,
	)

	// Background workers start right away:
	//   - uploadQueue: files go up one at a time ("по очереди") in priority
	//     order as soon as each file is fully downloaded — in parallel with
	//     segment uploads;
	//   - segmentLoop: progressive segment upload of the primary video, which
	//     keeps running until the film is fully on S3.
	destPrefix := "users/" + tr.task.TaskID + "/"
	logFile := filepath.Join(tr.cfg.StateDir, "rclone.log")
	su := newStreamUploader(tr.uploader, destPrefix, tr.cfg.StreamSegmentSize, logFile, tr.logger)
	tr.queue = newUploadQueue(tr.uploader, destPrefix, logFile, tr.items, tr.state, tr.logger)
	tr.filmOnS3 = make(chan struct{})
	tr.queueErr = make(chan error, 1)
	tr.segErr = make(chan error, 1)

	tr.queue.onUploaded = func(it uploadItem) {
		if it.FileIndex == tr.state.StreamFileIndex {
			tr.signalFilmOnS3()
		}
	}

	streamEnabled := tr.state.StreamFileIndex >= 0 && tr.state.StreamFileIndex < len(tr.state.Files)
	if streamEnabled {
		go func() { tr.segErr <- tr.segmentLoop(ctx, su) }()
	} else {
		tr.logger.Warn("stream upload disabled: no primary video file selected")
		tr.segErr <- nil
	}

	go func() {
		err := tr.queue.run(ctx)
		if err == nil {
			// Make sure the segment loop always gets its stop signal.
			tr.signalFilmOnS3()
		}
		tr.queueErr <- err
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastDownloaded int64
	lastProgressTime := time.Now()
	taskDeadline := time.After(tr.cfg.TaskMaxDuration)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-taskDeadline:
			return &TaskError{Reason: "task timeout exceeded", Permanent: false}
		case <-ticker.C:
			torrent, err := tr.qc.GetTorrentBySavePath(ctx, "seans", filepath.Join(tr.cfg.QBitDownloads, tr.task.TaskID))
			if err != nil {
				tr.logger.Warn("poll error", "error", err)
				continue
			}
			if torrent == nil {
				return fmt.Errorf("torrent disappeared")
			}

			tr.state.ProgressPct = torrent.Progress * 100
			tr.state.SpeedMbps = float64(torrent.DlSpeed) / (1024 * 1024)

			if torrent.Size > 0 && torrent.DlSpeed > 0 {
				remaining := float64(torrent.Size-int64(float64(torrent.Size)*torrent.Progress)) / float64(torrent.DlSpeed)
				tr.state.ETAMin = int(math.Ceil(remaining / 60))
			}

			if torrent.Downloaded > lastDownloaded {
				lastDownloaded = torrent.Downloaded
				lastProgressTime = time.Now()
			}
			if time.Since(lastProgressTime) > tr.cfg.DownloadStallTimeout {
				return &TaskError{Reason: "stalled: no download progress", Permanent: true}
			}

			// Feed the upload queue with fully downloaded files.
			if qfiles, ferr := tr.qc.GetFiles(ctx, torrent.Hash); ferr == nil {
				ready := make([]int, 0, len(qfiles))
				for _, f := range qfiles {
					if tr.state.Selected[f.Index] && f.Progress >= 1.0 {
						ready = append(ready, f.Index)
					}
				}
				tr.queue.SetReady(ready)
			}

			downloadDone := torrent.Progress >= 1.0 && !isActiveState(string(torrent.State))
			if downloadDone {
				tr.state.ProgressPct = 100
				tr.state.DownloadSeconds = int(time.Since(tr.state.DownloadStart).Seconds())
				tr.queue.MarkAllReady()
				tr.logger.Info("download complete", "seconds", tr.state.DownloadSeconds)
				return nil
			}
		}
	}
}

// segmentLoop uploads progressive segments of the primary video to S3 while
// the torrent downloads AND while the full film object is still uploading.
// It stops only when the film is fully on S3 (filmOnS3), then marks the
// stream index complete.
func (tr *taskRunner) segmentLoop(ctx context.Context, su *streamUploader) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var layout *stream.FileLayout
	var layouts []stream.FileLayout
	var streamReadyLogged bool
	var lastContiguous int64
	var lastSegUploads int

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tr.filmOnS3:
			// The film is fully on S3 — no more segment uploads needed.
			if layout != nil {
				states, serr := tr.qc.GetPieceStates(ctx, tr.state.TorrentHash)
				if serr == nil {
					if sp, ferr := su.Finalize(ctx, tr.task.TaskID, *layout, toPieceStates(states)); ferr == nil && sp != nil {
						tr.state.Stream = sp
					} else if ferr != nil {
						tr.logger.Warn("stream: finalize failed", "error", ferr)
					}
				} else if tr.state.Stream != nil {
					tr.state.Stream.Complete = true
					tr.state.Stream.ContiguousBytes = tr.state.Stream.SizeBytes
					tr.state.Stream.PercentSeekable = 100
					tr.state.Stream.PercentDownloaded = 100
				}
			}
			tr.logger.Info("stream: segment uploads stopped", "reason", "film fully on S3")
			return nil
		case <-ticker.C:
			if tr.state.StreamFileIndex < 0 || tr.state.StreamFileIndex >= len(tr.state.Files) {
				continue
			}
			if layouts == nil {
				pieceSize, perr := tr.qc.GetPieceSize(ctx, tr.state.TorrentHash)
				if perr != nil {
					tr.logger.Warn("stream: get piece size failed", "error", perr)
				} else if pieceSize <= 0 {
					tr.logger.Warn("stream: invalid piece size", "piece_size", pieceSize)
				} else {
					layouts = fileLayoutsFromQBit(tr.state.Files, pieceSize)
					for i := range layouts {
						if layouts[i].Index == tr.state.StreamFileIndex {
							layout = &layouts[i]
							break
						}
					}
					if layout == nil {
						tr.logger.Warn("stream: no layout for stream file",
							"stream_file_index", tr.state.StreamFileIndex, "layouts", len(layouts))
					} else {
						tr.logger.Info("stream: layout ready",
							"file", layout.Path,
							"size_bytes", layout.Size,
							"piece_size", layout.PieceSize,
							"piece_range", fmt.Sprintf("%d-%d", layout.PieceStart, layout.PieceEnd),
							"local_path", filepath.Join(tr.cfg.StagingDir, tr.task.TaskID, filepath.FromSlash(layout.Path)),
						)
					}
				}
			}
			if layout == nil {
				continue
			}
			states, serr := tr.qc.GetPieceStates(ctx, tr.state.TorrentHash)
			if serr != nil {
				tr.logger.Warn("stream: get piece states failed", "error", serr)
				continue
			}
			localPath := filepath.Join(tr.cfg.StagingDir, tr.task.TaskID, filepath.FromSlash(layout.Path))
			sp, uerr := su.UploadPending(ctx, tr.task.TaskID, *layout, localPath, toPieceStates(states))
			if uerr != nil {
				tr.logger.Warn("stream: segment upload failed", "error", uerr)
			}
			if sp != nil {
				tr.state.Stream = sp
				uploadsNow := sp.SegmentsUploaded
				if uploadsNow > lastSegUploads || sp.ContiguousBytes > lastContiguous {
					tr.logger.Info("stream: progress",
						"file", sp.FilePath,
						"segments_uploaded", sp.SegmentsUploaded,
						"segments_available", sp.SegmentsAvailable,
						"segments_total", sp.SegmentsTotal,
						"contiguous_bytes", sp.ContiguousBytes,
						"percent_seekable", fmt.Sprintf("%.2f", sp.PercentSeekable),
						"percent_downloaded", fmt.Sprintf("%.2f", sp.PercentDownloaded),
						"stream_ready", sp.StreamReady,
					)
					lastSegUploads = uploadsNow
					lastContiguous = sp.ContiguousBytes
				}
				if sp.StreamReady && !streamReadyLogged {
					tr.logger.Info("stream: ready for playback",
						"contiguous_bytes", sp.ContiguousBytes,
						"max_seekable_bytes", sp.ContiguousBytes,
					)
					streamReadyLogged = true
				}
			}
		}
	}
}

func isActiveState(state string) bool {
	switch state {
	case "downloading", "forcedDL", "stalledDL", "checkingDL", "queuedDL", "metaDL":
		return true
	default:
		return false
	}
}

func (tr *taskRunner) upload(ctx context.Context) error {
	tr.state.Stage = StageUploading
	tr.state.UploadStart = time.Now()
	tr.state.Message = "uploading to S3"
	tr.logger.Info("upload started",
		"items", len(tr.items),
		"bytes", tr.queue.totalBytes(),
	)

	// Files have been uploading since the download phase — the queue processes
	// them one at a time, in episode priority order. Wait until it drains.
	if err := <-tr.queueErr; err != nil {
		return &TaskError{Reason: fmt.Sprintf("upload failed: %v", err), Permanent: false}
	}
	// The segment loop exits right after the film lands on S3 (finalize).
	if err := <-tr.segErr; err != nil && !errors.Is(err, context.Canceled) {
		tr.logger.Warn("segment loop ended with error", "error", err)
	}

	tr.state.UploadSeconds = tr.queue.Seconds()
	tr.logger.Info("upload complete",
		"seconds", tr.state.UploadSeconds,
		"files", len(tr.items),
		"bytes", tr.queue.totalBytes(),
	)

	stagingPath := filepath.Join(tr.cfg.StagingDir, tr.task.TaskID)
	os.RemoveAll(stagingPath)

	if tr.state.TorrentHash != "" {
		if err := tr.qc.DeleteTorrent(ctx, tr.state.TorrentHash, true); err != nil {
			tr.logger.Warn("failed to delete torrent after upload", "error", err)
		}
	}

	return nil
}

func (tr *taskRunner) complete(ctx context.Context) {
	files := make([]backend.FileInfo, 0)
	for _, f := range tr.state.Files {
		// Only files this task selected (file_paths / media pick) were downloaded.
		if tr.state.Selected != nil && !tr.state.Selected[f.Index] {
			continue
		}
		files = append(files, backend.FileInfo{
			Path:      f.Name,
			SizeBytes: f.Size,
		})
	}

	if err := tr.bc.Complete(ctx, tr.task.TaskID, files); err != nil {
		tr.logger.Error("failed to report completion", "error", err)
		return
	}

	tr.state.Stage = StageDone
	tr.state.Message = "task completed"
	tr.logger.Info("task completed",
		"download_sec", tr.state.DownloadSeconds,
		"upload_sec", tr.state.UploadSeconds,
		"files", len(files),
	)
}

func (tr *taskRunner) failTask(ctx context.Context, err error) {
	var taskErr *TaskError
	reason := err.Error()

	if errors.As(err, &taskErr) {
		reason = taskErr.Reason
	}

	tr.logger.Error("task failed", "reason", reason)
	if reportErr := tr.bc.Fail(ctx, tr.task.TaskID, reason); reportErr != nil {
		tr.logger.Error("failed to report failure", "error", reportErr)
	}
}

type TaskError struct {
	Reason    string
	Permanent bool
}

func (e *TaskError) Error() string {
	return e.Reason
}

func relativePath(name string) string {
	return strings.TrimPrefix(name, "/")
}
