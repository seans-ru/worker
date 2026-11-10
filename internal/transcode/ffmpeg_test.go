package transcode

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestHLSArgs(t *testing.T) {
	r := Rung{Name: "480p", Height: 480, VideoBitrate: "1200k", MaxRate: "1500k", BufSize: "2400k"}
	args, err := HLSArgs("/media/in.mkv", r, "/out/480p", Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-i /media/in.mkv",
		"scale=-2:480",
		"expr:gte(t,n_forced*4)",
		"-c:v libx264",
		"-preset veryfast",
		"-b:v 1200k",
		"-maxrate 1500k",
		"-bufsize 2400k",
		"-f hls",
		"-hls_time 4",
		"-hls_playlist_type vod",
		"-hls_segment_filename " + filepath.Join("/out/480p", "seg_%05d.ts"),
		filepath.Join("/out/480p", "index.m3u8"),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q\n got: %s", want, joined)
		}
	}
}

func TestHLSArgsNVENC(t *testing.T) {
	r := Rung{Name: "720p", Height: 720, VideoBitrate: "2800k", MaxRate: "3500k", BufSize: "5600k"}
	args, err := HLSArgs("/in.mkv", r, "/out", Options{Encoder: EncoderNVENC, SegmentSeconds: 6})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c:v h264_nvenc") || !strings.Contains(joined, "-preset p4") {
		t.Fatalf("nvenc args wrong: %s", joined)
	}
	if !strings.Contains(joined, "n_forced*6") || !strings.Contains(joined, "-hls_time 6") {
		t.Fatalf("segment seconds not applied: %s", joined)
	}
}

func TestHLSArgsRejectsBadInput(t *testing.T) {
	if _, err := HLSArgs("", Rung{Name: "x", Height: 1, VideoBitrate: "1k", MaxRate: "1k", BufSize: "1k"}, "/out", Options{}); err == nil {
		t.Fatal("empty src must fail")
	}
	if _, err := HLSArgs("/in", Rung{}, "/out", Options{}); err == nil {
		t.Fatal("bad rung must fail")
	}
}

func TestRemuxFaststartArgs(t *testing.T) {
	args, err := RemuxFaststartArgs("/in.mp4", "/out.mp4", Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-i /in.mp4", "-c copy", "-movflags +faststart", "/out.mp4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q\n got: %s", want, joined)
		}
	}
}

func TestCmdUsesConfiguredBinary(t *testing.T) {
	cmd := Cmd(context.Background(), Options{FfmpegPath: "/opt/ffmpeg"}, []string{"-version"})
	if !strings.HasSuffix(cmd.Path, "ffmpeg") && cmd.Path != "/opt/ffmpeg" {
		t.Fatalf("unexpected cmd path %q", cmd.Path)
	}
}
