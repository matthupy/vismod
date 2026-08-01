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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
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

func runServe(parent context.Context) error {
	log := observe.NewLogger(cfg.LogLevel)
	metrics := observe.NewMetrics()
	bp := observe.NewBackpressure(
		cfg.Backpressure.ConsecutiveErrors, cfg.Backpressure.ErrorRatePct,
		cfg.Backpressure.Window, cfg.Backpressure.RecoverySuccesses)
	health := observe.NewHealth(bp)

	// Fail fast (§F.2): ffmpeg/ffprobe present, every workflow passes the
	// guardrails, and the selected adapter's credentials resolve.
	if err := validateFrameBoot(cfg); err != nil {
		return fmt.Errorf("boot validation: %w", err)
	}
	mod, err := buildModerator(cfg, log)
	if err != nil {
		return err
	}
	defer mod.Close()
	// After buildModerator: the declaration lives on the adapter, so this
	// cannot run in config.Load. Before instrumentation: the wrapper forwards
	// ModelVersion() but NOT ProviderLabels(), so asserting it here keeps the
	// check independent of what the wrapper happens to forward.
	if err := validateProviderLabelBoot(cfg, mod); err != nil {
		return fmt.Errorf("boot validation: %w", err)
	}
	mod = observe.InstrumentModerator(mod, metrics)

	auditLog, err := openAudit(cfg)
	if err != nil {
		return err
	}
	if auditLog != nil {
		defer auditLog.Close()
	}

	sink, closeSinks, err := buildSinks(cfg, os.Stdout, metrics)
	if err != nil {
		return err
	}
	defer func() {
		if err := closeSinks(); err != nil {
			log.Error("closing result sinks failed", "err", err)
		}
	}()
	p := buildPipeline(cfg, mod, sink, auditLog, log)
	p.FrameSource = newFrameSource(cfg, log)

	q, err := newQueue(cfg, log)
	if err != nil {
		return err
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

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Worker handler: pipeline + backpressure + metrics + UI job feed.
	tracker := observe.NewJobTracker(200)
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
			rec := observe.JobRecord{
				ID:         string(j.ID),
				Ref:        j.Source.Ref,
				MediaType:  j.Source.MediaType,
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

	sw := &intakeSwitch{}
	metricsSrv := observe.Serve(cfg.MetricsAddr, metrics, health, log)
	intakeSrv := serveIntake(cfg, q, bp, sw, log)

	var uiSrv *http.Server
	if cfg.UI.Enabled {
		uiSrv = ui.New(cfg, q, dlqOf(q), statesOf(q), activeOf(q), sw, tracker, log).Start()
	}

	log.Info("vismod serve started",
		"adapter", mod.Name(), "queue_driver", cfg.Queue.Driver,
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
		if req.Kind != "file" || req.Ref == "" {
			http.Error(w, `bad request: v1 accepts {"kind":"file","ref":"<abs path>","media_type":"image|video","workflows":["name",...]} (workflows optional)`, http.StatusBadRequest)
			return
		}
		if req.MediaType == "" {
			req.MediaType = mediaTypeFor(req.Ref)
		}
		if err := validateWorkflowSelection(cfg, req.Workflows); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateDedupThreshold(req.DedupThreshold); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		abs, err := filepath.Abs(req.Ref)
		if err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		j := queue.Job{
			ID:             queue.JobID(fmt.Sprintf("job-%d", time.Now().UnixNano())),
			Source:         moderation.Source{Kind: req.Kind, Ref: abs, MediaType: req.MediaType},
			Workflows:      req.Workflows,
			DedupThreshold: req.DedupThreshold,
			SubmittedAt:    time.Now().UTC(),
		}
		id, err := q.Enqueue(r.Context(), j)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"job_id": string(id)})
		case err == queue.ErrDeadLetterFull, err == queue.ErrQueueFull:
			w.Header().Set("Retry-After", "30")
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		case err == queue.ErrQueueClosed:
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
