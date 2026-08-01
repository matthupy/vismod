package pipeline

import (
	"testing"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/pkg/moderation"
)

func f(v float64) *float64 { return &v }

func th() config.Thresholds {
	return config.Thresholds{
		"default": {FlagAt: f(0.5), BlockAt: f(0.8)},
		"SEXUAL":  {FlagAt: f(0.4), BlockAt: f(0.7)},
	}
}

func okFrame(cats ...moderation.CategoryResult) moderation.FrameResult {
	return moderation.FrameResult{Status: moderation.FrameOK, Categories: cats}
}

func errFrame(msg string) moderation.FrameResult {
	return moderation.FrameResult{Status: moderation.FrameError, Error: msg, Categories: []moderation.CategoryResult{}}
}

func cat(c moderation.Category, score *float64) moderation.CategoryResult {
	return moderation.CategoryResult{Category: c, Score: score, ScoreOrigin: moderation.OriginProbability}
}

func TestApplyThresholds(t *testing.T) {
	cats := ApplyThresholds([]moderation.CategoryResult{
		cat(moderation.CategorySexual, f(0.45)),   // >= SEXUAL flag_at 0.4 -> flagged
		cat(moderation.CategoryViolence, f(0.45)), // < default 0.5 -> not flagged
		cat(moderation.CategoryHate, nil),         // nil score never flags
	}, th())

	if !cats[0].Flagged || cats[0].Threshold == nil || *cats[0].Threshold != 0.4 {
		t.Errorf("SEXUAL 0.45 should flag at 0.4: %+v", cats[0])
	}
	if cats[1].Flagged {
		t.Errorf("VIOLENCE 0.45 must not flag at default 0.5")
	}
	if cats[2].Flagged || cats[2].Score != nil {
		t.Errorf("nil score must stay nil and unflagged: %+v", cats[2])
	}
}

func TestRollupVerdictPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		frames  []moderation.FrameResult
		verdict moderation.Verdict
	}{
		{"all benign -> allow",
			[]moderation.FrameResult{okFrame(cat(moderation.CategoryViolence, f(0.1)))},
			moderation.VerdictAllow},
		{"flagged -> flag",
			[]moderation.FrameResult{okFrame(flagged(cat(moderation.CategorySexual, f(0.5))))},
			moderation.VerdictFlag},
		{"score >= block_at -> block",
			[]moderation.FrameResult{okFrame(flagged(cat(moderation.CategorySexual, f(0.9))))},
			moderation.VerdictBlock},
		{"any errored frame -> error even with benign ok frames",
			[]moderation.FrameResult{okFrame(cat(moderation.CategoryViolence, f(0.1))), errFrame("boom")},
			moderation.VerdictError},
		{"error beats flag (partial video never downgraded)",
			[]moderation.FrameResult{okFrame(flagged(cat(moderation.CategorySexual, f(0.5)))), errFrame("boom")},
			moderation.VerdictError},
		{"block beats error (harm evidence wins)",
			[]moderation.FrameResult{okFrame(flagged(cat(moderation.CategorySexual, f(0.95)))), errFrame("boom")},
			moderation.VerdictBlock},
		{"zero frames -> error, never allow",
			nil,
			moderation.VerdictError},
		{"zero ok frames -> error",
			[]moderation.FrameResult{errFrame("a"), errFrame("b")},
			moderation.VerdictError},
		{"all-nil scores -> error (could not evaluate)",
			[]moderation.FrameResult{okFrame(cat(moderation.CategoryHate, nil), cat(moderation.CategoryViolence, nil))},
			moderation.VerdictError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Rollup(tc.frames, th())
			if got.Verdict != tc.verdict {
				t.Errorf("verdict = %q, want %q (%+v)", got.Verdict, tc.verdict, got)
			}
			if got.Verdict == moderation.VerdictAllow && anyErrored(tc.frames) {
				t.Error("allow emitted while a frame errored — fail-safe violation")
			}
		})
	}
}

func TestRollupNilScoreDiscipline(t *testing.T) {
	// No non-nil score anywhere: MaxScore/Confidence must be nil, not 0.0.
	got := Rollup([]moderation.FrameResult{okFrame(cat(moderation.CategoryHate, nil))}, th())
	if got.MaxScore != nil || got.Confidence != nil {
		t.Errorf("MaxScore/Confidence must be nil when no non-nil score exists: %+v", got)
	}
	if got.Verdict != moderation.VerdictError {
		t.Errorf("all-nil must be error, got %q", got.Verdict)
	}

	// Mixed nil and non-nil: max over non-nil only.
	got = Rollup([]moderation.FrameResult{okFrame(
		cat(moderation.CategoryHate, nil),
		cat(moderation.CategoryViolence, f(0.3)),
	)}, th())
	if got.MaxScore == nil || *got.MaxScore != 0.3 {
		t.Errorf("MaxScore = %v, want 0.3", got.MaxScore)
	}
	if got.TopCategory == nil || *got.TopCategory != moderation.CategoryViolence {
		t.Errorf("TopCategory = %v, want VIOLENCE", got.TopCategory)
	}
}

