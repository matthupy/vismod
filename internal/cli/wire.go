package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/matthupy/vismod/internal/audit"
	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/hashmatch"
	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/internal/observe"
	"github.com/matthupy/vismod/internal/pipeline"
	"github.com/matthupy/vismod/internal/queue"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/internal/review"
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
		// §G.8: default to the log-only Diverter so a potential-CSAM frame is
		// always surfaced. Production supplies an encrypted review channel.
		Diverter: review.NewLogDiverter(log),
	}
	if cfg.AuditPath != "" {
		al, aerr := audit.Open(cfg.AuditPath)
		if aerr != nil {
			_ = mod.Close()
			return nil, nil, aerr
		}
		p.Audit = al
	}
	if metrics != nil {
		p.Moderator = metrics.Instrument(mod)
		p.Metrics = metrics
	}
	return p, mod, nil
}

// redisQueueName is the asynq queue namespace. A single logical queue keeps the
// FIFO contract simple; per-model-version namespacing is an M5 multi-replica
// concern (§L).
const redisQueueName = "vismod"

// memqDurabilityWarning surfaces the memq durability boundary (§D.3) on /readyz
// and at boot so an operator never mistakes the dev queue for production intake.
const memqDurabilityWarning = "queue driver=memory is non-durable, single-process (dev/CLI only); use driver=redis for production intake"

// buildQueue constructs the configured queue driver behind the DepthReporter
// interface (so serve wires depth metrics uniformly) and returns any durability
// warnings to surface. The memq->asynq swap is behavior-preserving: the same
// handler Disposition yields the same retry/DLQ outcome on both drivers.
func buildQueue(cfg config.Config, dlq result.Sink, log *slog.Logger) (queue.DepthReporter, []string, error) {
	qc := queue.QueueConfig{
		Workers:       cfg.Queue.Workers,
		Buffer:        cfg.Queue.Buffer,
		MaxRetries:    cfg.Queue.MaxRetries,
		RetryBackoff:  cfg.Queue.RetryBackoff,
		DrainTimeout:  cfg.Queue.DrainTimeout,
		JobTimeout:    cfg.Queue.JobTimeout,
		DeadLetterMax: cfg.Queue.DeadLetterMax,
		DeadLetter:    dlq,
	}
	switch cfg.Queue.Driver {
	case "memory":
		q, err := queue.NewMemQueue(qc, log)
		if err != nil {
			return nil, nil, err
		}
		return q, []string{memqDurabilityWarning}, nil
	case "redis":
		q, err := queue.NewAsynqQueue(qc, cfg.Queue.RedisAddr, redisQueueName, log)
		if err != nil {
			return nil, nil, err
		}
		return q, nil, nil
	default:
		return nil, nil, fmt.Errorf("serve: unsupported queue.driver %q", cfg.Queue.Driver)
	}
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
