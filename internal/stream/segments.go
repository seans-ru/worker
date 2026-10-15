// Package stream tracks which byte ranges of a torrent file are already
// downloaded and can be exposed to players, and manages fixed-size segment
// upload to object storage for progressive playback.
package stream

import (
	"sort"
)

// PieceState mirrors qBittorrent piece states.
type PieceState int

const (
	PieceMissing    PieceState = 0
	PieceDownloading PieceState = 1
	PieceDownloaded PieceState = 2
)

// DefaultSegmentSize is 8 MiB — large enough for efficient S3 PUTs,
// small enough that early playback starts after the first segment.
const DefaultSegmentSize = 8 << 20

// FileLayout describes one file's position inside the torrent piece space.
type FileLayout struct {
	Index      int    // torrent file index
	Path       string // torrent-relative path
	Size       int64  // file size in bytes
	Offset     int64  // byte offset of this file within the concatenated torrent
	PieceStart int    // first piece index covering this file
	PieceEnd   int    // last piece index (inclusive)
	PieceSize  int64  // torrent piece size
}

// FileEntry is a torrent file as reported by the client.
type FileEntry struct {
	Index int
	Path  string
	Size  int64
}

// BuildFileLayouts maps torrent files to piece ranges.
// pieceSize must be > 0. files must be in torrent order (same order as qBittorrent).
func BuildFileLayouts(files []FileEntry, pieceSize int64) []FileLayout {
	if pieceSize <= 0 {
		return nil
	}
	out := make([]FileLayout, 0, len(files))
	var offset int64
	for _, f := range files {
		if f.Size <= 0 {
			out = append(out, FileLayout{
				Index: f.Index, Path: f.Path, Size: f.Size,
				Offset: offset, PieceSize: pieceSize,
			})
			offset += f.Size
			continue
		}
		startPiece := int(offset / pieceSize)
		endPiece := int((offset + f.Size - 1) / pieceSize)
		out = append(out, FileLayout{
			Index:      f.Index,
			Path:       f.Path,
			Size:       f.Size,
			Offset:     offset,
			PieceStart: startPiece,
			PieceEnd:   endPiece,
			PieceSize:  pieceSize,
		})
		offset += f.Size
	}
	return out
}

// ContiguousBytes returns how many bytes from the start of the file are fully
// available (all covering pieces downloaded). This is the max seekable prefix.
func ContiguousBytes(l FileLayout, states []PieceState) int64 {
	if l.Size <= 0 || l.PieceSize <= 0 {
		return 0
	}
	// Walk pieces from the start of the file until the first missing piece.
	var have int64
	for p := l.PieceStart; p <= l.PieceEnd; p++ {
		if p < 0 || p >= len(states) || states[p] != PieceDownloaded {
			break
		}
		// Bytes of this piece that belong to the file and lie after `have`.
		// Absolute torrent offset of piece end:
		pieceEnd := int64(p+1) * l.PieceSize
		// File-relative end:
		fileEnd := pieceEnd - l.Offset
		if fileEnd > l.Size {
			fileEnd = l.Size
		}
		if fileEnd <= have {
			continue
		}
		have = fileEnd
	}
	return have
}

// DownloadedBytes sums fully downloaded pieces' contribution to the file
// (not necessarily contiguous). Used for percent-complete reporting.
func DownloadedBytes(l FileLayout, states []PieceState) int64 {
	if l.Size <= 0 || l.PieceSize <= 0 {
		return 0
	}
	var total int64
	for p := l.PieceStart; p <= l.PieceEnd; p++ {
		if p < 0 || p >= len(states) || states[p] != PieceDownloaded {
			continue
		}
		pieceStart := int64(p) * l.PieceSize
		pieceEnd := int64(p+1) * l.PieceSize
		// Intersect with file range [Offset, Offset+Size)
		s := pieceStart
		if s < l.Offset {
			s = l.Offset
		}
		e := pieceEnd
		if e > l.Offset+l.Size {
			e = l.Offset + l.Size
		}
		if e > s {
			total += e - s
		}
	}
	return total
}

