package moderate

import (
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

// jsonBuilder returns the per-attempt request builder DoJSON expects. It is
// rebuilt per attempt on purpose: a rewound body is what makes retries work.
func jsonBuilder(url string, body []byte) func() (*http.Request, error) {
	return func() (*http.Request, error) { return NewJSONRequest(url, body) }
}

func TestDoJSONReturnsBodyOnSuccess(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	got, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, []byte(`{}`)), 3, time.Millisecond, "")
	if err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("body = %q", got)
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Errorf("attempts = %d, want 1 (a 2xx must not be retried)", n)
	}
}

// TestDoJSONTerminalOn4xx pins the F.4 classification: a 400 is a request
// the provider will never accept, so retrying it only burns quota and delays
// the dead-letter. It must also NOT be marked retryable.
func TestDoJSONTerminalOn4xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("x-ms-error-code", "InvalidImageFormat")
		http.Error(w, "unsupported media", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, nil), 3, time.Millisecond, "x-ms-error-code")
	if err == nil {
		t.Fatal("a 400 must fail")
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Errorf("attempts = %d, want 1 (terminal 4xx must not be retried)", n)
	}
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("error is not *HTTPError: %v", err)
	}
	if herr.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want 400", herr.Status)
	}
	if herr.Code != "InvalidImageFormat" {
		t.Errorf("Code = %q, want the provider error code from the header", herr.Code)
	}
	if moderation.IsRetryable(err) {
		t.Error("a terminal 4xx must not be marked retryable")
	}
}

// TestDoJSONRetriesServerErrorsThenGivesUp: 5xx is retryable, and the
// exhausted error must carry the retryable mark so the pipeline routes it to
// verdict=error + dead-letter rather than treating it as a hard failure.
func TestDoJSONRetriesServerErrorsThenGivesUp(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, nil), 3, time.Millisecond, "")
	if err == nil {
		t.Fatal("exhausted retries must return an error, never a nil/empty body")
	}
	if n := atomic.LoadInt32(&attempts); n != 3 {
		t.Errorf("attempts = %d, want 3 (maxAttempts)", n)
	}
	if !moderation.IsRetryable(err) {
		t.Errorf("exhausted retries must be marked retryable, got %v", err)
	}
}

