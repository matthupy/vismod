package hive

import (
	"testing"

	"github.com/matthupy/vismod/pkg/moderation"
)

// findCat returns the CategoryResult for cat with the given provider label, or
// nil. Output order is deterministic (sorted) but tests look up by identity so
// they don't couple to the exact slice index.
func findCat(rs []moderation.CategoryResult, cat moderation.Category, label string) *moderation.CategoryResult {
	for i := range rs {
		if rs[i].Category == cat && rs[i].ProviderLabel == label {
			return &rs[i]
		}
	}
	return nil
}

func TestNormalize_NSFWHeadSplitsAcrossCategories(t *testing.T) {
	// The NSFW head carries TWO positive classes that map to DIFFERENT canonical
	// categories: general_nsfw->SEXUAL, general_suggestive->SUGGESTIVE_RACY. They
	// must NOT be summed together; each lands in its own category at its own score.
	classes := []hiveClass{
		{Class: "general_nsfw", Score: 0.94},
		{Class: "general_suggestive", Score: 0.05},
		{Class: "general_not_nsfw_not_suggestive", Score: 0.01},
	}
	got := normalize(classes)

	sexual := findCat(got, moderation.CategorySexual, "general_nsfw")
	if sexual == nil {
		t.Fatalf("missing SEXUAL/general_nsfw; got %+v", got)
	}
	if *sexual.Score != 0.94 {
		t.Errorf("SEXUAL score = %v, want 0.94", *sexual.Score)
	}
	if sexual.ScoreOrigin != moderation.ScoreOriginProbability {
		t.Errorf("ScoreOrigin = %q, want probability", sexual.ScoreOrigin)
	}
	racy := findCat(got, moderation.CategorySuggestiveRacy, "general_suggestive")
	if racy == nil || *racy.Score != 0.05 {
		t.Fatalf("missing/!=0.05 SUGGESTIVE_RACY/general_suggestive; got %+v", got)
	}
}

func TestNormalize_DropsNegativeClasses(t *testing.T) {
	// The "safe complement" classes (no_gun, general_not_*) are structural, not
	// harm signals. They must never appear as a CategoryResult.
	classes := []hiveClass{
		{Class: "general_not_nsfw_not_suggestive", Score: 0.99},
		{Class: "no_gun", Score: 0.99},
	}
	got := normalize(classes)
	if len(got) != 0 {
		t.Fatalf("negative-only input must yield no categories; got %+v", got)
	}
}

func TestNormalize_SumsPositivesWithinHeadSameCategory(t *testing.T) {
	// The Gun head has three positive classes, all mapping to WEAPONS. They are
	// mutually-exclusive sub-states of "a gun is present", so P(gun) = their sum.
	classes := []hiveClass{
		{Class: "gun_in_hand", Score: 0.50},
		{Class: "gun_not_in_hand", Score: 0.20},
		{Class: "animated_gun", Score: 0.10},
		{Class: "no_gun", Score: 0.20},
	}
	got := normalize(classes)
	weapons := findCat(got, moderation.CategoryWeapons, "gun_in_hand") // top contributor labels the row
	if weapons == nil {
		t.Fatalf("missing WEAPONS; got %+v", got)
	}
	if d := *weapons.Score - 0.80; d > 1e-9 || d < -1e-9 {
		t.Errorf("WEAPONS score = %v, want 0.80 (sum of positives)", *weapons.Score)
	}
}

func TestNormalize_MaxAcrossHeadsSameCategory(t *testing.T) {
	// Gun head and Knife head both map to WEAPONS. Across heads the category takes
	// the MAX (most confident weapon signal), not the sum.
	classes := []hiveClass{
		{Class: "gun_in_hand", Score: 0.30}, {Class: "no_gun", Score: 0.70},
		{Class: "knife_in_hand", Score: 0.60}, {Class: "no_knife", Score: 0.40},
	}
	got := normalize(classes)
	// exactly one WEAPONS row, from the knife head (0.60 > 0.30)
	var weapons []moderation.CategoryResult
	for _, r := range got {
		if r.Category == moderation.CategoryWeapons {
			weapons = append(weapons, r)
		}
	}
	if len(weapons) != 1 {
		t.Fatalf("want exactly 1 WEAPONS row, got %d: %+v", len(weapons), weapons)
	}
	if *weapons[0].Score != 0.60 || weapons[0].ProviderLabel != "knife_in_hand" {
		t.Errorf("WEAPONS = %+v, want score 0.60 label knife_in_hand", weapons[0])
	}
}

