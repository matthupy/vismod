// Package google implements the Cloud Vision SafeSearch adapter using the
// official Go SDK (cloud.google.com/go/vision/v2/apiv1 — a blessed
// dependency). Auth is Application Default Credentials / service account,
// env-only (GOOGLE_APPLICATION_CREDENTIALS); no secrets in yaml.
//
// Response schema verified against "Detect explicit content (SafeSearch)",
// cloud.google.com/vision/docs/detecting-safe-search, checked 2026-07-29:
// safeSearchAnnotation carries exactly the five fields read below, each
// holding one of the six Likelihood values (UNKNOWN, VERY_UNLIKELY,
// UNLIKELY, POSSIBLE, LIKELY, VERY_LIKELY). No drift found.
//
// SafeSearch returns five categories (adult, spoof, medical, violence,
// racy), each a likelihood ENUM — not a probability. Normalization uses a
// configurable lookup (default: VERY_UNLIKELY→0.0, UNLIKELY→0.25,
// POSSIBLE→0.5, LIKELY→0.75, VERY_LIKELY→1.0) with UNKNOWN→nil (never 0),
// ScoreOrigin="likelihood_enum".
//
// Mapping: adult→SEXUAL, racy→SUGGESTIVE_RACY, violence→VIOLENCE,
// medical→MEDICAL, spoof→SPOOF. MEDICAL and SPOOF are provenance
// carriers, NOT harm signals. SafeSearch is image-only → video goes
// through frame extraction.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	vision "cloud.google.com/go/vision/v2/apiv1"
	pb "cloud.google.com/go/vision/v2/apiv1/visionpb"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

func init() {
	moderate.Register("google", New)
}

// DefaultLikelihoodScores is the default likelihood→score lookup.
// UNKNOWN is intentionally absent: it normalizes to a nil Score.
var DefaultLikelihoodScores = map[string]float64{
	"VERY_UNLIKELY": 0.0,
	"UNLIKELY":      0.25,
	"POSSIBLE":      0.5,
	"LIKELY":        0.75,
	"VERY_LIKELY":   1.0,
}

var categoryMap = map[string]moderation.Category{
	"adult":    moderation.CategorySexual,
	"racy":     moderation.CategorySuggestiveRacy,
	"violence": moderation.CategoryViolence,
	"medical":  moderation.CategoryMedical,
	"spoof":    moderation.CategorySpoof,
}

type options struct {
	RateLimitRPS     float64            `json:"rate_limit_rps"`    // default 15
	LikelihoodScores map[string]float64 `json:"likelihood_scores"` // overrides DefaultLikelihoodScores
}

// annotator abstracts the SDK client for tests.
type annotator interface {
	DetectSafeSearch(ctx context.Context, img *pb.Image, ictx *pb.ImageContext) (*pb.SafeSearchAnnotation, error)
	Close() error
}

type Moderator struct {
	client  annotator
	limiter *moderate.Limiter
	lookup  map[string]float64
}

// New is the registry factory. Client construction fails fast when ADC
// credentials are absent (boot-time credential validation).
func New(cfg moderate.AdapterConfig) (moderation.Moderator, error) {
	var o options
	b, _ := json.Marshal(cfg.Options)
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("google: options: %w", err)
	}
	client, err := vision.NewImageAnnotatorClient(context.Background())
	if err != nil {
		return nil, fmt.Errorf("google: vision client (check ADC / GOOGLE_APPLICATION_CREDENTIALS): %w", err)
	}
	return newWith(sdkAnnotator{client}, o), nil
}

// sdkAnnotator adapts the SDK client to the test-friendly annotator
// interface via BatchAnnotateImages + SAFE_SEARCH_DETECTION (the v2 SDK
// exposes no per-feature convenience wrapper).
type sdkAnnotator struct{ c *vision.ImageAnnotatorClient }

