// Package moderation defines the public, model-agnostic contract for the
// visual content moderation pipeline: the normalized result schema that every
// adapter produces and that external consumers bind to, plus the Moderator /
// VideoModerator / HashMatcher interfaces adapters implement.
//
// This package has NO internal dependencies. It is the stable seam between
// provider-specific adapters (which map wildly different outputs into this
// schema) and downstream consumers (review consoles, rules engines).
package moderation

import (
	"context"
	"encoding/json"
)

// SchemaVersion is the current NormalizedResult schema version. The normalizer
// stamps it on every result; adapters never set it. Additive fields / additive
// Category values bump the minor; a removal/rename/meaning-change bumps major.
const SchemaVersion = "1.0"

// Verdict is the overall asset decision. Precedence is strict:
// block > error > flag > allow. "error" means could-not-evaluate and MUST be
// emitted instead of "allow" on any failure (fail-safe, never fail-silent).
type Verdict string

const (
	VerdictAllow Verdict = "allow"
	VerdictFlag  Verdict = "flag"
	VerdictBlock Verdict = "block"
	VerdictError Verdict = "error"
)

// Category is the canonical, provider-independent harm taxonomy. Any provider
// label with no canonical mapping normalizes to OTHER (raw label preserved in
// ProviderLabel) — a result is never dropped. Consumers MUST tolerate unknown
// future Category values by treating them as OTHER.
type Category string

const (
	CategorySexual         Category = "SEXUAL"
	CategorySuggestiveRacy Category = "SUGGESTIVE_RACY"
	CategoryViolence       Category = "VIOLENCE"
	CategoryGoreGraphic    Category = "GORE_GRAPHIC"
	CategoryWeapons        Category = "WEAPONS"
	CategorySelfHarm       Category = "SELF_HARM"
	CategoryHate           Category = "HATE"
	CategoryDrugs          Category = "DRUGS"
	// MEDICAL and SPOOF carry Google Vision SafeSearch's medical/spoof signals
	// only; they are NOT harm signals — document provenance to consumers.
	CategoryMedical Category = "MEDICAL"
	CategorySpoof   Category = "SPOOF"
	// CSAMHashMatch is reserved for the HashMatcher pre-stage. No classifier
	// emits it. A hit is binary list-membership (Score=nil), never a score.
	CategoryCSAMHashMatch Category = "CSAM_HASH_MATCH"
	CategoryOther         Category = "OTHER"
)

// ScoreOrigin tags how a normalized Score was derived so consumers know its
// provenance. Scores are within-provider comparable ONLY — a 0.667 threshold
// means different things across origins; thresholds are not portable.
type ScoreOrigin string

const (
	ScoreOriginProbability    ScoreOrigin = "probability"
	ScoreOriginConfidencePct  ScoreOrigin = "confidence_pct"
	ScoreOriginLikelihoodEnum ScoreOrigin = "likelihood_enum"
	ScoreOriginSeverity       ScoreOrigin = "severity"
	ScoreOriginListMembership ScoreOrigin = "list_membership"
)

// FrameStatus records whether a single frame could be evaluated. An "error"
// frame never contributes an "allow" to the asset rollup.
type FrameStatus string

const (
	FrameStatusOK    FrameStatus = "ok"
	FrameStatusError FrameStatus = "error"
)

// CategoryResult is one provider signal mapped onto the canonical taxonomy.
//
// Nullable scalars (Score, Threshold) serialize as explicit JSON null, never
// omitted — consumers read null, not an absent field. A flagged hash-match row
// emits score:null, threshold:null, flagged:true.
type CategoryResult struct {
	Category      Category    `json:"category"`
	ProviderLabel string      `json:"provider_label"` // raw native class/Name/enum
	Score         *float64    `json:"score"`          // normalized 0..1; nil = unknown/unsupported/list-membership
	ScoreOrigin   ScoreOrigin `json:"score_origin"`
	Threshold     *float64    `json:"threshold"`            // flag_at boundary; nil for list_membership rows
	Flagged       bool        `json:"flagged"`              // (Score!=nil && Threshold!=nil && *Score>=*Threshold) OR list-membership match
	MatchType     string      `json:"match_type,omitempty"` // list_membership only, e.g. "pdq"/"md5"
	MatchList     string      `json:"match_list,omitempty"` // list_membership only, e.g. "ncmec"/"iwf"
}

