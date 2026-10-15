package engine

import (
	"context"
	"fmt"
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
	uploader *s3uploader.Uploader
	destPrefix string // e.g. "users/{task_id}/"
	logFile    string

	segmentSize int64
	// uploaded maps file index -> set of uploaded segment indices
	uploaded map[int]map[int]string
}

func newStreamUploader(up *s3uploader.Uploader, destPrefix string, segmentSize int64, logFile string) *streamUploader {
	if segmentSize <= 0 {
		segmentSize = stream.DefaultSegmentSize
	}
	return &streamUploader{
		uploader:    up,
		destPrefix:  destPrefix,
		logFile:     logFile,
		segmentSize: segmentSize,
		uploaded:    make(map[int]map[int]string),
	}
}

// streamFileKey returns the object key prefix for one torrent file's stream segments.
func (s *streamUploader) streamFileKey(fileIndex int) string {
	return fmt.Sprintf("%s.stream/f%04d/", s.destPrefix, fileIndex)
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
	TaskID            string                  `json:"task_id"`
	FilePath          string                  `json:"file_path"`
	FileIndex         int                     `json:"file_index"`
	SizeBytes         int64                   `json:"size_bytes"`
	SegmentSize       int64                   `json:"segment_size"`
	ContiguousBytes   int64                   `json:"contiguous_bytes"` // exclusive max seekable offset
	DownloadedBytes   int64                   `json:"downloaded_bytes"`
	PercentDownloaded float64                 `json:"percent_downloaded"`
	PercentSeekable   float64                 `json:"percent_seekable"`
	Segments          []stream.SegmentStatus  `json:"segments"`
	Complete          bool                    `json:"complete"`
	UpdatedAt         time.Time               `json:"updated_at"`
}

// UploadPending reads the local file, uploads any newly available segments,
// and refreshes the stream index. Returns the current progress snapshot.
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
	for _, seg := range pending {
		if err := s.uploadSegment(ctx, layout, localPath, seg); err != nil {
			// non-fatal: retry next tick
			return progressFrom(fp, layout), fmt.Errorf("upload segment %d: %w", seg.Index, err)
		}
		key := s.segmentKey(layout.Index, seg.Index)
		s.uploaded[layout.Index][seg.Index] = key
		fp.Segments[seg.Index].Uploaded = true
		fp.Segments[seg.Index].S3Key = key
	}

	idx := StreamIndex{
		TaskID:            taskID,
		FilePath:          layout.Path,
		FileIndex:         layout.Index,
		SizeBytes:         layout.Size,
		SegmentSize:       s.segmentSize,
		ContiguousBytes:   fp.ContiguousBytes,
		DownloadedBytes:   fp.DownloadedBytes,
		PercentDownloaded: fp.PercentDownloaded,
		PercentSeekable:   fp.PercentSeekable,
		Segments:          fp.Segments,
		Complete:          fp.Complete,
		UpdatedAt:         time.Now().UTC(),
	}
	if err := s.uploader.UploadJSON(ctx, s.indexKey(layout.Index), idx, s.logFile); err != nil {
		return progressFrom(fp, layout), fmt.Errorf("upload stream index: %w", err)
	}

	return progressFrom(fp, layout), nil
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
	// StreamReady: at least one segment or 5% contiguous — enough for a player to start.
	streamReady := fp.ContiguousBytes > 0 && (up > 0 || fp.ContiguousBytes >= fp.Size/20)
	return &backend.StreamProgress{
		FilePath:          layout.Path,
		SizeBytes:         layout.Size,
		ContiguousBytes:   fp.ContiguousBytes,
		DownloadedBytes:   fp.DownloadedBytes,
		PercentDownloaded: fp.PercentDownloaded,
		PercentSeekable:   fp.PercentSeekable,
		SegmentSize:       fp.SegmentSize,
		SegmentsTotal:     len(fp.Segments),
		SegmentsUploaded:  up,
		SegmentsAvailable: av,
		StreamReady:       streamReady && !fp.Complete,
		Complete:          fp.Complete,
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
