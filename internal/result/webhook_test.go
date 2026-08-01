package result

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vismod/vismod/pkg/moderation"
)

func TestWebhookSinkPostsEnvelope(t *testing.T) {
	var gotMethod, gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 3)
	sent := envFixture("job-1")
	if err := s.Write(context.Background(), sent); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %s want POST", gotMethod)
	}
	if !strings.HasPrefix(gotType, "application/json") {
		t.Errorf("content-type: got %q", gotType)
	}
	var round ResultEnvelope
	if err := json.Unmarshal(gotBody, &round); err != nil {
		t.Fatalf("body is not a decodable envelope: %v", err)
	}
	if round.JobID != sent.JobID {
		t.Errorf("job_id must reach the receiver for its own dedupe: got %q", round.JobID)
	}
	if round.Result == nil || round.Result.Overall.Verdict != moderation.VerdictAllow {
		t.Errorf("envelope did not round-trip: %+v", round)
	}
}

func TestWebhookSinkRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 3)
	if err := s.Write(context.Background(), envFixture("job-1")); err != nil {
		t.Fatalf("want success after retry, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("want 2 attempts, got %d", calls.Load())
	}
}

func TestWebhookSinkRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 5*time.Second, 3)
	if err := s.Write(context.Background(), envFixture("job-1")); err != nil {
		t.Fatalf("want success after 429 retry, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("want 2 attempts, got %d", calls.Load())
	}
}

func TestWebhookSinkTerminalOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 3)
	if err := s.Write(context.Background(), envFixture("job-1")); err == nil {
		t.Fatal("want error on 400, got nil")
	}
	if calls.Load() != 1 {
		t.Errorf("400 is terminal: want 1 attempt, got %d", calls.Load())
	}
}

func TestWebhookSinkCapsAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 2)
	if err := s.Write(context.Background(), envFixture("job-1")); err == nil {
		t.Fatal("want error after exhausting attempts, got nil")
	}
	if calls.Load() != 2 {
		t.Errorf("want exactly max_attempts=2 calls, got %d", calls.Load())
	}
}

func TestWebhookSinkIdempotentPerJobID(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 3)
	env := envFixture("job-1")
	for range 3 {
		if err := s.Write(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("redelivery double-posted: %d calls, want 1", calls.Load())
	}
}

func TestWebhookSinkFailedWriteIsRetriableLater(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 1)
	env := envFixture("job-1")
	if err := s.Write(context.Background(), env); err == nil {
		t.Fatal("want first write to fail")
	}
	// A failed send must NOT be recorded as written, or the queue's
	// redelivery of this job would silently skip the webhook forever.
	fail.Store(false)
	if err := s.Write(context.Background(), env); err != nil {
		t.Fatalf("redelivery after failure must retry the send, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("want 2 calls, got %d", calls.Load())
	}
}

// TestWebhookSinkDoesNotFollowRedirects proves the CheckRedirect hook
// errors. config.validateWebhookURL refuses the cloud-metadata range at
// boot; a receiver answering 307 Location: http://169.254.169.254/… would
// otherwise make Go re-send the POST to exactly the destination the
// validator rejected. Mirrors the shieldgemma adapter's test.
func TestWebhookSinkDoesNotFollowRedirects(t *testing.T) {
	var reachedTarget atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedTarget.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	s := NewWebhookSink(redirector.URL, 2*time.Second, 1)
	if err := s.Write(context.Background(), envFixture("job-redirect")); err == nil {
		t.Fatal("redirect was followed or tolerated; want an error from CheckRedirect")
	}
	if reachedTarget.Load() {
		t.Error("client reached the redirect target")
	}
}
