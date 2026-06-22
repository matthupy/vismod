package cli

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/matthupy/vismod/internal/observe"
	"github.com/matthupy/vismod/internal/pipeline"
	"github.com/matthupy/vismod/internal/queue"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the long-running moderation worker",
		Long: "serve runs the FIFO queue and worker pool. With the memory driver " +
			"it is a non-durable, single-process dev worker. Pipe newline-delimited " +
			"file paths on stdin to enqueue jobs; the process drains gracefully on " +
			"SIGINT/SIGTERM.",
		RunE: runServe,
	}
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, log, err := loadConfigAndLogger()
	if err != nil {
		return err
	}

	// Boot validation (§F.2): videosift execs ffmpeg+ffprobe for every video
	// job. Validate once at boot so a missing binary is a clear operator error,
	// not a per-job failure surfacing as error-verdict envelopes.
	if err := probeFrameSource(cfg); err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	resultSink := result.NewJSONLSink(os.Stdout)
	dlqSink := result.NewJSONLSink(os.Stderr)

	// Metrics (§F.6): one registry, instruments the adapter + pipeline and backs
	// /metrics. Depth gauges are registered below once the queue exists.
	metrics := observe.NewMetrics()

	p, mod, err := buildPipeline(cfg, resultSink, log, metrics)
	if err != nil {
		return err
	}
	defer mod.Close()

	// Cross-process dedup gate (§L, issue #9): under the at-least-once redis
	// driver a redelivery to a fresh process/replica must not double-write the
	// Sink/audit. Non-nil only for driver=redis; memq is single-process.
	deduper, closeDedup, err := buildDeduper(cfg, log)
	if err != nil {
		return err
	}
	defer func() { _ = closeDedup() }()
	p.Dedup = deduper

	q, qWarnings, err := buildQueue(cfg, dlqSink, metrics, log)
	if err != nil {
		return err
	}

	// Scrape-time depth gauges read live queue state (uniform QueueDepth across
	// drivers; memq exposes its DLQ depth). The depth read is bounded by a short
	// timeout: memq is instant, but a future redis driver (M5) on a slow/down
	// backend must NOT block the /metrics handler (a hung scrape blinds alerting
	// exactly when the queue is sick). On failure the gauge reports 0 and bumps
	// vismod_queue_depth_scrape_errors_total.
	metrics.RegisterQueueDepth(
		func() (float64, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			n, err := q.QueueDepth(ctx)
			return float64(n), err
		},
		func() float64 { return float64(q.DeadLetterDepth()) },
		func() float64 { return float64(q.ActiveDepth()) },
	)

	for _, w := range qWarnings {
		log.Warn(w)
	}

	health := observe.NewHealth(cfg.MetricsAddr, log, metrics.Registry())

	// Redis driver boot validation (§F.2): PING fails fast so an unreachable
	// Redis is a clear operator error, not silently black-holed jobs. The same
	// ping is registered as a live /readyz probe so a Redis outage flips
	// readiness (backpressure) instead of accepting jobs it cannot durably hold.
	checks := map[string]string{"ffmpeg": "ok", "adapter": "ok"}
	if pinger, ok := q.(queue.Pinger); ok {
		pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := pinger.Ping(pingCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("serve: redis unreachable at boot (driver=redis): %w", err)
		}
		checks["redis"] = "ok"
		health.SetReadinessProbe("redis", pinger.Ping)
	}

	health.Start()

	// §L: the worker's loaded-model fingerprint, computed ONCE. Stamped on every
	// enqueue and checked on every dequeue so a rolling deploy never silently
	// moderates with the wrong model.
	workerFP := cfg.ModelFingerprint()

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	if err := q.Start(workerCtx, jobHandler(p, workerFP, metrics, log)); err != nil {
		return err
	}
	// Readiness carries boot-validation detail (§F.2): ffmpeg/ffprobe + adapter
	// (+ redis when applicable), plus any driver durability warnings (§D.3).
	health.SetReadyDetail(observe.ReadyDetail{
		Ready:       true,
		AdapterName: cfg.Adapter.Name,
		Checks:      checks,
		Warnings:    qWarnings,
	})
	log.Info("serve ready", "workers", cfg.Queue.Workers, "health_addr", cfg.MetricsAddr)

	// Ingress: enqueue file paths from stdin (if any).
	go enqueueFromStdin(workerCtx, q, workerFP, log)

	// Wait for shutdown signal.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()

	log.Info("shutdown signal received; draining")
	health.SetReady(false)

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.Queue.DrainTimeout+5*time.Second)
	defer cancelDrain()
	if err := q.Close(drainCtx); err != nil {
		log.Warn("queue close", "err", err)
	}
	cancelWorkers()
	_ = health.Stop(drainCtx)
	log.Info("serve stopped")
	return nil
}

