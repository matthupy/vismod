package hive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

// maxBackoff caps every wait between attempts (exponential fallback or a
// server-supplied Retry-After) so a hostile/buggy server cannot park a worker.
const maxBackoff = 30 * time.Second

// hiveErrorResponse is a best-effort decode of Hive's error body. The wire
// schema for errors is not strongly documented, so we read the common
// message/error fields opportunistically and otherwise fall back to the HTTP
// status as the operator-facing code.
type hiveErrorResponse struct {
	Message string `json:"message"`
	Error   string `json:"error"`
	Code    string `json:"code"`
}

// apiError carries a classified Hive failure. Retryable drives bounded backoff;
// Code is surfaced to observability via moderation.CodedError.
type apiError struct {
	Status     int
	Code       string
	Message    string
	Retryable  bool
	retryAfter time.Duration
}

func (e *apiError) Error() string {
	return fmt.Sprintf("hive: HTTP %d code=%s: %s", e.Status, e.Code, e.Message)
}

// ErrorCode implements moderation.CodedError so observability can label
// vismod_adapter_errors_total{code}.
func (e *apiError) ErrorCode() string { return e.Code }

// client is the direct-REST Hive v2 /task/sync client. Hive has no Go SDK, so it
// calls net/http directly and submits the image as multipart/form-data.
type client struct {
	httpClient *http.Client
	endpoint   string // full sync URL, e.g. https://api.thehive.ai/api/v2/task/sync
	token      string // project API token (sent as "Authorization: Token <token>")
	limiter    *rate.Limiter
	maxRetries int
	backoff    time.Duration
	rng        *rand.Rand // backoff jitter source; nil disables jitter (tests)
}

// newClient constructs a Hive client. A nil rng disables backoff jitter (used by
// tests for determinism).
func newClient(endpoint, token string, rps float64, maxRetries int, backoff time.Duration, rng *rand.Rand) *client {
	return &client{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		endpoint:   endpoint,
		token:      token,
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
// backoff, respecting Retry-After.
func (c *client) analyze(ctx context.Context, img []byte, mimeType string) (hiveResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			wait := c.retryWait(attempt, lastErr)
			select {
			case <-ctx.Done():
				return hiveResponse{}, ctx.Err()
			case <-time.After(wait):
			}
		}

		if err := c.limiter.Wait(ctx); err != nil {
			return hiveResponse{}, err
		}

		resp, aerr := c.do(ctx, img, mimeType)
		if aerr == nil {
			return resp, nil
		}
		lastErr = aerr

		var ae *apiError
		if errors.As(aerr, &ae) && !ae.Retryable {
			return hiveResponse{}, aerr // terminal (4xx validation, unsupported)
		}
	}
	return hiveResponse{}, fmt.Errorf("hive: exhausted %d retries: %w", c.maxRetries, lastErr)
}

// do performs a single multipart HTTP attempt and classifies the outcome.
func (c *client) do(ctx context.Context, img []byte, mimeType string) (hiveResponse, error) {
	body, contentType, err := buildMultipart(img, mimeType)
	if err != nil {
		return hiveResponse{}, fmt.Errorf("hive: build multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, body)
	if err != nil {
		return hiveResponse{}, fmt.Errorf("hive: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Token "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport errors (timeout, conn reset) are transient -> retryable.
		return hiveResponse{}, &apiError{Status: 0, Code: "network", Message: err.Error(), Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return hiveResponse{}, &apiError{Status: resp.StatusCode, Code: "read_body", Message: err.Error(), Retryable: true}
	}

	if resp.StatusCode == http.StatusOK {
		var out hiveResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			// A 200 we can't parse is terminal (won't fix on retry).
			return hiveResponse{}, &apiError{Status: 200, Code: "decode", Message: err.Error(), Retryable: false}
		}
		return out, nil
	}
	return hiveResponse{}, classifyHTTPError(resp, raw)
}

// buildMultipart writes the image as a multipart/form-data "media" field — the
// field name Hive's /task/sync expects for a streamed local file.
func buildMultipart(img []byte, mimeType string) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("media", "upload"+extForMIME(mimeType))
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(img); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}

// extForMIME gives the uploaded part a plausible extension; Hive sniffs content,
// so this is cosmetic but keeps the part filename honest.
func extForMIME(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

// classifyHTTPError maps a non-200 response into a retryable/terminal apiError.
//   - 429, 5xx, 408 -> retryable (transient)
//   - other 4xx     -> terminal (validation, unsupported/oversize media)
func classifyHTTPError(resp *http.Response, raw []byte) *apiError {
	var body hiveErrorResponse
	_ = json.Unmarshal(raw, &body)
	msg := body.Message
	if msg == "" {
		msg = body.Error
	}
	code := body.Code
	if code == "" {
		// Fall back to the HTTP status as the operator-facing code.
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
	return half + time.Duration(c.rng.Int64N(int64(half)+1))
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
