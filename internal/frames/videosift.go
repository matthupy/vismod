package frames

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/matthupy/videosift"
)

// VideosiftOptions configures the videosift-backed FrameSource. It is a plain
// value carrier so this package does not depend on internal/config; the
// composition root translates config.FramesConfig into these fields.
type VideosiftOptions struct {
	// WorkDir is the BASE directory under which each job's caller-owned,
	// ephemeral extraction dir is created. Empty => the OS temp dir.
	WorkDir string

	// MaxFrames bounds per-video classifier cost AND peak disk (it materializes
	// MaxFrames PNGs). 0 => unlimited; the pipeline always sets a non-zero value.
	MaxFrames int

	// Strategy toggles. The zero value disables a strategy, so the composition
	// root passes the resolved (defaulted-true) config values explicitly.
	Scene      bool
	Keyframe   bool
	Temporal   bool
	MPDecimate bool

	// Extraction tuning. The composition root passes the resolved config defaults
	// (which mirror videosift.DefaultConfig()); a zero numeric here would override
	// the upstream default, so config.setDefaults seeds non-zero values. HashAlgo
	// is empty-guarded below so "" keeps the DefaultConfig algorithm.
	SceneThreshold       float64
	TemporalInterval     float64
	MPDecimateHi         int
	MPDecimateLo         int
	MPDecimateFrac       float64
	HashAlgo             string // "phash" | "dhash"; empty => videosift default
	HammingThreshold     int
	HashResizeWidth      int
	VideosiftConcurrency int

	// Binary overrides; empty => "ffmpeg"/"ffprobe" discovered on PATH.
	FFmpegPath  string
	FFprobePath string
}

// VideosiftSource is the videosift-backed FrameSource. It owns each job's
// extraction directory: it creates an absolute WorkDir, decodes nothing itself
// (the pipeline lazily decodes), and returns a cleanup that deletes the dir.
type VideosiftSource struct {
	opts VideosiftOptions
}

// NewVideosiftSource builds a videosift-backed FrameSource from opts.
func NewVideosiftSource(opts VideosiftOptions) *VideosiftSource {
	return &VideosiftSource{opts: opts}
}

func (s *VideosiftSource) ffmpegPath() string {
	if s.opts.FFmpegPath != "" {
		return s.opts.FFmpegPath
	}
	return "ffmpeg"
}

func (s *VideosiftSource) ffprobePath() string {
	if s.opts.FFprobePath != "" {
		return s.opts.FFprobePath
	}
	return "ffprobe"
}

// Probe validates that ffmpeg and ffprobe are resolvable. videosift execs both
// external binaries, so a CGO-free static Go binary is NOT self-sufficient at
// runtime; call Probe once at boot so a missing binary surfaces as a clear
// operator error rather than a per-job failure. The returned error wraps
// videosift.ErrNoBinaries.
//
// TODO(videosift): this re-implements videosift's unexported validateBinaries
// (iffmpeg.LookPath) with stdlib exec.LookPath. Identical today, but if
// videosift adds a version/codec check the boot probe could pass while real
// jobs fail. videosift exposes no public probe — track exporting e.g.
// ValidateBinaries(cfg) upstream and call it here so both paths share one check.
func (s *VideosiftSource) Probe(_ context.Context) error {
	for _, bin := range []string{s.ffmpegPath(), s.ffprobePath()} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%w: %q not found: %v", videosift.ErrNoBinaries, bin, err)
		}
	}
	return nil
}

// Frames extracts the frames of videoPath into a fresh, absolute, caller-owned
// WorkDir and returns them mapped to pipeline-owned Frames plus an idempotent
// cleanup that deletes the WorkDir.
//
// Fail-safe (§B): any extraction failure — *videosift.FFmpegError, ErrNoBinaries
// at runtime, or ErrNoFrames for video — is a could-not-evaluate condition. The
// error is returned verbatim (wrapped) so callers keep errors.Is/As; the
// pipeline maps it to Verdict=error and never to allow. A non-nil cleanup is
// ALWAYS returned so the caller can `defer cleanup()` on every path.
func (s *VideosiftSource) Frames(ctx context.Context, videoPath string) ([]Frame, func() error, error) {
	base := s.opts.WorkDir
	if base == "" {
		base = os.TempDir()
	}
	// WorkDir MUST be absolute (videosift's temp-file rule, §B): a relative
	// base would yield a relative Frame.Path the pipeline can't reliably clean.
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, noopCleanup, fmt.Errorf("frames: resolve absolute workdir %q: %w", base, err)
	}
	base = abs
	// 0o700: the base holds per-job workdirs of extracted PNGs (sensitive
	// moderation content). Owner-only keeps the base as locked-down as the
	// 0o700 MkdirTemp workdirs beneath it.
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, noopCleanup, fmt.Errorf("frames: create base dir %q: %w", base, err)
	}

	workDir, err := os.MkdirTemp(base, "vismod-frames-*")
	if err != nil {
		return nil, noopCleanup, fmt.Errorf("frames: create workdir: %w", err)
	}
	cleanup := newCleanup(workDir)

	cfg := videosift.DefaultConfig()
	cfg.WorkDir = workDir // absolute, caller-owned: videosift will NOT delete it
	cfg.MaxFrames = s.opts.MaxFrames
	cfg.Scene = s.opts.Scene
	cfg.Keyframe = s.opts.Keyframe
	cfg.Temporal = s.opts.Temporal
	cfg.MPDecimate = s.opts.MPDecimate
	cfg.SceneThreshold = s.opts.SceneThreshold
	cfg.TemporalInterval = s.opts.TemporalInterval
	cfg.MPDecimateHi = s.opts.MPDecimateHi
	cfg.MPDecimateLo = s.opts.MPDecimateLo
	cfg.MPDecimateFrac = s.opts.MPDecimateFrac
	cfg.HammingThreshold = s.opts.HammingThreshold
	cfg.HashResizeWidth = s.opts.HashResizeWidth
	cfg.Concurrency = s.opts.VideosiftConcurrency
	// Empty => keep DefaultConfig's HashAlgo (a zero-value "" is not a valid algo).
	if s.opts.HashAlgo != "" {
		cfg.HashAlgo = videosift.HashAlgo(s.opts.HashAlgo)
	}
	cfg.FFmpegPath = s.ffmpegPath()
	cfg.FFprobePath = s.ffprobePath()

	vframes, err := videosift.Extract(ctx, videoPath, cfg)
	if err != nil {
		// Wrap, preserving the sentinel/typed cause for errors.Is/As upstream.
		return nil, cleanup, fmt.Errorf("frames: videosift extract: %w", err)
	}
	return mapFrames(vframes), cleanup, nil
}

// mapFrames translates videosift frames into pipeline-owned Frames. It MUST NOT
// leak videosift types through the seam.
func mapFrames(in []videosift.Frame) []Frame {
	out := make([]Frame, len(in))
	for i, f := range in {
		out[i] = Frame{Index: f.Index, TimestampSec: f.TimestampSec, Path: f.Path}
	}
	return out
}

func noopCleanup() error { return nil }

// newCleanup returns an idempotent closure that removes dir exactly once.
func newCleanup(dir string) func() error {
	var once sync.Once
	var err error
	return func() error {
		once.Do(func() { err = os.RemoveAll(dir) })
		return err
	}
}
