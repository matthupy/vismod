package observe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordJobCountsByVerdict(t *testing.T) {
	m := NewMetrics()
	m.RecordJob(moderation.VerdictBlock)
	m.RecordJob(moderation.VerdictBlock)
	m.RecordJob(moderation.VerdictAllow)

	want := `
# HELP vismod_jobs_total Total moderation jobs finished, partitioned by overall verdict.
# TYPE vismod_jobs_total counter
vismod_jobs_total{verdict="allow"} 1
vismod_jobs_total{verdict="block"} 2
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "vismod_jobs_total"); err != nil {
		t.Errorf("jobs_total mismatch: %v", err)
	}
}

func TestRecordDivertFailureCounts(t *testing.T) {
	m := NewMetrics()
	m.RecordDivertFailure()
	m.RecordDivertFailure()

	want := `
# HELP vismod_divert_failures_total Total potential-CSAM diverts that failed to reach the review channel (frame may never reach a human).
# TYPE vismod_divert_failures_total counter
vismod_divert_failures_total 2
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "vismod_divert_failures_total"); err != nil {
		t.Errorf("divert_failures_total mismatch: %v", err)
	}
}

func TestRecordModelMismatchCountsByReason(t *testing.T) {
	m := NewMetrics()
	m.RecordModelMismatch("mismatch")
	m.RecordModelMismatch("mismatch")
	m.RecordModelMismatch("unstamped")

	want := `
# HELP vismod_jobs_model_mismatch_total Jobs whose stamped model fingerprint did not match the worker's loaded model (reason=mismatch) or carried no fingerprint (reason=unstamped).
# TYPE vismod_jobs_model_mismatch_total counter
vismod_jobs_model_mismatch_total{reason="mismatch"} 2
vismod_jobs_model_mismatch_total{reason="unstamped"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "vismod_jobs_model_mismatch_total"); err != nil {
		t.Errorf("model_mismatch_total mismatch: %v", err)
	}
}

// A nil *Metrics must be a safe no-op so the queue/handler stay decoupled from a
// metrics instance in tests and in a CLI run without /metrics.
func TestRecordModelMismatchNilSafe(t *testing.T) {
	var m *Metrics
	m.RecordModelMismatch("mismatch") // must not panic
}

func TestRecordJobLifecycleCountersNilSafe(t *testing.T) {
	var m *Metrics
	m.RecordJobCompleted() // must not panic
	m.RecordJobFailed()    // must not panic
}

func TestRecordJobLifecycleCounters(t *testing.T) {
	m := NewMetrics()
	m.RecordJobCompleted()
	m.RecordJobCompleted()
	m.RecordJobCompleted()
	m.RecordJobFailed()

	want := `
# HELP vismod_jobs_completed_total Jobs acked (successfully processed) at the queue layer, driver-uniform.
# TYPE vismod_jobs_completed_total counter
vismod_jobs_completed_total 3
# HELP vismod_jobs_failed_total Jobs dead-lettered (retry-exhausted / terminal / panic / model mismatch); these never carry a verdict.
# TYPE vismod_jobs_failed_total counter
vismod_jobs_failed_total 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"vismod_jobs_completed_total", "vismod_jobs_failed_total"); err != nil {
		t.Errorf("job lifecycle counters mismatch: %v", err)
	}
}

