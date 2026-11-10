package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Geardung/seans-worker/internal/backend"
	"github.com/Geardung/seans-worker/internal/qbit"
	"github.com/Geardung/seans-worker/internal/s3uploader"
	"github.com/Geardung/seans-worker/internal/stream"
)

// streamUploader uploads completed file segments to S3 while the torrent
// is still downloading, and publishes a stream index with the seekable prefix.
type streamUploader struct {
	uploader   *s3uploader.Uploader
	destPrefix string // e.g. "users/{task_id}/"
	logFile    string
	logger     *slog.Logger

	segmentSize int64
	// uploaded maps file index -> set of uploaded segment indices
	uploaded map[int]map[int]string
	// lastContiguous tracks last published seekable prefix to avoid redundant index uploads
	lastContiguous map[int]int64
}

func newStreamUploader(up *s3uploader.Uploader, destPrefix string, segmentSize int64, logFile string, logger *slog.Logger) *streamUploader {
	if segmentSize <= 0 {
		segmentSize = stream.DefaultSegmentSize
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &streamUploader{
		uploader:       up,
		destPrefix:     destPrefix,
		logFile:        logFile,
		logger:         logger,
		segmentSize:    segmentSize,
		uploaded:       make(map[int]map[int]string),
		lastContiguous: make(map[int]int64),
	}
}

// streamFileKey returns the object key prefix for one torrent file's stream segments.
// Uses "stream/" (no leading dot) — safer on S3-compatible stores.
func (s *streamUploader) streamFileKey(fileIndex int) string {
	return fmt.Sprintf("%sstream/f%04d/", s.destPrefix, fileIndex)
}

// segmentKey is the object key for a single segment.
func (s *streamUploader) segmentKey(fileIndex, segIndex int) string {
	return fmt.Sprintf("%sseg_%05d.bin", s.streamFileKey(fileIndex), segIndex)
}

// indexKey is the stream index (manifest) for one file.
func (s *streamUploader) indexKey(fileIndex int) string {
	return s.streamFileKey(fileIndex) + "index.json"
}

// StreamIndex is published to S3 so clients/CDN can resolve playable ranges.
// Clients MUST NOT seek past contiguous_bytes.
type StreamIndex struct {
	TaskID            string                 `json:"task_id"`
	FilePath          string                 `json:"file_path"`
	FileIndex         int                    `json:"file_index"`
	SizeBytes         int64                  `json:"size_bytes"`
	SegmentSize       int64                  `json:"segment_size"`
	ContiguousBytes   int64                  `json:"contiguous_bytes"` // exclusive max seekable offset
	DownloadedBytes   int64                  `json:"downloaded_bytes"`
	PercentDownloaded float64                `json:"percent_downloaded"`
	PercentSeekable   float64                `json:"percent_seekable"`
	Segments          []stream.SegmentStatus `json:"segments"`
	Complete          bool                   `json:"complete"`
	UpdatedAt         time.Time              `json:"updated_at"`
}

// UploadPending uploads newly available segments (bounded per call) and
// refreshes the stream index. Returns the current progress snapshot.
//
// Segments keep being uploaded even after the file is fully downloaded —
// they stop only when the film is fully on S3 (Finalize), so the stream
// stays seekable while the final full-object upload is still running.
func (s *streamUploader) UploadPending(
	ctx context.Context,
	taskID string,
	layout stream.FileLayout,
	localPath string,
	states []stream.PieceState,
) (*backend.StreamProgress, error) {
	if _, ok := s.uploaded[layout.Index]; !ok {
		s.uploaded[layout.Index] = make(map[int]string)
	}

	fp := stream.BuildProgress(layout, states, s.segmentSize, s.uploaded[layout.Index])

	pending := stream.NextUploadable(fp)
	// Keep the poll loop responsive: at most maxSegmentsPerCall uploads per tick.
	const maxSegmentsPerCall = 2
	if len(pending) > maxSegmentsPerCall {
		pending = pending[:maxSegmentsPerCall]
	}

	uploadedNow := 0
	for _, seg := range pending {
		start := time.Now()
		if err := s.uploadSegment(ctx, layout, localPath, seg); err != nil {
			// non-fatal: retry next tick
			return progressFrom(fp, layout), fmt.Errorf("upload segment %d: %w", seg.Index, err)
		}
		key := s.segmentKey(layout.Index, seg.Index)
		s.uploaded[layout.Index][seg.Index] = key
		fp.Segments[seg.Index].Uploaded = true
		fp.Segments[seg.Index].S3Key = key
		uploadedNow++
		s.logger.Info("stream: segment uploaded",
			"file", layout.Path,
			"segment", seg.Index,
			"bytes", seg.End-seg.Start,
			"range", fmt.Sprintf("[%d,%d)", seg.Start, seg.End),
			"s3_key", key,
			"took_ms", time.Since(start).Milliseconds(),
		)
	}

	// Upload index when anything changed (new segments or seekable prefix grew).
	upCont := stream.UploadedContiguousBytes(fp)
	idxChanged := uploadedNow > 0 || s.lastContiguous[layout.Index] != upCont
	if idxChanged {
		s.lastContiguous[layout.Index] = upCont
		if err := s.publishIndex(ctx, taskID, layout, fp); err != nil {
			return progressFrom(fp, layout), err
		}
	}

	return progressFrom(fp, layout), nil
}

// Finalize marks the stream complete and publishes the index. Call this only
// when the film is fully uploaded to S3 — from here on clients use the full
// object, so segment uploads can stop.
func (s *streamUploader) Finalize(ctx context.Context, taskID string, layout stream.FileLayout, states []stream.PieceState) (*backend.StreamProgress, error) {
	if _, ok := s.uploaded[layout.Index]; !ok {
		s.uploaded[layout.Index] = make(map[int]string)
	}
	fp := stream.BuildProgress(layout, states, s.segmentSize, s.uploaded[layout.Index])
	return s.finalize(ctx, taskID, layout, fp)
}

func (s *streamUploader) finalize(ctx context.Context, taskID string, layout stream.FileLayout, fp stream.FileProgress) (*backend.StreamProgress, error) {
	skipped := 0
	for _, seg := range fp.Segments {
		if !seg.Uploaded {
			skipped++
		}
	}
	if skipped > 0 {
		s.logger.Info("stream: segment uploads done, film fully on S3 (full object covers unpushed segments)",
			"file", layout.Path,
			"segment_objects_skipped", skipped,
		)
	}

	// The full file is on S3 now: treat every segment as present there.
	for i := range fp.Segments {
		fp.Segments[i].Available = true
		fp.Segments[i].Uploaded = true
	}
	fp.ContiguousBytes = fp.Size
	fp.PercentSeekable = 100
	fp.PercentDownloaded = 100
	fp.Complete = true

	s.lastContiguous[layout.Index] = fp.ContiguousBytes
	if err := s.publishIndex(ctx, taskID, layout, fp); err != nil {
		return progressFrom(fp, layout), err
	}
	sp := progressFrom(fp, layout)
	sp.Complete = true
	sp.ContiguousBytes = layout.Size
	sp.PercentSeekable = 100
	sp.PercentDownloaded = 100
	sp.StreamReady = false
	return sp, nil
}

func (s *streamUploader) publishIndex(ctx context.Context, taskID string, layout stream.FileLayout, fp stream.FileProgress) error {
	// Seekability on S3 is limited to the contiguous uploaded segment prefix
	// (after finalize everything is marked present).
	upCont := stream.UploadedContiguousBytes(fp)
	var pctSeek float64
	if fp.Size > 0 {
		pctSeek = float64(upCont) / float64(fp.Size) * 100
	}
	idx := StreamIndex{
		TaskID:            taskID,
		FilePath:          layout.Path,
		FileIndex:         layout.Index,
		SizeBytes:         layout.Size,
		SegmentSize:       s.segmentSize,
		ContiguousBytes:   upCont,
		DownloadedBytes:   fp.DownloadedBytes,
		PercentDownloaded: fp.PercentDownloaded,
		PercentSeekable:   pctSeek,
		Segments:          fp.Segments,
		Complete:          fp.Complete,
		UpdatedAt:         time.Now().UTC(),
	}
	if err := s.uploader.UploadJSON(ctx, s.indexKey(layout.Index), idx, s.logFile); err != nil {
		return fmt.Errorf("upload stream index: %w", err)
	}
	return nil
}

func (s *streamUploader) uploadSegment(ctx context.Context, layout stream.FileLayout, localPath string, seg stream.SegmentStatus) error {
	size := seg.End - seg.Start
	if size <= 0 {
		return nil
	}
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open media: %w", err)
	}
	defer f.Close()

	buf := make([]byte, size)
	n, err := f.ReadAt(buf, seg.Start)
	if err != nil && n == 0 {
		return fmt.Errorf("read segment [%d,%d): %w", seg.Start, seg.End, err)
	}

	tmpDir, err := os.MkdirTemp("", "seans-seg-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	tmpPath := filepath.Join(tmpDir, fmt.Sprintf("seg_%05d.bin", seg.Index))
	if err := os.WriteFile(tmpPath, buf[:n], 0o600); err != nil {
		return fmt.Errorf("write temp segment: %w", err)
	}

	return s.uploader.UploadFile(ctx, tmpPath, s.segmentKey(layout.Index, seg.Index), s.logFile)
}

func progressFrom(fp stream.FileProgress, layout stream.FileLayout) *backend.StreamProgress {
	var up, av int
	for _, s := range fp.Segments {
		if s.Uploaded {
			up++
		}
		if s.Available {
			av++
		}
	}
	// What players can actually use from S3: the contiguous uploaded prefix.
	upCont := stream.UploadedContiguousBytes(fp)
	var pctSeek float64
	if layout.Size > 0 {
		pctSeek = float64(upCont) / float64(layout.Size) * 100
	}
	// StreamReady: at least one uploaded segment — enough for a player to start.
	streamReady := upCont > 0
	return &backend.StreamProgress{
		FilePath:          layout.Path,
		SizeBytes:         layout.Size,
		ContiguousBytes:   upCont,
		DownloadedBytes:   fp.DownloadedBytes,
		PercentDownloaded: fp.PercentDownloaded,
		PercentSeekable:   pctSeek,
		SegmentSize:       fp.SegmentSize,
		SegmentsTotal:     len(fp.Segments),
		SegmentsUploaded:  up,
		SegmentsAvailable: av,
		StreamReady:       streamReady,
		// Complete is set by finalize() when the film is fully on S3.
		Complete: false,
	}
}

// toPieceStates converts qbit int states to stream.PieceState.
func toPieceStates(in []int) []stream.PieceState {
	out := make([]stream.PieceState, len(in))
	for i, v := range in {
		out[i] = stream.PieceState(v)
	}
	return out
}

// fileLayoutsFromQBit builds layouts from qbit files + piece size.
func fileLayoutsFromQBit(files []qbit.TorrentFile, pieceSize int64) []stream.FileLayout {
	entries := make([]stream.FileEntry, len(files))
	for i, f := range files {
		entries[i] = stream.FileEntry{Index: f.Index, Path: f.Name, Size: f.Size}
	}
	return stream.BuildFileLayouts(entries, pieceSize)
}
