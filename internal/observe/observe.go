// Package observe wires structured logging and the serve health endpoints.
// Prometheus /metrics is added in M3; M0 ships slog + /healthz + /readyz so the
// serve daemon is a real, probeable process.
//
// Logging rule: NEVER log media bytes, PII, Raw free-text, OCR or captions.
package observe

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"
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

// Health serves /healthz (liveness) and /readyz (readiness). Readiness is a
// flag the daemon flips (boot validation, provider-outage backpressure M0+).
type Health struct {
	ready atomic.Bool
	srv   *http.Server
}

// NewHealth builds the health server bound to addr.
func NewHealth(addr string, log *slog.Logger) *Health {
	h := &Health{}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if h.ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
	})
	h.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return h
}

// SetReady flips the readiness flag.
func (h *Health) SetReady(ready bool) { h.ready.Store(ready) }

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
