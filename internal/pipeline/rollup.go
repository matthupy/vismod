package pipeline

import "github.com/matthupy/vismod/pkg/moderation"

// applyThresholds stamps the flag_at Threshold and computes Flagged for every
// score-based CategoryResult. List-membership rows (hash matches) keep
// Threshold=nil and their pre-set Flagged=true.
func (p *Pipeline) applyThresholds(res *moderation.NormalizedResult) {
	for fi := range res.Frames {
		for ci := range res.Frames[fi].Categories {
			c := &res.Frames[fi].Categories[ci]
			if c.ScoreOrigin == moderation.ScoreOriginListMembership {
				// Binary list membership: Flagged already set, no threshold.
				continue
			}
			ct := p.Cfg.Thresholds.For(c.Category)
			flagAt := ct.FlagAt
			c.Threshold = moderation.Ptr(flagAt)
			c.Flagged = c.Score != nil && *c.Score >= flagAt
		}
	}
}

// rollup computes the asset-level OverallVerdict from per-frame results.
//
// Verdict precedence is STRICT: block > error > flag > allow.
//   - block: any ok category has *Score >= block_at[cat] OR a list-membership match
//   - error: any frame Status=error, OR zero ok frames, OR every score is nil
//   - flag:  any CategoryResult.Flagged
//   - allow: otherwise
//
// MaxScore/Confidence/TopCategory are computed over non-nil scores in ok frames
// and are nil when no non-nil score exists (never collapsed to 0.0).
func (p *Pipeline) rollup(framesIn []moderation.FrameResult) moderation.OverallVerdict {
	var (
		anyError    bool
		okFrames    int
		anyFlagged  bool
		anyBlock    bool
		anyNonNil   bool
		maxScore    float64
		topCategory moderation.Category
		haveTop     bool
	)

	for _, f := range framesIn {
		if f.Status == moderation.FrameStatusError {
			anyError = true
			continue
		}
		okFrames++
		for _, c := range f.Categories {
			// list-membership match => block.
			if c.ScoreOrigin == moderation.ScoreOriginListMembership && c.Flagged {
				anyBlock = true
			}
			if c.Flagged {
				anyFlagged = true
			}
			if c.Score == nil {
				continue
			}
			anyNonNil = true
			blockAt := p.Cfg.Thresholds.For(c.Category).BlockAt
			if *c.Score >= blockAt {
				anyBlock = true
			}
			if !haveTop || *c.Score > maxScore {
				maxScore = *c.Score
				topCategory = c.Category
				haveTop = true
			}
		}
	}

	ov := moderation.OverallVerdict{Flagged: anyFlagged}
	if haveTop {
		ov.MaxScore = moderation.Ptr(maxScore)
		ov.Confidence = moderation.Ptr(maxScore)
		tc := topCategory
		ov.TopCategory = &tc
	}

	switch {
	case anyBlock:
		ov.Verdict = moderation.VerdictBlock
		ov.Flagged = true
	case anyError || okFrames == 0 || !anyNonNil:
		// Could-not-evaluate: never allow.
		ov.Verdict = moderation.VerdictError
		// A pure error rollup carries no flag.
		if !anyFlagged {
			ov.Flagged = false
		}
	case anyFlagged:
		ov.Verdict = moderation.VerdictFlag
	default:
		ov.Verdict = moderation.VerdictAllow
	}
	return ov
}