func TestNormalize_SkipsDescriptiveHeads(t *testing.T) {
	// Descriptive heads (image type, text, qr) are informational, not harm. They
	// are deliberately not emitted as harm CategoryResults (documented provenance).
	classes := []hiveClass{
		{Class: "natural", Score: 0.90},
		{Class: "text", Score: 0.80},
		{Class: "yes_qr_code", Score: 0.70},
	}
	got := normalize(classes)
	if len(got) != 0 {
		t.Fatalf("descriptive-only input must yield no categories; got %+v", got)
	}
}

func TestNormalize_UnknownClassFallsBackToOther(t *testing.T) {
	// A future/unmapped Hive class must never be dropped: it maps to OTHER with the
	// raw label preserved and its score carried (spec §E fallback discipline).
	classes := []hiveClass{
		{Class: "some_future_class", Score: 0.42},
	}
	got := normalize(classes)
	other := findCat(got, moderation.CategoryOther, "some_future_class")
	if other == nil {
		t.Fatalf("unknown class must map to OTHER; got %+v", got)
	}
	if *other.Score != 0.42 || other.ScoreOrigin != moderation.ScoreOriginProbability {
		t.Errorf("OTHER = %+v, want score 0.42 origin probability", *other)
	}
}

func TestNormalize_DistinctOtherHeadsBothSurvive(t *testing.T) {
	// OTHER is a catch-all for semantically UNRELATED harms (child_safety vs
	// gambling). Unlike WEAPONS (gun vs knife = same concept, max is right), OTHER
	// heads must NOT max-collapse — the lower signal would be silently dropped.
	// Both must emit their own row, each labeled with its own head.
	classes := []hiveClass{
		{Class: "yes_child_safety", Score: 0.40}, {Class: "no_child_safety", Score: 0.60},
		{Class: "yes_gambling", Score: 0.60}, {Class: "no_gambling", Score: 0.40},
	}
	got := normalize(classes)

	child := findCat(got, moderation.CategoryOther, "yes_child_safety")
	if child == nil || *child.Score != 0.40 {
		t.Fatalf("child_safety OTHER row must survive at 0.40; got %+v", got)
	}
	gambling := findCat(got, moderation.CategoryOther, "yes_gambling")
	if gambling == nil || *gambling.Score != 0.60 {
		t.Fatalf("gambling OTHER row must survive at 0.60; got %+v", got)
	}
	// Exactly two OTHER rows — neither folded into the other.
	var others int
	for _, r := range got {
		if r.Category == moderation.CategoryOther {
			others++
		}
	}
	if others != 2 {
		t.Errorf("want 2 distinct OTHER rows, got %d: %+v", others, got)
	}
}

func TestNormalize_OmitsZeroEvidenceCategories(t *testing.T) {
	// Hive returns EVERY class on every call, so a clean image carries dozens of
	// positive classes at score ~0. A zero-evidence category ("not present") must
	// be OMITTED, not emitted as score 0 — non-emitted == absent, per §E
	// worked-example-1 (consumers must never read absence as a 0 harm signal).
	classes := []hiveClass{
		{Class: "general_nsfw", Score: 0.0},
		{Class: "general_suggestive", Score: 0.0},
		{Class: "general_not_nsfw_not_suggestive", Score: 1.0},
		{Class: "gun_in_hand", Score: 0.0}, {Class: "no_gun", Score: 1.0},
		{Class: "some_future_class", Score: 0.0}, // zero-evidence unknown -> also omitted
	}
	got := normalize(classes)
	if len(got) != 0 {
		t.Fatalf("zero-evidence input must yield no categories; got %+v", got)
	}
}

func TestNormalize_ClampsSumToOne(t *testing.T) {
	// Floating-point accumulation of positives must never produce a score > 1.0.
	classes := []hiveClass{
		{Class: "gun_in_hand", Score: 0.6},
		{Class: "gun_not_in_hand", Score: 0.6}, // sums to 1.2 -> clamp to 1.0
	}
	got := normalize(classes)
	w := findCat(got, moderation.CategoryWeapons, "gun_in_hand")
	if w == nil {
		t.Fatalf("missing WEAPONS; got %+v", got)
	}
	if *w.Score != 1.0 {
		t.Errorf("WEAPONS score = %v, want clamp to 1.0", *w.Score)
	}
}
