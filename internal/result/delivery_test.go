package result

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func plainEnvelope(id string) ResultEnvelope {
	return ResultEnvelope{
		JobID:  queue.JobID(id),
		Source: moderation.Source{Kind: "file", Ref: "/tmp/x.jpg", MediaType: "image"},
		Result: &moderation.NormalizedResult{
			Provider: "microsoft",
			Overall:  moderation.OverallVerdict{Verdict: moderation.VerdictBlock},
		},
	}
}

// TestWebhookExhaustionIsErrDeliveryFailed gives the pipeline one value to
// test for when a destination received nothing at all, regardless of
// whether the budget ran out or the receiver rejected the payload.
func TestWebhookExhaustionIsErrDeliveryFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	s := NewWebhookSink(srv.URL, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 2, Backoff: time.Millisecond,
		Name: "webhook[1]", Logger: captureLogger(&buf),
	})
	err := s.Write(context.Background(), plainEnvelope("job-1"))
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("a sink that delivered nothing must return ErrDeliveryFailed, got %v", err)
	}
}

// TestWebhookLogsBackoffAndFinalGiveUp: the backoff records explain the
// stall, and exactly one error record marks the give-up. Without the
// latter an operator sees warnings that simply stop, with no way to tell
// recovery from abandonment.
func TestWebhookLogsBackoffAndFinalGiveUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	s := NewWebhookSink(srv.URL, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 3, Backoff: time.Millisecond,
		Name: "webhook[1]", Logger: captureLogger(&buf),
	})
	_ = s.Write(context.Background(), plainEnvelope("job-2"))

	recs := logRecords(t, &buf)
	var warns, errs int
	var final map[string]any
	for _, r := range recs {
		switch r["level"] {
		case "WARN":
			warns++
		case "ERROR":
			errs++
			final = r
		}
	}
	if warns != 2 {
		t.Errorf("want one warn per backoff (2), got %d: %s", warns, buf.String())
	}
	if errs != 1 {
		t.Fatalf("want exactly one error record for the give-up, got %d: %s", errs, buf.String())
	}
	if final["sink"] != "webhook[1]" {
		t.Errorf("give-up record must name the sink, got %v", final["sink"])
	}
	if final["job_id"] != "job-2" {
		t.Errorf("give-up record must name the job, got %v", final["job_id"])
	}
	if final["reason"] != "exhausted" {
		t.Errorf("reason must be the bounded value \"exhausted\", got %v", final["reason"])
	}
}

// TestWebhookRetryTargetMatchesGiveUpSink pins the one thing that lets an
// operator join a stall to its outcome. The two records use DIFFERENT
// field names — `target` on the backoff warning (moderate.WithRetryLog,
// which also labels adapter calls) and `sink` on the give-up error (this
// package, matching output.sinks and the vismod_sink_* metrics) — so the
// values must be identical or the two lines cannot be correlated.
func TestWebhookRetryTargetMatchesGiveUpSink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	s := NewWebhookSink(srv.URL, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 2, Backoff: time.Millisecond,
		Name: "webhook[3]", Logger: captureLogger(&buf),
	})
	_ = s.Write(context.Background(), plainEnvelope("job-join"))

	var target, sink any
	for _, r := range logRecords(t, &buf) {
		if v, ok := r["target"]; ok {
			target = v
		}
		if v, ok := r["sink"]; ok {
			sink = v
		}
	}
	if target != "webhook[3]" {
		t.Errorf("retry warning must carry target=%q, got %v", "webhook[3]", target)
	}
	if sink != target {
		t.Errorf("give-up sink %v must equal retry target %v, or the records cannot be joined", sink, target)
	}
}

// TestWebhookTerminalRejectionIsDistinctFromExhaustion: a 400 is never
// retried, so labelling it "exhausted" would point an operator at an
// outage that is not happening.
func TestWebhookTerminalRejectionIsDistinctFromExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	s := NewWebhookSink(srv.URL, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 3, Backoff: time.Millisecond,
		Name: "webhook[0]", Logger: captureLogger(&buf),
	})
	err := s.Write(context.Background(), plainEnvelope("job-3"))
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("a rejected payload still delivered nothing, want ErrDeliveryFailed, got %v", err)
	}

	recs := logRecords(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("a terminal rejection must log once and not back off, got %d records: %s", len(recs), buf.String())
	}
	if recs[0]["reason"] != "rejected" {
		t.Errorf("reason must be \"rejected\", got %v", recs[0]["reason"])
	}
	if recs[0]["status"] != float64(400) {
		t.Errorf("a rejection must carry the status, got %v", recs[0]["status"])
	}
}

// TestWebhookDeliveryLogNeverCarriesTheURL is a credential test: a Discord
// webhook URL holds its token in the path.
func TestWebhookDeliveryLogNeverCarriesTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	secret := srv.URL + "/api/webhooks/9/SUPER_SECRET_TOKEN"
	s := NewWebhookSink(secret, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 2, Backoff: time.Millisecond,
		Name: "webhook[1]", Logger: captureLogger(&buf),
	})
	err := s.Write(context.Background(), plainEnvelope("job-4"))

	for _, forbidden := range []string{"SUPER_SECRET_TOKEN", "/api/webhooks/"} {
		if strings.Contains(buf.String(), forbidden) {
			t.Errorf("delivery log must not contain %q: %s", forbidden, buf.String())
		}
		if err != nil && strings.Contains(err.Error(), forbidden) {
			t.Errorf("delivery error must not contain %q: %v", forbidden, err)
		}
	}
}

// TestWebhookOnRetryFiresPerBackoff wires the retry count to a metric
// without the result package importing observe.
func TestWebhookOnRetryFiresPerBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	retries := 0
	s := NewWebhookSink(srv.URL, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 3, Backoff: time.Millisecond,
		Name: "webhook[0]", OnRetry: func() { retries++ },
	})
	_ = s.Write(context.Background(), plainEnvelope("job-5"))
	if retries != 2 {
		t.Errorf("want one OnRetry per backoff (2), got %d", retries)
	}
}

// TestWebhookSuccessLogsNothing keeps the happy path quiet: a per-job info
// line on every allow would bury the failures this feature exists to
// surface.
func TestWebhookSuccessLogsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	s := NewWebhookSink(srv.URL, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 3, Backoff: time.Millisecond,
		Name: "webhook[0]", Logger: captureLogger(&buf),
	})
	if err := s.Write(context.Background(), plainEnvelope("job-6")); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("a successful delivery must log nothing, got: %s", buf.String())
	}
}

// TestFileSinkFailureIsErrDeliveryFailed: the sentinel is about "this
// destination received nothing", so it cannot be webhook-only or a caller
// has to special-case every transport.
func TestFileSinkFailureIsErrDeliveryFailed(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileSink(dir + "/out.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// Closing underneath makes the next append fail.
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Write(context.Background(), plainEnvelope("job-7")); !errors.Is(err, ErrDeliveryFailed) {
		t.Errorf("a failed file append must be ErrDeliveryFailed, got %v", err)
	}
}
