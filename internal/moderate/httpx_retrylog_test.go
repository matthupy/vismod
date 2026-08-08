package moderate

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

	"github.com/vismod/vismod/pkg/moderation"
)

// capture builds a logger writing JSON records into buf at debug level, so
// a test can assert on what an operator would actually see.
func capture(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// records parses the captured stream into one map per emitted line.
func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
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

func postTo(url string) func() (*http.Request, error) {
	return func() (*http.Request, error) { return NewJSONRequest(url, []byte(`{}`)) }
}

// TestDoJSONLogsEachBackoff is the whole point of the retry log: a worker
// that sleeps through an outage must say so, or an operator sees a stalled
// job with no explanation.
func TestDoJSONLogsEachBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	_, err := DoJSON(context.Background(), srv.Client(), postTo(srv.URL), 3, time.Millisecond, "",
		WithRetryLog(capture(&buf), "webhook[1]"))
	if err == nil {
		t.Fatal("want an error after exhausting attempts")
	}

	recs := records(t, &buf)
	// 3 attempts means 2 sleeps between them.
	if len(recs) != 2 {
		t.Fatalf("want one log line per backoff (2), got %d: %s", len(recs), buf.String())
	}
	first := recs[0]
	if first["target"] != "webhook[1]" {
		t.Errorf("log must name the target, got %v", first["target"])
	}
	if first["attempt"] != float64(1) || first["max_attempts"] != float64(3) {
		t.Errorf("log must carry attempt/max_attempts, got %v of %v", first["attempt"], first["max_attempts"])
	}
	if first["status"] != float64(503) {
		t.Errorf("log must carry the status, got %v", first["status"])
	}
	if first["reason"] != "status" {
		t.Errorf("reason must be the bounded value \"status\", got %v", first["reason"])
	}
	if _, ok := first["delay_ms"]; !ok {
		t.Error("log must say how long it is about to sleep")
	}
	// Backoff is exponential, so the second sleep must exceed the first.
	if recs[1]["delay_ms"].(float64) <= first["delay_ms"].(float64) {
		t.Errorf("backoff must grow: %v then %v", first["delay_ms"], recs[1]["delay_ms"])
	}
}

// TestDoJSONRetryLogNeverCarriesTheURL is a credential test, not a
// cosmetic one. A Discord webhook URL carries its token in the PATH, so a
// log line naming the URL publishes a credential to stderr — and
// url.Redacted() does not help, because it only strips userinfo.
func TestDoJSONRetryLogNeverCarriesTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	secretPath := srv.URL + "/api/webhooks/12345/SUPER_SECRET_TOKEN"
	_, _ = DoJSON(context.Background(), srv.Client(), postTo(secretPath), 2, time.Millisecond, "",
		WithRetryLog(capture(&buf), "webhook[0]"))

	for _, forbidden := range []string{"SUPER_SECRET_TOKEN", "/api/webhooks/"} {
		if strings.Contains(buf.String(), forbidden) {
			t.Errorf("retry log must not contain %q, got: %s", forbidden, buf.String())
		}
	}
}

// TestDoJSONExhaustionIsADistinctError separates "we tried and gave up"
// from "the server said no". They need different operator responses, so
// they cannot be the same error value.
func TestDoJSONExhaustionIsADistinctError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := DoJSON(context.Background(), srv.Client(), postTo(srv.URL), 2, time.Millisecond, "")
	if !errors.Is(err, ErrAttemptsExhausted) {
		t.Errorf("an exhausted retry budget must be ErrAttemptsExhausted, got %v", err)
	}
	if !moderation.IsRetryable(err) {
		t.Error("exhaustion must stay marked retryable so the fail-safe path still dead-letters")
	}
}

// TestDoJSONTerminalStatusIsNotExhaustion: a 400 never retried, so
// reporting it as an exhausted budget would send an operator hunting for
// an outage that does not exist.
func TestDoJSONTerminalStatusIsNotExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	_, err := DoJSON(context.Background(), srv.Client(), postTo(srv.URL), 3, time.Millisecond, "",
		WithRetryLog(capture(&buf), "webhook[0]"))
	if errors.Is(err, ErrAttemptsExhausted) {
		t.Errorf("a terminal 4xx is not an exhausted budget, got %v", err)
	}
	if len(records(t, &buf)) != 0 {
		t.Errorf("a terminal status must not log a backoff, got: %s", buf.String())
	}
}

// TestDoJSONWithoutRetryLogIsUnchanged pins backward compatibility: every
// adapter calls DoJSON with no options and must keep working.
func TestDoJSONWithoutRetryLogIsUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	body, err := DoJSON(context.Background(), srv.Client(), postTo(srv.URL), 3, time.Millisecond, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("unexpected body %s", body)
	}
}

// TestDoJSONLogsNetworkFailureWithBoundedReason: a dial failure has no
// status, and the reason must still be one of the bounded values rather
// than an error string (which would be unbounded and could carry a URL).
func TestDoJSONLogsNetworkFailureWithBoundedReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	var buf bytes.Buffer
	_, _ = DoJSON(context.Background(), http.DefaultClient, postTo(url), 2, time.Millisecond, "",
		WithRetryLog(capture(&buf), "webhook[0]"))

	recs := records(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 backoff log, got %d: %s", len(recs), buf.String())
	}
	if recs[0]["reason"] != "network" {
		t.Errorf("reason must be \"network\", got %v", recs[0]["reason"])
	}
	if recs[0]["status"] != float64(0) {
		t.Errorf("a network failure has no status; want 0, got %v", recs[0]["status"])
	}
}