func TestDoJSONRecoversAfterTransient5xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"recovered":true}`))
	}))
	defer srv.Close()

	got, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, nil), 3, time.Millisecond, "")
	if err != nil {
		t.Fatalf("second attempt should have succeeded: %v", err)
	}
	if string(got) != `{"recovered":true}` {
		t.Errorf("body = %q", got)
	}
}

// TestDoJSONHonorsRetryAfter: the header, not the exponential schedule,
// decides the wait when the provider supplies one. Sleeping less than asked
// is what walks a client straight back into the rate window.
func TestDoJSONHonorsRetryAfter(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	start := time.Now()
	if _, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, nil), 3, time.Millisecond, ""); err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("waited %v, want >= 1s (Retry-After ignored)", elapsed)
	}
}

// TestDoJSON429WithoutRetryAfterRespectsContext covers the Azure-style 429
// that carries no usable header: the backoff floor is seconds long, so a
// cancelled context must cut it short and still return a retryable error.
func TestDoJSON429WithoutRetryAfterRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "quota exhausted", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := DoJSON(ctx, srv.Client(), jsonBuilder(srv.URL, nil), 3, time.Millisecond, "")
	if err == nil {
		t.Fatal("want an error when the context ends mid-backoff")
	}
	if !moderation.IsRetryable(err) {
		t.Errorf("a cancelled backoff is still a transient failure, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > rate429Floor {
		t.Errorf("waited %v; cancellation must cut the %v 429 floor short", elapsed, rate429Floor)
	}
}

// TestDoJSONBuildErrorIsTerminal: a builder failure is a bug in our own
// request construction. Retrying it cannot help.
func TestDoJSONBuildErrorIsTerminal(t *testing.T) {
	buildErr := errors.New("cannot build request")
	var calls int
	_, err := DoJSON(context.Background(), http.DefaultClient, func() (*http.Request, error) {
		calls++
		return nil, buildErr
	}, 3, time.Millisecond, "")

	if !errors.Is(err, buildErr) {
		t.Fatalf("err = %v, want the builder error", err)
	}
	if calls != 1 {
		t.Errorf("builder called %d times, want 1", calls)
	}
	if moderation.IsRetryable(err) {
		t.Error("a request-construction bug must not be marked retryable")
	}
}

// TestDoJSONRetriesNetworkFailure: a connection that never opens is
// transient by the same rule as a 5xx.
func TestDoJSONRetriesNetworkFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	var builds int
	_, err := DoJSON(context.Background(), http.DefaultClient, func() (*http.Request, error) {
		builds++
		return NewJSONRequest(url, nil)
	}, 2, time.Millisecond, "")
	if err == nil {
		t.Fatal("a dead endpoint must return an error")
	}
	if builds != 2 {
		t.Errorf("builds = %d, want 2 (network failures are retried)", builds)
	}
	if !moderation.IsRetryable(err) {
		t.Errorf("network failure must be marked retryable, got %v", err)
	}
}

// TestDoJSONDefaultsAreSane: zero values must not mean "never attempt" or
// "hot-loop the provider".
func TestDoJSONZeroAttemptsFallsBackToDefault(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := DoJSON(context.Background(), srv.Client(), jsonBuilder(srv.URL, nil), 0, 0, ""); err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		http.StatusOK:                    false,
		http.StatusBadRequest:            false,
		http.StatusUnauthorized:          false,
		http.StatusForbidden:             false,
		http.StatusRequestEntityTooLarge: false,
		http.StatusTooManyRequests:       true,
		http.StatusInternalServerError:   true,
		http.StatusBadGateway:            true,
		http.StatusServiceUnavailable:    true,
		http.StatusGatewayTimeout:        true,
	}
	for code, want := range cases {
		if got := RetryableStatus(code); got != want {
			t.Errorf("RetryableStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

// TestRetryAfter bounds the header: an absurd or unparseable value must not
// be able to park a worker. 120s is the documented ceiling.
func TestRetryAfter(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"120", 120 * time.Second},
		{"121", 0},                           // over the ceiling: fall back to backoff
		{"0", 0},                             // useless value
		{"-3", 0},                            // nonsense
		{"soon", 0},                          // unparseable
		{"Wed, 21 Oct 2015 07:28:00 GMT", 0}, // HTTP-date form is not honored
	}
	for _, tc := range cases {
		resp := &http.Response{Header: http.Header{}}
		if tc.header != "" {
			resp.Header.Set("Retry-After", tc.header)
		}
		if got := RetryAfter(resp); got != tc.want {
			t.Errorf("RetryAfter(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestSleepCtxReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleepCtx = %v, want context.Canceled", err)
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleepCtx = %v, want nil after the timer fires", err)
	}
}

// TestHTTPErrorMessageIsTruncated: provider bodies are attacker-influenced
// and can be megabytes. The error string is logged, so it stays bounded.
func TestHTTPErrorMessageIsTruncated(t *testing.T) {
	herr := &HTTPError{Status: 500, Code: "ServerBusy", Body: strings.Repeat("x", 5000)}
	msg := herr.Error()
	if !strings.Contains(msg, "500") || !strings.Contains(msg, "ServerBusy") {
		t.Errorf("message drops the status/code: %q", msg)
	}
	if len(msg) > 400 {
		t.Errorf("message length %d; a 5000-byte body must be truncated", len(msg))
	}
	if !strings.HasSuffix(msg, "…") {
		t.Errorf("truncated message should be marked with an ellipsis: %q", msg[len(msg)-10:])
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate under the limit changed the string: %q", got)
	}
	if got := truncate("exactly10!", 10); got != "exactly10!" {
		t.Errorf("truncate at the limit changed the string: %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate(%q, 3) = %q", "abcdef", got)
	}
}

func TestNewJSONRequest(t *testing.T) {
	req, err := NewJSONRequest("https://example.invalid/analyze", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("NewJSONRequest: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	body, _ := io.ReadAll(req.Body)
	if string(body) != `{"a":1}` {
		t.Errorf("body = %q", body)
	}
	if _, err := NewJSONRequest("://not a url", nil); err == nil {
		t.Error("a malformed URL must fail at construction (terminal, not retried)")
	}
}

// TestNewMultipartRequestRoundTrip: hive takes an uploaded file, not a
// JSON-encoded blob. The part must survive a real multipart parse, and each
// call must produce a fresh rewound body so DoJSON's retries resend it.
func TestNewMultipartRequestRoundTrip(t *testing.T) {
	content := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10} // JPEG SOI bytes
	req, err := NewMultipartRequest("https://example.invalid/task/sync", "media", "frame.jpg", content)
	if err != nil {
		t.Fatalf("NewMultipartRequest: %v", err)
	}

	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse Content-Type: %v", err)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}

	mr := multipart.NewReader(req.Body, params["boundary"])
	part, err := mr.NextPart()
	if err != nil {
		t.Fatalf("read part: %v", err)
	}
	if part.FormName() != "media" {
		t.Errorf("form name = %q, want media", part.FormName())
	}
	if part.FileName() != "frame.jpg" {
		t.Errorf("filename = %q, want frame.jpg", part.FileName())
	}
	got, err := io.ReadAll(part)
	if err != nil {
		t.Fatalf("read part body: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("part bytes = %v, want %v", got, content)
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Errorf("want exactly one part, got another (%v)", err)
	}

	if _, err := NewMultipartRequest("://not a url", "media", "f.jpg", content); err == nil {
		t.Error("a malformed URL must fail at construction")
	}
}

// TestMultipartBodyIsRewoundPerAttempt: DoJSON rebuilds the request each
// attempt precisely so a retry resends the bytes. A shared reader would make
// the retry upload an empty file — a silent misclassification, not an error.
func TestMultipartBodyIsRewoundPerAttempt(t *testing.T) {
	content := []byte("frame-bytes")
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse form: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("media")
		if err != nil {
			t.Errorf("form file: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		defer func() { _ = f.Close() }()
		sizes = append(sizes, int(hdr.Size))
		if len(sizes) == 1 {
			http.Error(w, "transient", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := DoJSON(context.Background(), srv.Client(), func() (*http.Request, error) {
		return NewMultipartRequest(srv.URL, "media", "frame.jpg", content)
	}, 3, time.Millisecond, "")
	if err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if len(sizes) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(sizes))
	}
	if sizes[1] != len(content) {
		t.Errorf("retry uploaded %d bytes, want %d — the body was not rewound", sizes[1], len(content))
	}
}
