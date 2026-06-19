package observe

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthReadinessToggle(t *testing.T) {
	h := NewHealth(":0", NewLogger("error"))

	// /healthz is always 200 (liveness).
	rec := httptest.NewRecorder()
	h.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}

	// /readyz is 503 until ready, 200 after.
	rec = httptest.NewRecorder()
	h.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before ready = %d, want 503", rec.Code)
	}

	h.SetReady(true)
	rec = httptest.NewRecorder()
	h.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz after ready = %d, want 200", rec.Code)
	}
}

func TestNewLoggerLevels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", "weird"} {
		if NewLogger(lvl) == nil {
			t.Fatalf("nil logger for %q", lvl)
		}
	}
}