func TestRegisterQueueDepthExposesGaugesAtScrapeTime(t *testing.T) {
	m := NewMetrics()
	qd, dlq, active := 7.0, 3.0, 2.0
	m.RegisterQueueDepth(func() (float64, error) { return qd, nil }, func() float64 { return dlq }, func() float64 { return active })

	// Mutate the source AFTER registration: a GaugeFunc reads at scrape time.
	qd = 9
	active = 5
	want := `
# HELP vismod_jobs_active Jobs pulled by a worker and not yet acked or dead-lettered (in-flight).
# TYPE vismod_jobs_active gauge
vismod_jobs_active 5
# HELP vismod_queue_depth Jobs buffered in the queue and not yet started.
# TYPE vismod_queue_depth gauge
vismod_queue_depth 9
# HELP vismod_deadletter_depth Jobs that have been dead-lettered.
# TYPE vismod_deadletter_depth gauge
vismod_deadletter_depth 3
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"vismod_queue_depth", "vismod_deadletter_depth", "vismod_jobs_active"); err != nil {
		t.Errorf("depth gauges mismatch: %v", err)
	}
}

// A failing live-depth read must report depth 0 AND bump the scrape-error
// counter, so a backend that went dark is distinguishable from an empty queue.
func TestRegisterQueueDepthSurfacesScrapeError(t *testing.T) {
	m := NewMetrics()
	m.RegisterQueueDepth(
		func() (float64, error) { return 0, errors.New("backend down") },
		func() float64 { return 0 },
		func() float64 { return 0 },
	)

	// The error counter increments once PER scrape (it is bumped inside the gauge
	// collect), so assert against a known number of scrapes. Gather the registry
	// exactly once, then read the counter directly via ToFloat64 — a separate
	// collect pass, so it neither adds a scrape nor races the gauge's collect
	// order within a single Gather.
	if _, err := m.Registry().Gather(); err != nil {
		t.Fatalf("gather: %v", err)
	}
	if got := testutil.ToFloat64(m.queueDepthErrors); got != 1 {
		t.Errorf("scrape-error counter = %v, want 1 after one failed scrape", got)
	}

	// Depth itself must read 0 on the failed scrape (not stale/garbage). This
	// gather is a second scrape (counter now 2); we assert only the depth value.
	wantDepth := `
# HELP vismod_queue_depth Jobs buffered in the queue and not yet started.
# TYPE vismod_queue_depth gauge
vismod_queue_depth 0
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(wantDepth), "vismod_queue_depth"); err != nil {
		t.Errorf("failed-scrape depth should be 0: %v", err)
	}
}

// Wrapping a video-native adapter must panic at wiring time — never silently
// strip the VideoModerator implementation the pipeline relies on.
func TestInstrumentPanicsOnVideoNativeAdapter(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Instrument on a SupportsVideo adapter should panic, did not")
		}
	}()
	m := NewMetrics()
	m.Instrument(videoModerator{})
}

// videoModerator is a fake video-native adapter (Caps.SupportsVideo=true).
type videoModerator struct{ fakeModerator }

func (videoModerator) Capabilities() moderation.Caps { return moderation.Caps{SupportsVideo: true} }

// codedErr is a fake adapter error exposing a provider code.
type codedErr struct{ code string }

func (e codedErr) Error() string     { return "boom " + e.code }
func (e codedErr) ErrorCode() string { return e.code }

// fakeModerator returns a fixed error (or nil) from AnalyzeImage.
type fakeModerator struct {
	name string
	err  error
}

func (f fakeModerator) Name() string { return f.name }
func (f fakeModerator) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	return moderation.NormalizedResult{}, f.err
}
func (f fakeModerator) Capabilities() moderation.Caps { return moderation.Caps{} }
func (f fakeModerator) Close() error                  { return nil }

func TestInstrumentRecordsLatencyAndCodedError(t *testing.T) {
	m := NewMetrics()
	mod := m.Instrument(fakeModerator{name: "azure", err: codedErr{code: "TooManyRequests"}})

	if _, err := mod.AnalyzeImage(context.Background(), moderation.Image{}); err == nil {
		t.Fatal("want error propagated through decorator")
	}

	// One latency observation recorded for the adapter.
	if got := testutil.CollectAndCount(m.Registry(), "vismod_adapter_request_seconds"); got == 0 {
		t.Error("expected an adapter_request_seconds observation")
	}
	want := `
# HELP vismod_adapter_errors_total Total adapter request errors, partitioned by adapter and provider error code.
# TYPE vismod_adapter_errors_total counter
vismod_adapter_errors_total{adapter="azure",code="TooManyRequests"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "vismod_adapter_errors_total"); err != nil {
		t.Errorf("adapter_errors_total mismatch: %v", err)
	}
}

func TestInstrumentUnknownCodeOnPlainError(t *testing.T) {
	m := NewMetrics()
	mod := m.Instrument(fakeModerator{name: "stub", err: errors.New("plain")})
	_, _ = mod.AnalyzeImage(context.Background(), moderation.Image{})

	want := `
# HELP vismod_adapter_errors_total Total adapter request errors, partitioned by adapter and provider error code.
# TYPE vismod_adapter_errors_total counter
vismod_adapter_errors_total{adapter="stub",code="unknown"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "vismod_adapter_errors_total"); err != nil {
		t.Errorf("plain error should map to code=unknown: %v", err)
	}
}

func TestInstrumentNoErrorRecordsNoErrorSeries(t *testing.T) {
	m := NewMetrics()
	mod := m.Instrument(fakeModerator{name: "stub"})
	if _, err := mod.AnalyzeImage(context.Background(), moderation.Image{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := testutil.CollectAndCount(m.Registry(), "vismod_adapter_errors_total"); got != 0 {
		t.Errorf("no error => no error series, got %d", got)
	}
}
