package observe

import (
	"context"
	"errors"
	"time"

	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the Prometheus collectors for the pipeline (§F.6). They live in
// a dedicated registry (not the global default) so exposition is isolated,
// testable, and a second serve in-process never panics on duplicate registration.
//
// Exposed series:
//   - vismod_jobs_total{verdict}              counter (per finished job)
//   - vismod_adapter_request_seconds{adapter} histogram (per AnalyzeImage call)
//   - vismod_adapter_errors_total{adapter,code} counter (per failed call)
//   - vismod_queue_depth                      gauge (scrape-time, via GaugeFunc)
//   - vismod_deadletter_depth                 gauge (scrape-time, via GaugeFunc)
//   - vismod_queue_depth_scrape_errors_total  counter (live-depth read failed)
//   - vismod_divert_failures_total            counter (potential-CSAM divert dropped)
type Metrics struct {
	reg              *prometheus.Registry
	jobsTotal        *prometheus.CounterVec
	adapterSeconds   *prometheus.HistogramVec
	adapterErrors    *prometheus.CounterVec
	queueDepthErrors prometheus.Counter
	divertFailures   prometheus.Counter
	modelMismatch    *prometheus.CounterVec
	jobsCompleted    prometheus.Counter
	jobsFailed       prometheus.Counter
}

// NewMetrics builds the collectors and registers the job/adapter series. Queue
// and dead-letter depth are registered separately via RegisterQueueDepth once a
// queue exists (serve), since they read live state at scrape time.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		jobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vismod_jobs_total",
			Help: "Total moderation jobs finished, partitioned by overall verdict.",
		}, []string{"verdict"}),
		adapterSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "vismod_adapter_request_seconds",
			Help: "Latency of a single adapter AnalyzeImage call.",
			// DefBuckets cap at 10s; a remote moderation provider (Azure) under
			// load/retry routinely exceeds that, collapsing the p95/p99 tail into
			// +Inf exactly when it matters. Extend to 60s so slow-call latency
			// stays observable.
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60},
		}, []string{"adapter"}),
		adapterErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vismod_adapter_errors_total",
			Help: "Total adapter request errors, partitioned by adapter and provider error code.",
		}, []string{"adapter", "code"}),
		queueDepthErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vismod_queue_depth_scrape_errors_total",
			Help: "Times a scrape-time queue-depth read failed (depth reported as 0 for that scrape).",
		}),
		divertFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vismod_divert_failures_total",
			Help: "Total potential-CSAM diverts that failed to reach the review channel (frame may never reach a human).",
		}),
		modelMismatch: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vismod_jobs_model_mismatch_total",
			Help: "Jobs whose stamped model fingerprint did not match the worker's loaded model (reason=mismatch) or carried no fingerprint (reason=unstamped).",
		}, []string{"reason"}),
		jobsCompleted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vismod_jobs_completed_total",
			Help: "Jobs acked (successfully processed) at the queue layer, driver-uniform.",
		}),
		jobsFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vismod_jobs_failed_total",
			Help: "Jobs dead-lettered (retry-exhausted / terminal / panic / model mismatch); these never carry a verdict.",
		}),
	}
	reg.MustRegister(m.jobsTotal, m.adapterSeconds, m.adapterErrors, m.queueDepthErrors,
		m.divertFailures, m.modelMismatch, m.jobsCompleted, m.jobsFailed)
	return m
}

// Registry returns the underlying registry for /metrics exposition.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// RecordJob counts one finished job under its overall verdict. Called by the
// pipeline after the asset rollup.
func (m *Metrics) RecordJob(verdict moderation.Verdict) {
	m.jobsTotal.WithLabelValues(string(verdict)).Inc()
}

// RecordModelMismatch counts one job the worker could not safely process under
// its loaded model (§L). reason="mismatch" => the job's stamped fingerprint != the
// worker's (wrong model deployed; the job is dead-lettered, never silently
// processed). reason="unstamped" => the job carried no fingerprint (a pre-feature
// older-binary job; processed but surfaced, never silent). Nil-safe so the queue
// handler stays decoupled from a metrics instance.
func (m *Metrics) RecordModelMismatch(reason string) {
	if m == nil {
		return
	}
	m.modelMismatch.WithLabelValues(reason).Inc()
}

