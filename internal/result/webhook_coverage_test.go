package result

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// webhookCovFormatter is a Formatter double for the paths a real formatter
// only reaches on malformed input: it can fail its first N renders and then
// succeed, which is what proves the sink RELEASED the JobID claim after a
// render failure instead of marking the job delivered.
type webhookCovFormatter struct {
	name        string
	contentType string
	body        []byte
	failCalls   int32 // fail this many leading Format calls
	calls       atomic.Int32
}

func (f *webhookCovFormatter) Name() string { return f.name }

func (f *webhookCovFormatter) ContentType() string { return f.contentType }

func (f *webhookCovFormatter) Format(ResultEnvelope) ([]byte, error) {
	if f.calls.Add(1) <= f.failCalls {
		return nil, errors.New("result: envelope is not renderable for this destination")
	}
	return f.body, nil
}

var _ Formatter = (*webhookCovFormatter)(nil)

// countingWebhook returns an httptest server that records how many POSTs it
// received and answers with the given status.
func countingWebhook(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestNewWebhookSinkResolvesOptionDefaults pins both sides of every option
// default. A zero or negative Timeout/MaxAttempts/Backoff must fall back —
// a zero http.Client.Timeout means "wait forever", and a zero MaxAttempts
// would send nothing at all — while an operator's explicit value must
// survive untouched.
func TestNewWebhookSinkResolvesOptionDefaults(t *testing.T) {
	stub := &webhookCovFormatter{name: "stub", contentType: "text/plain", body: []byte("x")}

	tests := []struct {
		name         string
		opts         WebhookOptions
		wantTimeout  time.Duration
		wantAttempts int
		wantBackoff  time.Duration
		wantFormat   string
		wantSinkName string
	}{
		{
			name:         "zero value takes every default",
			opts:         WebhookOptions{},
			wantTimeout:  defaultWebhookTimeout,
			wantAttempts: defaultWebhookAttempts,
			wantBackoff:  webhookBaseBackoff,
			wantFormat:   "json",
			wantSinkName: "webhook",
		},
		{
			name:         "negative values are refused, not honored",
			opts:         WebhookOptions{Timeout: -time.Second, MaxAttempts: -3, Backoff: -time.Minute},
			wantTimeout:  defaultWebhookTimeout,
			wantAttempts: defaultWebhookAttempts,
			wantBackoff:  webhookBaseBackoff,
			wantFormat:   "json",
			wantSinkName: "webhook",
		},
		{
			name: "explicit values survive",
			opts: WebhookOptions{
				Timeout:     7 * time.Second,
				MaxAttempts: 9,
				Backoff:     250 * time.Millisecond,
				Formatter:   stub,
				Name:        "webhook[2]",
			},
			wantTimeout:  7 * time.Second,
			wantAttempts: 9,
			wantBackoff:  250 * time.Millisecond,
			wantFormat:   "stub",
			wantSinkName: "webhook[2]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewWebhookSink("http://127.0.0.1:1/hook", tc.opts)
			if s.client.Timeout != tc.wantTimeout {
				t.Errorf("timeout: got %v want %v", s.client.Timeout, tc.wantTimeout)
			}
			if s.maxAttempts != tc.wantAttempts {
				t.Errorf("max attempts: got %d want %d", s.maxAttempts, tc.wantAttempts)
			}
			if s.backoff != tc.wantBackoff {
				t.Errorf("backoff: got %v want %v", s.backoff, tc.wantBackoff)
			}
			if s.fmtr == nil || s.fmtr.Name() != tc.wantFormat {
				t.Errorf("formatter: got %v want %q", s.fmtr, tc.wantFormat)
			}
			if s.name != tc.wantSinkName {
				t.Errorf("sink label: got %q want %q", s.name, tc.wantSinkName)
			}
			// The label is the ONLY identifier that reaches a log line, so
			// the default must never be derived from the URL.
			if strings.Contains(s.name, "127.0.0.1") || strings.Contains(s.name, "/hook") {
				t.Errorf("sink label must never be derived from the url: %q", s.name)
			}
			if s.client.CheckRedirect == nil {
				t.Error("redirects must be refused regardless of options")
			}
		})
	}
}

