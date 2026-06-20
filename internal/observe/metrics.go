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
type Metrics struct {
	reg            *prometheus.Registry
	jobsTotal      *prometheus.CounterVec
	adapterSeconds *prometheus.HistogramVec
	adapterErrors  *prometheus.CounterVec
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
	}
	reg.MustRegister(m.jobsTotal, m.adapterSeconds, m.adapterErrors)
	return m
}

// Registry returns the underlying registry for /metrics exposition.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// RecordJob counts one finished job under its overall verdict. Called by the
// pipeline after the asset rollup.
func (m *Metrics) RecordJob(verdict moderation.Verdict) {
	m.jobsTotal.WithLabelValues(string(verdict)).Inc()
}

// RegisterQueueDepth registers scrape-time gauges fed by the live queue. Queue
// depth (buffered, not-yet-started) comes from Queue.QueueDepth; dead-letter
// depth from the memq accessor. Both read at scrape time, so they never go stale.
func (m *Metrics) RegisterQueueDepth(queueDepth, deadLetterDepth func() float64) {
	m.reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "vismod_queue_depth",
			Help: "Jobs buffered in the queue and not yet started.",
		}, queueDepth),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "vismod_deadletter_depth",
			Help: "Jobs that have been dead-lettered.",
		}, deadLetterDepth),
	)
}

// Instrument wraps a Moderator so every AnalyzeImage call records request
// latency and, on failure, an error keyed by the provider error code.
//
// NOTE (v1 scope): the wrapper deliberately does NOT forward the optional
// VideoModerator interface. v1 adapters (stub, azure) are not video-native
// (Caps.SupportsVideo=false), so the pipeline's frame-by-frame path is used and
// nothing is lost. A future video-native adapter must instrument AnalyzeVideo
// here and forward the assertion.
func (m *Metrics) Instrument(mod moderation.Moderator) moderation.Moderator {
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
