// Package observe wires structured logging, Prometheus metrics, and the serve
// health endpoints (§F.6).
//
// Logging rule: NEVER log media bytes, PII, Raw free-text, OCR or captions.
package observe

import (
	"context"
	"encoding/json"
	"log/slog"
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
// results (e.g. "ffmpeg":"ok"); Warnings carries non-fatal operator notes (e.g.
// the memq non-durability boundary — §D.3).
type ReadyDetail struct {
	Ready    bool              `json:"ready"`
	Checks   map[string]string `json:"checks,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
}

// Health serves /healthz (liveness), /readyz (readiness incl. boot-validation
// detail) and, when a registry is supplied, /metrics. Readiness is a flag the
// daemon flips: boot validation, and provider-outage backpressure (§F.5).
type Health struct {
	ready  atomic.Bool
	detail atomic.Pointer[ReadyDetail]
	srv    *http.Server
	mux    *http.ServeMux
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

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		d := h.detail.Load()
		if d == nil {
			d = &ReadyDetail{Ready: h.ready.Load()}
		}
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
