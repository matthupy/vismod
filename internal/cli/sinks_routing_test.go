package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

func envelopeWithVerdict(id string, v moderation.Verdict) result.ResultEnvelope {
	top := moderation.CategorySexual
	score := 0.93
	return result.ResultEnvelope{
		JobID:   queue.JobID(id),
		Source:  moderation.Source{Kind: "file", Ref: "/tmp/a.jpg", MediaType: "image"},
		ModelID: result.ModelIdentity{Adapter: "microsoft", ModelVersion: "2024-02", ConfigHash: "abc1234"},
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{Verdict: v, TopCategory: &top, MaxScore: &score},
		},
		FinishedAt: time.Now().UTC(),
	}
}

// TestBuildSinksDeliversDiscordNotificationOnlyForBlock is the end-to-end
// proof of the feature: a webhook sink configured with format=discord and
// a block-only predicate posts a Discord embed for a block verdict and
// stays silent for an allow.
func TestBuildSinksDeliversDiscordNotificationOnlyForBlock(t *testing.T) {
	posts := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		posts <- b
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := config.Defaults() // stdout catch-all
	cfg.Output.Sinks = append(cfg.Output.Sinks, config.SinkConfig{
		Type:      "webhook",
		URL:       srv.URL,
		Format:    "discord",
		Predicate: result.Predicate{Verdicts: []moderation.Verdict{moderation.VerdictBlock}},
	})

	sink, closeFn, err := buildSinks(cfg, io.Discard, observe.NewMetrics(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	if err := sink.Write(context.Background(), envelopeWithVerdict("allow-1", moderation.VerdictAllow)); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-posts:
		t.Fatalf("an allow verdict must not notify discord, got: %s", b)
	default:
	}

	if err := sink.Write(context.Background(), envelopeWithVerdict("block-1", moderation.VerdictBlock)); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-posts:
		var payload struct {
			Embeds []struct {
				Title  string `json:"title"`
				Fields []struct {
					Value string `json:"value"`
				} `json:"fields"`
			} `json:"embeds"`
		}
		if err := json.Unmarshal(b, &payload); err != nil {
			t.Fatalf("want a discord payload, got %s (%v)", b, err)
		}
		if len(payload.Embeds) != 1 {
			t.Fatalf("want 1 embed, got %d", len(payload.Embeds))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a block verdict must notify discord, got no POST")
	}
}

// TestBuildSinksUnknownFormatRefusesBoot covers the buildSinks path
// directly: config.Validate normally catches it, but buildSinks is
// reachable with a directly-constructed Config.
func TestBuildSinksUnknownFormatRefusesBoot(t *testing.T) {
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{{Type: "stdout", Format: "smoke-signal"}}
	if _, _, err := buildSinks(cfg, io.Discard, observe.NewMetrics(), nil); err == nil {
		t.Fatal("want a boot refusal for an unknown format, got nil")
	}
}

// TestBuildSinksRefusesAConfigWithNoUnconditionalSink is the buildSinks
// half of the catch-all rule — same reasoning as the zero-sinks refusal
// already guarded here: a directly-constructed Config never passes
// through config.Validate, and every-sink-predicated means some envelopes
// reach nothing at all.
func TestBuildSinksRefusesAConfigWithNoUnconditionalSink(t *testing.T) {
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{{
		Type:      "stdout",
		Predicate: result.Predicate{Verdicts: []moderation.Verdict{moderation.VerdictBlock}},
	}}
	if _, _, err := buildSinks(cfg, io.Discard, observe.NewMetrics(), nil); err == nil {
		t.Fatal("want a boot refusal when no sink is unconditional, got nil")
	}
}

func TestBuildSinksAllowUnroutedPermitsAllPredicated(t *testing.T) {
	cfg := config.Defaults()
	cfg.Output.AllowUnrouted = true
	cfg.Output.Sinks = []config.SinkConfig{{
		Type:      "stdout",
		Predicate: result.Predicate{Verdicts: []moderation.Verdict{moderation.VerdictBlock}},
	}}
	_, closeFn, err := buildSinks(cfg, io.Discard, observe.NewMetrics(), nil)
	if err != nil {
		t.Fatalf("allow_unrouted must permit an all-predicated config: %v", err)
	}
	_ = closeFn()
}
