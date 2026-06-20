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

	if cfg.Queue.Driver != "memory" {
		return fmt.Errorf("serve: queue.driver=%q not supported in v1 (redis is M5)", cfg.Queue.Driver)
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

	q, err := queue.NewMemQueue(queue.QueueConfig{
		Workers:       cfg.Queue.Workers,
		Buffer:        cfg.Queue.Buffer,
		MaxRetries:    cfg.Queue.MaxRetries,
		RetryBackoff:  cfg.Queue.RetryBackoff,
		DrainTimeout:  cfg.Queue.DrainTimeout,
		JobTimeout:    cfg.Queue.JobTimeout,
		DeadLetterMax: cfg.Queue.DeadLetterMax,
		DeadLetter:    dlqSink,
	}, log)
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
	)

	// DURABILITY WARNING: memory driver is non-durable; a crash loses jobs.
	const memqWarning = "queue driver=memory is non-durable, single-process (dev/CLI only); use driver=redis for production intake"
	log.Warn(memqWarning)

	health := observe.NewHealth(cfg.MetricsAddr, log, metrics.Registry())
	health.Start()

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	if err := q.Start(workerCtx, jobHandler(p)); err != nil {
		return err
	}
	// Readiness carries boot-validation detail (§F.2): ffmpeg/ffprobe probed
	// above, plus the memq non-durability warning (§D.3).
	health.SetReadyDetail(observe.ReadyDetail{
		Ready:       true,
		AdapterName: cfg.Adapter.Name,
		Checks:      map[string]string{"ffmpeg": "ok", "adapter": "ok"},
		Warnings:    []string{memqWarning},
	})
	log.Info("serve ready", "workers", cfg.Queue.Workers, "health_addr", cfg.MetricsAddr)

	// Ingress: enqueue file paths from stdin (if any).
	go enqueueFromStdin(workerCtx, q, log)

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
func jobHandler(p *pipeline.Pipeline) queue.Handler {
	return func(ctx context.Context, j queue.Job) (queue.Disposition, error) {
		if err := p.Process(ctx, j.ID, j.Source); err != nil {
			return queue.Retry, err
		}
		return queue.Ack, nil
	}
}

func enqueueFromStdin(ctx context.Context, q queue.Queue, log *slog.Logger) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		path := strings.TrimSpace(sc.Text())
		if path == "" {
			continue
		}
		if _, err := q.Enqueue(ctx, queue.Job{Source: moderation.Source{Kind: "file", Ref: path}}); err != nil {
			log.Error("enqueue failed", "path", path, "err", err)
			return
		}
	}
}