// RecordJobCompleted counts one job acked (successfully processed) at the queue
// layer. Emitted by the driver at its terminal Ack disposition (driver-uniform,
// unlike asynq's daily-resetting info.Processed). It implements queue.Recorder.
func (m *Metrics) RecordJobCompleted() { m.jobsCompleted.Inc() }

// RecordJobFailed counts one job dead-lettered (retry-exhausted / terminal /
// panic / model mismatch). These produce no verdict envelope, so they are
// invisible in vismod_jobs_total — this is the queue-layer failure signal. It
// implements queue.Recorder.
func (m *Metrics) RecordJobFailed() { m.jobsFailed.Inc() }

// RecordDivertFailure counts one potential-CSAM divert that failed to reach its
// review channel (§G.8). The pipeline stays fail-safe — a dropped divert never
// blocks the job — so this counter is the ONLY signal that a frame which should
// have reached a human did not. Alert on rate(vismod_divert_failures_total) > 0.
func (m *Metrics) RecordDivertFailure() {
	m.divertFailures.Inc()
}

// RegisterQueueDepth registers scrape-time gauges fed by the live queue. Queue
// depth (buffered, not-yet-started) comes from Queue.QueueDepth, which can fail
// against a remote driver (redis, M5) — a failed read is reported as depth 0 AND
// bumps vismod_queue_depth_scrape_errors_total, so an operator can tell a genuine
// empty queue from a backend that went dark. Dead-letter depth is an in-memory
// accessor (no error). Both read at scrape time, so they never go stale.
func (m *Metrics) RegisterQueueDepth(queueDepth func() (float64, error), deadLetterDepth func() float64, active func() float64) {
	m.reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "vismod_queue_depth",
			Help: "Jobs buffered in the queue and not yet started.",
		}, func() float64 {
			n, err := queueDepth()
			if err != nil {
				m.queueDepthErrors.Inc()
				return 0
			}
			return n
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "vismod_deadletter_depth",
			Help: "Jobs that have been dead-lettered.",
		}, deadLetterDepth),
		// vismod_jobs_active: in-flight == pulled-by-a-worker == unacked are one
		// state, so one gauge (not three). Backlog (queue_depth) can read 0 while
		// jobs are wedged in processing — this is the signal for a stuck/slow worker
		// (or, under at-least-once redis, a crashed worker holding an unacked job).
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "vismod_jobs_active",
			Help: "Jobs pulled by a worker and not yet acked or dead-lettered (in-flight).",
		}, active),
	)
}

// Instrument wraps a Moderator so every AnalyzeImage call records request
// latency and, on failure, an error keyed by the provider error code.
//
// NOTE (v1 scope): the wrapper deliberately does NOT forward the optional
// VideoModerator interface. v1 adapters (stub, azure) are not video-native
// (Caps.SupportsVideo=false), so the pipeline's frame-by-frame path is used and
// nothing is lost.
//
// GUARD: wrapping a video-native adapter would silently strip its VideoModerator
// implementation — the pipeline's mod.(VideoModerator) assertion fails and falls
// back to the frame path with no error. That regression must NOT ship silently,
// so we panic at wiring time (a programmer error, caught at boot like
// prometheus MustRegister). A future video adapter must first teach this wrapper
// to instrument AnalyzeVideo and forward the assertion, then drop this guard.
func (m *Metrics) Instrument(mod moderation.Moderator) moderation.Moderator {
	if mod.Capabilities().SupportsVideo {
		panic("observe.Instrument: adapter " + mod.Name() + " is video-native (SupportsVideo=true) " +
			"but the instrument wrapper does not forward VideoModerator; teach it AnalyzeVideo before wiring")
	}
	return &instrumentedModerator{Moderator: mod, m: m, name: mod.Name()}
}

type instrumentedModerator struct {
	moderation.Moderator
	m    *Metrics
	name string
}

func (im *instrumentedModerator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	start := time.Now()
	res, err := im.Moderator.AnalyzeImage(ctx, img)
	im.m.adapterSeconds.WithLabelValues(im.name).Observe(time.Since(start).Seconds())
	if err != nil {
		im.m.adapterErrors.WithLabelValues(im.name, errorCode(err)).Inc()
	}
	return res, err
}

// errorCode extracts a provider error code from err if it (or anything it wraps)
// implements moderation.CodedError; otherwise "unknown".
func errorCode(err error) string {
	if ce, ok := errors.AsType[moderation.CodedError](err); ok {
		if code := ce.ErrorCode(); code != "" {
			return code
		}
	}
	return "unknown"
}
