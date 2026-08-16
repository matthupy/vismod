package result

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/vismod/vismod/pkg/moderation"
)

// Formatter renders an envelope into the bytes one destination expects.
//
// It is deliberately separate from the transport (HTTP POST, file append,
// stdout write): "where bytes go" and "what the bytes look like" are two
// orthogonal axes, so a chat integration is a Formatter, not a new Sink.
//
// A Formatter is a NARROWING boundary, never a widening one. It may drop
// envelope fields; it must never reach for anything the envelope does not
// already carry, and a formatter targeting a third-party destination must
// not carry caller Metadata (invariant: metadata is permitted in a sink
// envelope, but a rendered chat message is a disclosure to someone else's
// service).
type Formatter interface {
	Name() string
	ContentType() string
	Format(env ResultEnvelope) ([]byte, error)
}

// MetadataWildcard names every key in a job's metadata object. It is the
// "pass the whole thing through" setting, for operators who do not control
// which keys their callers attach.
const MetadataWildcard = "*"

// FormatOptions is the per-sink configuration a Formatter is built with.
type FormatOptions struct {
	// MetadataFields names the caller-metadata keys this sink may publish,
	// in the order they should render. Empty publishes none, which is the
	// backward-compatible default. A single MetadataWildcard entry
	// publishes every key, sorted for determinism.
	//
	// This is an ALLOW-LIST by design: naming keys means a caller adding a
	// new metadata key can never silently start publishing it to a
	// third-party service. See the disclosure note on discordFormatter.
	MetadataFields []string
}

// normalized validates the options and returns a canonical copy.
func (o FormatOptions) normalized() (FormatOptions, error) {
	if len(o.MetadataFields) == 0 {
		return FormatOptions{}, nil
	}
	seen := make(map[string]bool, len(o.MetadataFields))
	out := make([]string, 0, len(o.MetadataFields))
	wildcard := false
	for _, k := range o.MetadataFields {
		k = strings.TrimSpace(k)
		if k == "" {
			return FormatOptions{}, fmt.Errorf("result: metadata_fields contains an empty key")
		}
		if k == MetadataWildcard {
			wildcard = true
		}
		if seen[k] {
			return FormatOptions{}, fmt.Errorf("result: metadata_fields lists %q twice", k)
		}
		seen[k] = true
		out = append(out, k)
	}
	// A wildcard already covers every key, so naming one alongside it is
	// an operator who expects ordering or filtering they will not get.
	if wildcard && len(out) > 1 {
		return FormatOptions{}, fmt.Errorf("result: metadata_fields %q cannot be combined with named keys", MetadataWildcard)
	}
	return FormatOptions{MetadataFields: out}, nil
}

// formatterFactories builds a Formatter per sink, because format options
// are per-sink configuration rather than process-wide.
var formatterFactories = map[string]func(FormatOptions) (Formatter, error){
	"json": func(o FormatOptions) (Formatter, error) {
		// The json envelope already carries metadata in full, so naming
		// fields here configures nothing. Accepting it silently is exactly
		// the misconfiguration this project refuses to boot on.
		if len(o.MetadataFields) > 0 {
			return nil, fmt.Errorf("result: metadata_fields is not valid for format \"json\" — the envelope already carries all metadata")
		}
		return jsonFormatter{}, nil
	},
	"discord": func(o FormatOptions) (Formatter, error) {
		return discordFormatter{metaFields: o.MetadataFields}, nil
	},
}

// FormatterFor resolves a configured format name. An empty name is the
// JSON envelope, so a sink with no `format:` key keeps sending exactly
// what it sends today.
func FormatterFor(name string, opts FormatOptions) (Formatter, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = "json"
	}
	newFormatter, ok := formatterFactories[key]
	if !ok {
		return nil, fmt.Errorf("result: unknown format %q (want one of: %s)", name, strings.Join(KnownFormats(), ", "))
	}
	o, err := opts.normalized()
	if err != nil {
		return nil, err
	}
	return newFormatter(o)
}

// KnownFormats lists every registered format name, sorted, for config
// validation and error messages.
func KnownFormats() []string {
	names := make([]string, 0, len(formatterFactories))
	for n := range formatterFactories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// jsonFormatter emits the ResultEnvelope unchanged. This is the existing
// wire contract (docs/result-envelope.md) and must stay byte-identical.
type jsonFormatter struct{}

func (jsonFormatter) Name() string        { return "json" }
func (jsonFormatter) ContentType() string { return "application/json" }
func (jsonFormatter) Format(env ResultEnvelope) ([]byte, error) {
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("result: marshal envelope: %w", err)
	}
	return b, nil
}