// FrameResult holds the categories for one frame (one still image => one frame
// with TimestampSec nil).
type FrameResult struct {
	TimestampSec *float64         `json:"timestamp_sec"` // nil for still images
	Status       FrameStatus      `json:"status"`
	Error        string           `json:"error,omitempty"`
	Categories   []CategoryResult `json:"categories"`
}

// OverallVerdict is the asset-level rollup across all frames. MaxScore and
// Confidence are nil (never 0.0) when no non-nil score exists.
type OverallVerdict struct {
	Verdict     Verdict   `json:"verdict"`
	Flagged     bool      `json:"flagged"`
	TopCategory *Category `json:"top_category"`
	MaxScore    *float64  `json:"max_score"`
	Confidence  *float64  `json:"confidence"`
}

// NormalizedResult is the single model-agnostic schema every adapter produces.
// SchemaVersion and ModelVersion are set by the normalizer, never the adapter.
type NormalizedResult struct {
	SchemaVersion string          `json:"schema_version"`
	Provider      string          `json:"provider"`
	ModelVersion  string          `json:"model_version"` // api-version/model id; "" if none
	MediaType     string          `json:"media_type"`    // "image" | "video"
	AssetID       string          `json:"asset_id"`      // stamped by pipeline from Source.Ref
	Frames        []FrameResult   `json:"frames"`
	Overall       OverallVerdict  `json:"overall"`
	Raw           json.RawMessage `json:"raw,omitempty"` // OPTIONAL + SANITIZED (no free-text/OCR/captions)
}

// Image is a single decoded image handed to an adapter.
type Image struct {
	Bytes  []byte
	MIME   string
	Width  int
	Height int
	Meta   map[string]string
}

// Source describes what to moderate. Kind "file" is the v1 default; "url"/"s3"
// require an SSRF allow-list before they are enabled.
type Source struct {
	Kind      string `json:"kind"`       // "file" (v1); "url"/"s3" later
	Ref       string `json:"ref"`        // path or URI
	MediaType string `json:"media_type"` // "image" | "video" | "" (auto-detect)
}

// Caps declares an adapter's capabilities so the pipeline can dispatch and
// pre-flight correctly.
type Caps struct {
	SupportsVideo bool       // true => pipeline prefers AnalyzeVideo (if impl)
	MaxImageBytes int64      // pipeline pre-flights oversize images (0 = no limit)
	Categories    []Category // canonical categories this adapter can emit
}

// Moderator is the one interface every adapter implements. Exactly one
// Moderator is active per process, selected from config at startup.
type Moderator interface {
	Name() string
	AnalyzeImage(ctx context.Context, img Image) (NormalizedResult, error)
	Capabilities() Caps
	Close() error
}

// VideoModerator is an OPTIONAL second interface for video-native providers.
// The pipeline type-asserts for it. Do NOT fold AnalyzeVideo into Moderator —
// that would break every existing adapter.
type VideoModerator interface {
	AnalyzeVideo(ctx context.Context, video Source) (NormalizedResult, error)
}

// HashMatch is a binary list-membership result, NOT a score.
type HashMatch struct {
	Matched  bool
	ListName string // -> CategoryResult.MatchList
	Algo     string // -> CategoryResult.MatchType
}

// HashMatcher is the CSAM pre-stage seam. v1 ships a no-op default; the
// PDQ/TMK matcher is v1.1. A match short-circuits the classifier.
type HashMatcher interface {
	Match(ctx context.Context, img Image) (HashMatch, error)
}

// Ptr returns a pointer to v. Helper for building nullable scalars.
func Ptr[T any](v T) *T { return &v }
