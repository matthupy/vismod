package observe

import (
	"context"
	"encoding/json"
	"errors"
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

	// With boot-validation detail + a durability warning. Adapter identity rides
	// in AdapterName; Checks holds health verdicts ONLY (adapter:"ok"), never the
	// adapter name.
	h.SetReadyDetail(ReadyDetail{
		Ready:       true,
		AdapterName: "azure",
		Checks:      map[string]string{"ffmpeg": "ok", "adapter": "ok"},
		Warnings:    []string{"queue driver=memory is non-durable"},
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
	// Identity is separate from health: name in AdapterName, status in Checks.
	if d.AdapterName != "azure" {
		t.Errorf("adapter_name = %q, want azure", d.AdapterName)
	}
	if d.Checks["adapter"] != "ok" {
		t.Errorf("checks[adapter] = %q, want health verdict ok (not the name)", d.Checks["adapter"])
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

func TestReadyzLiveProbeFlipsOnDependencyFailure(t *testing.T) {
	h := NewHealth(":0", NewLogger("error"), nil)
	// Boot says ready; a live dependency probe is the source of truth for /readyz.
	h.SetReadyDetail(ReadyDetail{Ready: true, AdapterName: "stub",
		Checks: map[string]string{"adapter": "ok"}})

	var fail bool
	h.SetReadinessProbe("redis", func(_ context.Context) error {
		if fail {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	})

	// Healthy dependency: 200, and the probe records an "ok" check.
	rec := get(t, h, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy probe readyz = %d, want 200", rec.Code)
	}
	var d ReadyDetail
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Checks["redis"] != "ok" {
		t.Errorf("checks[redis] = %q, want ok", d.Checks["redis"])
	}

	// Dependency down: readiness flips to 503 even though boot detail said ready.
	fail = true
	rec = get(t, h, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed-probe readyz = %d, want 503", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Ready {
		t.Error("detail.Ready must be false when the probe fails")
	}
	if d.Checks["redis"] == "" || d.Checks["redis"] == "ok" {
		t.Errorf("checks[redis] = %q, want the failure reason", d.Checks["redis"])
	}
	// The stored boot checks survive the merge.
	if d.Checks["adapter"] != "ok" {
		t.Errorf("checks[adapter] = %q, want ok preserved", d.Checks["adapter"])
	}
}