// Discord's documented embed limits. Exceeding any of them is a 400,
// which moderate.DoJSON classifies as terminal — i.e. a notification lost
// with no retry — so every rendered value is clamped before it is sent.
// Verified against the Discord API reference on 2026-08-07
// (docs.discord.com/developers/resources/message, Embed object limits;
// .../resources/webhook, Execute Webhook — embeds is an array of at most
// 10, and a successful call answers 204 No Content).
//
// discordMaxEmbedTotal is the one that per-field clamping does NOT imply:
// seven fields each clamped to 1024 sum to well over it.
const (
	discordMaxFieldValue = 1024
	discordMaxFieldName  = 256
	discordMaxTitle      = 256
	discordMaxEmbedTotal = 6000
	// An embed carries at most 25 fields. The core fields alone never
	// approach it, but a metadata wildcard over a large object does.
	discordMaxFields = 25
)

// Embed strip colors, chosen so the verdict is readable at a glance
// without reading text. Error is deliberately NOT green-ish: an
// unevaluated asset is a call to action, not a pass.
const (
	discordColorBlock = 0xE03A3A // red
	discordColorFlag  = 0xE0A33A // amber
	discordColorError = 0x8A63D2 // violet
	discordColorAllow = 0x3AA76D // green
)

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type discordEmbed struct {
	Title     string              `json:"title"`
	Color     int                 `json:"color"`
	Fields    []discordEmbedField `json:"fields,omitempty"`
	Timestamp string              `json:"timestamp,omitempty"`
}

type discordPayload struct {
	Username string         `json:"username,omitempty"`
	Embeds   []discordEmbed `json:"embeds"`
}

// discordFormatter renders a human-readable Discord webhook message.
//
// It is an ALLOW-LIST renderer: it names each field it emits, so adding a
// field to ResultEnvelope can never silently start publishing that field
// to a third-party chat service. Source.RefDigest and the provider raw
// digest are deliberately absent and stay that way.
//
// Caller Metadata is the one exception, and only by explicit opt-in.
// Metadata exists to be passed through the whole pipeline — a caller's
// correlation id is useless if it survives the queue and the envelope but
// dies at the notification a human actually reads. metaFields therefore
// names the keys this sink may publish (or MetadataWildcard for all),
// defaulting to none so an existing config is unchanged. The allow-list
// shape is what keeps the guarantee: a caller adding a key cannot start
// publishing it without an operator naming it first.
//
// What vismod CANNOT promise is that a named key holds safe content.
// Metadata is opaque and arrives per job, so only the key names are
// knowable at boot. Naming a key is a disclosure decision about every
// future value under it.
type discordFormatter struct {
	metaFields []string
}

func (discordFormatter) Name() string        { return "discord" }
func (discordFormatter) ContentType() string { return "application/json" }

