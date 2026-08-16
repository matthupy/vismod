package result

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/vismod/vismod/internal/moderate"
)

const (
	defaultWebhookTimeout  = 5 * time.Second
	defaultWebhookAttempts = 3
	webhookBaseBackoff     = 500 * time.Millisecond
)

// WebhookOptions configures a WebhookSink. Every field is optional; the
// zero value reproduces the original behavior (5s timeout, 3 attempts,
// raw JSON envelope, no logging).
//
// It is a struct rather than positional parameters because this
// constructor now carries six concerns, and a bare
// NewWebhookSink(url, 5*time.Second, 3, nil, log, "webhook[1]", onRetry)
// is unreadable at the call site and silently mis-ordered on the next
// addition.
type WebhookOptions struct {
	Timeout     time.Duration
	MaxAttempts int
	Backoff     time.Duration
	Formatter   Formatter

	// Logger receives one warn per backoff and one error on give-up. Nil
	// disables delivery logging entirely.
	Logger *slog.Logger

	// Name is the OPERATOR-FACING label for this sink, and the ONLY thing
	// that identifies it in logs. cli.buildSinks sets it to the sink's
	// position in output.sinks ("webhook[1]"), which is what an operator
	// can map back to their config.
	//
	// It must never be the URL. A Discord webhook URL carries its token in
	// the path, so logging it publishes a credential, and url.Redacted()
	// does not help — it strips userinfo and leaves the path intact.
	//
	// It surfaces under TWO different field names, because two layers emit
	// it: `target` on the per-backoff warning (from moderate.WithRetryLog,
	// which also labels adapter calls) and `sink` on the give-up error
	// (this package, matching output.sinks and the vismod_sink_* metrics).
	// Same string, both times — that correspondence is what lets an
	// operator join the two lines, and it is asserted by
	// TestWebhookRetryTargetMatchesGiveUpSink.
	Name string

	// OnRetry fires once per backoff. It exists so the CLI can drive a
	// Prometheus counter without this package importing observe.
	OnRetry func()
}

// WebhookSink POSTs each envelope to an operator-configured receiver.
//
// Retry classification is delegated to moderate.DoJSON — 429/5xx/timeout
// retryable with Retry-After honored and exponential backoff, other 4xx
// terminal — so there is exactly one copy of that policy in the codebase.
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
	backoff     time.Duration
	fmtr        Formatter
	log         *slog.Logger
	name        string
	onRetry     func()
	d           dedupe
}

// NewWebhookSink builds the sink. A nil Formatter means the raw JSON
// envelope, which is the existing wire contract every current receiver
// is written against.
func NewWebhookSink(url string, opts WebhookOptions) *WebhookSink {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultWebhookTimeout
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = defaultWebhookAttempts
	}
	if opts.Backoff <= 0 {
		opts.Backoff = webhookBaseBackoff
	}
	if opts.Formatter == nil {
		opts.Formatter = jsonFormatter{}
	}
	if opts.Name == "" {
		opts.Name = "webhook"
	}
	return &WebhookSink{
		fmtr: opts.Formatter,
		url:  url,
		client: &http.Client{
			Timeout: opts.Timeout,
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				// The redirect TARGET is deliberately not named here: it is
				// attacker-chosen text on an error path that gets logged.
				return errors.New("result: webhook refusing redirect (url is config-only)")
			},
		},
		maxAttempts: opts.MaxAttempts,
		backoff:     opts.Backoff,
		log:         opts.Logger,
		name:        opts.Name,
		onRetry:     opts.OnRetry,
	}
}

func (s *WebhookSink) Write(ctx context.Context, env ResultEnvelope) error {
	// Claim the JobID BEFORE sending so two concurrent redeliveries of the
	// same job cannot both POST. The claim is released on failure, or the
	// queue's redelivery would skip this sink forever.
	if !s.d.Claim(env.JobID) {
		return nil
	}
	b, err := s.fmtr.Format(env)
	if err != nil {
		s.d.Release(env.JobID)
		return s.fail(env, deliveryReasonFormat, 0, err)
	}
	contentType := s.fmtr.ContentType()
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}

	opts := []moderate.DoOption{}
	if s.log != nil {
		opts = append(opts, moderate.WithRetryLog(s.log, s.name))
	}
	if s.onRetry != nil {
		opts = append(opts, moderate.OnRetry(s.onRetry))
	}

	if _, err := moderate.DoJSON(ctx, s.client, build, s.maxAttempts, s.backoff, "", opts...); err != nil {
		s.d.Release(env.JobID)
		return s.fail(env, classify(err), statusOf(err), err)
	}
	return nil
}

// classify maps a DoJSON error to one of the bounded delivery reasons. An
// exhausted budget and a refused payload look alike in a message and need
// opposite operator responses: one is "the destination is down", the other
// is "this will never succeed, stop waiting for it".
func classify(err error) string {
	if errors.Is(err, moderate.ErrAttemptsExhausted) {
		return deliveryReasonExhausted
	}
	return deliveryReasonRejected
}

func statusOf(err error) int {
	var herr *moderate.HTTPError
	if errors.As(err, &herr) {
		return herr.Status
	}
	return 0
}

// fail logs the give-up once and returns the sentinel-wrapped error.
//
// The URL is never logged or included in the message; s.name is the
// operator's handle on which destination this was. The underlying error is
// wrapped rather than replaced so moderation.IsRetryable still answers
// correctly for the queue's disposition.
func (s *WebhookSink) fail(env ResultEnvelope, reason string, status int, err error) error {
	if s.log != nil {
		s.log.Error("result sink delivered nothing",
			"sink", s.name,
			"job_id", string(env.JobID),
			"reason", reason,
			"status", status,
			"max_attempts", s.maxAttempts,
		)
	}
	return fmt.Errorf("%w: sink %s (%s): %w", ErrDeliveryFailed, s.name, reason, err)
}

var _ Sink = (*WebhookSink)(nil)
