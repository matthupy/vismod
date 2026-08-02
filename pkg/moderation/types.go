// Package moderation defines the public contract types that external
// consumers of vismod bind to. It has no dependencies on internal packages.
//
// Scores are normalized to [0,1] but are within-provider comparable ONLY:
// a Microsoft severity/6, a Google likelihood bucket, and a Hive head
// probability are not the same quantity. Thresholds are per-adapter and
// not portable across providers. See MODEL_LIMITATIONS.md.
package moderation

import (
	"context"
	"encoding/json"
)

// SchemaVersion is stamped by the normalizer (the pipeline), never by an
// adapter. Additive fields/values bump the minor version; any
// remove/rename/meaning change bumps the major version.
// 1.1.0: added the GAMBLING, ALCOHOL_TOBACCO, OFFENSIVE_GESTURE and
// ANIMATED_SYNTHETIC categories (additive values only; no field or
// meaning changed).
// 1.2.0: added Source.RefDigest for url-kind sources (additive field
// only; no field or meaning changed). Source is serialized into
// result.ResultEnvelope rather than NormalizedResult, and the envelope
// carries no version of its own, so this constant is the only version
// signal consumers have for that change.
const SchemaVersion = "1.2.0"

// Verdict is the final decision for an asset.
// Precedence at rollup is strict: block > error > flag > allow.
type Verdict string

const (
	VerdictAllow Verdict = "allow"
	VerdictFlag  Verdict = "flag"
	VerdictBlock Verdict = "block"
	// VerdictError means could-not-evaluate. Fail safe: an errored or
	// partially-errored asset is never "allow".
	VerdictError Verdict = "error"
)

// Category is the canonical, model-agnostic category taxonomy.
// Consumers MUST tolerate unknown future values by treating them as OTHER.
type Category string

const (
	CategorySexual         Category = "SEXUAL"
	CategorySuggestiveRacy Category = "SUGGESTIVE_RACY"
	CategoryViolence       Category = "VIOLENCE"
	CategoryGoreGraphic    Category = "GORE_GRAPHIC"
	CategoryWeapons        Category = "WEAPONS"
	CategorySelfHarm       Category = "SELF_HARM"
	CategoryHate           Category = "HATE"
	// CategoryDrugs is illicit drugs only. Legal vice (alcohol, tobacco)
	// is CategoryAlcoholTobacco — the two carry different policy weight
	// and most operators threshold them differently.
	CategoryDrugs          Category = "DRUGS"
	CategoryAlcoholTobacco Category = "ALCOHOL_TOBACCO"
	CategoryGambling       Category = "GAMBLING"
	// CategoryOffensiveGesture covers rude-gesture signals (e.g. Hive's
	// yes_middle_finger). Distinct from HATE: offensive, not targeted at a
	// protected group.
	CategoryOffensiveGesture Category = "OFFENSIVE_GESTURE"
	// CategoryMedical, CategorySpoof and CategoryAnimatedSynthetic are
	// provenance carriers, NOT harm signals; do not treat them as
	// moderation categories. MEDICAL and SPOOF carry Google SafeSearch's
	// medical/spoof signals. ANIMATED_SYNTHETIC carries "is this drawn,
	// rendered, or otherwise not a photograph" signals, which change how a
	// harm score should be read (animated gore is not filmed gore) but are
	// not themselves harm.
	CategoryMedical           Category = "MEDICAL"
	CategorySpoof             Category = "SPOOF"
	CategoryAnimatedSynthetic Category = "ANIMATED_SYNTHETIC"
	// CategoryOther is the fallback for any provider label with no
	// canonical mapping. The raw label is preserved in ProviderLabel and
	// its score is carried. Results are never dropped.
	CategoryOther Category = "OTHER"
)

// Canonicalize maps an arbitrary category value onto the canonical set,
// folding unknown (e.g. future) values to OTHER.
func Canonicalize(c Category) Category {
	switch c {
	case CategorySexual, CategorySuggestiveRacy, CategoryViolence,
		CategoryGoreGraphic, CategoryWeapons, CategorySelfHarm,
		CategoryHate, CategoryDrugs, CategoryAlcoholTobacco,
		CategoryGambling, CategoryOffensiveGesture, CategoryMedical,
		CategorySpoof, CategoryAnimatedSynthetic, CategoryOther:
		return c
	default:
		return CategoryOther
	}
}