func (d discordFormatter) Format(env ResultEnvelope) ([]byte, error) {
	verdict := verdictOf(env)

	color := discordColorAllow
	switch verdict {
	case moderation.VerdictBlock:
		color = discordColorBlock
	case moderation.VerdictFlag:
		color = discordColorFlag
	case moderation.VerdictError:
		color = discordColorError
	}

	title := "vismod: " + string(verdict)
	if cat := topCategoryName(env); cat != "" {
		title += " — " + cat
	}

	// Category is its own field, not only part of the title: the title is
	// clamped to 256 runes and a long category must never be the thing
	// that gets truncated away.
	fields := []discordEmbedField{
		{Name: "Job", Value: string(env.JobID), Inline: true},
		{Name: "Verdict", Value: string(verdict), Inline: true},
		{Name: "Top category", Value: orDash(topCategoryName(env)), Inline: true},
		{Name: "Score", Value: scoreText(env), Inline: true},
	}

	// Metadata sits AHEAD of the unbounded free-text fields on purpose.
	// fitEmbedBudget squeezes in field order, so a correlation id placed
	// after the source ref would be the first thing truncated away — the
	// one field the operator configured this for.
	fields = append(fields, d.metadataFields(env)...)

	fields = append(fields,
		discordEmbedField{Name: "Source", Value: env.Source.Kind + " · " + env.Source.Ref, Inline: false},
	)
	if env.ModelID.Adapter != "" {
		model := env.ModelID.Adapter
		if env.ModelID.ModelVersion != "" {
			model += " · " + env.ModelID.ModelVersion
		}
		if env.ModelID.ConfigHash != "" {
			model += " · cfg " + env.ModelID.ConfigHash
		}
		fields = append(fields, discordEmbedField{Name: "Model", Value: model, Inline: true})
	}
	if env.Error != "" {
		fields = append(fields, discordEmbedField{Name: "Error", Value: env.Error, Inline: false})
	}

	title, fields = fitEmbedBudget(title, fields)

	var ts string
	if !env.FinishedAt.IsZero() {
		ts = env.FinishedAt.UTC().Format("2006-01-02T15:04:05Z")
	}

	b, err := json.Marshal(discordPayload{
		Username: "vismod",
		Embeds: []discordEmbed{{
			Title:     title,
			Color:     color,
			Fields:    fields,
			Timestamp: ts,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("result: marshal discord payload: %w", err)
	}
	return b, nil
}

// metadataFields renders the caller-metadata keys this sink is configured
// to publish. It returns nothing when none are configured, when the job
// carried no metadata, or when the metadata will not parse — a rendering
// path must never fail a delivery over caller free text.
//
// queue.ValidateMetadata guarantees a compacted JSON OBJECT under a size
// cap, so the unmarshal target is safe; the error branch exists for
// envelopes constructed outside that path.
func (d discordFormatter) metadataFields(env ResultEnvelope) []discordEmbedField {
	if len(d.metaFields) == 0 || len(env.Metadata) == 0 {
		return nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(env.Metadata, &meta); err != nil {
		return nil
	}

	keys := d.metaFields
	if len(keys) == 1 && keys[0] == MetadataWildcard {
		// Go map iteration is randomized, so an unsorted wildcard would
		// reorder fields between two renders of the same envelope.
		keys = make([]string, 0, len(meta))
		for k := range meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}

	// Leave room for the core fields, which carry the verdict and must
	// never be the thing a metadata wildcard pushes out of the embed.
	budget := discordMaxFields - discordCoreFieldCount
	out := make([]discordEmbedField, 0, min(len(keys), budget))
	for _, k := range keys {
		if len(out) >= budget {
			break
		}
		raw, ok := meta[k]
		if !ok {
			continue // a key the caller did not send renders nothing
		}
		out = append(out, discordEmbedField{
			Name:   clamp(k, discordMaxFieldName),
			Value:  orDash(metadataValueText(raw)),
			Inline: true,
		})
	}
	return out
}

// discordCoreFieldCount is the most fields Format emits before metadata:
// Job, Verdict, Top category, Score, Source, Model, Error.
const discordCoreFieldCount = 7

// metadataValueText renders one metadata value. Scalars render bare so a
// correlation id reads as itself rather than as a quoted JSON string;
// objects and arrays keep their compact JSON, which is honest about the
// caller having sent a structure.
func metadataValueText(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return clamp(string(raw), discordMaxFieldValue)
	}
	switch t := v.(type) {
	case string:
		return clamp(t, discordMaxFieldValue)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// -1 renders the shortest form that round-trips, so an integer id
		// does not come back as 1.234567e+06.
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return "null"
	default:
		return clamp(string(raw), discordMaxFieldValue)
	}
}

// fitEmbedBudget clamps the title and every field so the embed satisfies
// BOTH the per-value limits and the 6000-character combined budget.
//
// Values are budgeted in order, so the leading fields (job, verdict,
// category, score) always survive intact and only the trailing free-text
// ones (source ref, error string) get squeezed. The alternative —
// clamping each field independently — passes every per-field check and
// still produces a 400, which moderate.DoJSON treats as terminal.
func fitEmbedBudget(title string, fields []discordEmbedField) (string, []discordEmbedField) {
	title = clamp(title, discordMaxTitle)
	used := len([]rune(title))
	for i := range fields {
		fields[i].Name = clamp(fields[i].Name, discordMaxFieldName)
		used += len([]rune(fields[i].Name))
	}
	// Field names are short and fixed and the title is capped at 256, so
	// used is far below the budget here and avail stays positive.
	for i := range fields {
		avail := discordMaxEmbedTotal - used
		budget := min(discordMaxFieldValue, avail)
		if budget < 1 {
			budget = 1
		}
		fields[i].Value = clamp(fields[i].Value, budget)
		if fields[i].Value == "" {
			// Discord rejects an empty field value with a 400.
			fields[i].Value = "—"
		}
		used += len([]rune(fields[i].Value))
	}
	return title, fields
}

// orDash renders an absent value as an em dash. Discord rejects an empty
// field value with a 400.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func topCategoryName(env ResultEnvelope) string {
	if env.Result == nil || env.Result.Overall.TopCategory == nil {
		return ""
	}
	return string(*env.Result.Overall.TopCategory)
}

// scoreText renders the asset-level max score. A nil MaxScore means no
// non-nil score existed anywhere, and it renders as "unknown" — never as
// a number, because a human reading "0.00" reads "confidently safe" when
// the truth is "could not evaluate" (invariant 2).
func scoreText(env ResultEnvelope) string {
	if env.Result == nil || env.Result.Overall.MaxScore == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.2f", *env.Result.Overall.MaxScore)
}

// clamp truncates on RUNE boundaries: Discord counts characters, and
// slicing a multi-byte rune in half yields invalid UTF-8 that the API
// rejects.
func clamp(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