func (s sdkAnnotator) DetectSafeSearch(ctx context.Context, img *pb.Image, ictx *pb.ImageContext) (*pb.SafeSearchAnnotation, error) {
	resp, err := s.c.BatchAnnotateImages(ctx, &pb.BatchAnnotateImagesRequest{
		Requests: []*pb.AnnotateImageRequest{{
			Image:        img,
			ImageContext: ictx,
			Features:     []*pb.Feature{{Type: pb.Feature_SAFE_SEARCH_DETECTION}},
		}},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.GetResponses()) == 0 {
		return nil, fmt.Errorf("empty batch response")
	}
	r := resp.GetResponses()[0]
	if e := r.GetError(); e != nil {
		return nil, fmt.Errorf("annotate error %d: %s", e.GetCode(), e.GetMessage())
	}
	return r.GetSafeSearchAnnotation(), nil
}
func (s sdkAnnotator) Close() error { return s.c.Close() }

func newWith(client annotator, o options) *Moderator {
	if o.RateLimitRPS == 0 {
		o.RateLimitRPS = 15
	}
	lookup := make(map[string]float64, len(DefaultLikelihoodScores))
	for k, v := range DefaultLikelihoodScores {
		lookup[k] = v
	}
	for k, v := range o.LikelihoodScores {
		lookup[strings.ToUpper(k)] = v
	}
	return &Moderator{
		client:  client,
		limiter: moderate.NewLimiter(o.RateLimitRPS),
		lookup:  lookup,
	}
}

func (m *Moderator) Name() string         { return "google" }
func (m *Moderator) ModelVersion() string { return "vision-v1-safesearch" }
func (m *Moderator) Close() error         { return m.client.Close() }

func (m *Moderator) Capabilities() moderation.Caps {
	return moderation.Caps{
		SupportsVideo: false, // SafeSearch is image-only → frames
		MaxImageBytes: 20 << 20,
		Categories: []moderation.Category{
			moderation.CategorySexual, moderation.CategorySuggestiveRacy,
			moderation.CategoryViolence, moderation.CategoryMedical,
			moderation.CategorySpoof,
		},
	}
}

func (m *Moderator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	if err := m.limiter.Wait(ctx); err != nil {
		return moderation.NormalizedResult{}, err
	}
	ann, err := m.client.DetectSafeSearch(ctx, &pb.Image{Content: img.Bytes}, nil)
	if err != nil {
		// gRPC transient classification is handled by the SDK's own retry
		// policy; a surviving error is surfaced as retryable so the
		// fail-safe path applies (never allow).
		return moderation.NormalizedResult{}, moderation.Retryable(fmt.Errorf("google: safesearch: %w", err))
	}
	return m.Normalize(ann)
}

// Normalize maps a SafeSearchAnnotation into the common schema. Exported
// for golden tests (pure function, no network).
func (m *Moderator) Normalize(ann *pb.SafeSearchAnnotation) (moderation.NormalizedResult, error) {
	if ann == nil {
		return moderation.NormalizedResult{}, fmt.Errorf("google: nil SafeSearch annotation (could not evaluate)")
	}
	fields := []struct {
		label string
		lk    pb.Likelihood
	}{
		{"adult", ann.Adult},
		{"spoof", ann.Spoof},
		{"medical", ann.Medical},
		{"violence", ann.Violence},
		{"racy", ann.Racy},
	}
	cats := make([]moderation.CategoryResult, 0, len(fields))
	rawMap := make(map[string]string, len(fields))
	for _, f := range fields {
		name := f.lk.String() // e.g. "VERY_UNLIKELY"; "UNKNOWN" for unset
		rawMap[f.label] = name
		var score *float64
		if v, ok := m.lookup[name]; ok && name != "UNKNOWN" {
			s := v
			score = &s
		} // UNKNOWN (or an unlisted enum value) stays nil — never 0
		canonical, ok := categoryMap[f.label]
		if !ok {
			canonical = moderation.CategoryOther
		}
		cats = append(cats, moderation.CategoryResult{
			Category:      canonical,
			ProviderLabel: f.label + ":" + name,
			Score:         score,
			ScoreOrigin:   moderation.OriginLikelihoodEnum,
		})
	}
	raw, err := json.Marshal(rawMap) // sanitized: enum names only
	if err != nil {
		return moderation.NormalizedResult{}, err
	}
	return moderation.NormalizedResult{
		Provider:     "google",
		ModelVersion: m.ModelVersion(),
		Frames:       []moderation.FrameResult{{Status: moderation.FrameOK, Categories: cats}},
		Raw:          raw,
	}, nil
}
