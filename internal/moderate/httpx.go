package moderate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"github.com/vismod/vismod/pkg/moderation"
)

// HTTPError carries the provider status code for error classification and
// metrics.
type HTTPError struct {
	Status int
	Code   string // provider error code (e.g. x-ms-error-code) when present
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("provider returned %d (code=%q): %s", e.Status, e.Code, truncate(e.Body, 200))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DoJSON POSTs body and returns the response body, retrying transient
// failures with bounded exponential backoff.
//
// Classification (F.4): 429, 5xx, timeouts, and transient network errors
// are retryable; other 4xx are terminal (no retry). Retry-After is
// honored. After retries are exhausted the error is marked
// moderation.Retryable so the caller's fail-safe path (Verdict=error →
// dead-letter) can distinguish it — it never becomes "allow".
func DoJSON(ctx context.Context, client *http.Client, build func() (*http.Request, error), maxAttempts int, baseBackoff time.Duration, errCodeHeader string) ([]byte, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if baseBackoff <= 0 {
		baseBackoff = 500 * time.Millisecond
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err // terminal: request construction bug
		}
		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			lastErr = err // network/timeout: retryable
		} else {
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			_ = resp.Body.Close()
			if rerr != nil {
				lastErr = rerr
			} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return body, nil
			} else {
				herr := &HTTPError{Status: resp.StatusCode, Body: string(body)}
				if errCodeHeader != "" {
					herr.Code = resp.Header.Get(errCodeHeader)
				}
				if !RetryableStatus(resp.StatusCode) {
					return nil, herr // terminal 4xx: fail now, no retry
				}
				lastErr = herr
				if ra := RetryAfter(resp); ra > 0 && attempt < maxAttempts {
					if err := sleepCtx(ctx, ra); err != nil {
						return nil, moderation.Retryable(lastErr)
					}
					continue
				}
				// A 429 without a usable Retry-After means we're inside the
				// provider's rate window: exponential backoff from a floor
				// long enough to actually leave it (Azure returns 429 with
				// no header on quota exhaustion).
				if resp.StatusCode == http.StatusTooManyRequests && attempt < maxAttempts {
					if err := sleepCtx(ctx, rate429Floor*time.Duration(1<<(attempt-1))); err != nil {
						return nil, moderation.Retryable(lastErr)
					}
					continue
				}
			}
		}
		if attempt < maxAttempts {
			if err := sleepCtx(ctx, baseBackoff*time.Duration(1<<(attempt-1))); err != nil {
				return nil, moderation.Retryable(lastErr)
			}
		}
	}
	return nil, moderation.Retryable(fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr))
}

// rate429Floor is the minimum backoff for a 429 that carries no usable
// Retry-After header (grows exponentially per attempt).
const rate429Floor = 2 * time.Second

// RetryableStatus reports whether an HTTP status is transient (F.4):
// 429 and 5xx retry, every other 4xx is terminal.
func RetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// RetryAfter returns a usable Retry-After delay, or 0. Values above 120s
// are ignored so a hostile or broken header cannot stall a worker.
func RetryAfter(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 && secs <= 120 {
			return time.Duration(secs) * time.Second
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// NewJSONRequest builds a POST with a JSON body and content type.
func NewJSONRequest(url string, body []byte) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// NewMultipartRequest builds a POST carrying content as a single
// multipart/form-data file part. Providers that take an uploaded file
// rather than a JSON-encoded blob need this.
//
// The body is materialized in memory and re-encoded on every call, so
// DoJSON's per-attempt builder gets a fresh, rewound reader. Callers pass
// frame bytes already bounded by Caps.MaxImageBytes.
func NewMultipartRequest(url, field, filename string, content []byte) (*http.Request, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(content); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req, nil
}
