package moderate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

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

// errorBodyKeep bounds how much of a failed response is retained.
//
// The body is read under a 4 MiB LimitReader, but Error() only ever shows
// the first 200 bytes. Materializing the rest as a string allocated up to
// 4 MiB per failed attempt — on the retry path, so a 429/5xx storm did it
// repeatedly per frame, at exactly the moment the process was already
// under pressure. A kilobyte leaves ample room for the message plus any
// structured error a provider puts after it.
const errorBodyKeep = 1 << 10

// retainedErrorBody keeps the head of an error body, on a rune boundary so
// the retained text is still valid UTF-8 for logs.
func retainedErrorBody(body []byte) string {
	if len(body) <= errorBodyKeep {
		return string(body)
	}
	b := body[:errorBodyKeep]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1] // trim a split multi-byte rune
	}
	return string(b)
}

// ErrAttemptsExhausted marks the error returned when the retry budget ran
// out, as opposed to a terminal status that was never retried.
//
// The two need different operator responses — one means "the destination
// is down or throttling", the other means "the request itself is wrong and
// will never succeed" — so callers must be able to tell them apart with
// errors.Is rather than by reading a message.
var ErrAttemptsExhausted = errors.New("attempts exhausted")

// Bounded reasons for a retry. These reach log fields and metric labels,
// so they are a closed set: an error string here would be unbounded
// cardinality, and could carry a URL (i.e. a credential — see
// WithRetryLog).
const (
	retryReasonStatus  = "status"
	retryReasonNetwork = "network"
	retryReasonBody    = "read_body"
)

type doOpts struct {
	log     *slog.Logger
	target  string
	onRetry func()
}

// DoOption configures DoJSON. It is variadic so every existing adapter
// call site keeps compiling unchanged.
type DoOption func(*doOpts)

// WithRetryLog emits one warn-level record before each backoff sleep, so
// a worker that stalls for seconds inside a retry budget says why.
//
// WHAT "target" MEANS — read this before passing anything.
//
// target is an OPERATOR-FACING LABEL that answers "which configured thing
// was being called", NOT an address. It is emitted verbatim as the
// `target` log field. The name is a frequent source of confusion because
// it reads like a destination URL, which is the one thing it must never
// be.
//
//	CORRECT:   "webhook[1]"      the sink's position in output.sinks
//	CORRECT:   "adapter:hive"    the configured adapter
//	FORBIDDEN: "https://discord.com/api/webhooks/123/abc"
//	FORBIDDEN: anything derived from the request URL, including
//	           url.Redacted() — it strips userinfo and leaves the PATH
//	           intact, and a Discord webhook's token lives in the path.
//
// A URL here publishes a credential to stderr on every 429, i.e. exactly
// when a throttled destination is generating the most log volume.
// TestDoJSONRetryLogNeverCarriesTheURL fails on any regression.
//
// The value must also be LOW-CARDINALITY and caller-chosen — never a job
// id, a source ref, or an error string, all of which are unbounded and
// can carry caller data.
//
// In the result package, target is set from WebhookOptions.Name, so the
// `target` field of a retry warning and the `sink` field of the matching
// give-up error carry the same string. See docs/result-envelope.md.
func WithRetryLog(log *slog.Logger, target string) DoOption {
	return func(o *doOpts) {
		o.log = log
		o.target = target
	}
}

// OnRetry registers a callback fired once per backoff. It lets a caller
// drive a counter without this package taking a metrics dependency.
func OnRetry(f func()) DoOption {
	return func(o *doOpts) { o.onRetry = f }
}

// DoJSON POSTs body and returns the response body, retrying transient
// failures with bounded exponential backoff.
//
// Classification (F.4): 429, 5xx, timeouts, and transient network errors
// are retryable; other 4xx are terminal (no retry). Retry-After is
// honored. After retries are exhausted the error wraps ErrAttemptsExhausted
// and is marked moderation.Retryable so the caller's fail-safe path
// (Verdict=error → dead-letter) can distinguish it — it never becomes
// "allow".
func DoJSON(ctx context.Context, client *http.Client, build func() (*http.Request, error), maxAttempts int, baseBackoff time.Duration, errCodeHeader string, opts ...DoOption) ([]byte, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if baseBackoff <= 0 {
		baseBackoff = 500 * time.Millisecond
	}
	var o doOpts
	for _, opt := range opts {
		opt(&o)
	}

	// backoff logs the wait and then performs it. Every sleep in this
	// function goes through here, so there is no silent stall path.
	backoff := func(attempt int, d time.Duration, status int, reason string) error {
		if o.onRetry != nil {
			o.onRetry()
		}
		if o.log != nil {
			o.log.Warn("retrying after transient failure",
				"target", o.target,
				"attempt", attempt,
				"max_attempts", maxAttempts,
				"delay_ms", d.Milliseconds(),
				"status", status,
				"reason", reason,
			)
		}
		return sleepCtx(ctx, d)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err // terminal: request construction bug
		}
		status, reason := 0, retryReasonNetwork
		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			lastErr = err // network/timeout: retryable
		} else {
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			_ = resp.Body.Close()
			status = resp.StatusCode
			if rerr != nil {
				lastErr = rerr
				reason = retryReasonBody
			} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return body, nil
			} else {
				herr := &HTTPError{Status: resp.StatusCode, Body: retainedErrorBody(body)}
				if errCodeHeader != "" {
					herr.Code = resp.Header.Get(errCodeHeader)
				}
				if !RetryableStatus(resp.StatusCode) {
					return nil, herr // terminal 4xx: fail now, no retry
				}
				lastErr = herr
				reason = retryReasonStatus
				if ra := RetryAfter(resp); ra > 0 && attempt < maxAttempts {
					if err := backoff(attempt, ra, status, reason); err != nil {
						return nil, moderation.Retryable(lastErr)
					}
					continue
				}
				// A 429 without a usable Retry-After means we're inside the
				// provider's rate window: exponential backoff from a floor
				// long enough to actually leave it (Azure returns 429 with
				// no header on quota exhaustion).
				if resp.StatusCode == http.StatusTooManyRequests && attempt < maxAttempts {
					d := rate429Floor * time.Duration(1<<(attempt-1))
					if err := backoff(attempt, d, status, reason); err != nil {
						return nil, moderation.Retryable(lastErr)
					}
					continue
				}
			}
		}
		if attempt < maxAttempts {
			d := baseBackoff * time.Duration(1<<(attempt-1))
			if err := backoff(attempt, d, status, reason); err != nil {
				return nil, moderation.Retryable(lastErr)
			}
		}
	}
	return nil, moderation.Retryable(fmt.Errorf("%w after %d attempts: %w", ErrAttemptsExhausted, maxAttempts, lastErr))
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
