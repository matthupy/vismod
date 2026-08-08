package result

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// richEnvelope carries every field a formatter could reach for, including
// the two that must NEVER cross into a third-party destination: caller
// metadata and a url source's full ref.
func richEnvelope() ResultEnvelope {
	top := moderation.CategorySexual
	return ResultEnvelope{
		JobID: queue.JobID("job-123"),
		Source: moderation.Source{
			Kind:      "url",
			Ref:       "https://cdn.example.com/a.jpg",
			RefDigest: "deadbeef",
			MediaType: "image",
		},
		ModelID: ModelIdentity{Adapter: "microsoft", ModelVersion: "2024-02", ConfigHash: "abc1234"},
		Result: &moderation.NormalizedResult{
			SchemaVersion: moderation.SchemaVersion,
			Provider:      "microsoft",
			MediaType:     "image",
			Frames: []moderation.FrameResult{{
				Status: moderation.FrameOK,
				Categories: []moderation.CategoryResult{{
					Category: moderation.CategorySexual,
					Score:    f64(0.94),
					Flagged:  true,
				}},
			}},
			Overall: moderation.OverallVerdict{
				Verdict:     moderation.VerdictBlock,
				Flagged:     true,
				TopCategory: &top,
				MaxScore:    f64(0.94),
				Confidence:  f64(0.94),
			},
		},
		Metadata:   json.RawMessage(`{"tenant":"SECRET_TENANT_MARKER"}`),
		StartedAt:  time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 8, 7, 12, 0, 3, 0, time.UTC),
	}
}

func TestFormatterForUnknownNameIsAnError(t *testing.T) {
	if _, err := FormatterFor("smoke-signal"); err == nil {
		t.Fatal("want an error for an unknown format name, got nil")
	}
}

// TestFormatterForEmptyNameIsJSON keeps the existing wire format the
// default: a sink with no `format:` key must send exactly what it sends
// today.
func TestFormatterForEmptyNameIsJSON(t *testing.T) {
	f, err := FormatterFor("")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name() != "json" {
		t.Errorf("empty format must default to json, got %q", f.Name())
	}
}

// TestJSONFormatterIsByteIdenticalToTheEnvelope pins backward
// compatibility: existing webhook receivers must see no change.
func TestJSONFormatterIsByteIdenticalToTheEnvelope(t *testing.T) {
	env := richEnvelope()
	f, err := FormatterFor("json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Format(env)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("json formatter must be byte-identical to the envelope\n got: %s\nwant: %s", got, want)
	}
}

// TestDiscordFormatterNeverLeaksMetadata is the load-bearing test of this
// whole feature. Metadata is opaque caller JSON permitted in a sink
// envelope only; a Discord webhook is a third-party destination, and the
// renderer must not carry it there. Same for a url source's RefDigest and
// the provider raw digest.
func TestDiscordFormatterNeverLeaksMetadata(t *testing.T) {
	f, err := FormatterFor("discord")
	if err != nil {
		t.Fatal(err)
	}
	body, err := f.Format(richEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SECRET_TENANT_MARKER", "tenant", "deadbeef", "schema_version"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("discord payload must not contain %q, got: %s", forbidden, body)
		}
	}
}