// TestWebhookSinkZeroOptionsDelivers proves the defaulted sink is a working
// sink and not just a well-populated struct: WebhookOptions{} must POST the
// raw JSON envelope, which is the wire contract every existing receiver is
// written against.
func TestWebhookSinkZeroOptionsDelivers(t *testing.T) {
	var gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, WebhookOptions{})
	if err := s.Write(context.Background(), plainEnvelope("job-defaults")); err != nil {
		t.Fatalf("default options must deliver, got %v", err)
	}
	if !strings.HasPrefix(gotType, "application/json") {
		t.Errorf("default formatter must send json, got %q", gotType)
	}
	var round ResultEnvelope
	if err := json.Unmarshal(gotBody, &round); err != nil {
		t.Fatalf("default body is not a decodable envelope: %v", err)
	}
	if round.JobID != queue.JobID("job-defaults") {
		t.Errorf("job_id must reach the receiver, got %q", round.JobID)
	}
}

// TestWebhookSinkFormatFailureSendsNothingAndReleasesTheClaim covers the
// render-failure path. Two things matter beyond the returned error: nothing
// may be POSTed (a half-rendered payload is worse than none), and the JobID
// claim must be released or the queue's redelivery would skip this sink
// forever.
func TestWebhookSinkFormatFailureSendsNothingAndReleasesTheClaim(t *testing.T) {
	srv, calls := countingWebhook(t, http.StatusNoContent)

	fmtr := &webhookCovFormatter{
		name:        "stub",
		contentType: "application/json",
		body:        []byte(`{"ok":true}`),
		failCalls:   1,
	}
	var buf bytes.Buffer
	secret := srv.URL + "/api/webhooks/9/FORMAT_TOKEN"
	s := NewWebhookSink(secret, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 3, Backoff: time.Millisecond,
		Name: "webhook[2]", Logger: captureLogger(&buf), Formatter: fmtr,
	})

	env := plainEnvelope("job-format")
	err := s.Write(context.Background(), env)
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("an unrenderable envelope reached nobody, want ErrDeliveryFailed, got %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("nothing may be sent when the render failed, got %d posts", calls.Load())
	}

	recs := logRecords(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("want exactly one give-up record, got %d: %s", len(recs), buf.String())
	}
	if recs[0]["reason"] != deliveryReasonFormat {
		t.Errorf("reason must be the bounded value %q, got %v", deliveryReasonFormat, recs[0]["reason"])
	}
	if recs[0]["status"] != float64(0) {
		t.Errorf("a render failure has no HTTP status, got %v", recs[0]["status"])
	}
	if recs[0]["sink"] != "webhook[2]" {
		t.Errorf("give-up record must name the sink, got %v", recs[0]["sink"])
	}
	for _, forbidden := range []string{"FORMAT_TOKEN", "/api/webhooks/"} {
		if strings.Contains(buf.String(), forbidden) {
			t.Errorf("delivery log must not contain %q: %s", forbidden, buf.String())
		}
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("delivery error must not contain %q: %v", forbidden, err)
		}
	}

	// The claim was released, so the redelivery renders again and sends.
	if err := s.Write(context.Background(), env); err != nil {
		t.Fatalf("redelivery after a render failure must be attempted, got %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("want 1 post on redelivery, got %d", calls.Load())
	}
}

// TestWebhookSinkUnbuildableRequestIsADeliveryFailure covers the
// request-construction error branch: a URL that cannot be parsed never
// reaches the network, and DoJSON returns it terminally with no retry. It
// is unreachable in a booted process — config.validateWebhookURL parses the
// configured URL first — but it must still fail safe rather than look like
// a delivered envelope.
func TestWebhookSinkUnbuildableRequestIsADeliveryFailure(t *testing.T) {
	// A DEL byte makes net/url reject the string; nothing is dialed.
	bad := "http://127.0.0.1:1/api/webhooks/9/BUILD_TOKEN\x7f"

	var buf bytes.Buffer
	s := NewWebhookSink(bad, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 3, Backoff: time.Millisecond,
		Name: "webhook[4]", Logger: captureLogger(&buf),
	})

	env := plainEnvelope("job-build")
	err := s.Write(context.Background(), env)
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("an unbuildable request delivered nothing, want ErrDeliveryFailed, got %v", err)
	}

	recs := logRecords(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("a terminal build failure must log once and not back off, got %d: %s", len(recs), buf.String())
	}
	if recs[0]["reason"] != deliveryReasonRejected {
		t.Errorf("reason must be %q (the budget was never spent), got %v", deliveryReasonRejected, recs[0]["reason"])
	}
	if recs[0]["status"] != float64(0) {
		t.Errorf("no response means no status, got %v", recs[0]["status"])
	}
	if recs[0]["max_attempts"] != float64(3) {
		t.Errorf("give-up record must carry max_attempts, got %v", recs[0]["max_attempts"])
	}
	// The log is the credential boundary: no part of the URL may appear in
	// any log field, whatever the transport did.
	for _, forbidden := range []string{"BUILD_TOKEN", "/api/webhooks/", "127.0.0.1"} {
		if strings.Contains(buf.String(), forbidden) {
			t.Errorf("delivery log must not contain %q: %s", forbidden, buf.String())
		}
	}

	// A failed send is not a written job: the claim must have been released.
	if err := s.Write(context.Background(), env); !errors.Is(err, ErrDeliveryFailed) {
		t.Errorf("redelivery must re-attempt and fail again, got %v", err)
	}
}

