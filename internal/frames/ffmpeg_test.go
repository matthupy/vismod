package frames

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/config"
)

// requireFFmpeg skips integration tests on machines without ffmpeg (CI
// still runs the pure guardrail/render tests).
func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping integration test", bin)
		}
	}
}

// makeTestVideo synthesizes a short test clip with lavfi testsrc.
func makeTestVideo(t *testing.T, dur string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "clip.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=duration="+dur+":size=320x240:rate=10",
		"-pix_fmt", "yuv420p", out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("synthesize test video: %v", err)
	}
	return out
}

func intervalCfg() config.FFmpegConfig {
	return config.FFmpegConfig{
		FFmpegPath: "ffmpeg", FFprobePath: "ffprobe",
		DefaultWorkflow: "interval", MaxFrames: 4, MaxWidth: 320,
		Timeout: 60 * time.Second, Workflows: config.DefaultWorkflows(),
	}
}

func TestFFmpegSourceExtractsFrames(t *testing.T) {
	requireFFmpeg(t)
	video := makeTestVideo(t, "5")

	src := NewFFmpegSource(intervalCfg(), nil)
	frames, cleanup, err := src.Frames(context.Background(), video, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) == 0 {
		t.Fatal("expected frames from a 5s clip")
	}
	// Extraction is bounded by the budget (4 x max_frames by default);
	// the tighter max_frames scan cap is the pipeline's job (post-dedup).
	if len(frames) > intervalCfg().ExtractBudget() {
		t.Errorf("extraction budget violated: %d frames", len(frames))
	}
	workDir := filepath.Dir(frames[0].Path)
	for i, f := range frames {
		if !filepath.IsAbs(f.Path) {
			t.Errorf("frame path must be absolute: %s", f.Path)
		}
		if _, err := os.Stat(f.Path); err != nil {
			t.Errorf("frame %d missing: %v", i, err)
		}
		if i > 0 && f.TimestampSec < frames[i-1].TimestampSec {
			t.Errorf("timestamps must be non-decreasing: %v", frames)
		}
	}

	// Cleanup contract: idempotent, removes the whole WorkDir.
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Errorf("cleanup must be idempotent: %v", err)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("workdir must be deleted after cleanup: %v", err)
	}
}

func TestFFmpegSourceGarbageInputFails(t *testing.T) {
	requireFFmpeg(t)
	bad := filepath.Join(t.TempDir(), "not-video.mp4")
	if err := os.WriteFile(bad, []byte("this is not media"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := NewFFmpegSource(intervalCfg(), nil)
	_, cleanup, err := src.Frames(context.Background(), bad, nil)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		t.Fatal("garbage input must fail (could-not-evaluate), never pass as clean")
	}
}

func TestFFmpegSourceRejectsProtocolInput(t *testing.T) {
	src := NewFFmpegSource(intervalCfg(), nil)
	_, _, err := src.Frames(context.Background(), "https://evil.example/clip.mp4", nil)
	if err == nil {
		t.Fatal("protocol inputs must be rejected (plain local paths only)")
	}
}

func TestFFmpegSourceMissingInput(t *testing.T) {
	src := NewFFmpegSource(intervalCfg(), nil)
	_, _, err := src.Frames(context.Background(), filepath.Join(t.TempDir(), "missing.mp4"), nil)
	if err == nil {
		t.Fatal("missing input must error")
	}
}

// Multiple selected workflows produce the union of their frames, each in
// its own subdirectory, all deleted by the one cleanup.
func TestFFmpegSourceMultipleWorkflows(t *testing.T) {
	requireFFmpeg(t)
	video := makeTestVideo(t, "5")

	cfg := intervalCfg()
	cfg.MaxFrames = 16
	src := NewFFmpegSource(cfg, nil)
	frames, cleanup, err := src.Frames(context.Background(), video, []string{"interval", "keyframe"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	single, cleanup2, err := src.Frames(context.Background(), video, []string{"interval"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()

	if len(frames) <= len(single) {
		t.Errorf("union of two workflows should exceed one: union=%d single=%d", len(frames), len(single))
	}
	dirs := map[string]bool{}
	for i, f := range frames {
		if f.Index != i {
			t.Errorf("indices must be renumbered across the union: %d != %d", f.Index, i)
		}
		dirs[filepath.Base(filepath.Dir(f.Path))] = true
	}
	if !dirs["interval"] || !dirs["keyframe"] {
		t.Errorf("expected per-workflow subdirectories, got %v", dirs)
	}
	jobDir := filepath.Dir(filepath.Dir(frames[0].Path))
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Errorf("job workdir (incl. subdirs) must be deleted: %v", err)
	}
}

func TestFFmpegSourceExtractBudgetCapsUnion(t *testing.T) {
	requireFFmpeg(t)
	video := makeTestVideo(t, "5")
	cfg := intervalCfg()
	cfg.MaxFrames = 2
	cfg.MaxExtractFrames = 3 // explicit disk bound
	src := NewFFmpegSource(cfg, nil)
	frames, cleanup, err := src.Frames(context.Background(), video, []string{"interval", "keyframe"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(frames) > 3 {
		t.Errorf("extraction budget must cap the union: %d frames", len(frames))
	}
}

func TestFFmpegSourceUnknownWorkflowFails(t *testing.T) {
	src := NewFFmpegSource(intervalCfg(), nil)
	_, _, err := src.Frames(context.Background(), "whatever.mp4", []string{"nope"})
	if err == nil {
		t.Fatal("unknown workflow selection must fail, never silently fall back")
	}
}

func TestValidateBinariesMissing(t *testing.T) {
	cfg := intervalCfg()
	cfg.FFmpegPath = "definitely-not-a-real-binary-xyz"
	if err := ValidateBinaries(cfg); err == nil {
		t.Error("missing binary must fail boot validation")
	}
}
