package result

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// TestFormatterIdentity pins the two strings every sink reads off a
// Formatter. ContentType is what a webhook puts on the wire and Name is
// what an operator sees in config errors and metrics, so a rename here is
// a config break, not a cosmetic change.
func TestFormatterIdentity(t *testing.T) {
	cases := []struct {
		configured  string
		wantName    string
		wantContent string
	}{
		{configured: "json", wantName: "json", wantContent: "application/json"},
		{configured: "", wantName: "json", wantContent: "application/json"},
		{configured: "discord", wantName: "discord", wantContent: "application/json"},
		{configured: "  DISCORD  ", wantName: "discord", wantContent: "application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.configured, func(t *testing.T) {
			f, err := FormatterFor(tc.configured, FormatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if f.Name() != tc.wantName {
				t.Errorf("Name() = %q, want %q", f.Name(), tc.wantName)
			}
			if f.ContentType() != tc.wantContent {
				t.Errorf("ContentType() = %q, want %q", f.ContentType(), tc.wantContent)
			}
		})
	}
}

// TestDiscordEmbedColorPerVerdict covers every strip color, including the
// fail-safe one: an error verdict must never be rendered in the allow
// color, because a human glancing at the strip would read an unevaluated
// asset as a pass.
func TestDiscordEmbedColorPerVerdict(t *testing.T) {
	cases := []struct {
		name      string
		verdict   moderation.Verdict
		nilResult bool
		wantColor int
	}{
		{name: "block", verdict: moderation.VerdictBlock, wantColor: discordColorBlock},
		{name: "flag", verdict: moderation.VerdictFlag, wantColor: discordColorFlag},
		{name: "error", verdict: moderation.VerdictError, wantColor: discordColorError},
		{name: "allow", verdict: moderation.VerdictAllow, wantColor: discordColorAllow},
		{name: "missing result is an error", nilResult: true, wantColor: discordColorError},
	}
	f, err := FormatterFor("discord", FormatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := richEnvelope()
			if tc.nilResult {
				env.Result = nil
			} else {
				env.Result.Overall.Verdict = tc.verdict
			}
			body, err := f.Format(env)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Embeds []struct {
					Title string `json:"title"`
					Color int    `json:"color"`
				} `json:"embeds"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Embeds) != 1 {
				t.Fatalf("want 1 embed, got %d", len(payload.Embeds))
			}
			if payload.Embeds[0].Color != tc.wantColor {
				t.Errorf("color = %#x, want %#x", payload.Embeds[0].Color, tc.wantColor)
			}
			if tc.wantColor == discordColorError && payload.Embeds[0].Color == discordColorAllow {
				t.Error("an error verdict must never render in the allow color")
			}
		})
	}
}

// TestDiscordOptionalFieldsAreOmitted walks the optional parts of the
// embed. Each one is its own branch, and a half-populated ModelIdentity is
// the normal case for an errored job that never reached the adapter.
func TestDiscordOptionalFieldsAreOmitted(t *testing.T) {
	cases := []struct {
		name       string
		model      ModelIdentity
		errText    string
		finishedAt time.Time
		wantModel  string
		wantFields map[string]bool // field name -> must be present
		wantTS     string
	}{
		{
			name:       "no adapter yields no model field",
			model:      ModelIdentity{},
			finishedAt: time.Date(2026, 8, 7, 12, 0, 3, 0, time.UTC),
			wantFields: map[string]bool{"Model": false, "Error": false},
			wantTS:     "2026-08-07T12:00:03Z",
		},
		{
			name:       "adapter alone",
			model:      ModelIdentity{Adapter: "hive"},
			finishedAt: time.Date(2026, 8, 7, 12, 0, 3, 0, time.UTC),
			wantModel:  "hive",
			wantFields: map[string]bool{"Model": true},
			wantTS:     "2026-08-07T12:00:03Z",
		},
		{
			name:       "adapter and version, no config hash",
			model:      ModelIdentity{Adapter: "hive", ModelVersion: "v3"},
			finishedAt: time.Date(2026, 8, 7, 12, 0, 3, 0, time.UTC),
			wantModel:  "hive · v3",
			wantFields: map[string]bool{"Model": true},
			wantTS:     "2026-08-07T12:00:03Z",
		},
		{
			name:       "adapter and config hash, no version",
			model:      ModelIdentity{Adapter: "hive", ConfigHash: "abc1234"},
			finishedAt: time.Date(2026, 8, 7, 12, 0, 3, 0, time.UTC),
			wantModel:  "hive · cfg abc1234",
			wantFields: map[string]bool{"Model": true},
			wantTS:     "2026-08-07T12:00:03Z",
		},
		{
			name:       "an unfinished job renders no timestamp",
			model:      ModelIdentity{Adapter: "hive", ModelVersion: "v3", ConfigHash: "abc1234"},
			errText:    "provider timeout",
			wantModel:  "hive · v3 · cfg abc1234",
			wantFields: map[string]bool{"Model": true, "Error": true},
			wantTS:     "",
		},
	}
	f, err := FormatterFor("discord", FormatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := richEnvelope()
			env.ModelID = tc.model
			env.Error = tc.errText
			env.FinishedAt = tc.finishedAt

			body, err := f.Format(env)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Embeds []struct {
					Fields []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"fields"`
					Timestamp string `json:"timestamp"`
				} `json:"embeds"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			e := payload.Embeds[0]
			if e.Timestamp != tc.wantTS {
				t.Errorf("timestamp = %q, want %q", e.Timestamp, tc.wantTS)
			}
			present := map[string]string{}
			for _, fl := range e.Fields {
				present[fl.Name] = fl.Value
			}
			for name, want := range tc.wantFields {
				if _, ok := present[name]; ok != want {
					t.Errorf("field %q present = %v, want %v (fields %+v)", name, ok, want, e.Fields)
				}
			}
			if tc.wantModel != "" && present["Model"] != tc.wantModel {
				t.Errorf("Model = %q, want %q", present["Model"], tc.wantModel)
			}
		})
	}
}

// TestJSONFormatterPropagatesAMarshalFailure keeps the fail-safe posture at
// the rendering boundary: an envelope that cannot be marshalled must
// surface an error so the sink reports a failed delivery. Silently
// emitting a partial record would let a job look delivered when it is not.
func TestJSONFormatterPropagatesAMarshalFailure(t *testing.T) {
	f, err := FormatterFor("json", FormatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// json.RawMessage is validated at marshal time, so caller metadata that
	// never passed queue.ValidateMetadata is the one way an envelope fails.
	env := richEnvelope()
	env.Metadata = json.RawMessage(`{"broken":`)

	b, err := f.Format(env)
	if err == nil {
		t.Fatalf("want a marshal error, got payload %s", b)
	}
	if b != nil {
		t.Errorf("a failed render must return no bytes, got %s", b)
	}
	if !strings.Contains(err.Error(), "result:") {
		t.Errorf("error must be attributed to this package, got %v", err)
	}
}

// TestDiscordMetadataMalformedStillDelivers is the counterpart rule for a
// chat destination: caller free text must never fail a notification. A
// metadata blob that will not parse renders nothing and the verdict still
// reaches the operator.
func TestDiscordMetadataMalformedStillDelivers(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "truncated object", raw: `{"request_id":`},
		{name: "not an object", raw: `[1,2,3]`},
		{name: "bare scalar", raw: `"just-a-string"`},
		{name: "not json at all", raw: `request_id=abc-123`},
		{name: "json null", raw: `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, fields := range [][]string{{"request_id"}, {MetadataWildcard}} {
				f, err := FormatterFor("discord", FormatOptions{MetadataFields: fields})
				if err != nil {
					t.Fatal(err)
				}
				rendered := discordFields(t, f, metaEnvelope(tc.raw))
				if v, ok := fieldNamed(rendered, "request_id"); ok {
					t.Errorf("%v: unparsable metadata must render nothing, got %q", fields, v)
				}
				if v, ok := fieldNamed(rendered, "Verdict"); !ok || v == "" {
					t.Errorf("%v: the verdict must still be delivered, got %+v", fields, rendered)
				}
			}
		})
	}
}

