// Package observe holds logging, Prometheus metrics, health endpoints,
// and the fail-safe backpressure tracker.
//
// Logging rule: never log media bytes, PII, Raw free-text, OCR output, or
// captions — job ids, adapter names, latencies, and verdicts only.
package observe

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewLogger builds the process slog.Logger.
func NewLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

// Metrics are the exported Prometheus series. vismod_queue_depth is the
// horizontal autoscaling signal (KEDA/HPA target).
type Metrics struct {
	Registry               *prometheus.Registry
	JobsTotal              *prometheus.CounterVec
	AdapterRequestSeconds  *prometheus.HistogramVec
	AdapterErrorsTotal     *prometheus.CounterVec
	QueueDepth             prometheus.Gauge
	DeadletterDepth        prometheus.Gauge
	WorkersActive          prometheus.Gauge
	FramesScannedTotal     prometheus.Counter
	JobFrames              *prometheus.HistogramVec
	SinkWriteFailuresTotal *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		JobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vismod_jobs_total", Help: "Jobs processed, by final verdict.",
		}, []string{"verdict"}),
		AdapterRequestSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "vismod_adapter_request_seconds", Help: "Moderation API request latency.",
		}, []string{"adapter"}),
		AdapterErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vismod_adapter_errors_total", Help: "Moderation API errors, by adapter and code.",
		}, []string{"adapter", "code"}),
		QueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "vismod_queue_depth", Help: "Pending jobs; the horizontal autoscaling signal.",
		}),
		DeadletterDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "vismod_deadletter_depth", Help: "Dead-letter queue depth.",
		}),
		WorkersActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "vismod_workers_active", Help: "Workers currently processing a job.",
		}),
		FramesScannedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vismod_frames_scanned_total",
			Help: "Frames evaluated by the moderation adapter (images count 1).",
		}),
		JobFrames: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vismod_job_frames",
			Help:    "Frames evaluated per job, by media type (FFmpeg workflow tuning signal).",
			Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128},
		}, []string{"media_type"}),
		SinkWriteFailuresTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vismod_sink_write_failures_total",
			Help: "Result-sink write failures, by sink type.",
		}, []string{"type"}),
	}
	reg.MustRegister(m.JobsTotal, m.AdapterRequestSeconds, m.AdapterErrorsTotal,
		m.QueueDepth, m.DeadletterDepth, m.WorkersActive,
		m.FramesScannedTotal, m.JobFrames, m.SinkWriteFailuresTotal)
	return m
}

// Backpressure implements the §F.5 surge/outage policy: on sustained
// provider failure (>= N consecutive errors OR error rate >= X% over
// window W) readiness flips not-ready and intake rejects with a retryable
// signal. Readiness restores only after M consecutive successes
// (hysteresis). Jobs are never auto-allowed and never black-holed.
type Backpressure struct {
	mu sync.Mutex

	n      int           // consecutive-error trip threshold
	ratePc float64       // error-rate trip threshold (percent)
	window time.Duration // rate window
	m      int           // consecutive successes to restore

	consecErr int
	consecOK  int
	tripped   bool
	outcomes  []outcome
}

type outcome struct {
	at time.Time
	ok bool
}

// minWindowSamples avoids tripping the rate rule on a near-empty window.
const minWindowSamples = 10

func NewBackpressure(consecutiveErrors int, errorRatePct float64, window time.Duration, recoverySuccesses int) *Backpressure {
	return &Backpressure{n: consecutiveErrors, ratePc: errorRatePct, window: window, m: recoverySuccesses}
}

// Record feeds one job outcome into the tracker.
func (b *Backpressure) Record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.outcomes = append(b.outcomes, outcome{at: now, ok: success})
	b.prune(now)

	if success {
		b.consecOK++
		b.consecErr = 0
		if b.tripped && b.consecOK >= b.m {
			b.tripped = false
			b.outcomes = nil
		}
		return
	}
	b.consecErr++
	b.consecOK = 0
	if b.consecErr >= b.n {
		b.tripped = true
		return
	}
	total, errs := 0, 0
	for _, o := range b.outcomes {
		total++
		if !o.ok {
			errs++
		}
	}
	if total >= minWindowSamples && float64(errs)*100/float64(total) >= b.ratePc {
		b.tripped = true
	}
}

func (b *Backpressure) prune(now time.Time) {
	cut := now.Add(-b.window)
	i := 0
	for ; i < len(b.outcomes); i++ {
		if b.outcomes[i].at.After(cut) {
			break
		}
	}
	b.outcomes = b.outcomes[i:]
}

// Ready reports whether intake should accept new jobs.
func (b *Backpressure) Ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.tripped
}

// Health serves /healthz (liveness) and /readyz (readiness including boot
// validation and backpressure; drivers add their own probes, e.g. Redis
// PING).
type Health struct {
	mu       sync.Mutex
	bp       *Backpressure
	probes   map[string]func() error
	warnings []string
}

func NewHealth(bp *Backpressure) *Health {
	return &Health{bp: bp, probes: map[string]func() error{}}
}

// AddProbe registers a named readiness probe (e.g. "redis" -> PING).
func (h *Health) AddProbe(name string, probe func() error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.probes[name] = probe
}

// AddWarning surfaces a permanent operator warning in /readyz output
// (e.g. memq in serve mode) without flipping readiness.
func (h *Health) AddWarning(w string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.warnings = append(h.warnings, w)
}

func (h *Health) Healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (h *Health) Readyz(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	probes := make(map[string]func() error, len(h.probes))
	for k, v := range h.probes {
		probes[k] = v
	}
	warnings := append([]string(nil), h.warnings...)
	h.mu.Unlock()

	var failures []string
	for name, probe := range probes {
		if err := probe(); err != nil {
			failures = append(failures, name+": "+err.Error())
		}
	}
	if h.bp != nil && !h.bp.Ready() {
		failures = append(failures, "backpressure: sustained provider failure; intake paused (retry later)")
	}

	if len(failures) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		for _, f := range failures {
			_, _ = w.Write([]byte("not ready: " + f + "\n"))
		}
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
	for _, warn := range warnings {
		_, _ = w.Write([]byte("warning: " + warn + "\n"))
	}
}

// Serve starts the metrics/health HTTP server. Returns the server for
// graceful shutdown.
func Serve(addr string, m *Metrics, h *Health, log *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", h.Healthz)
	mux.HandleFunc("/readyz", h.Readyz)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server failed", "addr", addr, "err", err)
		}
	}()
	return srv
}
