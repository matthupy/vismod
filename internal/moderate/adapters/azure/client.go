package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

// analyzeRequest is the image:analyze request body. v1 sends inline base64
// content only — blobUrl is an SSRF/egress vector and stays disabled (§C).
type analyzeRequest struct {
	Image      imageContent `json:"image"`
	OutputType string       `json:"outputType,omitempty"`
}

type imageContent struct {
	Content string `json:"content"` // base64-encoded bytes
}

// apiError carries a classified Azure failure. Retryable drives bounded backoff;
// Code is the x-ms-error-code (header, else body) surfaced to operators.
type apiError struct {
	Status     int
	Code       string
	Message    string
	Retryable  bool
	retryAfter time.Duration // parsed Retry-After on 429; 0 if absent
}

func (e *apiError) Error() string {
	return fmt.Sprintf("azure: HTTP %d code=%s: %s", e.Status, e.Code, e.Message)
}

// client is the direct-REST Content Safety data-plane client. There is no Go
// SDK for this surface, so it calls net/http directly.
type client struct {
	httpClient *http.Client
	endpoint   string // https://<resource>.cognitiveservices.azure.com
	apiVersion string
	auth       authProvider
	limiter    *rate.Limiter // shared token bucket, owned by the adapter
	maxRetries int
	backoff    time.Duration
}

// analyze sends one image and returns the parsed response. It honors the shared
// rate limiter (Wait blocks until a token is free) and retries transient
// failures (429/5xx/net) with bounded backoff, respecting Retry-After on 429.
func (c *client) analyze(ctx context.Context, img []byte) (analyzeResponse, error) {
	body := analyzeRequest{
		Image:      imageContent{Content: base64.StdEncoding.EncodeToString(img)},
		OutputType: "FourSeverityLevels",
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return analyzeResponse{}, fmt.Errorf("azure: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/contentsafety/image:analyze?api-version=%s", c.endpoint, c.apiVersion)

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			wait := c.retryWait(attempt, lastErr)
			select {
			case <-ctx.Done():
				return analyzeResponse{}, ctx.Err()
			case <-time.After(wait):
			}
		}

		// The shared limiter bounds throughput regardless of worker/frame
		// parallelism. Only real calls consume tokens (pre-flight happens
		// upstream in the pipeline before AnalyzeImage).
		if err := c.limiter.Wait(ctx); err != nil {
			return analyzeResponse{}, err
		}

		resp, aerr := c.do(ctx, url, payload)
		if aerr == nil {
			return resp, nil
		}
		lastErr = aerr

		var ae *apiError
		if errors.As(aerr, &ae) && !ae.Retryable {
			return analyzeResponse{}, aerr // terminal (4xx validation, unsupported)
		}
		// retryable (429/5xx/net) -> loop and back off
	}
	return analyzeResponse{}, fmt.Errorf("azure: exhausted %d retries: %w", c.maxRetries, lastErr)
}

// do performs a single HTTP attempt and classifies the outcome.
func (c *client) do(ctx context.Context, url string, payload []byte) (analyzeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return analyzeResponse{}, fmt.Errorf("azure: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth.apply(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport errors (timeout, conn reset) are transient -> retryable.
		return analyzeResponse{}, &apiError{Status: 0, Code: "network", Message: err.Error(), Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return analyzeResponse{}, &apiError{Status: resp.StatusCode, Code: "read_body", Message: err.Error(), Retryable: true}
	}

	if resp.StatusCode == http.StatusOK {
		var out analyzeResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			// A 200 we can't parse is terminal (won't fix on retry).
			return analyzeResponse{}, &apiError{Status: 200, Code: "decode", Message: err.Error(), Retryable: false}
		}
		return out, nil
	}
	return analyzeResponse{}, classifyHTTPError(resp, raw)
}

// classifyHTTPError maps a non-200 response into a retryable/terminal apiError.
//   - 429, 5xx, 408 -> retryable (transient)
//   - other 4xx     -> terminal (validation, unsupported/oversize media)
func classifyHTTPError(resp *http.Response, raw []byte) *apiError {
	code := resp.Header.Get("x-ms-error-code")
	var msg string
	var body azureErrorResponse
	if json.Unmarshal(raw, &body) == nil && body.Error.Code != "" {
		if code == "" {
			code = body.Error.Code
		}
		msg = body.Error.Message
	}
	if code == "" {
		code = "unknown"
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

// retryWait computes the backoff for the next attempt: Retry-After when the
// server set it on a 429, else exponential (backoff * 2^(attempt-1)).
func (c *client) retryWait(attempt int, lastErr error) time.Duration {
	var ae *apiError
	if errors.As(lastErr, &ae) && ae.Status == http.StatusTooManyRequests && ae.retryAfter > 0 {
		return ae.retryAfter
	}
	w := c.backoff
	for i := 1; i < attempt; i++ {
		w *= 2
	}
	return w
}

// parseRetryAfter reads the Retry-After header in either RFC 7231 form:
// delta-seconds (integer) or an HTTP-date. A past/invalid date yields 0.
func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