// TestMetadataValueText covers every rendering branch for one metadata
// value. Scalars render bare so a correlation id reads as itself;
// structures keep compact JSON; a JSON null renders as "null" rather than
// as an empty field, which Discord rejects with a 400.
func TestMetadataValueText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "string renders unquoted", raw: `"abc-123"`, want: "abc-123"},
		{name: "empty string stays empty for orDash", raw: `""`, want: ""},
		{name: "bool true", raw: `true`, want: "true"},
		{name: "bool false", raw: `false`, want: "false"},
		{name: "integer keeps integer form", raw: `1234567`, want: "1234567"},
		{name: "fraction keeps its digits", raw: `0.94`, want: "0.94"},
		{name: "negative number", raw: `-17`, want: "-17"},
		{name: "json null", raw: `null`, want: "null"},
		{name: "object keeps compact json", raw: `{"a":1}`, want: `{"a":1}`},
		{name: "array keeps compact json", raw: `[1,2]`, want: `[1,2]`},
		{name: "unparsable raw falls back to its bytes", raw: `{"a":`, want: `{"a":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := metadataValueText(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("metadataValueText(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestMetadataValueTextClampsLongValues: a caller can attach a value far
// past Discord's 1024-character field limit, on both the parsed-string and
// the unparsable-bytes path. Neither may produce a payload the API rejects.
func TestMetadataValueTextClampsLongValues(t *testing.T) {
	cases := map[string]json.RawMessage{
		"long string":     json.RawMessage(`"` + strings.Repeat("s", 4000) + `"`),
		"long object":     json.RawMessage(`{"k":"` + strings.Repeat("o", 4000) + `"}`),
		"long broken raw": json.RawMessage(`{"k":"` + strings.Repeat("b", 4000)),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got := metadataValueText(raw)
			if n := len([]rune(got)); n > discordMaxFieldValue {
				t.Errorf("value is %d runes, discord's limit is %d", n, discordMaxFieldValue)
			}
		})
	}
}