func TestDiscordFormatterShape(t *testing.T) {
	f, _ := FormatterFor("discord")
	body, err := f.Format(richEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Embeds []struct {
			Title  string `json:"title"`
			Color  int    `json:"color"`
			Fields []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"fields"`
			Timestamp string `json:"timestamp"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("discord payload must be valid JSON: %v", err)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("want exactly 1 embed, got %d", len(payload.Embeds))
	}
	e := payload.Embeds[0]
	if !strings.Contains(strings.ToLower(e.Title), "block") {
		t.Errorf("title must name the verdict, got %q", e.Title)
	}
	if e.Timestamp != "2026-08-07T12:00:03Z" {
		t.Errorf("timestamp must be finished_at in RFC3339, got %q", e.Timestamp)
	}
	joined := ""
	for _, fl := range e.Fields {
		joined += fl.Name + "=" + fl.Value + ";"
	}
	for _, want := range []string{"job-123", "SEXUAL", "0.94", "microsoft"} {
		if !strings.Contains(joined, want) {
			t.Errorf("fields must mention %q, got %q", want, joined)
		}
	}
}

// TestDiscordFormatterNullScoreRendersUnknown is invariant 2 at the
// presentation boundary: a null max_score must never render as "0.00",
// which a human reads as "confidently safe".
func TestDiscordFormatterNullScoreRendersUnknown(t *testing.T) {
	env := richEnvelope()
	env.Result.Overall.MaxScore = nil
	env.Result.Overall.TopCategory = nil
	env.Result.Overall.Verdict = moderation.VerdictError
	f, _ := FormatterFor("discord")
	body, err := f.Format(env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "0.00") {
		t.Errorf("a nil max_score must not render as a numeric score: %s", body)
	}
	if !strings.Contains(string(body), "unknown") {
		t.Errorf("a nil max_score must render as unknown, got: %s", body)
	}
}

// TestDiscordFormatterHandlesNilResult: an errored job has no
// NormalizedResult at all. It must still render, because a failed scan is
// exactly what an operator watching Discord needs to see.
func TestDiscordFormatterHandlesNilResult(t *testing.T) {
	env := ResultEnvelope{
		JobID:      queue.JobID("job-err"),
		Source:     moderation.Source{Kind: "file", Ref: "/tmp/x.jpg", MediaType: "image"},
		Error:      "provider timeout after 3 attempts",
		FinishedAt: time.Date(2026, 8, 7, 12, 0, 3, 0, time.UTC),
	}
	f, _ := FormatterFor("discord")
	body, err := f.Format(env)
	if err != nil {
		t.Fatalf("a nil-result envelope must still format: %v", err)
	}
	if !strings.Contains(string(body), "job-err") || !strings.Contains(string(body), "error") {
		t.Errorf("errored job must render its id and error verdict: %s", body)
	}
}

// TestDiscordFormatterTruncatesLongValues guards Discord's hard limits
// (256 title, 1024 per field value); an over-long payload is rejected
// with a 400, which DoJSON classifies as terminal — a silently lost
// notification.
func TestDiscordFormatterTruncatesLongValues(t *testing.T) {
	env := richEnvelope()
	env.Source.Ref = "https://cdn.example.com/" + strings.Repeat("a", 4000) + ".jpg"
	f, _ := FormatterFor("discord")
	body, err := f.Format(env)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Embeds []struct {
			Fields []struct {
				Value string `json:"value"`
			} `json:"fields"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, fl := range payload.Embeds[0].Fields {
		if len([]rune(fl.Value)) > discordMaxFieldValue {
			t.Errorf("field value %d runes exceeds the discord limit %d", len([]rune(fl.Value)), discordMaxFieldValue)
		}
	}
}

// TestDiscordFormatterRespectsTheTotalEmbedBudget guards the limit that
// per-field clamping does NOT imply: Discord caps the combined title +
// field names + field values across all embeds at 6000 characters. Seven
// fields each clamped to 1024 sum well past that, and the resulting 400
// is terminal in moderate.DoJSON — a notification lost with no retry.
func TestDiscordFormatterRespectsTheTotalEmbedBudget(t *testing.T) {
	env := richEnvelope()
	env.JobID = queue.JobID(strings.Repeat("j", 3000))
	env.Source.Ref = "https://cdn.example.com/" + strings.Repeat("a", 4000)
	env.Error = strings.Repeat("e", 4000)
	env.ModelID.ModelVersion = strings.Repeat("v", 3000)

	f, _ := FormatterFor("discord")
	body, err := f.Format(env)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Embeds []struct {
			Title  string `json:"title"`
			Fields []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"fields"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	e := payload.Embeds[0]
	total := len([]rune(e.Title))
	for _, fl := range e.Fields {
		total += len([]rune(fl.Name)) + len([]rune(fl.Value))
		if fl.Value == "" {
			t.Error("discord rejects an empty field value")
		}
	}
	if total > discordMaxEmbedTotal {
		t.Errorf("combined embed characters %d exceeds the discord budget %d", total, discordMaxEmbedTotal)
	}
}

func TestKnownFormatsIncludesJSONAndDiscord(t *testing.T) {
	got := strings.Join(KnownFormats(), ",")
	for _, want := range []string{"json", "discord"} {
		if !strings.Contains(got, want) {
			t.Errorf("KnownFormats must include %q, got %q", want, got)
		}
	}
}