// jobHandler adapts the pipeline to the queue Handler. The pipeline already
// writes a fail-safe error-verdict envelope on a could-not-evaluate decision
// (that is an Ack); a non-nil error here is an infrastructure failure (e.g.
// sink write) and is retried.
//
// §L one-model-cluster-wide invariant: workerFP is this worker's loaded-model
// fingerprint (computed once in runServe). Before processing, the job's stamped
// fingerprint is checked against it:
//   - == workerFP : normal processing.
//   - non-empty, != workerFP : a rolling deploy landed a job that requires a
//     DIFFERENT model on this worker. DeadLetter (not Retry — the mismatch is
//     deterministic; retrying loops on the same wrong replica), never call
//     Process, so no wrong-model verdict is ever emitted. The honest scope is a
//     misconfiguration / rollout-skew guard, not an anti-adversary control.
//   - empty : a pre-feature (older-binary) job. Process it, but surface it
//     (WARN + metric) — never silently process an unknown identity unbounded.
func jobHandler(p *pipeline.Pipeline, workerFP string, m *observe.Metrics, log *slog.Logger) queue.Handler {
	return func(ctx context.Context, j queue.Job) (queue.Disposition, error) {
		switch {
		case j.ModelFingerprint == "":
			m.RecordModelMismatch("unstamped")
			log.Warn("job has no model fingerprint; processing (pre-feature job?)", "job_id", j.ID)
		case j.ModelFingerprint != workerFP:
			m.RecordModelMismatch("mismatch")
			err := fmt.Errorf("model fingerprint mismatch: job=%s worker=%s", fpPrefix(j.ModelFingerprint), fpPrefix(workerFP))
			log.Warn("dead-lettering job: model fingerprint mismatch",
				"job_id", j.ID, "job_fp", fpPrefix(j.ModelFingerprint), "worker_fp", fpPrefix(workerFP))
			return queue.DeadLetter, err
		}
		if err := p.Process(ctx, j.ID, j.Source); err != nil {
			return queue.Retry, err
		}
		return queue.Ack, nil
	}
}

// fpPrefix shortens a fingerprint hash for logs/DLQ envelopes — enough to
// disambiguate two model versions without dumping the full 64-hex digest.
func fpPrefix(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	if fp == "" {
		return "<none>"
	}
	return fp
}

// enqueueJob is the SINGLE stamping ingress: every enqueue goes through here so
// that, in steady state, an empty Job.ModelFingerprint can only mean a
// pre-feature (older-binary) job. memq: enqueuer == worker == same process ==
// same config => fingerprints always match (guard is a no-op). asynq: a rolling
// deploy may stamp model X and have a worker loaded with model Y dequeue it =>
// jobHandler dead-letters it.
func enqueueJob(ctx context.Context, q queue.Queue, src moderation.Source, fp string) (result.JobID, error) {
	return q.Enqueue(ctx, queue.Job{Source: src, ModelFingerprint: fp})
}

func enqueueFromStdin(ctx context.Context, q queue.Queue, fp string, log *slog.Logger) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		path := strings.TrimSpace(sc.Text())
		if path == "" {
			continue
		}
		if _, err := enqueueJob(ctx, q, moderation.Source{Kind: "file", Ref: path}, fp); err != nil {
			log.Error("enqueue failed", "path", path, "err", err)
			return
		}
	}
}