// TestDiscordMetadataNullValueRendersNull walks the same null through the
// real formatter: invariant 2 at the presentation boundary again, an
// unknown must read as unknown rather than vanish.
func TestDiscordMetadataNullValueRendersNull(t *testing.T) {
	f, err := FormatterFor("discord", FormatOptions{MetadataFields: []string{"request_id", "blank"}})
	if err != nil {
		t.Fatal(err)
	}
	fields := discordFields(t, f, metaEnvelope(`{"request_id":null,"blank":""}`))

	if v, ok := fieldNamed(fields, "request_id"); !ok || v != "null" {
		t.Errorf("a null metadata value must render as %q, got %q (ok=%v)", "null", v, ok)
	}
	if v, ok := fieldNamed(fields, "blank"); !ok || v != "—" {
		t.Errorf("an empty metadata value must render as an em dash, got %q (ok=%v)", v, ok)
	}
}

// TestFitEmbedBudget exercises the squeeze directly, including the two
// states the happy path never reaches: a budget already spent by names and
// title, and a value that clamps away to nothing. Both would otherwise
// produce a 400, which moderate.DoJSON treats as terminal — a notification
// lost with no retry.
func TestFitEmbedBudget(t *testing.T) {
	longName := strings.Repeat("N", 300)
	cases := []struct {
		name       string
		title      string
		fields     []discordEmbedField
		wantTitle  string
		checkValue func(t *testing.T, got []discordEmbedField)
	}{
		{
			name:      "everything already fits",
			title:     "vismod: allow",
			fields:    []discordEmbedField{{Name: "Job", Value: "job-1"}},
			wantTitle: "vismod: allow",
			checkValue: func(t *testing.T, got []discordEmbedField) {
				if got[0].Value != "job-1" {
					t.Errorf("a value inside budget must pass through, got %q", got[0].Value)
				}
			},
		},
		{
			name:      "an over-long title is clamped with an ellipsis",
			title:     strings.Repeat("T", 400),
			fields:    []discordEmbedField{{Name: "Job", Value: "job-1"}},
			wantTitle: strings.Repeat("T", discordMaxTitle-1) + "…",
			checkValue: func(t *testing.T, got []discordEmbedField) {
				if got[0].Value != "job-1" {
					t.Errorf("value = %q, want it untouched", got[0].Value)
				}
			},
		},
		{
			name:      "an empty value becomes an em dash",
			title:     "vismod: allow",
			fields:    []discordEmbedField{{Name: "Job", Value: ""}, {Name: "Verdict", Value: "allow"}},
			wantTitle: "vismod: allow",
			checkValue: func(t *testing.T, got []discordEmbedField) {
				if got[0].Value != "—" {
					t.Errorf("discord rejects an empty field value; got %q", got[0].Value)
				}
			},
		},
		{
			name:  "a budget already spent still leaves one rune per value",
			title: strings.Repeat("T", 400),
			fields: func() []discordEmbedField {
				out := make([]discordEmbedField, 0, discordMaxFields)
				for range discordMaxFields {
					out = append(out, discordEmbedField{Name: longName, Value: strings.Repeat("v", 2000)})
				}
				return out
			}(),
			wantTitle: strings.Repeat("T", discordMaxTitle-1) + "…",
			checkValue: func(t *testing.T, got []discordEmbedField) {
				for i, fl := range got {
					if fl.Value == "" {
						t.Fatalf("field %d clamped to an empty value", i)
					}
					if n := len([]rune(fl.Value)); n != 1 {
						t.Errorf("field %d value is %d runes, want the 1-rune floor", i, n)
					}
					if n := len([]rune(fl.Name)); n > discordMaxFieldName {
						t.Errorf("field %d name is %d runes, discord's limit is %d", i, n, discordMaxFieldName)
					}
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTitle, gotFields := fitEmbedBudget(tc.title, tc.fields)
			if gotTitle != tc.wantTitle {
				t.Errorf("title = %q, want %q", gotTitle, tc.wantTitle)
			}
			if n := len([]rune(gotTitle)); n > discordMaxTitle {
				t.Errorf("title is %d runes, discord's limit is %d", n, discordMaxTitle)
			}
			for i, fl := range gotFields {
				if n := len([]rune(fl.Value)); n > discordMaxFieldValue {
					t.Errorf("field %d value is %d runes, discord's limit is %d", i, n, discordMaxFieldValue)
				}
			}
			tc.checkValue(t, gotFields)
		})
	}
}

// TestClamp pins the truncation rule. It cuts on RUNE boundaries because
// Discord counts characters and half a multi-byte rune is invalid UTF-8
// that the API rejects outright. The tiny limits are real: fitEmbedBudget
// floors an exhausted budget at 1.
func TestClamp(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "shorter than the limit is untouched", in: "abc", max: 10, want: "abc"},
		{name: "exactly at the limit is untouched", in: "abc", max: 3, want: "abc"},
		{name: "empty stays empty", in: "", max: 5, want: ""},
		{name: "over the limit ends in an ellipsis", in: "abcdef", max: 4, want: "abc…"},
		{name: "limit of two keeps one rune", in: "abcdef", max: 2, want: "a…"},
		{name: "limit of one has no room for an ellipsis", in: "abcdef", max: 1, want: "a"},
		{name: "limit of zero yields nothing", in: "abcdef", max: 0, want: ""},
		{name: "multi-byte runes are not split", in: "héllo wörld", max: 4, want: "hél…"},
		{name: "multi-byte at a limit of one", in: "héllo", max: 1, want: "h"},
		{name: "wide runes count as one each", in: "日本語のテキスト", max: 3, want: "日本…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clamp(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("clamp(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if n := len([]rune(got)); n > tc.max {
				t.Errorf("clamp(%q, %d) returned %d runes", tc.in, tc.max, n)
			}
			if !utf8.ValidString(got) {
				t.Errorf("clamp(%q, %d) produced invalid UTF-8: %q", tc.in, tc.max, got)
			}
		})
	}
}

