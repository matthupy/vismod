package observe

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBackpressureConsecutiveErrorsTrip(t *testing.T) {
	bp := NewBackpressure(3, 100, time.Minute, 2)
	if !bp.Ready() {
		t.Fatal("fresh tracker must be ready")
	}
	bp.Record(false)
	bp.Record(false)
	if !bp.Ready() {
		t.Fatal("2 errors < N=3 must stay ready")
	}
	bp.Record(false)
	if bp.Ready() {
		t.Fatal("3 consecutive errors must trip")
	}
	// Hysteresis: one success is not enough (M=2).
	bp.Record(true)
	if bp.Ready() {
		t.Fatal("recovery needs M consecutive successes")
	}
	bp.Record(true)
	if !bp.Ready() {
		t.Fatal("M consecutive successes must restore readiness")
	}
}

func TestBackpressureErrorRateTrip(t *testing.T) {
	// M=100 so interleaved successes can't restore mid-test.
	bp := NewBackpressure(100, 50, time.Minute, 100)
	// 6 errors / 12 samples = 50% >= X, never 100 consecutive.
	for range 6 {
		bp.Record(true)
		bp.Record(false)
	}
	if bp.Ready() {
		t.Fatal("50% error rate over the window must trip")
	}
}

func TestBackpressureRateNeedsMinSamples(t *testing.T) {
	bp := NewBackpressure(100, 50, time.Minute, 1)
	bp.Record(false) // 100% rate but only 1 sample
	if !bp.Ready() {
		t.Fatal("rate rule must not trip on a near-empty window")
	}
}

func TestReadyzReflectsBackpressureAndProbes(t *testing.T) {
	bp := NewBackpressure(1, 100, time.Minute, 1)
	h := NewHealth(bp)
	h.AddWarning("memq warning")

	rec := httptest.NewRecorder()
	h.Readyz(rec, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "memq warning") {
		t.Fatalf("ready with warning expected: %d %s", rec.Code, rec.Body.String())
	}

	bp.Record(false) // N=1 trips immediately
	rec = httptest.NewRecorder()
	h.Readyz(rec, nil)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "backpressure") {
		t.Fatalf("tripped backpressure must 503: %d %s", rec.Code, rec.Body.String())
	}

	bp.Record(true)
	failing := NewHealth(nil)
	failing.AddProbe("redis", func() error { return errTest })
	rec = httptest.NewRecorder()
	failing.Readyz(rec, nil)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "redis") {
		t.Fatalf("failing probe must 503 naming the probe: %d %s", rec.Code, rec.Body.String())
	}
}

var errTest = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "connection refused" }
