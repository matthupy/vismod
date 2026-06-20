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

func TestRegisterQueueDepthExposesGaugesAtScrapeTime(t *testing.T) {
	m := NewMetrics()
	qd, dlq := 7.0, 3.0
	m.RegisterQueueDepth(func() (float64, error) { return qd, nil }, func() float64 { return dlq })

	// Mutate the source AFTER registration: a GaugeFunc reads at scrape time.
	qd = 9
	want := `
# HELP vismod_queue_depth Jobs buffered in the queue and not yet started.
# TYPE vismod_queue_depth gauge
vismod_queue_depth 9
# HELP vismod_deadletter_depth Jobs that have been dead-lettered.
# TYPE vismod_deadletter_depth gauge
vismod_deadletter_depth 3
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"vismod_queue_depth", "vismod_deadletter_depth"); err != nil {
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
	)

	// One scrape: gauge reads, error bumps the counter.
	if got := testutil.CollectAndCount(m.Registry(), "vismod_queue_depth"); got == 0 {
		t.Error("expected vismod_queue_depth to be collectable")
	}
	want := `
# HELP vismod_queue_depth_scrape_errors_total Times a scrape-time queue-depth read failed (depth reported as 0 for that scrape).
# TYPE vismod_queue_depth_scrape_errors_total counter
vismod_queue_depth_scrape_errors_total 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"vismod_queue_depth_scrape_errors_total"); err != nil {
		t.Errorf("scrape-error counter mismatch: %v", err)
	}

	// Depth itself must read 0 on the failed scrape (not stale/garbage).
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
