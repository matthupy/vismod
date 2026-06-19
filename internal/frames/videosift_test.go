package frames

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/matthupy/videosift"
)

// mapFrames must translate videosift frames into pipeline-owned frames without
// leaking the videosift type, preserving Index/TimestampSec/Path.
func TestMapFrames(t *testing.T) {
	in := []videosift.Frame{
		{Index: 0, TimestampSec: 0.0, Path: "/work/0.png", Strategy: videosift.StrategyScene, Hash: 123},
		{Index: 1, TimestampSec: 2.5, Path: "/work/1.png", Strategy: videosift.StrategyTemporal, Hash: 456},
	}
	got := mapFrames(in)
	want := []Frame{
		{Index: 0, TimestampSec: 0.0, Path: "/work/0.png"},
		{Index: 1, TimestampSec: 2.5, Path: "/work/1.png"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Probe must fail clearly when a configured binary is absent (boot-time check).
func TestProbeMissingBinary(t *testing.T) {
	src := NewVideosiftSource(VideosiftOptions{
		FFmpegPath:  "definitely-not-a-real-ffmpeg-binary",
		FFprobePath: "definitely-not-a-real-ffprobe-binary",
	})
	err := src.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe with bogus binaries returned nil, want error")
	}
	if !errors.Is(err, videosift.ErrNoBinaries) {
		t.Errorf("Probe error = %v, want errors.Is ErrNoBinaries", err)
	}
}

// A missing/invalid video is a could-not-evaluate condition: Frames returns an
// error AND a non-nil, idempotent cleanup that removes the WorkDir.
func TestFramesMissingVideoCleansWorkDir(t *testing.T) {
	base := t.TempDir()
	src := NewVideosiftSource(VideosiftOptions{WorkDir: base})

	frames, cleanup, err := src.Frames(context.Background(), filepath.Join(base, "does-not-exist.mp4"))
	if err == nil {
		t.Fatal("Frames on missing video returned nil error, want error")
	}
	if frames != nil {
		t.Errorf("frames = %v, want nil on error", frames)
	}
	if cleanup == nil {
		t.Fatal("cleanup is nil; lifecycle contract requires a non-nil cleanup on every path")
	}
	// Cleanup is idempotent: safe to call repeatedly.
	if err := cleanup(); err != nil {
		t.Errorf("first cleanup: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Errorf("second cleanup (must be idempotent): %v", err)
	}
	// No vismod-frames-* workdir should survive under base.
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "vismod-frames-") {
			t.Errorf("workdir %q survived cleanup", e.Name())
		}
	}
}

// The base WorkDir holds per-job extracted PNGs (sensitive content); it must be
// created owner-only (0o700), not world-traversable. Frames creates the base
// even when extraction later fails, so a missing-video call exercises this path.
func TestFramesBaseDirIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go only models the read-only bit on Windows; POSIX perms not enforced")
	}
	// A not-yet-existing base under TempDir so MkdirAll actually creates it.
	base := filepath.Join(t.TempDir(), "vismod-base")
	src := NewVideosiftSource(VideosiftOptions{WorkDir: base})

	_, cleanup, err := src.Frames(context.Background(), filepath.Join(base, "does-not-exist.mp4"))
	if err == nil {
		t.Fatal("Frames on missing video returned nil error, want error")
	}
	t.Cleanup(func() { _ = cleanup() })

	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("base dir perm = %o, want 0700", perm)
	}
}

// Integration: with a real ffmpeg, extract frames from a generated clip, then
// prove the lifecycle — PNGs exist on disk, then cleanup deletes the WorkDir.
func TestFramesExtractsRealVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH; skipping integration test")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH; skipping integration test")
	}

	base := t.TempDir()
	videoPath := filepath.Join(base, "clip.mp4")
	// 3-second 64x64 test pattern at 10fps.
	gen := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i",
		"testsrc=duration=3:size=64x64:rate=10", "-pix_fmt", "yuv420p", videoPath)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate test video: %v\n%s", err, out)
	}

	src := NewVideosiftSource(VideosiftOptions{
		WorkDir:    base,
		MaxFrames:  8,
		Scene:      true,
		Keyframe:   true,
		Temporal:   true,
		MPDecimate: true,
	})

	frames, cleanup, err := src.Frames(context.Background(), videoPath)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("extracted 0 frames from a real clip")
	}
	var workDir string
	for _, f := range frames {
		if !filepath.IsAbs(f.Path) {
			t.Errorf("frame path %q is not absolute", f.Path)
		}
		if filepath.Ext(f.Path) != ".png" {
			t.Errorf("frame path %q is not a PNG", f.Path)
		}
		if _, err := os.Stat(f.Path); err != nil {
			t.Errorf("frame PNG missing before cleanup: %v", err)
		}
		workDir = filepath.Dir(f.Path)
	}

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(workDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workdir %q survived cleanup (stat err=%v)", workDir, err)
	}
}
