package cli

import (
	"context"
	"log/slog"
	"os"

	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/hashmatch"
	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/internal/observe"
	"github.com/matthupy/vismod/internal/pipeline"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// buildModerator instantiates the single configured adapter, passing an
// env-backed secret accessor (VISMOD_<NAME>_<KEY>).
func buildModerator(cfg config.Config) (moderation.Moderator, error) {
	ac := moderate.AdapterConfig{
		Name:    cfg.Adapter.Name,
		Options: cfg.Adapter.Options,
		Secret: func(key string) string {
			return os.Getenv("VISMOD_" + key)
		},
	}
	return moderate.New(cfg.Adapter.Name, ac)
}

// buildFrameSource wires the videosift-backed FrameSource (M2) from config,
// carrying the frame-extraction knobs. The pipeline lazily decodes each frame;
// this source owns only extraction + WorkDir lifecycle.
func buildFrameSource(cfg config.Config) frames.FrameSource {
	return frames.NewVideosiftSource(frames.VideosiftOptions{
		WorkDir:     cfg.Frames.WorkDir,
		MaxFrames:   cfg.Frames.MaxFrames,
		Scene:       cfg.Frames.Scene,
		Keyframe:    cfg.Frames.Keyframe,
		Temporal:    cfg.Frames.Temporal,
		MPDecimate:  cfg.Frames.MPDecimate,
		FFmpegPath:  cfg.Frames.FFmpegPath,
		FFprobePath: cfg.Frames.FFprobePath,
	})
}

// probeFrameSource validates ffmpeg/ffprobe at boot (§F.2) so a missing binary
// surfaces as a clear operator error, not a per-job failure. Wraps
// videosift.ErrNoBinaries.
func probeFrameSource(cfg config.Config) error {
	src := frames.NewVideosiftSource(frames.VideosiftOptions{
		FFmpegPath:  cfg.Frames.FFmpegPath,
		FFprobePath: cfg.Frames.FFprobePath,
	})
	return src.Probe(context.Background())
}

// buildPipeline wires a Pipeline from config with the given sink. The
// FrameSource is videosift-backed (M2); the HashMatcher is the no-op default.
// When metrics is non-nil (serve), the Moderator is wrapped to record adapter
// latency/errors and the pipeline records jobs_total{verdict}. The returned
// moderation.Moderator is the UNWRAPPED adapter — callers Close() that one.
func buildPipeline(cfg config.Config, sink result.Sink, log *slog.Logger, metrics *observe.Metrics) (*pipeline.Pipeline, moderation.Moderator, error) {
	mod, err := buildModerator(cfg)
	if err != nil {
		return nil, nil, err
	}
	p := &pipeline.Pipeline{
		Moderator: mod,
		Frames:    buildFrameSource(cfg),
		Matcher:   hashmatch.NoOp{},
		Sink:      sink,
		Cfg:       cfg,
		Log:       log,
	}
	if metrics != nil {
		p.Moderator = metrics.Instrument(mod)
		p.Metrics = metrics
	}
	return p, mod, nil
}

// loadConfigAndLogger is the shared command bootstrap.
func loadConfigAndLogger() (config.Config, *slog.Logger, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	log := observe.NewLogger(cfg.LogLevel)
	return cfg, log, nil
}
