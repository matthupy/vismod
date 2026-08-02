package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/vismod/vismod/internal/audit"
	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/fetch"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/pipeline"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/ui"
	"github.com/vismod/vismod/pkg/moderation"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the long-running moderation worker",
	Long: `serve consumes the configured job queue with a fixed worker pool,
exposes /metrics, /healthz and /readyz on metrics_addr, and (for the
memory driver) a local HTTP intake on intake_addr (POST /jobs).

Scale-out model: each replica runs a fixed pool of queue.workers
goroutines; horizontal scale comes from adding replicas driven by the
vismod_queue_depth metric (KEDA/HPA). Multi-replica requires
queue.driver=redis.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runServe(cmd.Context())
	},
}

func init() { rootCmd.AddCommand(serveCmd) }

// server is the assembled worker: everything runServe needs, built and
// validated before anything is started or any port is bound.
//
// The split exists so boot wiring can be exercised without entering the
// blocking run loop — a 100-statement func that ends in <-ctx.Done() can
// only be tested end to end, and then only by the one path that happens
// to boot cleanly.
type server struct {
	cfg      config.Config
	log      *slog.Logger
	metrics  *observe.Metrics
	bp       *observe.Backpressure
	health   *observe.Health
	mod      moderation.Moderator
	pipeline *pipeline.Pipeline
	queue    queue.Queue
	tracker  *observe.JobTracker
	sw       *intakeSwitch

	auditLog   *audit.Log
	closeSinks func() error
}

// newServer performs all of boot: validation, adapter construction, sinks,
// audit log, pipeline, queue. Every failure here is a boot failure — the
// caller gets an error and nothing has been started.
//
// On any error the resources already opened are released, so a failed boot
// never leaks a file handle or a Redis connection.
func newServer(cfg config.Config) (*server, error) {
	log := observe.NewLogger(cfg.LogLevel)
	metrics := observe.NewMetrics()
	bp := observe.NewBackpressure(
		cfg.Backpressure.ConsecutiveErrors, cfg.Backpressure.ErrorRatePct,
		cfg.Backpressure.Window, cfg.Backpressure.RecoverySuccesses)
	health := observe.NewHealth(bp)

	// Fail fast (§F.2): ffmpeg/ffprobe present, every workflow passes the
	// guardrails, and the selected adapter's credentials resolve.
	if err := validateFrameBoot(cfg); err != nil {
		return nil, fmt.Errorf("boot validation: %w", err)
	}
	mod, err := buildModerator(cfg, log)
	if err != nil {
		return nil, err
	}
	// After buildModerator: the declaration lives on the adapter, so this
	// cannot run in config.Load. Before instrumentation: the wrapper forwards
	// ModelVersion() but NOT ProviderLabels(), so asserting it here keeps the
	// check independent of what the wrapper happens to forward.
	if err := validateProviderLabelBoot(cfg, mod); err != nil {
		_ = mod.Close()
		return nil, fmt.Errorf("boot validation: %w", err)
	}
	mod = observe.InstrumentModerator(mod, metrics)

	auditLog, err := openAudit(cfg)
	if err != nil {
		_ = mod.Close()
		return nil, err
	}

	sink, closeSinks, err := buildSinks(cfg, os.Stdout, metrics)
	if err != nil {
		_ = mod.Close()
		if auditLog != nil {
			_ = auditLog.Close()
		}
		return nil, err
	}
	fetcher, err := newFetcher(cfg)
	if err != nil {
		_ = mod.Close()
		if auditLog != nil {
			_ = auditLog.Close()
		}
		_ = closeSinks()
		return nil, err
	}
	p := buildPipeline(cfg, mod, sink, auditLog, fetcher, log)
	p.FrameSource = newFrameSource(cfg, log)
	p.OnFetch = fetchRecorder(metrics)

	q, err := newQueue(cfg, log)
	if err != nil {
		_ = mod.Close()
		if auditLog != nil {
			_ = auditLog.Close()
		}
		_ = closeSinks()
		return nil, err
	}
	if cfg.Queue.Driver == "memory" {
		warn := "queue.driver=memory is NON-DURABLE, at-most-once, single-process; a crash loses queued and in-flight jobs — not for production intake (multi-replica requires driver=redis)"
		log.Warn(warn)
		health.AddWarning(warn)
	}
	if rq, ok := q.(*queue.Redisq); ok {
		// Readiness tracks Redis health: an outage flips /readyz to
		// not-ready so ingress stops routing, never black-holing jobs.
		health.AddProbe("redis", func() error {
			pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return rq.Ping(pctx)
		})
	}

	return &server{
		cfg: cfg, log: log, metrics: metrics, bp: bp, health: health,
		mod: mod, pipeline: p, queue: q,
		tracker: observe.NewJobTracker(200), sw: &intakeSwitch{},
		auditLog: auditLog, closeSinks: closeSinks,
	}, nil
}

// close releases everything newServer opened. Ordering mirrors the
// original defers: sinks and the audit log are the two that can lose the
// tail of the record, so their failures are logged rather than swallowed.
func (s *server) close() {
	_ = s.mod.Close()
	if s.auditLog != nil {
		// A failed close on the audit log means the last decision may not
		// have reached disk, and an audit trail that silently loses its
		// tail is worse than one that admits the gap.
		if err := s.auditLog.Close(); err != nil {
			s.log.Error("closing audit log failed", "err", err)
		}
	}
	if err := s.closeSinks(); err != nil {
		s.log.Error("closing result sinks failed", "err", err)
	}
}

// runServe boots the worker and runs it until a signal arrives.
func runServe(parent context.Context) error {
	s, err := newServer(cfg)
	if err != nil {
		return err
	}
	defer s.close()

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return s.run(ctx)
}

// run starts the worker pool, the metrics/intake/UI servers and the depth
// poller, then blocks until ctx is done and drains.
//
// Cancelling ctx is the ONLY exit: a drain failure is reported, never
// silently accepted, because jobs left in flight at exit were accepted and
// never answered.
func (s *server) run(ctx context.Context) error {
	cfg, log, metrics, p, q := s.cfg, s.log, s.metrics, s.pipeline, s.queue
	bp, tracker := s.bp, s.tracker

	// Worker handler: pipeline + backpressure + metrics + UI job feed.
	handler := func(hctx context.Context, j queue.Job) (queue.Disposition, error) {
		metrics.WorkersActive.Inc()
		defer metrics.WorkersActive.Dec()
		env, disp, perr := p.ProcessJob(hctx, j)
		verdict := string(moderation.VerdictError)
		switch {
		case env.Result != nil && disp == queue.Ack:
			verdict = string(env.Result.Overall.Verdict)
		case env.Result == nil && disp == queue.Ack:
			verdict = "skip" // gated empty-video override: acked, no verdict
		}
		if disp != queue.Retry { // retries aren't final outcomes
			metrics.JobsTotal.WithLabelValues(verdict).Inc()
			// The envelope's Source is the RECORDED one: for a url job that
			// is the redacted ref, never the presigned original sitting in
			// j.Source.Ref. Falling back to the job's own ref is safe only
			// for non-url kinds.
			ref, mediaType := env.Source.Ref, env.Source.MediaType
			if ref == "" && j.Source.Kind != "url" {
				ref, mediaType = j.Source.Ref, j.Source.MediaType
			}
			rec := observe.JobRecord{
				ID:         string(j.ID),
				Ref:        ref,
				MediaType:  mediaType,
				Verdict:    verdict,
				FinishedAt: env.FinishedAt,
				DurationMS: env.FinishedAt.Sub(env.StartedAt).Milliseconds(),
			}
			if env.Result != nil {
				if tc := env.Result.Overall.TopCategory; tc != nil {
					rec.TopCategory = string(*tc)
				}
				rec.MaxScore = env.Result.Overall.MaxScore
				rec.Confidence = env.Result.Overall.Confidence
				rec.FramesScanned = len(env.Result.Frames)
				for _, fr := range env.Result.Frames {
					for _, c := range fr.Categories {
						if c.Flagged {
							rec.FramesFlagged++
							break
						}
					}
				}
				metrics.FramesScannedTotal.Add(float64(rec.FramesScanned))
				metrics.JobFrames.WithLabelValues(rec.MediaType).Observe(float64(rec.FramesScanned))
			}
			tracker.Record(rec)
		}
		bp.Record(disp == queue.Ack)
		return disp, perr
	}
	if err := q.Start(ctx, handler); err != nil {
		return err
	}

	// Depth gauges poll loop (uniform across drivers; the autoscaling signal).
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if d, err := q.QueueDepth(ctx); err == nil {
					metrics.QueueDepth.Set(float64(d))
				}
				if dlq := dlqOf(q); dlq != nil {
					if d, err := dlq.Depth(ctx); err == nil {
						metrics.DeadletterDepth.Set(float64(d))
					}
				}
			}
		}
	}()

	sw := s.sw
	metricsSrv := observe.Serve(cfg.MetricsAddr, metrics, s.health, log)
	intakeSrv := serveIntake(cfg, q, bp, sw, log)

	var uiSrv *http.Server
	if cfg.UI.Enabled {
		uiSrv = ui.New(cfg, q, dlqOf(q), statesOf(q), activeOf(q), sw, tracker, log).Start()
	}

	if cfg.Source.URL.Enabled {
		log.Warn("url media sources are ENABLED; jobs may cause outbound fetches",
			"allow_hosts", strings.Join(cfg.Source.URL.AllowHosts, ","))
	}

	log.Info("vismod serve started",
		"adapter", s.mod.Name(), "queue_driver", cfg.Queue.Driver,
		"workers", cfg.Queue.Workers, "metrics_addr", cfg.MetricsAddr,
		"intake_addr", cfg.IntakeAddr)

	<-ctx.Done()
	log.Info("shutdown signal received; draining", "drain_timeout", cfg.Queue.DrainTimeout)

	// Stop intake first (no new jobs), then drain the queue gracefully.
	shCtx, cancel := context.WithTimeout(context.Background(), cfg.Queue.DrainTimeout+5*time.Second)
	defer cancel()
	if intakeSrv != nil {
		_ = intakeSrv.Shutdown(shCtx)
	}
	if uiSrv != nil {
		_ = uiSrv.Shutdown(shCtx)
	}
	drainErr := q.Close(shCtx)
	_ = metricsSrv.Shutdown(shCtx)
	if drainErr != nil {
		return fmt.Errorf("drain: %w", drainErr)
	}
	log.Info("drained cleanly")
	return nil
}

func newQueue(cfg config.Config, log *slog.Logger) (queue.Queue, error) {
	qc := queue.QueueConfig{
		Workers:       cfg.Queue.Workers,
		Buffer:        cfg.Queue.Buffer,
		MaxRetries:    cfg.Queue.MaxRetries,
		RetryBackoff:  cfg.Queue.RetryBackoff,
		DrainTimeout:  cfg.Queue.DrainTimeout,
		JobTimeout:    cfg.Queue.JobTimeout,
		DeadLetterMax: cfg.Queue.DeadLetterMax,
	}
	switch cfg.Queue.Driver {
	case "memory":
		return queue.NewMemq(qc, log), nil
	case "redis":
		rdb := redis.NewClient(&redis.Options{
			Addr:     cfg.Queue.Redis.Addr,
			Password: config.Secret()("queue.redis.password"), // env-only
			DB:       cfg.Queue.Redis.DB,
		})
		q := queue.NewRedisq(qc, rdb, "vismod", log)
		// Boot validation (§F.2): Redis is the SPOF — an unreachable
		// instance is a fatal operator error, not a per-job failure.
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := q.Ping(pctx); err != nil {
			return nil, fmt.Errorf("queue.driver=redis: cannot reach %s: %w", cfg.Queue.Redis.Addr, err)
		}
		return q, nil
	default:
		return nil, fmt.Errorf("unknown queue.driver %q", cfg.Queue.Driver)
	}
}

func dlqOf(q queue.Queue) queue.DeadLetterSink {
	switch d := q.(type) {
	case *queue.Memq:
		return d.DLQ()
	case *queue.Redisq:
		return d.DLQ()
	}
	return nil
}

// intakeSwitch is the operator pause/resume control (the UI's whole
// "manage workers" surface, by design).
type intakeSwitch struct{ paused atomic.Bool }

func (s *intakeSwitch) PauseIntake()       { s.paused.Store(true) }
func (s *intakeSwitch) ResumeIntake()      { s.paused.Store(false) }
func (s *intakeSwitch) IntakePaused() bool { return s.paused.Load() }

func statesOf(q queue.Queue) func() map[queue.JobID]string {
	if m, ok := q.(*queue.Memq); ok {
		return m.States
	}
	return nil
}

func activeOf(q queue.Queue) func() int {
	switch d := q.(type) {
	case *queue.Memq:
		return d.ActiveWorkers
	case *queue.Redisq:
		return d.ActiveWorkers
	}
	return nil
}

// intakeRequest is the POST /jobs body: a Source plus an optional list
// of extraction workflows for video inputs (any number; frames are the
// union). Workflow names must exist in the validated config set.
type intakeRequest struct {
	Kind      string   `json:"kind"`
	Ref       string   `json:"ref"`
	MediaType string   `json:"media_type"`
	Workflows []string `json:"workflows,omitempty"`
	// DedupThreshold overrides frames.dedup for this job: omitted
	// inherits the config; 0..64 enables dedup at that Hamming distance;
	// -1 disables it for this job.
	DedupThreshold *int `json:"dedup_threshold,omitempty"`
}

// validateURLIntake is the intake-side half of url validation. The
// execution-side half runs in the fetcher, because a job can also arrive
// straight onto the redis queue without passing through here.
func validateURLIntake(cfg config.Config, rawRef string) error {
	if !cfg.Source.URL.Enabled {
		return fmt.Errorf(`kind "url" requires source.url.enabled=true`)
	}
	allow := make(map[string]bool, len(cfg.Source.URL.AllowHosts))
	for _, h := range cfg.Source.URL.AllowHosts {
		allow[strings.ToLower(strings.TrimSpace(h))] = true
	}
	if _, err := fetch.ValidateURL(rawRef, allow); err != nil {
		return err
	}
	return nil
}

// serveIntake exposes the dev/demo HTTP intake: POST /jobs with a JSON
// body. Rejections are retryable signals (503 + Retry-After), never
// silent drops. Payloads carry file refs only, never media bytes.
func serveIntake(cfg config.Config, q queue.Queue, bp *observe.Backpressure, sw *intakeSwitch, log *slog.Logger) *http.Server {
	addr := cfg.IntakeAddr
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		if !bp.Ready() {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "intake paused: sustained provider failure (backpressure)", http.StatusServiceUnavailable)
			return
		}
		if sw != nil && sw.IntakePaused() {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "intake paused by operator", http.StatusServiceUnavailable)
			return
		}
		var req intakeRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Ref == "" {
			http.Error(w, `bad request: ref is required — {"kind":"file|url","ref":"<abs path|https url>","media_type":"image|video","workflows":["name",...]} (workflows optional)`, http.StatusBadRequest)
			return
		}
		switch req.Kind {
		case "file":
		case "url":
			if err := validateURLIntake(cfg, req.Ref); err != nil {
				// err is built from the REDACTED url — never echo a query
				// string back to the caller or into the access log.
				http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
				return
			}
		default:
			http.Error(w, `bad request: kind must be "file" or "url"`, http.StatusBadRequest)
			return
		}
		if req.MediaType == "" {
			// For a url, infer from the redacted path: a query string can
			// carry an extension that is not the asset's.
			ref := req.Ref
			if req.Kind == "url" {
				ref, _ = fetch.Redact(ref)
			}
			req.MediaType = mediaTypeFor(ref)
		}
		if err := validateWorkflowSelection(cfg, req.Workflows); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateDedupThreshold(req.DedupThreshold); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		ref := req.Ref
		if req.Kind == "file" {
			abs, err := filepath.Abs(req.Ref)
			if err != nil {
				http.Error(w, "bad path", http.StatusBadRequest)
				return
			}
			ref = abs
		}
		j := queue.Job{
			ID:             queue.JobID(fmt.Sprintf("job-%d", time.Now().UnixNano())),
			Source:         moderation.Source{Kind: req.Kind, Ref: ref, MediaType: req.MediaType},
			Workflows:      req.Workflows,
			DedupThreshold: req.DedupThreshold,
			SubmittedAt:    time.Now().UTC(),
		}
		id, err := q.Enqueue(r.Context(), j)
		// Identity comparison, not errors.Is: these are the queue's own
		// sentinels returned unwrapped, and matching a wrapped variant
		// would change which rejections get a Retry-After.
		switch err {
		case nil:
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"job_id": string(id)})
		case queue.ErrDeadLetterFull, queue.ErrQueueFull:
			w.Header().Set("Retry-After", "30")
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		case queue.ErrQueueClosed:
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("intake server failed", "addr", addr, "err", err)
		}
	}()
	return srv
}
