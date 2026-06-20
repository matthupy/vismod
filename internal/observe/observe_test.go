package observe

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matthupy/vismod/pkg/moderation"
)

func get(t *testing.T, h *Health, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	h.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHealthzAlwaysOK(t *testing.T) {
	h := NewHealth(":0", NewLogger("error"), nil)
	rec := get(t, h, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

func TestReadyzReflectsReadiness(t *testing.T) {
	h := NewHealth(":0", NewLogger("error"), nil)

	// Before ready: 503.
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("pre-ready readyz = %d, want 503", rec.Code)
	}

	// SetReady minimal path stays 200.
	h.SetReady(true)
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("SetReady(true) readyz = %d, want 200", rec.Code)
	}

	// With boot-validation detail + a durability warning.
	h.SetReadyDetail(ReadyDetail{
		Ready:    true,
		Checks:   map[string]string{"ffmpeg": "ok"},
		Warnings: []string{"queue driver=memory is non-durable"},
	})
	rec := get(t, h, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("ready readyz = %d, want 200", rec.Code)
	}
	var d ReadyDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("readyz body not JSON: %v", err)
	}
	if !d.Ready || d.Checks["ffmpeg"] != "ok" || len(d.Warnings) != 1 {
		t.Errorf("readyz detail = %+v, want ready+ffmpeg ok+1 warning", d)
	}
}

func TestMetricsEndpointExposesRegistry(t *testing.T) {
	m := NewMetrics()
	m.RecordJob(moderation.VerdictAllow)
	h := NewHealth(":0", NewLogger("error"), m.Registry())

	rec := get(t, h, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "vismod_jobs_total") {
		t.Errorf("metrics body missing vismod_jobs_total:\n%s", rec.Body.String())
	}
}

func TestMetricsAbsentWhenNoRegistry(t *testing.T) {
	h := NewHealth(":0", NewLogger("error"), nil)
	if rec := get(t, h, "/metrics"); rec.Code != http.StatusNotFound {
		t.Errorf("metrics without registry = %d, want 404", rec.Code)
	}
}

func TestNewLoggerLevels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", "weird"} {
		if NewLogger(lvl) == nil {
			t.Fatalf("nil logger for %q", lvl)
		}
	}
}
