package moderate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vismod/vismod/pkg/moderation"
)

// stubTransport drives DoJSON's response handling without a socket, so a
// test can cancel a context at an EXACT point in the flow (here: after the
// response is in hand, before the backoff sleep) instead of racing a real
// server with a hopeful timeout. Nothing in this file sleeps waiting for a
// cancellation to land.
type stubTransport struct {
	calls int32
	fn    func(*http.Request) (*http.Response, error)
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	atomic.AddInt32(&s.calls, 1)
	return s.fn(r)
}

// TestOnRetryFiresOncePerBackoff pins the counter hook DoJSON exposes so a
// caller can drive vismod_sink_retries_total without this package taking a
// metrics dependency. The count must equal the number of SLEEPS, not the
// number of attempts: rising retries against flat failures is exactly how an
// operator tells "throttled but still delivering" from "down".
func TestOnRetryFiresOncePerBackoff(t *testing.T) {
	transient := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}
	var recovers int32
	cases := []struct {
		name        string
		handler     http.HandlerFunc
		maxAttempts int
		wantCalls   int32
		wantErr     bool
	}{
		{
			name:        "a 2xx never sleeps",
			handler:     func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
			maxAttempts: 3,
		},
		{
			name:        "a terminal 4xx never sleeps",
			handler:     func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusBadRequest) },
			maxAttempts: 3,
			wantErr:     true,
		},
		{
			name:        "three attempts against a 5xx means two sleeps",
			handler:     transient,
			maxAttempts: 3,
			wantCalls:   2,
			wantErr:     true,
		},
		{
			name: "recovering on the second attempt counts one sleep",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&recovers, 1) == 1 {
					transient(w, r)
					return
				}
				_, _ = w.Write([]byte(`{}`))
			},
			maxAttempts: 3,
			wantCalls:   1,
		},
		{
			name:        "a single-attempt budget never sleeps",
			handler:     transient,
			maxAttempts: 1,
			wantErr:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			var calls int32
			_, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, nil),
				tc.maxAttempts, time.Millisecond, "",
				OnRetry(func() { atomic.AddInt32(&calls, 1) }))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got := atomic.LoadInt32(&calls); got != tc.wantCalls {
				t.Errorf("OnRetry fired %d times, want %d (one per backoff)", got, tc.wantCalls)
			}
		})
	}
}

// TestOnRetryAndRetryLogAgreeOnCount: the counter and the warn record are two
// views of the same event. If they can disagree, a metrics spike has no log
// line to explain it — and the log line is the only place the target is named.
func TestOnRetryAndRetryLogAgreeOnCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	var calls int32
	_, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, nil), 3, time.Millisecond, "",
		OnRetry(func() { atomic.AddInt32(&calls, 1) }),
		WithRetryLog(capture(&buf), "webhook[2]"))
	if err == nil {
		t.Fatal("want an error after exhausting attempts")
	}

	recs := records(t, &buf)
	if got := int(atomic.LoadInt32(&calls)); got != len(recs) {
		t.Errorf("OnRetry fired %d times but %d backoff records were logged", got, len(recs))
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 backoffs across 3 attempts, got %d: %s", len(recs), buf.String())
	}
	if recs[0]["target"] != "webhook[2]" {
		t.Errorf("target = %v, want the operator-facing label", recs[0]["target"])
	}
}

// TestDoJSONOptionsAreNilSafe: DoOption is variadic and caller-supplied, so a
// nil logger or nil callback must be inert rather than a panic on the retry
// path — the one path that only runs when the process is already degraded.
func TestDoJSONOptionsAreNilSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cases := []struct {
		name string
		opts []DoOption
	}{
		{name: "no options at all"},
		{name: "nil retry callback", opts: []DoOption{OnRetry(nil)}},
		{name: "nil logger", opts: []DoOption{WithRetryLog(nil, "webhook[0]")}},
		{name: "both nil", opts: []DoOption{OnRetry(nil), WithRetryLog(nil, "")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, nil), 2, time.Millisecond, "", tc.opts...)
			if !errors.Is(err, ErrAttemptsExhausted) {
				t.Errorf("err = %v, want the budget to still be exhausted normally", err)
			}
			if !moderation.IsRetryable(err) {
				t.Error("exhaustion must stay retryable so the fail-safe path dead-letters")
			}
		})
	}
}

