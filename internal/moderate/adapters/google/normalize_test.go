package google

import (
	"testing"

	"github.com/matthupy/vismod/pkg/moderation"
)

// findCat returns the first CategoryResult for cat, or false if absent.
func findCat(rs []moderation.CategoryResult, cat moderation.Category) (moderation.CategoryResult, bool) {
	for _, r := range rs {
		if r.Category == cat {
			return r, true
		}
	}
	return moderation.CategoryResult{}, false
}

// TestLikelihoodScore pins the §E ordinal-enum -> score lookup, including the
// VERY_UNLIKELY(0.0) vs UNKNOWN(nil) distinction.
func TestLikelihoodScore(t *testing.T) {
	cases := []struct {
		enum string
		want *float64
	}{
		{"VERY_UNLIKELY", moderation.Ptr(0.0)},
		{"UNLIKELY", moderation.Ptr(0.25)},
		{"POSSIBLE", moderation.Ptr(0.5)},
		{"LIKELY", moderation.Ptr(0.75)},
		{"VERY_LIKELY", moderation.Ptr(1.0)},
		{"UNKNOWN", nil},
		{"", nil},        // missing field
		{"GARBAGE", nil}, // unrecognized future enum -> nil, never a guessed score
	}
	for _, c := range cases {
		got := likelihoodScore(c.enum)
		switch {
		case got == nil && c.want == nil:
			// ok
		case got == nil || c.want == nil:
			t.Errorf("likelihoodScore(%q) = %v, want %v", c.enum, got, c.want)
		case *got != *c.want:
			t.Errorf("likelihoodScore(%q) = %v, want %v", c.enum, *got, *c.want)
		}
	}
}

// TestNormalizeCategoryMapping pins the SafeSearch 5-field -> canonical taxonomy
// mapping and the ScoreOrigin tag, and that ProviderLabel keeps the raw field.
func TestNormalizeCategoryMapping(t *testing.T) {
	ann := &safeSearchAnnotation{
		Adult:    "VERY_LIKELY",
		Racy:     "LIKELY",
		Violence: "POSSIBLE",
		Medical:  "UNLIKELY",
		Spoof:    "VERY_UNLIKELY",
	}
	got := normalize(ann)

	want := []struct {
		cat   moderation.Category
		label string
		score float64
	}{
		{moderation.CategorySexual, "adult", 1.0},
		{moderation.CategorySuggestiveRacy, "racy", 0.75},
		{moderation.CategoryViolence, "violence", 0.5},
		{moderation.CategoryMedical, "medical", 0.25},
		{moderation.CategorySpoof, "spoof", 0.0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d categories, want %d: %+v", len(got), len(want), got)
	}
	for _, w := range want {
		r, ok := findCat(got, w.cat)
		if !ok {
			t.Errorf("missing category %s", w.cat)
			continue
		}
		if r.ProviderLabel != w.label {
			t.Errorf("%s ProviderLabel = %q, want %q", w.cat, r.ProviderLabel, w.label)
		}
		if r.ScoreOrigin != moderation.ScoreOriginLikelihoodEnum {
			t.Errorf("%s ScoreOrigin = %q, want likelihood_enum", w.cat, r.ScoreOrigin)
		}
		if r.Score == nil || *r.Score != w.score {
			t.Errorf("%s Score = %v, want %v", w.cat, r.Score, w.score)
		}
	}
}

// TestNormalizeUnknownEmitsNilScore proves an UNKNOWN field is emitted as a row
// with Score=nil (could-not-evaluate), not omitted and not 0.0. The asset rollup
// turns an all-nil result into Verdict=error (fail-safe).
func TestNormalizeUnknownEmitsNilScore(t *testing.T) {
	ann := &safeSearchAnnotation{
		Adult:    "UNKNOWN",
		Racy:     "UNKNOWN",
		Violence: "UNKNOWN",
		Medical:  "UNKNOWN",
		Spoof:    "UNKNOWN",
	}
	got := normalize(ann)
	if len(got) != 5 {
		t.Fatalf("got %d categories, want 5 (UNKNOWN still emits a row)", len(got))
	}
	for _, r := range got {
		if r.Score != nil {
			t.Errorf("%s: UNKNOWN must emit Score=nil, got %v", r.Category, *r.Score)
		}
		if r.ScoreOrigin != moderation.ScoreOriginLikelihoodEnum {
			t.Errorf("%s: ScoreOrigin = %q, want likelihood_enum", r.Category, r.ScoreOrigin)
		}
	}
}
