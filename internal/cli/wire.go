package cli

import (
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

// buildPipeline wires a Pipeline from config with the given sink. The v1/M0
// FrameSource is a fake (videosift-backed source lands in M2); the HashMatcher
// is the no-op default.
func buildPipeline(cfg config.Config, sink result.Sink, log *slog.Logger) (*pipeline.Pipeline, moderation.Moderator, error) {
	mod, err := buildModerator(cfg)
	if err != nil {
		return nil, nil, err
	}
	p := &pipeline.Pipeline{
		Moderator: mod,
		Frames:    &frames.FakeFrameSource{}, // M2: videosift-backed source
		Matcher:   hashmatch.NoOp{},
		Sink:      sink,
		Cfg:       cfg,
		Log:       log,
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
