package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

// TestBuildSinksConstruction is the construction/validation table for the
// composition root. buildSinks is the LAST checkpoint before the
// destination is fixed for the process lifetime, and it is reachable with
// a directly-constructed Config that never passed config.Validate — so
// every misconfiguration below must be a boot refusal here, not a
// surprise on the first verdict.
func TestBuildSinksConstruction(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		cfg     config.Config
		wantErr string // substring; empty means the build must succeed
	}{
		{
			name: "type matching is case-insensitive",
			cfg:  outputCfg(config.SinkConfig{Type: "STDOUT"}),
		},
		{
			name: "type matching trims surrounding whitespace",
			cfg:  outputCfg(config.SinkConfig{Type: "  file  ", Path: filepath.Join(dir, "trimmed.jsonl")}),
		},
		{
			name:    "an empty type is refused, not defaulted to stdout",
			cfg:     outputCfg(config.SinkConfig{Type: ""}),
			wantErr: "unknown sink type",
		},
		{
			name:    "an unknown type names its index",
			cfg:     outputCfg(config.SinkConfig{Type: "stdout"}, config.SinkConfig{Type: "smoke-signal"}),
			wantErr: "output.sinks[1]: unknown sink type",
		},
		{
			name:    "an unknown format is refused before the transport is built",
			cfg:     outputCfg(config.SinkConfig{Type: "file", Path: filepath.Join(dir, "never.jsonl"), Format: "morse"}),
			wantErr: "unknown format",
		},
		{
			name: "an empty format is the json envelope",
			cfg:  outputCfg(config.SinkConfig{Type: "stdout", Format: ""}),
		},
		{
			name:    "metadata_fields on format json is a boot refusal, not a no-op",
			cfg:     outputCfg(config.SinkConfig{Type: "stdout", Format: "json", MetadataFields: []string{"correlation_id"}}),
			wantErr: "metadata_fields is not valid",
		},
		{
			name:    "a duplicate metadata_fields key is refused",
			cfg:     outputCfg(config.SinkConfig{Type: "stdout", Format: "discord", MetadataFields: []string{"a", "a"}}),
			wantErr: "twice",
		},
		{
			name: "discord format is accepted on a non-webhook transport",
			cfg:  outputCfg(config.SinkConfig{Type: "stdout", Format: "discord", MetadataFields: []string{"correlation_id"}}),
		},
		{
			name:    "a zero-value Config has no sinks and refuses to boot",
			cfg:     config.Config{},
			wantErr: "no sinks",
		},
		{
			name:    "an explicitly empty sink list refuses to boot",
			cfg:     config.Config{Output: config.OutputConfig{Sinks: []config.SinkConfig{}}},
			wantErr: "no sinks",
		},
		{
			name: "a predicated sink alongside an unconditional one is accepted",
			cfg: outputCfg(
				config.SinkConfig{Type: "stdout"},
				config.SinkConfig{Type: "file", Path: filepath.Join(dir, "blocks.jsonl"), Predicate: result.Predicate{
					Verdicts: []moderation.Verdict{moderation.VerdictBlock},
				}},
			),
		},
		{
			name: "allow_unrouted permits an all-predicated config",
			cfg: config.Config{Output: config.OutputConfig{
				AllowUnrouted: true,
				Sinks: []config.SinkConfig{{Type: "stdout", Predicate: result.Predicate{
					Verdicts: []moderation.Verdict{moderation.VerdictError},
				}}},
			}},
		},
		{
			name: "every sink predicated without allow_unrouted refuses to boot",
			cfg: outputCfg(config.SinkConfig{Type: "stdout", Predicate: result.Predicate{
				Verdicts: []moderation.Verdict{moderation.VerdictError},
			}}),
			wantErr: "emitted nowhere",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, closeFn, err := buildSinks(tt.cfg, io.Discard, observe.NewMetrics(), nil)
			if closeFn != nil {
				defer closeFn()
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want a sink, got error: %v", err)
				}
				if s == nil {
					t.Fatal("want a sink, got nil")
				}
				return
			}
			if err == nil {
				t.Fatalf("want a boot refusal containing %q, got a sink", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("want an error containing %q, got %v", tt.wantErr, err)
			}
			if s != nil {
				t.Errorf("a refused build must return no sink, got %v", s)
			}
		})
	}
}

