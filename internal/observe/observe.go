// Package observe wires structured logging, Prometheus metrics, and the serve
// health endpoints (§F.6).
//
// Logging rule: NEVER log media bytes, PII, Raw free-text, OCR or captions.
package observe

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewLogger builds a JSON slog logger at the given level ("debug"/"info"/
// "warn"/"error").
func NewLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

// ReadyDetail is the JSON body of /readyz. Checks carries boot-validation
// results as health verdicts ONLY (each value is a status like "ok"), so an
// operator can treat any non-"ok" as a failed check. Identity (which adapter is
// active) is a separate field — AdapterName — never mixed into Checks. Warnings
// carries non-fatal operator notes (e.g. the memq non-durability boundary — §D.3).
type ReadyDetail struct {
	Ready       bool              `json:"ready"`
	AdapterName string            `json:"adapter_name,omitempty"`
	Checks      map[string]string `json:"checks,omitempty"`
	Warnings    []string          `json:"warnings,omitempty"`
}

// Health serves /healthz (liveness), /readyz (readiness incl. boot-validation
// detail) and, when a registry is supplied, /metrics. Readiness is a flag the
// daemon flips: boot validation, and provider-outage backpressure (§F.5).
type Health struct {
	ready  atomic.Bool
	detail atomic.Pointer[ReadyDetail]
	probe  atomic.Pointer[readinessProbe]
	srv    *http.Server
	mux    *http.ServeMux
}

// readinessProbe is a named live dependency check (e.g. "redis"). When set,
// /readyz runs it on every request so a dependency outage flips readiness
// (§F.2/§F.5) rather than reporting a stale boot-time verdict.
type readinessProbe struct {
	name string
	fn   func(context.Context) error
}

// NewHealth builds the health server bound to addr. If reg is non-nil, /metrics
// exposes its collectors. Liveness/readiness/metrics share one addr (metrics.addr).
func NewHealth(addr string, log *slog.Logger, reg *prometheus.Registry) *Health {
	h := &Health{}
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		d := h.evaluateReadiness(r.Context())
		code := http.StatusOK
		if !d.Ready {
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(d)
	})

	if reg != nil {
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
		}))
	}

	h.mux = mux
	h.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return h
}

// Handler returns the configured router (for tests and embedding).
func (h *Health) Handler() http.Handler { return h.mux }

// SetReady flips the readiness flag and updates the detail body to a minimal
// {ready} state. Use SetReadyDetail to attach boot-validation checks/warnings.
func (h *Health) SetReady(ready bool) {
	h.ready.Store(ready)
	h.detail.Store(&ReadyDetail{Ready: ready})
}

// SetReadyDetail sets readiness together with its boot-validation detail body.
func (h *Health) SetReadyDetail(d ReadyDetail) {
	h.ready.Store(d.Ready)
	h.detail.Store(&d)
}

// SetReadinessProbe registers a named live dependency check that /readyz runs on
// every request (e.g. a Redis PING). A failing probe forces /readyz to 503 with
// the failure reason in Checks[name], even if the stored boot detail said ready.
func (h *Health) SetReadinessProbe(name string, fn func(context.Context) error) {
	h.probe.Store(&readinessProbe{name: name, fn: fn})
}

// evaluateReadiness builds the /readyz body: the stored boot detail, overlaid
// with the live probe result. A nil stored detail falls back to the ready flag.
func (h *Health) evaluateReadiness(ctx context.Context) ReadyDetail {
	stored := h.detail.Load()
	var d ReadyDetail
	if stored != nil {
		d = *stored
	} else {
		d.Ready = h.ready.Load()
	}

	p := h.probe.Load()
	if p == nil {
		return d
	}

	// Copy Checks so the live overlay never mutates the stored detail.
	checks := make(map[string]string, len(d.Checks)+1)
	maps.Copy(checks, d.Checks)
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := p.fn(pctx); err != nil {
		checks[p.name] = err.Error()
		d.Ready = false
	} else {
		checks[p.name] = "ok"
	}
	d.Checks = checks
	return d
}

// Start serves in a background goroutine.
func (h *Health) Start() {
	go func() {
		if err := h.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Default().Error("health server stopped", "err", err)
		}
	}()
}

// Stop shuts the health server down.
func (h *Health) Stop(ctx context.Context) error { return h.srv.Shutdown(ctx) }
