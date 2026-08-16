package result

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// countingSink records what reached it.
type countingSink struct{ got []queue.JobID }

func (c *countingSink) Write(_ context.Context, env ResultEnvelope) error {
	c.got = append(c.got, env.JobID)
	return nil
}

func TestRoutedSinkDeliversAMatchingEnvelope(t *testing.T) {
	inner := &countingSink{}
	rs := NewRoutedSink(inner, Predicate{Verdicts: []moderation.Verdict{moderation.VerdictBlock}})
	env := envWith("file", moderation.VerdictBlock, moderation.CategorySexual, f64(0.9))
	env.JobID = "j1"
	if err := rs.Write(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(inner.got) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(inner.got))
	}
}

// TestRoutedSinkSkipsANonMatchingEnvelopeWithoutError: a predicate miss is
// a routing decision, not a failure. Returning an error would reach the
// queue's Retry disposition and re-run a BILLED vendor call forever.
func TestRoutedSinkSkipsANonMatchingEnvelopeWithoutError(t *testing.T) {
	inner := &countingSink{}
	rs := NewRoutedSink(inner, Predicate{Verdicts: []moderation.Verdict{moderation.VerdictBlock}})
	env := envWith("file", moderation.VerdictAllow, moderation.CategorySexual, f64(0.1))
	if err := rs.Write(context.Background(), env); err != nil {
		t.Fatalf("a predicate miss must not be an error, got %v", err)
	}
	if len(inner.got) != 0 {
		t.Fatalf("want 0 deliveries, got %d", len(inner.got))
	}
}

func TestRoutedSinkPropagatesInnerError(t *testing.T) {
	rs := NewRoutedSink(errSink{}, Predicate{})
	if err := rs.Write(context.Background(), ResultEnvelope{}); err == nil {
		t.Fatal("inner sink error must propagate")
	}
}

type errSink struct{}

func (errSink) Write(context.Context, ResultEnvelope) error { return io.ErrUnexpectedEOF }

// TestWebhookSinkSendsTheDiscordPayload is the end-to-end proof that a
// Discord notification is producible: transport webhook, format discord.
func TestWebhookSinkSendsTheDiscordPayload(t *testing.T) {
	type capture struct {
		contentType string
		body        []byte
	}
	got := make(chan capture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- capture{contentType: r.Header.Get("Content-Type"), body: b}
		// Discord answers a successful webhook with 204 No Content.
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	f, err := FormatterFor("discord", FormatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := NewWebhookSink(srv.URL, WebhookOptions{Timeout: 2 * time.Second, MaxAttempts: 1, Formatter: f})
	env := richEnvelope()
	if err := s.Write(context.Background(), env); err != nil {
		t.Fatalf("discord webhook write failed: %v", err)
	}

	c := <-got
	if c.contentType != "application/json" {
		t.Errorf("content type: got %q", c.contentType)
	}
	var payload struct {
		Embeds []struct {
			Title string `json:"title"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(c.body, &payload); err != nil {
		t.Fatalf("body must be a discord payload: %v (%s)", err, c.body)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("want 1 embed, got %d", len(payload.Embeds))
	}
}

// TestWebhookSinkNilFormatterKeepsTheJSONEnvelope pins backward
// compatibility for every existing webhook receiver.
func TestWebhookSinkNilFormatterKeepsTheJSONEnvelope(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	env := richEnvelope()
	s := NewWebhookSink(srv.URL, WebhookOptions{Timeout: 2 * time.Second, MaxAttempts: 1})
	if err := s.Write(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(env)
	if string(<-got) != string(want) {
		t.Error("a nil formatter must send the raw JSON envelope")
	}
}
