package observe

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

// failingFake is a moderator whose calls fail with a caller-supplied error,
// so the wrapper's error classification can be observed through the metrics
// it emits.
type failingFake struct {
	name string
	err  error
	res  moderation.NormalizedResult
}

func (f failingFake) Name() string                { return f.name }
func (failingFake) Capabilities() moderation.Caps { return moderation.Caps{} }
func (failingFake) Close() error                  { return nil }
func (f failingFake) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	return f.res, f.err
}

type failingVideoFake struct{ failingFake }

func (f failingVideoFake) AnalyzeVideo(context.Context, moderation.Source) (moderation.NormalizedResult, error) {
	return f.res, f.err
}

// errorCount reads vismod_adapter_errors_total{adapter,code} off the
// registry the process actually exports, rather than the vec in isolation:
// an unregistered metric is invisible to operators no matter what it counts.
// A series that was never incremented is absent, which reads as 0.
func errorCount(t *testing.T, m *Metrics, adapter, code string) float64 {
	t.Helper()
	return gatheredValue(t, m, "vismod_adapter_errors_total",
		map[string]string{"adapter": adapter, "code": code})
}

// latencyCount reads the observation count of
// vismod_adapter_request_seconds{adapter}.
func latencyCount(t *testing.T, m *Metrics, adapter string) float64 {
	t.Helper()
	return gatheredValue(t, m, "vismod_adapter_request_seconds",
		map[string]string{"adapter": adapter})
}

func gatheredValue(t *testing.T, m *Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, metric := range fam.GetMetric() {
			got := make(map[string]string, len(metric.GetLabel()))
			for _, p := range metric.GetLabel() {
				got[p.GetName()] = p.GetValue()
			}
			matched := true
			for k, v := range labels {
				if got[k] != v {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			if h := metric.GetHistogram(); h != nil {
				return float64(h.GetSampleCount())
			}
			return metric.GetCounter().GetValue()
		}
	}
	return 0
}

// TestInstrumentedAnalyzeImagePassesResultThrough: the wrapper is meant to
// be transparent. A swallowed error would turn a provider failure into an
// empty result â€” exactly the silent "allow" the project forbids.
func TestInstrumentedAnalyzeImagePassesResultThrough(t *testing.T) {
	want := moderation.NormalizedResult{Provider: "fake", AssetID: "asset-1"}
	metrics := NewMetrics()
	m := InstrumentModerator(failingFake{name: "fake", res: want}, metrics)

	got, err := m.AnalyzeImage(context.Background(), moderation.Image{})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if got.AssetID != want.AssetID {
		t.Errorf("asset_id = %q, want %q", got.AssetID, want.AssetID)
	}
	if n := latencyCount(t, metrics, "fake"); n != 1 {
		t.Errorf("latency observations = %v, want 1", n)
	}
	if n := errorCount(t, metrics, "fake", "terminal"); n != 0 {
		t.Errorf("a successful call recorded %v errors", n)
	}
}

func TestInstrumentedAnalyzeImageForwardsError(t *testing.T) {
	sentinel := errors.New("provider exploded")
	metrics := NewMetrics()
	m := InstrumentModerator(failingFake{name: "fake", err: sentinel}, metrics)

	_, err := m.AnalyzeImage(context.Background(), moderation.Image{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the provider error unchanged", err)
	}
	if n := errorCount(t, metrics, "fake", "terminal"); n != 1 {
		t.Errorf("terminal errors = %v, want 1", n)
	}
	if n := latencyCount(t, metrics, "fake"); n != 1 {
		t.Errorf("a failed call must still be timed: observations = %v", n)
	}
}

// TestInstrumentedAnalyzeVideoIsMeasured: video-native providers bypass
// frame extraction entirely, so if the wrapper did not measure AnalyzeVideo
// those deployments would report no adapter latency at all.
func TestInstrumentedAnalyzeVideoIsMeasured(t *testing.T) {
	sentinel := errors.New("video failed")
	metrics := NewMetrics()
	m := InstrumentModerator(failingVideoFake{failingFake{name: "video-fake", err: sentinel}}, metrics)

	vm, ok := m.(moderation.VideoModerator)
	if !ok {
		t.Fatal("wrapper dropped VideoModerator")
	}
	if _, err := vm.AnalyzeVideo(context.Background(), moderation.Source{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the provider error unchanged", err)
	}
	if n := latencyCount(t, metrics, "video-fake"); n != 1 {
		t.Errorf("video latency observations = %v, want 1", n)
	}
	if n := errorCount(t, metrics, "video-fake", "terminal"); n != 1 {
		t.Errorf("video errors = %v, want 1", n)
	}
}

// TestErrCodeIsBoundedAndSpecific: this value becomes a Prometheus label.
// Free-form provider text there would blow up cardinality, so the label is
// the HTTP status when known and a retryable/terminal class otherwise.
func TestErrCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"http status is preferred", &moderate.HTTPError{Status: 429}, "429"},
		{"wrapped http status still resolves", fmt.Errorf("frame 2: %w", &moderate.HTTPError{Status: 503}), "503"},
		{"retryable without a status", moderation.Retryable(errors.New("dial tcp: timeout")), "retryable"},
		{"anything else is terminal", errors.New("unsupported media"), "terminal"},
		{"context cancellation is terminal", context.Canceled, "terminal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errCode(tc.err); got != tc.want {
				t.Errorf("errCode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInstrumentPreservesIdentity: Name() and Capabilities() drive adapter
// labels and the pipeline's oversize pre-flight. A wrapper that reported its
// own identity would mislabel every metric and drop MaxImageBytes.
func TestInstrumentPreservesIdentity(t *testing.T) {
	inner := failingFake{name: "microsoft"}
	m := InstrumentModerator(inner, NewMetrics())

	if m.Name() != "microsoft" {
		t.Errorf("Name = %q, want the inner adapter name", m.Name())
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