func flagged(c moderation.CategoryResult) moderation.CategoryResult {
	cats := ApplyThresholds([]moderation.CategoryResult{c}, th())
	return cats[0]
}

func anyErrored(frames []moderation.FrameResult) bool {
	for _, fr := range frames {
		if fr.Status == moderation.FrameError {
			return true
		}
	}
	return false
}

// labelTh is th() plus provider-label overrides in hybrid mode.
func labelTh() config.Thresholds {
	return th().Merge(config.ProviderThresholds{
		Mode: config.ProviderModeHybrid,
		Labels: config.Thresholds{
			// Stricter than SEXUAL's 0.7 block_at.
			"yes_genitals": {BlockAt: f(0.5)},
			// Looser than default's 0.5 flag_at: a noisy negative head an
			// operator wants quieted without touching OTHER globally.
			"no_gun": {FlagAt: f(0.99)},
		},
	})
}

// TestProviderLabelAppliesToFlagAndBlock is the maintainer decision made
// executable: one override must move BOTH the flag decision and the block
// decision. A label that flags but does not block means the two callers
// have drifted apart.
func TestProviderLabelAppliesToFlagAndBlock(t *testing.T) {
	c := moderation.CategoryResult{
		Category:      moderation.CategorySexual,
		ProviderLabel: "yes_genitals",
		Score:         f(0.55),
		ScoreOrigin:   moderation.OriginProbability,
	}
	// Flagging pass sees the label.
	got := ApplyThresholds([]moderation.CategoryResult{c}, labelTh())
	if !got[0].Flagged || got[0].Threshold == nil || *got[0].Threshold != 0.4 {
		t.Errorf("flag pass: %+v", got[0])
	}
	// Block pass sees the same label: 0.55 >= label block_at 0.5, even
	// though it is below SEXUAL's 0.7.
	overall := Rollup([]moderation.FrameResult{okFrame(got...)}, labelTh())
	if overall.Verdict != moderation.VerdictBlock {
		t.Errorf("label block_at must produce a block verdict, got %q", overall.Verdict)
	}
	// Without the override the same score is a flag, not a block — proving
	// the override, not the score, moved the verdict.
	plain := ApplyThresholds([]moderation.CategoryResult{c}, th())
	if v := Rollup([]moderation.FrameResult{okFrame(plain...)}, th()).Verdict; v != moderation.VerdictFlag {
		t.Errorf("without the label override this must be a flag, got %q", v)
	}
}

// A looser override is allowed (full override, not a clamp) but must not
// reach past the verdict precedence: it can only make THIS label quieter,
// never turn another category's block into an allow.
func TestLooserProviderLabelCannotSuppressOtherSignals(t *testing.T) {
	quiet := moderation.CategoryResult{
		Category:      moderation.CategoryOther,
		ProviderLabel: "no_gun",
		Score:         f(0.6), // >= default 0.5, but < label flag_at 0.99
		ScoreOrigin:   moderation.OriginProbability,
	}
	loud := moderation.CategoryResult{
		Category:      moderation.CategorySexual,
		ProviderLabel: "general_nsfw", // no override: SEXUAL block_at 0.7
		Score:         f(0.9),
		ScoreOrigin:   moderation.OriginProbability,
	}
	cats := ApplyThresholds([]moderation.CategoryResult{quiet, loud}, labelTh())
	if cats[0].Flagged {
		t.Errorf("looser label override should have quieted no_gun: %+v", cats[0])
	}
	if got := Rollup([]moderation.FrameResult{okFrame(cats...)}, labelTh()); got.Verdict != moderation.VerdictBlock {
		t.Errorf("an unrelated blocking signal must still block, got %q", got.Verdict)
	}
}

// A nil score never flags, whatever the label says. Null discipline is not
// negotiable by configuration.
func TestProviderLabelCannotFlagNilScore(t *testing.T) {
	c := moderation.CategoryResult{
		Category:      moderation.CategorySexual,
		ProviderLabel: "yes_genitals",
		Score:         nil,
	}
	got := ApplyThresholds([]moderation.CategoryResult{c}, labelTh())
	if got[0].Flagged {
		t.Errorf("nil score must never flag: %+v", got[0])
	}
	if v := Rollup([]moderation.FrameResult{okFrame(got...)}, labelTh()).Verdict; v != moderation.VerdictError {
		t.Errorf("all-nil scores must roll up to error, got %q", v)
	}
}