// TestWebhookSinkIgnoresTheReceiversResponse: the sink is fire-and-forget
// on the response side. Any 2xx is a delivery, whatever bytes come back, so
// a receiver that answers 200 with an HTML error page or an empty body must
// not be turned into a failed delivery (which would re-run the whole job
// and re-bill the vendor).
func TestWebhookSinkIgnoresTheReceiversResponse(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "200 with a non-JSON body", status: http.StatusOK, body: "<html>not json</html>"},
		{name: "200 with a truncated JSON body", status: http.StatusOK, body: `{"ok":`},
		{name: "201 with an empty body", status: http.StatusCreated},
		{name: "204 no content", status: http.StatusNoContent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = io.WriteString(w, tc.body)
				}
			}))
			defer srv.Close()

			var buf bytes.Buffer
			s := NewWebhookSink(srv.URL, WebhookOptions{
				Timeout: time.Second, MaxAttempts: 3, Backoff: time.Millisecond,
				Name: "webhook[0]", Logger: captureLogger(&buf),
			})
			if err := s.Write(context.Background(), plainEnvelope("job-resp")); err != nil {
				t.Fatalf("a 2xx is a delivery regardless of the body, got %v", err)
			}
			if calls.Load() != 1 {
				t.Errorf("want exactly 1 post, got %d", calls.Load())
			}
			if buf.Len() != 0 {
				t.Errorf("a successful delivery must log nothing, got: %s", buf.String())
			}
		})
	}
}

// TestWebhookSinkCanceledContextDeliversNothing: a shutdown mid-job must
// produce a failure the queue can redeliver, never a silent success. The
// context is canceled before the write, so no request leaves the process
// and no timing is involved.
func TestWebhookSinkCanceledContextDeliversNothing(t *testing.T) {
	srv, calls := countingWebhook(t, http.StatusNoContent)

	var buf bytes.Buffer
	s := NewWebhookSink(srv.URL, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 1, Backoff: time.Millisecond,
		Name: "webhook[1]", Logger: captureLogger(&buf),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	env := plainEnvelope("job-canceled")
	err := s.Write(ctx, env)
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("a canceled delivery reached nobody, want ErrDeliveryFailed, got %v", err)
	}
	if !moderation.IsRetryable(err) {
		t.Error("wrapping must preserve retryability, or the queue cannot redeliver a transport failure")
	}
	if calls.Load() != 0 {
		t.Errorf("a canceled context must send nothing, got %d posts", calls.Load())
	}
	recs := logRecords(t, &buf)
	if len(recs) != 1 || recs[0]["reason"] != deliveryReasonExhausted {
		t.Fatalf("want one give-up record with reason %q, got %s", deliveryReasonExhausted, buf.String())
	}

	// The claim is released, so a redelivery on a live context still sends.
	if err := s.Write(context.Background(), env); err != nil {
		t.Fatalf("redelivery after cancellation must send, got %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("want 1 post after redelivery, got %d", calls.Load())
	}
}

// TestWebhookGiveUpWithoutALoggerReturnsTheSentinel: logging is optional,
// failing safe is not. With Logger and OnRetry nil the sink must still
// return the wrapped sentinel — and the message must name the sink label
// and the bounded reason rather than the destination.
func TestWebhookGiveUpWithoutALoggerReturnsTheSentinel(t *testing.T) {
	srv, calls := countingWebhook(t, http.StatusForbidden)

	secret := srv.URL + "/api/webhooks/9/NO_LOGGER_TOKEN"
	s := NewWebhookSink(secret, WebhookOptions{
		Timeout: time.Second, MaxAttempts: 3, Backoff: time.Millisecond,
		Name: "webhook[7]",
	})
	err := s.Write(context.Background(), plainEnvelope("job-nolog"))
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("want ErrDeliveryFailed without a logger, got %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("403 is terminal: want 1 attempt, got %d", calls.Load())
	}
	if !strings.Contains(err.Error(), "webhook[7]") {
		t.Errorf("error must name the operator-facing sink label, got %v", err)
	}
	if !strings.Contains(err.Error(), deliveryReasonRejected) {
		t.Errorf("error must carry the bounded reason %q, got %v", deliveryReasonRejected, err)
	}
	for _, forbidden := range []string{"NO_LOGGER_TOKEN", "/api/webhooks/"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("delivery error must not contain %q: %v", forbidden, err)
		}
	}
}

