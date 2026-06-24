package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/matthupy/vismod/internal/audit"
	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/dedup"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/hashmatch"
	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/internal/observe"
	"github.com/matthupy/vismod/internal/pipeline"
	"github.com/matthupy/vismod/internal/queue"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/internal/review"
	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/redis/go-redis/v9"
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

// dedupPingTimeout bounds the deduper's boot ping so a dropped (not refused)
// Redis connection cannot hang serve startup indefinitely.
const dedupPingTimeout = 5 * time.Second

// buildDeduper wires the cross-process dedup gate (§L, issue #9). It returns a
// non-nil Deduper ONLY for the redis driver — the memory driver is
// single-process so the in-memory Sink/audit guards already suffice (a nil
// Deduper). The returned closer releases the Redis client on shutdown.
func buildDeduper(cfg config.Config, log *slog.Logger) (pipeline.Deduper, func() error, error) {
	if cfg.Queue.Driver != "redis" {
		return nil, func() error { return nil }, nil
	}
	if cfg.Queue.DedupTTL <= 0 {
		return nil, nil, fmt.Errorf("serve: queue.dedup_ttl must be > 0 for the redis driver")
	}
	// Correctness depends on dedup_ttl OUTLIVING the redelivery window: a claim
	// that expires while the same job can still be redelivered silently reopens
	// the double-write this gate exists to close. Backoff is LINEAR — retryDelay
	// returns (n+1)*RetryBackoff (queue/asynqq.go) — so the true span across all
	// M attempts is the triangular sum RetryBackoff * M*(M+1)/2, NOT
	// MaxRetries*RetryBackoff (which undercounts by a factor of (M+1)/2). M*(M+1)/2
	// is 0 when M=0, which is correct. asynq's own completed-task retention extends
	// the window further, so this remains a conservative floor, not the full budget.
	// Warn loudly rather than fail — operators may run a shorter TTL deliberately —
	// but make the invisible failure mode visible at boot, not when it bites in prod.
	m := cfg.Queue.MaxRetries
	if retryBudget := cfg.Queue.RetryBackoff * time.Duration(m*(m+1)/2); cfg.Queue.DedupTTL <= retryBudget {
		log.Warn("queue.dedup_ttl is below the retry budget; a redelivery after the claim expires can double-write",
			"dedup_ttl", cfg.Queue.DedupTTL, "retry_budget", retryBudget)
	}
	client := redis.NewClient(&redis.Options{Addr: cfg.Queue.RedisAddr})
	// Validate the deduper's OWN connection at boot. It opens a second pool to the
	// same Redis the queue uses, but the queue's Pinger does not cover this client
	// — without this ping a deduper-only connection fault would first surface on
	// job #1 instead of at startup. Fail closed: a broken dedup path must not run.
	// Bound the ping: a REFUSED connection fails fast, but a DROPPED one
	// (firewall/blackhole) would hang boot indefinitely under context.Background().
	// A bounded context fails closed within a known window instead of wedging serve.
	pingCtx, cancel := context.WithTimeout(context.Background(), dedupPingTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("serve: dedup redis ping: %w", err)
	}
	return dedup.NewRedisDeduper(client, cfg.Queue.DedupTTL), client.Close, nil
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
func buildQueue(cfg config.Config, dlq result.Sink, metrics *observe.Metrics, log *slog.Logger) (queue.DepthReporter, []string, error) {
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
	// Wire queue-layer lifecycle counters (vismod_jobs_completed_total /
	// _failed_total). Guarded so a nil metrics leaves QueueConfig.Metrics a true
	// nil interface (a typed-nil *observe.Metrics would defeat the nil-safe guard).
	if metrics != nil {
		qc.Metrics = metrics
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
