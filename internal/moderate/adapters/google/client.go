package google

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// maxBackoff caps every wait between attempts (exponential fallback or a
// server-supplied Retry-After) so a hostile/buggy server cannot park a worker.
const maxBackoff = 30 * time.Second

// safeSearchFeature is the Vision feature type requested for moderation.
const safeSearchFeature = "SAFE_SEARCH_DETECTION"

// annotateRequest is the images:annotate request body. One image per request.
type annotateRequest struct {
	Requests []annotateRequestItem `json:"requests"`
}

type annotateRequestItem struct {
	Image    requestImage     `json:"image"`
	Features []requestFeature `json:"features"`
}

type requestImage struct {
	Content string `json:"content"` // base64-encoded image bytes
}

type requestFeature struct {
	Type string `json:"type"`
}

// topLevelError is the request-level error envelope returned with a non-2xx
// status ({"error":{"code","message","status"}}). Per-image failures instead
// sit at responses[].error (annotateError) and arrive with HTTP 200.
type topLevelError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// apiError carries a classified Vision failure. Retryable drives bounded
// backoff; Code is surfaced to observability via moderation.CodedError.
type apiError struct {
	Status     int
	Code       string
	Message    string
	Retryable  bool
	retryAfter time.Duration
}

func (e *apiError) Error() string {
	return fmt.Sprintf("google: HTTP %d code=%s: %s", e.Status, e.Code, e.Message)
}

// ErrorCode implements moderation.CodedError so observability can label
// vismod_adapter_errors_total{code}.
func (e *apiError) ErrorCode() string { return e.Code }

// client is the direct-REST Vision v1 images:annotate client. Vision's Go SDK
// pulls a heavy dependency tree, so v1 calls net/http directly with a JSON body.
type client struct {
	httpClient *http.Client
	endpoint   string // full annotate URL, e.g. https://vision.googleapis.com/v1/images:annotate
	auth       authProvider
	limiter    *rate.Limiter
	maxRetries int
	backoff    time.Duration

	// rngMu guards rng: one client is shared across worker goroutines and
	// math/rand/v2 *Rand is not goroutine-safe, so concurrent retries-with-jitter
	// would race on the PCG state.
	rngMu sync.Mutex
	rng   *rand.Rand // backoff jitter source; nil disables jitter (tests)
}

// newClient constructs a Vision client. A nil rng disables backoff jitter (used
// by tests for determinism).
func newClient(endpoint string, auth authProvider, rps float64, maxRetries int, backoff time.Duration, rng *rand.Rand) *client {
	return &client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		endpoint:   endpoint,
		auth:       auth,
		// Shared bucket, burst 1: aggregate rate == rps regardless of worker x
		// frame parallelism (spec §F.3).
		limiter:    rate.NewLimiter(rate.Limit(rps), 1),
		maxRetries: maxRetries,
		backoff:    backoff,
		rng:        rng,
	}
}

// analyze submits one image and returns the parsed response. It honors the
// shared rate limiter and retries transient failures (429/5xx/net) with bounded
// backoff, respecting Retry-After. Per-response (HTTP 200) errors and missing
// annotations are the caller's (google.go) concern.
func (c *client) analyze(ctx context.Context, img []byte) (annotateResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			wait := c.retryWait(attempt, lastErr)
			select {
			case <-ctx.Done():
				return annotateResponse{}, ctx.Err()
			case <-time.After(wait):
			}
		}

		if err := c.limiter.Wait(ctx); err != nil {
			return annotateResponse{}, err
		}

		resp, aerr := c.do(ctx, img)
		if aerr == nil {
			return resp, nil
		}
		lastErr = aerr

		var ae *apiError
		if errors.As(aerr, &ae) && !ae.Retryable {
			return annotateResponse{}, aerr // terminal (4xx validation, decode)
		}
	}
	return annotateResponse{}, fmt.Errorf("google: exhausted %d retries: %w", c.maxRetries, lastErr)
}

// do performs a single HTTP attempt and classifies the outcome.
func (c *client) do(ctx context.Context, img []byte) (annotateResponse, error) {
	body, err := json.Marshal(annotateRequest{
		Requests: []annotateRequestItem{{
			Image:    requestImage{Content: base64.StdEncoding.EncodeToString(img)},
			Features: []requestFeature{{Type: safeSearchFeature}},
		}},
	})
	if err != nil {
		return annotateResponse{}, fmt.Errorf("google: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return annotateResponse{}, fmt.Errorf("google: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth.apply(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport errors (timeout, conn reset) are transient -> retryable.
		return annotateResponse{}, &apiError{Status: 0, Code: "network", Message: err.Error(), Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return annotateResponse{}, &apiError{Status: resp.StatusCode, Code: "read_body", Message: err.Error(), Retryable: true}
	}

	if resp.StatusCode == http.StatusOK {
		var out annotateResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			// A 200 we can't parse is terminal (won't fix on retry).
			return annotateResponse{}, &apiError{Status: 200, Code: "decode", Message: err.Error(), Retryable: false}
		}
		return out, nil
	}
	return annotateResponse{}, classifyHTTPError(resp, raw)
}

// classifyHTTPError maps a non-200 response into a retryable/terminal apiError.
//   - 429, 5xx, 408 -> retryable (transient)
//   - other 4xx     -> terminal (validation, unsupported/oversize media)
func classifyHTTPError(resp *http.Response, raw []byte) *apiError {
	var body topLevelError
	_ = json.Unmarshal(raw, &body)
	msg := body.Error.Message
	// Prefer the symbolic google.rpc status (e.g. INVALID_ARGUMENT) as the
	// operator-facing code; fall back to the HTTP status.
	code := body.Error.Status
	if code == "" {
		code = "http_" + strconv.Itoa(resp.StatusCode)
	}

	retryable := resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusRequestTimeout ||
		resp.StatusCode >= 500
	return &apiError{
		Status:     resp.StatusCode,
		Code:       code,
		Message:    msg,
		Retryable:  retryable,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

// retryWait computes the backoff for the next attempt: Retry-After when present,
// else jittered exponential (backoff * 2^(attempt-1), clamped to maxBackoff).
func (c *client) retryWait(attempt int, lastErr error) time.Duration {
	var ae *apiError
	if errors.As(lastErr, &ae) && ae.retryAfter > 0 {
		return ae.retryAfter
	}
	w := c.backoff << (attempt - 1)
	if w <= 0 || w > maxBackoff {
		w = maxBackoff
	}
	return c.applyJitter(w)
}

// applyJitter returns an equal-jitter wait in [d/2, d]. A nil rng returns d
// unchanged so tests stay deterministic.
func (c *client) applyJitter(d time.Duration) time.Duration {
	if c.rng == nil {
		return d
	}
	half := d / 2
	c.rngMu.Lock()
	j := c.rng.Int64N(int64(half) + 1)
	c.rngMu.Unlock()
	return half + time.Duration(j)
}

// parseRetryAfter reads the Retry-After header (delta-seconds or HTTP-date),
// clamped to maxBackoff; a past/invalid value yields 0.
func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = time.Until(t)
	}
	if d <= 0 {
		return 0
	}
	return min(d, maxBackoff)
}
