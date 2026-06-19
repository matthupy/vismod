// Package stub is a credential-free, deterministic Moderator used for the
// prototype and tests. It lets the full pipeline run end-to-end with no API key.
//
// The verdict is derived deterministically from the image bytes (a checksum
// over the content) so the same input always yields the same scores. It also
// emits one provider label with no canonical mapping to exercise the OTHER
// fallback in the normalizer.
package stub

import (
	"context"
	"hash/fnv"

	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/pkg/moderation"
)

const adapterName = "stub"

func init() {
	moderate.Register(adapterName, New)
}

// New builds a stub Moderator. It ignores secrets and most options.
func New(_ moderate.AdapterConfig) (moderation.Moderator, error) {
	return &stub{}, nil
}

type stub struct{}

func (s *stub) Name() string { return adapterName }

func (s *stub) Capabilities() moderation.Caps {
	return moderation.Caps{
		SupportsVideo: false,
		MaxImageBytes: 4 * 1024 * 1024, // mirror Azure's 4 MB cap so pre-flight is exercised
		Categories: []moderation.Category{
			moderation.CategorySexual,
			moderation.CategoryViolence,
			moderation.CategoryHate,
			moderation.CategorySelfHarm,
			moderation.CategoryOther,
		},
	}
}

func (s *stub) Close() error { return nil }

// AnalyzeImage returns deterministic, content-derived scores. The normalizer
// (pipeline) sets SchemaVersion/ModelVersion/AssetID and applies thresholds;
// the adapter only emits raw per-category Score + ScoreOrigin + ProviderLabel.
func (s *stub) AnalyzeImage(_ context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	h := fnv.New32a()
	_, _ = h.Write(img.Bytes)
	seed := h.Sum32()

	// Spread four deterministic scores in [0,1) from the seed.
	score := func(shift uint) float64 {
		return float64((seed>>shift)&0xFF) / 255.0
	}

	cats := []moderation.CategoryResult{
		{Category: moderation.CategorySexual, ProviderLabel: "sexual", Score: moderation.Ptr(score(0)), ScoreOrigin: moderation.ScoreOriginProbability},
		{Category: moderation.CategoryViolence, ProviderLabel: "violence", Score: moderation.Ptr(score(8)), ScoreOrigin: moderation.ScoreOriginProbability},
		{Category: moderation.CategoryHate, ProviderLabel: "hate", Score: moderation.Ptr(score(16)), ScoreOrigin: moderation.ScoreOriginProbability},
		{Category: moderation.CategorySelfHarm, ProviderLabel: "self_harm", Score: moderation.Ptr(score(24)), ScoreOrigin: moderation.ScoreOriginProbability},
		// Unmappable native label -> OTHER, score preserved, never dropped.
		{Category: moderation.CategoryOther, ProviderLabel: "stub_synthetic_label", Score: moderation.Ptr(score(4)), ScoreOrigin: moderation.ScoreOriginProbability},
	}

	return moderation.NormalizedResult{
		Provider:  adapterName,
		MediaType: "image",
		Frames: []moderation.FrameResult{{
			TimestampSec: nil,
			Status:       moderation.FrameStatusOK,
			Categories:   cats,
		}},
		// Overall is filled by the normalizer/aggregator (rollup), not the adapter.
	}, nil
}