// ScoreOrigin records what kind of quantity a normalized Score was derived
// from. Scores with different origins are not comparable.
type ScoreOrigin string

const (
	OriginProbability    ScoreOrigin = "probability"
	OriginConfidencePct  ScoreOrigin = "confidence_pct"
	OriginLikelihoodEnum ScoreOrigin = "likelihood_enum"
	OriginSeverity       ScoreOrigin = "severity"
)

// FrameStatus reports whether a single frame was successfully evaluated.
type FrameStatus string

const (
	FrameOK    FrameStatus = "ok"
	FrameError FrameStatus = "error"
)

// CategoryResult is one provider signal normalized into the common schema.
//
// Nullable scalars serialize as JSON null, never omitted: nil Score means
// could-not-evaluate / unknown. Never emit 0 for unknown.
type CategoryResult struct {
	Category      Category    `json:"category"`
	ProviderLabel string      `json:"provider_label"`
	Score         *float64    `json:"score"`
	ScoreOrigin   ScoreOrigin `json:"score_origin"`
	Threshold     *float64    `json:"threshold"`
	Flagged       bool        `json:"flagged"`
}

// FrameResult is the evaluation of one frame (or the single frame of a
// still image, in which case TimestampSec is nil).
type FrameResult struct {
	TimestampSec *float64         `json:"timestamp_sec"`
	Status       FrameStatus      `json:"status"`
	Error        string           `json:"error,omitempty"`
	Categories   []CategoryResult `json:"categories"`
}

// OverallVerdict is the asset-level rollup over ok frames.
type OverallVerdict struct {
	Verdict     Verdict   `json:"verdict"`
	Flagged     bool      `json:"flagged"`
	TopCategory *Category `json:"top_category"`
	// MaxScore and Confidence are nil when NO non-nil score exists across
	// all frames. They are never collapsed to 0.0.
	MaxScore   *float64 `json:"max_score"`
	Confidence *float64 `json:"confidence"`
}

// NormalizedResult is the one common scoring schema every adapter maps into.
type NormalizedResult struct {
	SchemaVersion string         `json:"schema_version"`
	Provider      string         `json:"provider"`
	ModelVersion  string         `json:"model_version"`
	MediaType     string         `json:"media_type"` // "image" | "video"
	AssetID       string         `json:"asset_id"`
	Frames        []FrameResult  `json:"frames"`
	Overall       OverallVerdict `json:"overall"`
	// Raw is optional and SANITIZED. It must never carry free-text, OCR
	// output, captions, or media bytes.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Image is the pipeline-owned in-memory image handed to a Moderator.
type Image struct {
	Bytes  []byte
	MIME   string
	Width  int
	Height int
	Meta   map[string]string
}

// Caps declares a Moderator's capabilities.
type Caps struct {
	// SupportsVideo means the pipeline prefers AnalyzeVideo (when the
	// impl also satisfies VideoModerator) over frame-by-frame.
	SupportsVideo bool
	// MaxImageBytes lets the pipeline pre-flight oversize images before
	// AnalyzeImage. Zero means no limit.
	MaxImageBytes int64
	// Categories are the canonical categories this adapter can emit.
	Categories []Category
}

// Source identifies an input asset.
//
// Kind is "file" or "url" ("s3" remains a future kind). For a "url"
// source, Ref carries only scheme+host+path: a presigned URL's query
// string is a CREDENTIAL, and Ref reaches the result envelope, the audit
// record, and structured logs. RefDigest is SHA-256 of the FULL original
// URL, so a verdict stays traceable to the exact request without storing
// that credential.
//
// RefDigest is empty (and omitted) for file sources.
type Source struct {
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	RefDigest string `json:"ref_digest,omitempty"`
	MediaType string `json:"media_type"` // "image" | "video"
}

// Moderator is the single-model moderation contract. Exactly one Moderator
// is active per process, selected from config at startup.
type Moderator interface {
	Name() string
	AnalyzeImage(ctx context.Context, img Image) (NormalizedResult, error)
	Capabilities() Caps
	Close() error
}

// VideoModerator is an OPTIONAL second interface for video-native
// providers. The pipeline type-asserts for it. AnalyzeVideo must never be
// added to Moderator itself (that would break every implementation).
type VideoModerator interface {
	AnalyzeVideo(ctx context.Context, video Source) (NormalizedResult, error)
}