// TestDoJSONBodyReadFailureIsRetryable: a truncated response is not a verdict.
// The status line said 200, but the bytes never arrived, so returning the
// partial body would hand an adapter a half-parsed provider response — a
// silent misclassification. It must be transient instead, and the bounded
// reason must be "read_body": an error string in that field is both a metric
// cardinality bomb and a way for a URL to reach a log.
func TestDoJSONBodyReadFailureIsRetryable(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		// Promise more than we deliver, flush so the client really has the
		// 200 in hand, then drop the connection: the header is clean and the
		// body ends in an unexpected EOF.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partial":`))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	body, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, nil), 2, time.Millisecond, "",
		WithRetryLog(capture(&buf), "adapter:test"))
	if err == nil {
		t.Fatal("a truncated body must be an error, never a partial body reported as success")
	}
	if body != nil {
		t.Errorf("body = %q, want nil on failure", body)
	}
	if !errors.Is(err, ErrAttemptsExhausted) {
		t.Errorf("err = %v, want ErrAttemptsExhausted after the budget ran out", err)
	}
	if !moderation.IsRetryable(err) {
		t.Errorf("a truncated read is transient and must stay retryable, got %v", err)
	}
	if n := atomic.LoadInt32(&attempts); n != 2 {
		t.Errorf("attempts = %d, want 2 (a body read failure is retried)", n)
	}

	recs := records(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 backoff log across 2 attempts, got %d: %s", len(recs), buf.String())
	}
	if recs[0]["reason"] != "read_body" {
		t.Errorf("reason = %v, want the bounded value \"read_body\"", recs[0]["reason"])
	}
	if recs[0]["status"] != float64(200) {
		t.Errorf("status = %v, want 200 — the status was fine, the body was not", recs[0]["status"])
	}
}

// TestDoJSONRetryAfterWaitAbandonedOnCancel: Retry-After is honored up to
// 120s, which is long enough that a shutting-down worker must be able to walk
// out of it. The error that comes back is still the provider's 503 marked
// retryable — the job dead-letters as a transient failure, never as an allow.
func TestDoJSONRetryAfterWaitAbandonedOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := &stubTransport{}
	tr.fn = func(r *http.Request) (*http.Response, error) {
		// The caller gives up while DoJSON is inside the Retry-After wait.
		cancel()
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Retry-After": []string{"120"}},
			Body:       io.NopCloser(strings.NewReader("slow down")),
			Request:    r,
		}, nil
	}

	start := time.Now()
	_, err := DoJSON(ctx, &http.Client{Transport: tr},
		jsonBuilder("https://example.invalid/analyze", nil), 3, time.Millisecond, "")
	if err == nil {
		t.Fatal("want an error when the context ends mid-backoff")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v; cancellation must cut the 120s Retry-After short", elapsed)
	}
	if n := atomic.LoadInt32(&tr.calls); n != 1 {
		t.Errorf("requests = %d, want 1 (the retry never happened)", n)
	}
	if !moderation.IsRetryable(err) {
		t.Errorf("an abandoned backoff is still a transient failure, got %v", err)
	}
	if errors.Is(err, ErrAttemptsExhausted) {
		t.Errorf("the budget was abandoned, not exhausted: %v", err)
	}
	var herr *HTTPError
	if !errors.As(err, &herr) || herr.Status != http.StatusServiceUnavailable {
		t.Errorf("err = %v, want the provider's 503 preserved as the cause", err)
	}
}

// TestDoJSONExponentialWaitAbandonedOnCancel is the same abandonment for the
// ordinary exponential schedule (no Retry-After, no 429 floor). The cause that
// survives is the network error, and the budget must not be reported as
// exhausted — "we gave up early" and "we tried N times" send an operator to
// different places.
func TestDoJSONExponentialWaitAbandonedOnCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead: the first attempt fails at the transport

	var builds int
	start := time.Now()
	// An hour of backoff would be scheduled; a cancelled context must skip it.
	_, err := DoJSON(ctx, srv.Client(), func() (*http.Request, error) {
		builds++
		return NewJSONRequest(srv.URL, nil)
	}, 3, time.Hour, "")
	if err == nil {
		t.Fatal("want an error when the context is already cancelled")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v; a cancelled context must not sleep the backoff", elapsed)
	}
	if builds != 1 {
		t.Errorf("builds = %d, want 1 (no second attempt after cancellation)", builds)
	}
	if !moderation.IsRetryable(err) {
		t.Errorf("a cancelled backoff is still transient, got %v", err)
	}
	if errors.Is(err, ErrAttemptsExhausted) {
		t.Errorf("the budget was abandoned, not exhausted: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation preserved as the cause", err)
	}
}