// outputCfg builds the minimal directly-constructed Config buildSinks
// needs. config.Defaults() is deliberately not used: these cases assert
// what buildSinks itself enforces, and a defaulted stdout catch-all would
// mask the checks.
func outputCfg(sinks ...config.SinkConfig) config.Config {
	return config.Config{Output: config.OutputConfig{Sinks: sinks}}
}

// TestBuildSinksDeliveryCountersFollowMetrics drives the two metric
// callbacks buildSinks closes over. Both must tolerate a nil *Metrics —
// scan mode passes one — by counting nothing rather than panicking, and
// both must count when metrics are present. The counters are separate on
// purpose: rising retries with flat failures is a destination that is
// throttling but still delivering.
func TestBuildSinksDeliveryCountersFollowMetrics(t *testing.T) {
	tests := []struct {
		name    string
		metrics bool
	}{
		{"a nil Metrics counts nothing and never panics", false},
		{"a live Metrics counts one retry and one give-up", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var posts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&posts, 1)
				w.WriteHeader(http.StatusServiceUnavailable) // retryable, always
			}))
			defer srv.Close()

			var m *observe.Metrics
			if tt.metrics {
				m = observe.NewMetrics()
			}
			cfg := outputCfg(config.SinkConfig{
				Type:        "webhook",
				URL:         srv.URL,
				MaxAttempts: 2, // one backoff, then give up
				Timeout:     2 * time.Second,
			})

			sink, closeFn, err := buildSinks(cfg, io.Discard, m, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeFn()

			err = sink.Write(context.Background(), envelopeWithVerdict("down-1", moderation.VerdictBlock))
			if err == nil {
				t.Fatal("a destination that never accepts the envelope must return an error, not silence")
			}
			if !errors.Is(err, result.ErrDeliveryFailed) {
				t.Errorf("want ErrDeliveryFailed, got %v", err)
			}
			if !moderation.IsRetryable(err) {
				t.Errorf("an exhausted retry budget must stay queue-retryable, got %v", err)
			}
			if got := atomic.LoadInt32(&posts); got != 2 {
				t.Errorf("want max_attempts=2 POSTs, got %d", got)
			}
			if !tt.metrics {
				return // reaching here without a panic IS the assertion
			}
			if got := testutil.ToFloat64(m.SinkRetriesTotal.WithLabelValues("webhook")); got != 1 {
				t.Errorf("want 1 counted retry (one backoff), got %v", got)
			}
			if got := testutil.ToFloat64(m.SinkWriteFailuresTotal.WithLabelValues("webhook")); got != 1 {
				t.Errorf("want 1 counted give-up, got %v", got)
			}
		})
	}
}

// TestBuildSinksCloseAllReportsTheFirstError covers the close path's error
// accounting. Every closer must be attempted — a MultiSink close that
// stopped at the first failure would leak the remaining file handles — and
// the FIRST error is the one returned.
//
// A second call to the returned close func is the portable way to make a
// FileSink.Close fail: the underlying file is already closed.
func TestBuildSinksCloseAllReportsTheFirstError(t *testing.T) {
	dir := t.TempDir()
	cfg := outputCfg(
		config.SinkConfig{Type: "file", Path: filepath.Join(dir, "a.jsonl")},
		config.SinkConfig{Type: "file", Path: filepath.Join(dir, "b.jsonl")},
	)

	_, closeFn, err := buildSinks(cfg, io.Discard, observe.NewMetrics(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeFn(); err != nil {
		t.Fatalf("first close of healthy file sinks must succeed, got %v", err)
	}
	// Now both closers fail: the first error is reported, and the second
	// failing closer must not overwrite it.
	err = closeFn()
	if err == nil {
		t.Fatal("want the first close error, got nil")
	}
	if !errors.Is(err, os.ErrClosed) {
		t.Errorf("want the underlying close error unwrapped, got %v", err)
	}
}

// TestBuildSinksStdoutWritesToTheGivenWriter pins the one sink whose
// destination is an injected io.Writer rather than config: scan mode hands
// buildSinks the command's stdout, and a sink writing somewhere else would
// silently strand every one-shot result.
func TestBuildSinksStdoutWritesToTheGivenWriter(t *testing.T) {
	var buf strings.Builder
	sink, closeFn, err := buildSinks(outputCfg(config.SinkConfig{Type: "stdout"}), &buf, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	if err := sink.Write(context.Background(), envelopeWithVerdict("out-1", moderation.VerdictAllow)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"out-1"`) {
		t.Errorf("stdout sink must write the envelope to the injected writer, got %q", buf.String())
	}
}
