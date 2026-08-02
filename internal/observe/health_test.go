package observe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHealthzIsLivenessOnly: /healthz must stay 200 while the process is
// alive even when it is not ready. Coupling liveness to readiness makes
// Kubernetes RESTART a pod that is correctly shedding load during a provider
// outage — turning a degradation into an outage.
func TestHealthzIsLivenessOnly(t *testing.T) {
	bp := NewBackpressure(1, 100, time.Minute, 1)
	bp.Record(false) // trip it
	h := NewHealth(bp)
	h.AddProbe("redis", func() error { return errors.New("connection refused") })

	rec := httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d while unready, want 200 (liveness must not follow readiness)", rec.Code)
	}
}

func TestReadyzReportsProbeAndBackpressureFailures(t *testing.T) {
	bp := NewBackpressure(1, 100, time.Minute, 1)
	h := NewHealth(bp)
	h.AddProbe("redis", func() error { return nil })

	rec := httptest.NewRecorder()
	h.Readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz = %d on a healthy process, want 200: %s", rec.Code, rec.Body)
	}

	bp.Record(false)
	rec = httptest.NewRecorder()
	h.Readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d under backpressure, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "backpressure") {
		t.Errorf("readyz body should name the cause: %s", rec.Body)
	}
}

// TestReadyzWarningsDoNotFlipReadiness: the memq-in-serve warning must be
// visible without making the pod unready — an operator warning that took a
// replica out of rotation would be worse than the thing it warns about.
func TestReadyzWarningsDoNotFlipReadiness(t *testing.T) {
	h := NewHealth(NewBackpressure(5, 100, time.Minute, 1))
	h.AddWarning("queue.driver=memory is non-durable")

	rec := httptest.NewRecorder()
	h.Readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("readyz = %d with only a warning, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ready") {
		t.Errorf("body = %q, want it to report ready", body)
	}
	if !strings.Contains(body, "non-durable") {
		t.Errorf("warning missing from readyz output: %q", body)
	}
}

// TestServeExposesMetricsAndHealth: /metrics is the autoscaling signal
// (vismod_queue_depth) and the probes are what keeps a broken replica out of
// rotation. This drives the real listener Serve starts.
func TestServeExposesMetricsAndHealth(t *testing.T) {
	metrics := NewMetrics()
	metrics.QueueDepth.Set(7)
	h := NewHealth(NewBackpressure(5, 100, time.Minute, 1))

	srv := Serve("127.0.0.1:0", metrics, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// Serve binds asynchronously; exercise the routes through the handler so
	// the test does not race the listener or depend on a port.
	for _, tc := range []struct{ path, want string }{
		{"/metrics", "vismod_queue_depth 7"},
		{"/healthz", "ok"},
		{"/readyz", "ready"},
	} {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s body missing %q:\n%s", tc.path, tc.want, rec.Body)
		}
	}
}

// TestNewLoggerLevels: log_level is operator config. A level that silently
// fell back to the wrong verbosity either floods or hides the decision log.
func TestNewLoggerLevels(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"error":   slog.LevelError,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo, // unset defaults to info
		"verbose": slog.LevelInfo, // unrecognized defaults to info, never silence
	}
	for level, want := range cases {
		log := NewLogger(level)
		if log == nil {
			t.Fatalf("NewLogger(%q) returned nil", level)
		}
		if !log.Enabled(context.Background(), want) {
			t.Errorf("NewLogger(%q) does not emit at %v", level, want)
		}
		if want > slog.LevelDebug && log.Enabled(context.Background(), want-4) {
			t.Errorf("NewLogger(%q) emits below its configured level", level)
		}
	}
}
