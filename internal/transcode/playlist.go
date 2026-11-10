package transcode

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// AudioBitrateBits is the audio stream budget added to variant bandwidths.
const AudioBitrateBits = 128_000

// Variant is one rung as advertised in the master playlist.
type Variant struct {
	Rung        Rung
	Width       int // 0 = unknown, RESOLUTION is omitted
	Height      int
	PeakBits    int // peak bandwidth, bits/s
	AverageBits int
}

// NewVariant derives playlist bandwidths from the rung bitrates.
// Width may be 0 when the source aspect ratio is not probed yet.
func NewVariant(r Rung, width int) (Variant, error) {
	avg, err := ParseBitrate(r.VideoBitrate)
	if err != nil {
		return Variant{}, err
	}
	peak, err := ParseBitrate(r.MaxRate)
	if err != nil {
		return Variant{}, err
	}
	return Variant{
		Rung:        r,
		Width:       width,
		Height:      r.Height,
		PeakBits:    peak + AudioBitrateBits,
		AverageBits: avg + AudioBitrateBits,
	}, nil
}

// ParseBitrate parses ffmpeg-style bitrate strings: "400k", "5M", "1200000".
func ParseBitrate(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("bitrate: empty string")
	}
	mult := 1
	switch s[len(s)-1] {
	case 'k', 'K':
		mult = 1000
		s = s[:len(s)-1]
	case 'm', 'M':
		mult = 1000_000
		s = s[:len(s)-1]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bitrate: invalid value %q", s)
	}
	return n * mult, nil
}

// MasterPlaylist renders an HLS master playlist. Variants are sorted by
// bandwidth ascending so players start from the cheap rungs.
func MasterPlaylist(variants []Variant) (string, error) {
	if len(variants) == 0 {
		return "", fmt.Errorf("master playlist: no variants")
	}
	sorted := make([]Variant, len(variants))
	copy(sorted, variants)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PeakBits < sorted[j].PeakBits })

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	for _, v := range sorted {
		if err := v.Rung.Validate(); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d", v.PeakBits, v.AverageBits)
		if v.Width > 0 && v.Height > 0 {
			fmt.Fprintf(&b, ",RESOLUTION=%dx%d", v.Width, v.Height)
		}
		b.WriteByte('\n')
		fmt.Fprintf(&b, "%s/%s\n", v.Rung.Name, PlaylistName)
	}
	return b.String(), nil
}

// Object key layout inside a task's S3 prefix:
//
//	{destPrefix}hls/master.m3u8
//	{destPrefix}hls/{rung}/index.m3u8
//	{destPrefix}hls/{rung}/seg_00000.ts
const (
	MasterName   = "master.m3u8"
	PlaylistName = "index.m3u8"
	SegmentFmt   = "seg_%05d.ts"
	hlsDir       = "hls"
)

// MasterKey is the S3 key of the master playlist.
func MasterKey(destPrefix string) string {
	return path.Join(destPrefix, hlsDir, MasterName)
}

// RungPrefix is the S3 key prefix of one rung's playlist and segments.
func RungPrefix(destPrefix, rung string) string {
	return path.Join(destPrefix, hlsDir, rung) + "/"
}

// PlaylistKey is the S3 key of one rung's media playlist.
func PlaylistKey(destPrefix, rung string) string {
	return RungPrefix(destPrefix, rung) + PlaylistName
}

// SegmentName is the file name of segment i.
func SegmentName(i int) string {
	return fmt.Sprintf(SegmentFmt, i)
}

// SegmentKey is the S3 key of segment i of one rung.
func SegmentKey(destPrefix, rung string, i int) string {
	return RungPrefix(destPrefix, rung) + SegmentName(i)
}
