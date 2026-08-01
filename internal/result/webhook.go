package result

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/vismod/vismod/internal/moderate"
)

const (
	defaultWebhookTimeout  = 5 * time.Second
	defaultWebhookAttempts = 3
	webhookBaseBackoff     = 500 * time.Millisecond
)

// WebhookSink POSTs each envelope as JSON to an operator-configured
// receiver.
//
// Retry classification is delegated to moderate.DoJSON — 429/5xx/timeout
// retryable with Retry-After honored, other 4xx terminal — so there is
// exactly one copy of that policy in the codebase.
//
// The receiver gets JobID in the body and is expected to dedupe on it:
// the in-process dedupe set below cannot survive a worker restart, and
// at-least-once delivery means a restart can resend.
//
// Redirects are refused. config.validateWebhookURL vets the configured
// URL at boot (including the cloud-metadata range); a receiver answering
// 307 with a Location vismod did not choose would make Go re-send the
// POST past that check, so following one would defeat the validator
// entirely. Same rule, same reason, as the shieldgemma adapter's client.
type WebhookSink struct {
	url         string
	client      *http.Client
	maxAttempts int
	d           dedupe
}

func NewWebhookSink(url string, timeout time.Duration, maxAttempts int) *WebhookSink {
	if timeout <= 0 {
		timeout = defaultWebhookTimeout
	}
	if maxAttempts <= 0 {
		maxAttempts = defaultWebhookAttempts
	}
	return &WebhookSink{
		url: url,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("result: webhook refusing redirect to %s (url is config-only)", req.URL.Redacted())
			},
		},
		maxAttempts: maxAttempts,
	}
}

func (s *WebhookSink) Write(ctx context.Context, env ResultEnvelope) error {
	// Claim the JobID BEFORE sending so two concurrent redeliveries of the
	// same job cannot both POST. The claim is released on failure, or the
	// queue's redelivery would skip this sink forever.
	if !s.d.Claim(env.JobID) {
		return nil
	}
	b, err := json.Marshal(env)
	if err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: marshal envelope: %w", err)
	}
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}
	if _, err := moderate.DoJSON(ctx, s.client, build, s.maxAttempts, webhookBaseBackoff, ""); err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: webhook post: %w", err)
	}
	return nil
}

var _ Sink = (*WebhookSink)(nil)