// TestDoJSON429FloorBackoffCompletesThenRetries covers the Azure quota case to
// its end: a 429 with no usable Retry-After waits the rate429Floor and then
// RETRIES, where the sibling cancellation test only proves the wait can be cut
// short. Sleeping less than the floor walks straight back into the provider's
// rate window and burns the next attempt too.
//
// This is the one slow test in the package's retry set (~2s): rate429Floor is
// a production constant with no injection point, so the wait cannot be
// shortened from a test without changing httpx.go. It is deterministic — it
// asserts a lower bound on elapsed time, never an upper one.
func TestDoJSON429FloorBackoffCompletesThenRetries(t *testing.T) {
	t.Parallel()

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			http.Error(w, "quota exhausted", http.StatusTooManyRequests) // no Retry-After
			return
		}
		_, _ = w.Write([]byte(`{"recovered":true}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	start := time.Now()
	got, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, nil), 3, time.Millisecond, "",
		WithRetryLog(capture(&buf), "adapter:test"))
	if err != nil {
		t.Fatalf("the second attempt should have succeeded: %v", err)
	}
	if string(got) != `{"recovered":true}` {
		t.Errorf("body = %q", got)
	}
	if n := atomic.LoadInt32(&attempts); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
	if elapsed := time.Since(start); elapsed < rate429Floor {
		t.Errorf("waited %v, want >= %v (the 429 floor, not the %v base backoff)",
			elapsed, rate429Floor, time.Millisecond)
	}

	recs := records(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 backoff log, got %d: %s", len(recs), buf.String())
	}
	if recs[0]["delay_ms"] != float64(rate429Floor.Milliseconds()) {
		t.Errorf("delay_ms = %v, want the %v floor rather than the base backoff",
			recs[0]["delay_ms"], rate429Floor)
	}
	if recs[0]["status"] != float64(http.StatusTooManyRequests) || recs[0]["reason"] != "status" {
		t.Errorf("log must carry status=429 reason=status, got %v/%v", recs[0]["status"], recs[0]["reason"])
	}
}

// TestNewJSONRequestNilBodyIsAnEmptyPost: an adapter that has nothing to send
// still gets a well-formed, rewindable request — DoJSON rebuilds per attempt
// and would otherwise dereference a nil reader on retry.
func TestNewJSONRequestNilBodyIsAnEmptyPost(t *testing.T) {
	req, err := NewJSONRequest("https://example.invalid/analyze", nil)
	if err != nil {
		t.Fatalf("NewJSONRequest: %v", err)
	}
	if req.ContentLength != 0 {
		t.Errorf("ContentLength = %d, want 0", req.ContentLength)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil || len(body) != 0 {
		t.Errorf("body = %q, err = %v; want an empty readable body", body, err)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json even for an empty body", got)
	}
}

// TestNewMultipartRequestInputEdges: the builder runs once per attempt with
// whatever the adapter holds, including a frame that read back empty and a URL
// assembled from config. A malformed URL must fail at construction (terminal,
// never retried); everything else must still produce a parseable part.
func TestNewMultipartRequestInputEdges(t *testing.T) {
	cases := []struct {
		name     string
		url      string
		field    string
		filename string
		content  []byte
		wantErr  bool
	}{
		{
			name: "nil content yields an empty part",
			url:  "https://example.invalid/task/sync", field: "media", filename: "frame.jpg",
		},
		{
			name: "empty content yields an empty part",
			url:  "https://example.invalid/task/sync", field: "media", filename: "frame.jpg",
			content: []byte{},
		},
		{
			name: "empty filename is still a file part",
			url:  "https://example.invalid/task/sync", field: "media", filename: "",
			content: []byte("bytes"),
		},
		{
			name: "empty field name is still encoded",
			url:  "https://example.invalid/task/sync", field: "", filename: "frame.jpg",
			content: []byte("bytes"),
		},
		{
			name: "a quote in the filename is escaped, not injected",
			url:  "https://example.invalid/task/sync", field: "media", filename: `a"b.jpg`,
			content: []byte("bytes"),
		},
		{
			name: "a NUL byte in content survives",
			url:  "https://example.invalid/task/sync", field: "media", filename: "frame.jpg",
			content: []byte{0x00, 0xFF, 0x00},
		},
		{
			name: "malformed url fails at construction",
			url:  "://not a url", field: "media", filename: "frame.jpg", wantErr: true,
		},
		{
			name: "control character in url fails at construction",
			url:  "https://example.invalid/\x7f", field: "media", filename: "frame.jpg", wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := NewMultipartRequest(tc.url, tc.field, tc.filename, tc.content)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want a construction error, got none")
				}
				if req != nil {
					t.Error("a failed construction must not return a request")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewMultipartRequest: %v", err)
			}

			_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if err != nil {
				t.Fatalf("parse Content-Type: %v", err)
			}
			part, err := multipart.NewReader(req.Body, params["boundary"]).NextPart()
			if err != nil {
				t.Fatalf("read part: %v", err)
			}
			if part.FormName() != tc.field {
				t.Errorf("form name = %q, want %q", part.FormName(), tc.field)
			}
			if part.FileName() != tc.filename {
				t.Errorf("filename = %q, want %q", part.FileName(), tc.filename)
			}
			got, err := io.ReadAll(part)
			if err != nil {
				t.Fatalf("read part body: %v", err)
			}
			// bytes.Equal treats nil and an empty slice as equal, so the
			// empty-content and nil-content rows need no special case:
			// io.ReadAll returns a non-nil empty slice for an empty part.
			if !bytes.Equal(got, tc.content) {
				t.Errorf("part bytes = %v, want %v", got, tc.content)
			}
		})
	}
}