// Segment is a fixed-size slice of a file used for progressive S3 upload.
type Segment struct {
	Index int   `json:"index"`
	Start int64 `json:"start"` // inclusive, file-relative
	End   int64 `json:"end"`   // exclusive
}

// SegmentStatus is the current availability/upload state of a segment.
type SegmentStatus struct {
	Index     int    `json:"index"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	Available bool   `json:"available"` // all bytes present locally
	Uploaded  bool   `json:"uploaded"`
	S3Key     string `json:"s3_key,omitempty"`
}

// FileProgress is what the worker reports for one streamable file.
type FileProgress struct {
	Path              string          `json:"path"`
	Size              int64           `json:"size"`
	PieceSize         int64           `json:"piece_size"`
	SegmentSize       int64           `json:"segment_size"`
	ContiguousBytes   int64           `json:"contiguous_bytes"` // max seekable
	DownloadedBytes   int64           `json:"downloaded_bytes"`
	PercentDownloaded float64         `json:"percent_downloaded"`
	PercentSeekable   float64         `json:"percent_seekable"`
	Segments          []SegmentStatus `json:"segments"`
	Complete          bool            `json:"complete"`
}

// SplitSegments splits file size into fixed-size segments.
func SplitSegments(size, segmentSize int64) []Segment {
	if size <= 0 || segmentSize <= 0 {
		return nil
	}
	var segs []Segment
	for i, start := int(0), int64(0); start < size; i++ {
		end := start + segmentSize
		if end > size {
			end = size
		}
		segs = append(segs, Segment{Index: i, Start: start, End: end})
		start = end
	}
	return segs
}

// SegmentAvailable reports whether a segment's bytes are fully present
// (covered only by downloaded pieces).
func SegmentAvailable(l FileLayout, seg Segment, states []PieceState) bool {
	if l.PieceSize <= 0 {
		return false
	}
	// Every piece overlapping [seg.Start, seg.End) in file space must be downloaded.
	absStart := l.Offset + seg.Start
	absEnd := l.Offset + seg.End
	pieceStart := int(absStart / l.PieceSize)
	pieceEnd := int((absEnd - 1) / l.PieceSize)
	for p := pieceStart; p <= pieceEnd; p++ {
		if p < 0 || p >= len(states) || states[p] != PieceDownloaded {
			return false
		}
	}
	return true
}

// BuildProgress assembles FileProgress for a layout and piece states.
// uploaded tracks which segment indices are already in object storage.
func BuildProgress(l FileLayout, states []PieceState, segmentSize int64, uploaded map[int]string) FileProgress {
	if segmentSize <= 0 {
		segmentSize = DefaultSegmentSize
	}
	segs := SplitSegments(l.Size, segmentSize)
	statuses := make([]SegmentStatus, 0, len(segs))
	var availCount, upCount int
	for _, s := range segs {
		av := SegmentAvailable(l, s, states)
		key := uploaded[s.Index]
		up := key != ""
		if av {
			availCount++
		}
		if up {
			upCount++
		}
		statuses = append(statuses, SegmentStatus{
			Index:     s.Index,
			Start:     s.Start,
			End:       s.End,
			Available: av,
			Uploaded:  up,
			S3Key:     key,
		})
	}

	cont := ContiguousBytes(l, states)
	_ = availCount
	_ = upCount
	dl := DownloadedBytes(l, states)

	var pctDl, pctSeek float64
	if l.Size > 0 {
		pctDl = float64(dl) / float64(l.Size) * 100
		pctSeek = float64(cont) / float64(l.Size) * 100
	}

	return FileProgress{
		Path:              l.Path,
		Size:              l.Size,
		PieceSize:         l.PieceSize,
		SegmentSize:       segmentSize,
		ContiguousBytes:   cont,
		DownloadedBytes:   dl,
		PercentDownloaded: pctDl,
		PercentSeekable:   pctSeek,
		Segments:          statuses,
		Complete:          cont >= l.Size && l.Size > 0,
	}
}

// NextUploadable returns segments that are available but not yet uploaded,
// ordered by index (sequential-friendly).
func NextUploadable(fp FileProgress) []SegmentStatus {
	var out []SegmentStatus
	for _, s := range fp.Segments {
		if s.Available && !s.Uploaded {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}