// TestWebhookSinkDeliversAnErrorEnvelope: a job that failed analysis still
// has to reach the receiver — that envelope carries verdict "error" with a
// nil Result, which is the fail-safe output, and dropping it would hide
// exactly the jobs a human must review.
func TestWebhookSinkDeliversAnErrorEnvelope(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, WebhookOptions{Timeout: time.Second, MaxAttempts: 1})
	env := ResultEnvelope{
		JobID:  queue.JobID("job-error"),
		Source: moderation.Source{Kind: "url", Ref: "https://example.com/a.jpg", MediaType: "image"},
		Error:  "extraction failed",
	}
	if err := s.Write(context.Background(), env); err != nil {
		t.Fatalf("an error envelope must still be delivered, got %v", err)
	}
	var round ResultEnvelope
	if err := json.Unmarshal(gotBody, &round); err != nil {
		t.Fatalf("body is not a decodable envelope: %v", err)
	}
	if round.Result != nil {
		t.Errorf("a nil Result must stay nil on the wire, got %+v", round.Result)
	}
	if round.Error != "extraction failed" {
		t.Errorf("error envelope did not round-trip: %+v", round)
	}
}

// TestWebhookDeliveryReasonClassification pins the two-way split an
// operator acts on. "The destination is down" (exhausted) and "the
// destination refused this payload" (rejected) need opposite responses, and
// the status must survive whatever depth the HTTPError is wrapped at, since
// it is the only bounded detail the give-up record carries.
func TestWebhookDeliveryReasonClassification(t *testing.T) {
	httpErr := &moderate.HTTPError{Status: http.StatusBadRequest, Body: "no"}

	tests := []struct {
		name       string
		err        error
		wantReason string
		wantStatus int
	}{
		{
			name:       "bare exhausted sentinel",
			err:        moderate.ErrAttemptsExhausted,
			wantReason: deliveryReasonExhausted,
		},
		{
			name:       "exhausted budget as DoJSON returns it",
			err:        moderation.Retryable(fmt.Errorf("%w after %d attempts: %w", moderate.ErrAttemptsExhausted, 3, errors.New("connection refused"))),
			wantReason: deliveryReasonExhausted,
		},
		{
			name:       "exhausted budget whose last attempt was a 5xx keeps that status",
			err:        moderation.Retryable(fmt.Errorf("%w after %d attempts: %w", moderate.ErrAttemptsExhausted, 2, &moderate.HTTPError{Status: http.StatusServiceUnavailable})),
			wantReason: deliveryReasonExhausted,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "terminal 4xx",
			err:        httpErr,
			wantReason: deliveryReasonRejected,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "terminal 4xx wrapped by a caller",
			err:        fmt.Errorf("post envelope: %w", httpErr),
			wantReason: deliveryReasonRejected,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "transport error carries no status",
			err:        errors.New("dial tcp: connection reset"),
			wantReason: deliveryReasonRejected,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.wantReason {
				t.Errorf("classify: got %q want %q", got, tc.wantReason)
			}
			if got := statusOf(tc.err); got != tc.wantStatus {
				t.Errorf("statusOf: got %d want %d", got, tc.wantStatus)
			}
		})
	}
}
