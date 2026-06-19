package azure

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// newTestClient points the adapter at an httptest server with fast retries.
func newTestClient(t *testing.T, srv *httptest.Server, maxRetries int) *client {
	t.Helper()
	return &client{
		httpClient: srv.Client(),
		endpoint:   srv.URL,
		apiVersion: defaultAPIVersion,
		auth:       apiKeyAuth{key: "test-key"},
		limiter:    rate.NewLimiter(rate.Inf, 1),
		maxRetries: maxRetries,
		backoff:    time.Millisecond,
	}
}

func TestClientAnalyzeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Ocp-Apim-Subscription-Key"); got != "test-key" {
			t.Errorf("missing/wrong auth header: %q", got)
		}
		if !strings.Contains(r.URL.RawQuery, "api-version="+defaultAPIVersion) {
			t.Errorf("api-version not in query: %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"categoriesAnalysis":[{"category":"Violence","severity":6}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 3)
	resp, err := c.analyze(context.Background(), []byte("imgbytes"))
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(resp.CategoriesAnalysis) != 1 || resp.CategoriesAnalysis[0].Severity != 6 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestClientRetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("x-ms-error-code", "TooManyRequests")
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"TooManyRequests","message":"slow down"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"categoriesAnalysis":[{"category":"Hate","severity":0}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 3)
	if _, err := c.analyze(context.Background(), []byte("x")); err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("want 2 calls (1 fail + 1 ok), got %d", got)
	}
}

func TestClientTerminalOn4xxNoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("x-ms-error-code", "InvalidRequest")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"InvalidRequest","message":"bad image"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 3)
	_, err := c.analyze(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("want terminal error on 400")
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("want *apiError, got %T", err)
	}
	if ae.Code != "InvalidRequest" {
		t.Errorf("error code = %q, want surfaced x-ms-error-code", ae.Code)
	}
	if ae.Retryable {
		t.Error("400 must be terminal, not retryable")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("4xx must not retry; got %d calls", got)
	}
}

func TestClientExhaustsRetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"InternalError","message":"boom"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 2)
	if _, err := c.analyze(context.Background(), []byte("x")); err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if got := atomic.LoadInt32(&calls); got != 3 { // 1 initial + 2 retries
		t.Fatalf("want 3 attempts, got %d", got)
	}
}

func TestRetryWaitHonorsRetryAfterOn5xx(t *testing.T) {
	c := &client{backoff: time.Second}
	// Azure sends Retry-After on 503 too, not just 429. The server hint must win
	// over blind exponential backoff for any retryable status.
	err := &apiError{Status: http.StatusServiceUnavailable, Retryable: true, retryAfter: 3 * time.Second}
	if got := c.retryWait(1, err); got != 3*time.Second {
		t.Fatalf("retryWait on 503 with Retry-After = %v, want 3s", got)
	}
}

func TestRetryWaitFallsBackToExponentialWithoutRetryAfter(t *testing.T) {
	c := &client{backoff: time.Second}
	// No Retry-After -> exponential: backoff * 2^(attempt-1) = 1s * 2^1 = 2s.
	err := &apiError{Status: http.StatusServiceUnavailable, Retryable: true}
	if got := c.retryWait(2, err); got != 2*time.Second {
		t.Fatalf("retryWait without Retry-After = %v, want 2s", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("2"); got != 2*time.Second {
		t.Errorf("parseRetryAfter(2) = %v", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("parseRetryAfter(empty) = %v", got)
	}
	if got := parseRetryAfter("Wed, 21 Oct 2099 07:28:00 GMT"); got <= 0 {
		t.Errorf("parseRetryAfter(future http-date) = %v, want > 0", got)
	}
	if got := parseRetryAfter("Wed, 21 Oct 1999 07:28:00 GMT"); got != 0 {
		t.Errorf("parseRetryAfter(past http-date) = %v, want 0", got)
	}
	if got := parseRetryAfter("garbage"); got != 0 {
		t.Errorf("parseRetryAfter(garbage) = %v, want 0", got)
	}
}