// TestTopCategoryNameAndScoreTextNilPaths covers both sides of the two nil
// guards. A missing result and a present result with nothing scoreable are
// different jobs that must render the same way: never as a number.
func TestTopCategoryNameAndScoreTextNilPaths(t *testing.T) {
	top := moderation.CategoryViolence
	cases := []struct {
		name     string
		env      ResultEnvelope
		wantCat  string
		wantText string
	}{
		{
			name:     "no result at all",
			env:      ResultEnvelope{JobID: queue.JobID("j")},
			wantCat:  "",
			wantText: "unknown",
		},
		{
			name: "result with no top category and no max score",
			env: ResultEnvelope{Result: &moderation.NormalizedResult{
				Overall: moderation.OverallVerdict{Verdict: moderation.VerdictError},
			}},
			wantCat:  "",
			wantText: "unknown",
		},
		{
			name: "result with a category but no score",
			env: ResultEnvelope{Result: &moderation.NormalizedResult{
				Overall: moderation.OverallVerdict{Verdict: moderation.VerdictError, TopCategory: &top},
			}},
			wantCat:  string(top),
			wantText: "unknown",
		},
		{
			name: "result with both",
			env: ResultEnvelope{Result: &moderation.NormalizedResult{
				Overall: moderation.OverallVerdict{Verdict: moderation.VerdictBlock, TopCategory: &top, MaxScore: f64(0.5)},
			}},
			wantCat:  string(top),
			wantText: "0.50",
		},
		{
			name: "a zero score is a real score, not an unknown",
			env: ResultEnvelope{Result: &moderation.NormalizedResult{
				Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow, MaxScore: f64(0)},
			}},
			wantCat:  "",
			wantText: "0.00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := topCategoryName(tc.env); got != tc.wantCat {
				t.Errorf("topCategoryName = %q, want %q", got, tc.wantCat)
			}
			if got := scoreText(tc.env); got != tc.wantText {
				t.Errorf("scoreText = %q, want %q", got, tc.wantText)
			}
			if got := orDash(topCategoryName(tc.env)); tc.wantCat == "" && got != "—" {
				t.Errorf("an absent category must render as an em dash, got %q", got)
			}
		})
	}
}
