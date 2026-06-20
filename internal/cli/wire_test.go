package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matthupy/videosift"
	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/observe"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// buildFrameSource must translate config into a videosift-backed source,
// carrying the frame knobs (so a video job actually extracts via videosift,
// not the M0 fake).
func TestBuildFrameSourceUsesVideosift(t *testing.T) {
	cfg := config.Config{Frames: config.FramesConfig{
		WorkDir:     t.TempDir(),
		MaxFrames:   16,
		Scene:       true,
		Keyframe:    true,
		Temporal:    true,
		MPDecimate:  true,
		FFmpegPath:  "ffmpeg",
		FFprobePath: "ffprobe",
	}}
	fs := buildFrameSource(cfg)
	if _, ok := fs.(*frames.VideosiftSource); !ok {
		t.Fatalf("buildFrameSource returned %T, want *frames.VideosiftSource", fs)
	}
}

// buildPipeline with a non-nil Metrics must instrument the adapter (wrap it)
// and record jobs_total{verdict} after processing. With nil Metrics the
// pipeline stays unmetered (one-shot scan path).
func TestBuildPipelineWiresMetrics(t *testing.T) {
	cfg := config.Config{
		Adapter: config.AdapterConfig{Name: "stub"},
		Frames:  config.FramesConfig{MaxFrames: 8, Concurrency: 1},
	}

	// Nil metrics: unmetered, raw moderator.
	plain, modPlain, err := buildPipeline(cfg, result.NewJSONLSink(&strings.Builder{}), observe.NewLogger("error"), nil)
	if err != nil {
		t.Fatalf("buildPipeline(nil metrics): %v", err)
	}
	defer modPlain.Close()
	if plain.Metrics != nil {
		t.Error("nil metrics should leave Pipeline.Metrics nil")
	}
	if plain.Moderator != modPlain {
		t.Error("nil metrics should not wrap the moderator")
	}

	// With metrics: instrumented moderator + recorder set.
	m := observe.NewMetrics()
	p, mod, err := buildPipeline(cfg, result.NewJSONLSink(&strings.Builder{}), observe.NewLogger("error"), m)
	if err != nil {
		t.Fatalf("buildPipeline(metrics): %v", err)
	}
	defer mod.Close()
	if p.Metrics == nil {
		t.Error("metrics should set Pipeline.Metrics")
	}
	if p.Moderator == mod {
		t.Error("metrics should wrap the moderator (instrumented != raw)")
	}

	// Process a still image through the metered pipeline → one job recorded.
	img := filepath.Join(t.TempDir(), "x.jpg")
	if err := os.WriteFile(img, []byte("not-a-real-jpeg-but-stub-ignores-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(context.Background(), result.JobID("t1"), moderation.Source{Kind: "file", Ref: img}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := testutil.CollectAndCount(m.Registry(), "vismod_jobs_total"); got == 0 {
		t.Error("expected vismod_jobs_total to record the processed job")
	}
}

// probeFrameSource must fail fast (boot validation) when ffmpeg/ffprobe are
// absent, wrapping videosift.ErrNoBinaries.
func TestProbeFrameSourceMissingBinaries(t *testing.T) {
	cfg := config.Config{Frames: config.FramesConfig{
		FFmpegPath:  "no-such-ffmpeg-xyz",
		FFprobePath: "no-such-ffprobe-xyz",
	}}
	err := probeFrameSource(cfg)
	if err == nil {
		t.Fatal("probeFrameSource with bogus binaries returned nil, want error")
	}
	if !errors.Is(err, videosift.ErrNoBinaries) {
		t.Errorf("err = %v, want errors.Is ErrNoBinaries", err)
	}
}
