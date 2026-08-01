package pipeline

import (
	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/pkg/moderation"
)

// ApplyThresholds stamps each CategoryResult with its resolved flag_at
// boundary and computes Flagged. A nil Score never flags — and never
// counts as clean either; the rollup's all-nil rule handles that.
//
// Resolution goes through config.ResolveFor with the provider label, so a
// per-label override applies here and in Rollup identically.
func ApplyThresholds(cats []moderation.CategoryResult, th config.Thresholds) []moderation.CategoryResult {
	out := make([]moderation.CategoryResult, len(cats))
	for i, c := range cats {
		resolved := th.ResolveFor(moderation.Canonicalize(c.Category), c.ProviderLabel)
		c.Threshold = resolved.FlagAt
		c.Flagged = c.Score != nil && c.Threshold != nil && *c.Score >= *c.Threshold
		out[i] = c
	}
	return out
}

// Rollup aggregates frame results into the asset-level OverallVerdict.
//
// Verdict precedence is STRICT: block > error > flag > allow, evaluated in
// order:
//   - block: any ok-frame category with *Score >= block_at.
//   - error: any frame errored, zero ok frames exist, or every score
//     across all frames is nil (could-not-evaluate). Fail safe: a
//     partially-errored or unevaluable asset is never "allow".
//   - flag:  any Flagged category.
//   - allow: everything else.
func Rollup(frames []moderation.FrameResult, th config.Thresholds) moderation.OverallVerdict {
	var (
		anyError   bool
		okFrames   int
		anyFlagged bool
		block      bool
		maxScore   *float64
		topCat     *moderation.Category
	)

	for _, f := range frames {
		if f.Status != moderation.FrameOK {
			anyError = true
			continue
		}
		okFrames++
		for _, c := range f.Categories {
			if c.Flagged {
				anyFlagged = true
			}
			if c.Score != nil {
				if maxScore == nil || *c.Score > *maxScore {
					s := *c.Score
					maxScore = &s
					cat := c.Category
					topCat = &cat
				}
				resolved := th.ResolveFor(moderation.Canonicalize(c.Category), c.ProviderLabel)
				if resolved.BlockAt != nil && *c.Score >= *resolved.BlockAt {
					block = true
				}
			}
		}
	}

	verdict := moderation.VerdictAllow
	switch {
	case block:
		verdict = moderation.VerdictBlock
	case anyError, okFrames == 0, maxScore == nil:
		// maxScore==nil means no non-nil score existed anywhere — that is
		// could-not-evaluate, never allow.
		verdict = moderation.VerdictError
	case anyFlagged:
		verdict = moderation.VerdictFlag
	}

	var confidence *float64
	if maxScore != nil {
		c := *maxScore
		confidence = &c
	}
	return moderation.OverallVerdict{
		Verdict:     verdict,
		Flagged:     anyFlagged,
		TopCategory: topCat,
		MaxScore:    maxScore,
		Confidence:  confidence,
	}
}
