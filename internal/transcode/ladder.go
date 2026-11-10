// Package transcode builds ffmpeg commands that turn a source media file into
// an HLS quality ladder: several encoded rungs, each cut into fixed-duration
// segments with a VOD playlist, plus a master playlist for players.
package transcode

import "fmt"

// Rung is one quality step of the encoding ladder.
type Rung struct {
	Name         string // "480p"
	Height       int    // target height in px, width follows aspect ratio
	VideoBitrate string // average video bitrate, e.g. "1200k"
	MaxRate      string // peak video bitrate, e.g. "1500k"
	BufSize      string // VBV buffer, e.g. "2400k"
}

// DefaultLadder is the standard ABR ladder: a low rung for cheap previews
// (peek) plus 480p–1080p for regular playback.
func DefaultLadder() []Rung {
	return []Rung{
		{Name: "240p", Height: 240, VideoBitrate: "400k", MaxRate: "500k", BufSize: "800k"},
		{Name: "480p", Height: 480, VideoBitrate: "1200k", MaxRate: "1500k", BufSize: "2400k"},
		{Name: "720p", Height: 720, VideoBitrate: "2800k", MaxRate: "3500k", BufSize: "5600k"},
		{Name: "1080p", Height: 1080, VideoBitrate: "5000k", MaxRate: "6250k", BufSize: "10000k"},
	}
}

// Validate checks that the rung can be encoded.
func (r Rung) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("rung: empty name")
	}
	if r.Height <= 0 {
		return fmt.Errorf("rung %q: height must be > 0", r.Name)
	}
	if r.VideoBitrate == "" || r.MaxRate == "" || r.BufSize == "" {
		return fmt.Errorf("rung %q: bitrates must be set", r.Name)
	}
	return nil
}

// CapLadder drops rungs taller than maxHeight (upscaling adds nothing).
// At least one rung is always kept — the lowest one.
func CapLadder(ladder []Rung, maxHeight int) []Rung {
	if maxHeight <= 0 {
		return ladder
	}
	out := make([]Rung, 0, len(ladder))
	for _, r := range ladder {
		if r.Height <= maxHeight {
			out = append(out, r)
		}
	}
	if len(out) == 0 && len(ladder) > 0 {
		return []Rung{ladder[0]}
	}
	return out
}
