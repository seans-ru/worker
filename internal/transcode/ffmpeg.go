package transcode

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
)

// Encoder selects the ffmpeg video encoder.
type Encoder string

const (
	EncoderX264  Encoder = "libx264"
	EncoderNVENC Encoder = "h264_nvenc"
)

// Options controls how ffmpeg is invoked.
type Options struct {
	FfmpegPath     string  // default "ffmpeg"
	Encoder        Encoder // default EncoderX264
	SegmentSeconds int     // default 4
	AudioBitrate   string  // default "128k"
}

func (o Options) withDefaults() Options {
	if o.FfmpegPath == "" {
		o.FfmpegPath = "ffmpeg"
	}
	if o.Encoder == "" {
		o.Encoder = EncoderX264
	}
	if o.SegmentSeconds <= 0 {
		o.SegmentSeconds = 4
	}
	if o.AudioBitrate == "" {
		o.AudioBitrate = "128k"
	}
	return o
}

// Cmd builds a cancellable ffmpeg command for the given arguments.
func Cmd(ctx context.Context, o Options, args []string) *exec.Cmd {
	o = o.withDefaults()
	return exec.CommandContext(ctx, o.FfmpegPath, args...)
}

// HLSArgs returns ffmpeg arguments that encode one rung of src into an HLS VOD
// playlist: outDir/index.m3u8 plus outDir/seg_%05d.ts segments.
func HLSArgs(src string, r Rung, outDir string, o Options) ([]string, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if src == "" {
		return nil, fmt.Errorf("hls: empty source path")
	}
	if outDir == "" {
		return nil, fmt.Errorf("hls: empty output dir")
	}
	o = o.withDefaults()

	args := []string{
		"-y", "-hide_banner", "-nostdin",
		"-i", src,
		"-vf", fmt.Sprintf("scale=-2:%d", r.Height),
		// Align keyframes to segment boundaries so rungs stay seekable
		// and later variant switching stays possible.
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", o.SegmentSeconds),
		"-c:v", string(o.Encoder),
	}
	if o.Encoder == EncoderNVENC {
		args = append(args, "-preset", "p4")
	} else {
		args = append(args, "-preset", "veryfast")
	}
	args = append(args,
		"-b:v", r.VideoBitrate,
		"-maxrate", r.MaxRate,
		"-bufsize", r.BufSize,
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", o.AudioBitrate,
		"-ac", "2",
		"-f", "hls",
		"-hls_time", fmt.Sprintf("%d", o.SegmentSeconds),
		"-hls_playlist_type", "vod",
		"-hls_segment_filename", filepath.Join(outDir, "seg_%05d.ts"),
		filepath.Join(outDir, "index.m3u8"),
	)
	return args, nil
}

// RemuxFaststartArgs rewrites an MP4 so the moov atom moves to the front.
// That lets players start progressive playback without reading the file tail.
func RemuxFaststartArgs(src, dst string, o Options) ([]string, error) {
	if src == "" || dst == "" {
		return nil, fmt.Errorf("remux: empty source or destination")
	}
	o = o.withDefaults()
	return []string{
		"-y", "-hide_banner", "-nostdin",
		"-i", src,
		"-c", "copy",
		"-movflags", "+faststart",
		dst,
	}, nil
}
