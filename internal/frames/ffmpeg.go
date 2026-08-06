package frames

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vismod/vismod/internal/config"
)

// FFmpegSource extracts frames by invoking the external ffmpeg/ffprobe
// binaries directly via os/exec — an arg slice, never a shell.
type FFmpegSource struct {
	cfg config.FFmpegConfig
	log *slog.Logger
}

// NewFFmpegSource builds the source. Call ValidateBinaries and
// ValidateAll(cfg) at boot first so a missing binary or bad workflow is
// an operator error, not a per-job failure.
func NewFFmpegSource(cfg config.FFmpegConfig, log *slog.Logger) *FFmpegSource {
	if log == nil {
		log = slog.Default()
	}
	return &FFmpegSource{cfg: cfg, log: log}
}

// ValidateBinaries confirms ffmpeg and ffprobe are runnable (boot
// prerequisite; Docker bundles both).
func ValidateBinaries(cfg config.FFmpegConfig) error {
	for _, bin := range []string{cfg.FFmpegPath, cfg.FFprobePath} {
		if bin == "" {
			return fmt.Errorf("ffmpeg/ffprobe path is empty")
		}
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("required binary %q not found on PATH (or configured path): %w", bin, err)
		}
	}
	return nil
}

// showinfoRe pulls pts_time values out of ffmpeg's showinfo filter log.
var showinfoRe = regexp.MustCompile(`pts_time:\s*([0-9]+(?:\.[0-9]+)?)`)

// Frames implements FrameSource. It creates and owns an absolute WorkDir
// and returns an idempotent cleanup closure deleting it; the caller MUST
// `defer cleanup()` immediately (lifecycle contract in frames.go).
//
// Multiple workflows run in order into per-workflow subdirectories of the
// job WorkDir; the result is the union of their frames. The EXTRACTION
// budget (ffmpeg.max_extract_frames, default 4 × max_frames) bounds the
// total frames materialized on disk across all selected workflows; the
// tighter max_frames SCAN cap is applied by the pipeline after
// post-processing (dedup), so dedup gets to remove near-duplicates before
// anything is cut for budget reasons.
func (s *FFmpegSource) Frames(ctx context.Context, videoPath string, workflows []string) ([]Frame, func() error, error) {
	noop := func() error { return nil }

	abs, err := filepath.Abs(videoPath)
	if err != nil {
		return nil, noop, fmt.Errorf("resolve input: %w", err)
	}
	// Defense in depth: the input must be a plain local file, never a
	// protocol reference.
	if strings.Contains(videoPath, "://") || forbiddenProtoRe.MatchString(videoPath) {
		return nil, noop, fmt.Errorf("input %q is not a plain local path", videoPath)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, noop, fmt.Errorf("input: %w", err)
	}

	if len(workflows) == 0 {
		workflows = []string{s.cfg.DefaultWorkflow}
	}
	// Re-validate names at execution time (defense in depth on top of the
	// intake check): unknown selection is could-not-evaluate, never a
	// silent fallback to some other workflow.
	for _, name := range workflows {
		if _, ok := s.cfg.Workflows[name]; !ok {
			return nil, noop, fmt.Errorf("workflow %q not configured (have: %v)", name, workflowNames(s.cfg))
		}
	}

	workDir, err := os.MkdirTemp("", "vismod-frames-*")
	if err != nil {
		return nil, noop, fmt.Errorf("create workdir: %w", err)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		_ = os.RemoveAll(workDir)
		return nil, noop, fmt.Errorf("resolve workdir: %w", err)
	}
	var once sync.Once
	cleanup := func() error {
		var cerr error
		once.Do(func() { cerr = os.RemoveAll(workDir) })
		return cerr
	}

	// ffprobe first: validates the input is actually parseable media and
	// surfaces duration for logging.
	duration, err := s.probeDuration(ctx, abs)
	if err != nil {
		return nil, cleanup, fmt.Errorf("ffprobe: %w", err)
	}

	budget := s.cfg.ExtractBudget()
	start := time.Now()
	var frames []Frame
	for _, name := range workflows {
		if len(frames) >= budget {
			s.log.Warn("extraction budget reached before all selected workflows ran",
				"skipped_workflow", name, "extract_budget", budget)
			break
		}
		wfFrames, err := s.runWorkflow(ctx, name, abs, workDir, budget)
		if err != nil {
			return nil, cleanup, err
		}
		frames = append(frames, wfFrames...)
	}

	// Hard disk bound across the union: workflows are told the budget via
	// {{.MaxFrames}}, but a workflow that ignores it must not blow it.
	if len(frames) > budget {
		s.log.Warn("selected workflows exceeded the extraction budget; truncating",
			"produced", len(frames), "extract_budget", budget)
		for _, extra := range frames[budget:] {
			_ = os.Remove(extra.Path)
		}
		frames = frames[:budget]
	}
	for i := range frames {
		frames[i].Index = i
	}

	s.log.Info("frames extracted",
		"workflows", workflows, "input_duration_sec", duration,
		"frames", len(frames), "latency", time.Since(start))
	return frames, cleanup, nil
}

