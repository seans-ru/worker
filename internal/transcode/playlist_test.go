package transcode

import (
	"strings"
	"testing"
)

func TestParseBitrate(t *testing.T) {
	cases := map[string]int{
		"400k":    400_000,
		"5M":      5_000_000,
		"1200000": 1_200_000,
	}
	for in, want := range cases {
		got, err := ParseBitrate(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: got %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "k", "0k", "abc"} {
		if _, err := ParseBitrate(bad); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
}

func TestNewVariant(t *testing.T) {
	r := Rung{Name: "480p", Height: 480, VideoBitrate: "1200k", MaxRate: "1500k", BufSize: "2400k"}
	v, err := NewVariant(r, 854)
	if err != nil {
		t.Fatal(err)
	}
	if v.PeakBits != 1_500_000+AudioBitrateBits {
		t.Fatalf("peak = %d", v.PeakBits)
	}
	if v.AverageBits != 1_200_000+AudioBitrateBits {
		t.Fatalf("avg = %d", v.AverageBits)
	}
	if v.Height != 480 || v.Width != 854 {
		t.Fatalf("resolution %dx%d", v.Width, v.Height)
	}
}

func TestMasterPlaylist(t *testing.T) {
	mk := func(name string, height, peak int) Variant {
		return Variant{
			Rung:        Rung{Name: name, Height: height, VideoBitrate: "1000k", MaxRate: "1200k", BufSize: "2000k"},
			Height:      height,
			PeakBits:    peak,
			AverageBits: peak / 2,
		}
	}
	high := mk("1080p", 1080, 6_000_000)
	high.Width = 1920
	low := mk("240p", 240, 500_000)

	out, err := MasterPlaylist([]Variant{high, low})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "#EXTM3U\n#EXT-X-VERSION:3\n") {
		t.Fatalf("bad header: %s", out)
	}
	// Sorted by bandwidth ascending: 240p line first.
	iLow := strings.Index(out, "240p/index.m3u8")
	iHigh := strings.Index(out, "1080p/index.m3u8")
	if iLow < 0 || iHigh < 0 || iLow > iHigh {
		t.Fatalf("variants not sorted:\n%s", out)
	}
	if !strings.Contains(out, "RESOLUTION=1920x1080") {
		t.Fatalf("missing resolution:\n%s", out)
	}
	if strings.Contains(out, "RESOLUTION=") && strings.Count(out, "RESOLUTION=") != 1 {
		t.Fatalf("resolution must be omitted for unknown width:\n%s", out)
	}
}

func TestMasterPlaylistEmpty(t *testing.T) {
	if _, err := MasterPlaylist(nil); err == nil {
		t.Fatal("empty variants must fail")
	}
}

func TestKeyLayout(t *testing.T) {
	prefix := "users/tsk_001/"
	if got := MasterKey(prefix); got != "users/tsk_001/hls/master.m3u8" {
		t.Fatalf("master key %q", got)
	}
	if got := PlaylistKey(prefix, "480p"); got != "users/tsk_001/hls/480p/index.m3u8" {
		t.Fatalf("playlist key %q", got)
	}
	if got := SegmentKey(prefix, "480p", 42); got != "users/tsk_001/hls/480p/seg_00042.ts" {
		t.Fatalf("segment key %q", got)
	}
}
