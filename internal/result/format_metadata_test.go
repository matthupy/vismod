package result

import (
	"encoding/json"
	"strings"
	"testing"
)

// discordFields renders env and returns the embed's fields in order.
func discordFields(t *testing.T, f Formatter, env ResultEnvelope) []struct {
	Name  string `json:"name"`
	Value string `json:"value"`
} {
	t.Helper()
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
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload must be valid JSON: %v", err)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("want 1 embed, got %d", len(payload.Embeds))
	}
	return payload.Embeds[0].Fields
}

func fieldNamed(fields []struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}, name string) (string, bool) {
	for _, f := range fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

func metaEnvelope(raw string) ResultEnvelope {
	env := richEnvelope()
	env.Metadata = json.RawMessage(raw)
	return env
}

// TestDiscordDefaultStillOmitsMetadata pins backward compatibility: a sink
// that does not name any metadata field publishes none, which is what
// every existing config does.
func TestDiscordDefaultStillOmitsMetadata(t *testing.T) {
	f, err := FormatterFor("discord", FormatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := f.Format(metaEnvelope(`{"request_id":"abc-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "abc-123") {
		t.Errorf("an unconfigured sink must not publish metadata: %s", body)
	}
}

// TestDiscordRendersNamedMetadataField is the point of the feature: a
// correlation id the caller attached must survive all the way to the
// notification a human reads.
func TestDiscordRendersNamedMetadataField(t *testing.T) {
	f, err := FormatterFor("discord", FormatOptions{MetadataFields: []string{"request_id"}})
	if err != nil {
		t.Fatal(err)
	}
	fields := discordFields(t, f, metaEnvelope(`{"request_id":"abc-123","tenant":"acme"}`))

	v, ok := fieldNamed(fields, "request_id")
	if !ok {
		t.Fatalf("want a request_id field, got %+v", fields)
	}
	if v != "abc-123" {
		t.Errorf("request_id = %q, want %q", v, "abc-123")
	}
	if _, ok := fieldNamed(fields, "tenant"); ok {
		t.Error("a key that was not named must not be published")
	}
}

// TestDiscordMetadataWildcardRendersEveryKey supports the "pass the whole
// thing through" case without making the operator enumerate keys they do
// not control.
func TestDiscordMetadataWildcardRendersEveryKey(t *testing.T) {
	f, err := FormatterFor("discord", FormatOptions{MetadataFields: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	fields := discordFields(t, f, metaEnvelope(`{"b":"two","a":"one"}`))
	for _, k := range []string{"a", "b"} {
		if _, ok := fieldNamed(fields, k); !ok {
			t.Errorf("wildcard must publish key %q, got %+v", k, fields)
		}
	}
}

// TestDiscordMetadataWildcardIsDeterministic: Go map iteration is random,
// so an unsorted wildcard would reorder fields between two renders of the
// same envelope and make notifications impossible to diff.
func TestDiscordMetadataWildcardIsDeterministic(t *testing.T) {
	f, _ := FormatterFor("discord", FormatOptions{MetadataFields: []string{"*"}})
	env := metaEnvelope(`{"z":"1","m":"2","a":"3"}`)

	var first []string
	for range 8 {
		var got []string
		for _, fl := range discordFields(t, f, env) {
			got = append(got, fl.Name)
		}
		if first == nil {
			first = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("field order must be stable: %v then %v", first, got)
		}
	}
}

// TestDiscordMetadataKeepsConfiguredOrder: when the operator names keys,
// that order is intent — the id they care about most goes first.
func TestDiscordMetadataKeepsConfiguredOrder(t *testing.T) {
	f, _ := FormatterFor("discord", FormatOptions{MetadataFields: []string{"second", "first"}})
	fields := discordFields(t, f, metaEnvelope(`{"first":"1","second":"2"}`))

	var order []string
	for _, fl := range fields {
		if fl.Name == "first" || fl.Name == "second" {
			order = append(order, fl.Name)
		}
	}
	if strings.Join(order, ",") != "second,first" {
		t.Errorf("want configured order [second first], got %v", order)
	}
}

// TestDiscordMetadataFieldsPrecedeFreeText: metadata is squeezed by
// fitEmbedBudget in field order, so a tracking id must sit AHEAD of the
// unbounded source ref and error string or the one field the operator
// added the feature for is the first one truncated away.
func TestDiscordMetadataFieldsPrecedeFreeText(t *testing.T) {
	f, _ := FormatterFor("discord", FormatOptions{MetadataFields: []string{"request_id"}})
	env := metaEnvelope(`{"request_id":"abc-123"}`)
	env.Source.Ref = "https://cdn.example.com/" + strings.Repeat("a", 5000)
	env.Error = strings.Repeat("e", 5000)

	fields := discordFields(t, f, env)
	idxOf := func(name string) int {
		for i, fl := range fields {
			if fl.Name == name {
				return i
			}
		}
		return -1
	}
	id, src := idxOf("request_id"), idxOf("Source")
	if id < 0 || src < 0 {
		t.Fatalf("want both request_id and Source fields, got %+v", fields)
	}
	if id > src {
		t.Errorf("request_id (%d) must precede Source (%d)", id, src)
	}
	if v, _ := fieldNamed(fields, "request_id"); v != "abc-123" {
		t.Errorf("the tracking id must survive the budget squeeze intact, got %q", v)
	}
}

// TestDiscordMetadataAbsentKeyIsSkipped: naming a key the caller did not
// send must not produce an empty field, which Discord rejects with a 400.
func TestDiscordMetadataAbsentKeyIsSkipped(t *testing.T) {
	f, _ := FormatterFor("discord", FormatOptions{MetadataFields: []string{"missing"}})
	for _, fl := range discordFields(t, f, metaEnvelope(`{"present":"x"}`)) {
		if fl.Name == "missing" {
			t.Error("an absent key must not render a field")
		}
	}
}

// TestDiscordMetadataNoMetadataAtAll: most jobs carry none, and a
// configured sink must render exactly as it did before for those.
func TestDiscordMetadataNoMetadataAtAll(t *testing.T) {
	f, _ := FormatterFor("discord", FormatOptions{MetadataFields: []string{"request_id"}})
	env := richEnvelope()
	env.Metadata = nil
	if _, err := f.Format(env); err != nil {
		t.Fatalf("an envelope with no metadata must still format: %v", err)
	}
}

// TestDiscordMetadataNonScalarRendersAsJSON: metadata is arbitrary caller
// JSON, so a value can be an object or array. It must render as something
// rather than panicking or emitting Go's %v syntax.
func TestDiscordMetadataNonScalarRendersAsJSON(t *testing.T) {
	f, _ := FormatterFor("discord", FormatOptions{MetadataFields: []string{"ctx", "n", "ok"}})
	fields := discordFields(t, f, metaEnvelope(`{"ctx":{"a":1},"n":42,"ok":true}`))

	if v, _ := fieldNamed(fields, "ctx"); !strings.Contains(v, `"a"`) {
		t.Errorf("object value must render as JSON, got %q", v)
	}
	if v, _ := fieldNamed(fields, "n"); v != "42" {
		t.Errorf("number must render without float noise, got %q", v)
	}
	if v, _ := fieldNamed(fields, "ok"); v != "true" {
		t.Errorf("bool must render as true, got %q", v)
	}
}

// TestDiscordMetadataRespectsThe25FieldLimit guards a limit the core
// fields alone never reach: Discord caps an embed at 25 fields, and a
// wildcard over a large metadata object blows past it into a terminal 400.
func TestDiscordMetadataRespectsThe25FieldLimit(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("{")
	for i := range 60 {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`"k`)
		sb.WriteString(strings.Repeat("0", 2))
		sb.WriteString(string(rune('a' + i%26)))
		sb.WriteString(string(rune('a' + i/26)))
		sb.WriteString(`":"v"`)
	}
	sb.WriteString("}")

	f, _ := FormatterFor("discord", FormatOptions{MetadataFields: []string{"*"}})
	fields := discordFields(t, f, metaEnvelope(sb.String()))
	if len(fields) > discordMaxFields {
		t.Errorf("embed carries %d fields, Discord's limit is %d", len(fields), discordMaxFields)
	}
}

// TestDiscordMetadataStillExcludesRefDigest: widening the carve-out for
// caller metadata must not widen it for anything else.
func TestDiscordMetadataStillExcludesRefDigest(t *testing.T) {
	f, _ := FormatterFor("discord", FormatOptions{MetadataFields: []string{"*"}})
	body, err := f.Format(metaEnvelope(`{"request_id":"abc-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"deadbeef", "schema_version"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("payload must still not contain %q: %s", forbidden, body)
		}
	}
}

// TestJSONFormatterRejectsMetadataFields: the json envelope already
// carries metadata in full, so naming fields there is a config that does
// nothing. Silently ignoring it is the failure mode this project avoids.
func TestJSONFormatterRejectsMetadataFields(t *testing.T) {
	if _, err := FormatterFor("json", FormatOptions{MetadataFields: []string{"request_id"}}); err == nil {
		t.Fatal("want an error: metadata_fields is meaningless for format: json")
	}
}

// TestFormatOptionsRejectBadMetadataFields: an unusable key is a boot
// refusal, not a rule that quietly never matches.
func TestFormatOptionsRejectBadMetadataFields(t *testing.T) {
	cases := map[string][]string{
		"empty key":     {""},
		"blank key":     {"   "},
		"duplicate key": {"id", "id"},
		"wildcard plus": {"*", "id"},
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FormatterFor("discord", FormatOptions{MetadataFields: fields}); err == nil {
				t.Errorf("want an error for %s (%v)", name, fields)
			}
		})
	}
}