// runWorkflow executes one named workflow into its own subdirectory of
// the job WorkDir and returns its frames (workflow-local ordering).
// {{.MaxFrames}} renders as the extraction budget, not the scan cap.
func (s *FFmpegSource) runWorkflow(ctx context.Context, name, input, jobDir string, budget int) ([]Frame, error) {
	wf := s.cfg.Workflows[name]
	subDir := filepath.Join(jobDir, sanitizeDirName(name))
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		return nil, fmt.Errorf("workflow %q: workdir: %w", name, err)
	}

	args, err := RenderWorkflow(wf, TemplateValues{
		Input:     input,
		WorkDir:   filepath.ToSlash(subDir),
		MaxFrames: budget,
		MaxWidth:  s.cfg.MaxWidth,
	})
	if err != nil {
		return nil, fmt.Errorf("render workflow %q: %w", name, err)
	}

	tctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	cmd := exec.CommandContext(tctx, s.cfg.FFmpegPath, args...) // arg slice, never a shell
	// Bounded: showinfo logs a line per decoded frame, so the raw stream
	// scales with the input while what we need from it does not.
	stderr := &stderrScanner{}
	cmd.Stderr = stderr
	cmd.Stdout = nil
	err = cmd.Run()
	stderr.Flush()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg workflow %q failed: %w (stderr tail: %s)",
			name, err, tail(stderr.Tail(), 400))
	}
	return s.collect(subDir, stderr.Timestamps())
}

// collect lists the materialized PNGs (sorted by sequence number) and
// attaches showinfo pts_time timestamps when available (frame index
// otherwise).
func (s *FFmpegSource) collect(workDir string, ts []float64) ([]Frame, error) {
	entries, err := filepath.Glob(filepath.Join(workDir, "*.png"))
	if err != nil {
		return nil, err
	}
	sort.Strings(entries) // frame-%06d ordering == numeric ordering

	frames := make([]Frame, len(entries))
	for i, p := range entries {
		t := float64(i) // ordinal fallback when showinfo isn't in the graph
		if i < len(ts) {
			t = ts[i]
		}
		frames[i] = Frame{Index: i, TimestampSec: t, Path: p}
	}
	return frames, nil
}

// sanitizeDirName keeps workflow-name-derived subdirectories plain.
func sanitizeDirName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
}

func (s *FFmpegSource) probeDuration(ctx context.Context, input string) (float64, error) {
	tctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	cmd := exec.CommandContext(tctx, s.cfg.FFprobePath,
		"-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", input)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("input is not parseable media: %w", err)
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return d, nil
}

func (s *FFmpegSource) timeout() time.Duration {
	if s.cfg.Timeout > 0 {
		return s.cfg.Timeout
	}
	return 2 * time.Minute
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

var _ FrameSource = (*FFmpegSource)(nil)
