package transcode

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Step is one ffmpeg invocation of the ladder plan.
type Step struct {
	Rung   Rung
	OutDir string
	Args   []string
}

// Plan is the full ladder build: one step per rung plus the master playlist.
type Plan struct {
	Src        string
	OutRoot    string
	Steps      []Step
	MasterPath string
	Master     string

	opt Options
}

// BuildPlan prepares all ffmpeg invocations for the ladder and renders the
// master playlist. Output layout under outRoot mirrors the S3 key layout:
//
//	outRoot/hls/master.m3u8
//	outRoot/hls/{rung}/index.m3u8
//	outRoot/hls/{rung}/seg_00000.ts
//
// Variant width is left unset (RESOLUTION omitted) until source probing exists.
func BuildPlan(src, outRoot string, ladder []Rung, o Options) (*Plan, error) {
	if src == "" || outRoot == "" {
		return nil, fmt.Errorf("plan: empty source or output root")
	}
	if len(ladder) == 0 {
		return nil, fmt.Errorf("plan: empty ladder")
	}
	o = o.withDefaults()

	p := &Plan{Src: src, OutRoot: outRoot, opt: o}
	variants := make([]Variant, 0, len(ladder))
	for _, r := range ladder {
		outDir := filepath.Join(outRoot, hlsDir, r.Name)
		args, err := HLSArgs(src, r, outDir, o)
		if err != nil {
			return nil, err
		}
		p.Steps = append(p.Steps, Step{Rung: r, OutDir: outDir, Args: args})
		v, err := NewVariant(r, 0)
		if err != nil {
			return nil, err
		}
		variants = append(variants, v)
	}

	master, err := MasterPlaylist(variants)
	if err != nil {
		return nil, err
	}
	p.Master = master
	p.MasterPath = filepath.Join(outRoot, hlsDir, MasterName)
	return p, nil
}

// Execute runs all ffmpeg steps and writes the master playlist.
func (p *Plan) Execute(ctx context.Context) error {
	o := p.opt.withDefaults()
	for _, s := range p.Steps {
		if err := os.MkdirAll(s.OutDir, 0o755); err != nil {
			return fmt.Errorf("rung %s: %w", s.Rung.Name, err)
		}
		out, err := Cmd(ctx, o, s.Args).CombinedOutput()
		if err != nil {
			return fmt.Errorf("rung %s: ffmpeg: %w: %s", s.Rung.Name, err, outputTail(out))
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.MasterPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p.MasterPath, []byte(p.Master), 0o644); err != nil {
		return fmt.Errorf("write master playlist: %w", err)
	}
	return nil
}

// outputTail keeps the last bytes of ffmpeg output for error context.
func outputTail(b []byte) string {
	const max = 400
	b = bytes.TrimSpace(b)
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}
