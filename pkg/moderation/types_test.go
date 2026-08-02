package moderation

import (
	"encoding/json"
	"testing"
)

// TestCanonicalizeKeepsEveryCanonicalValue: Canonicalize is the fold every
// consumer runs on an incoming category. If it ever dropped a canonical
// value to OTHER the provider label would survive but the category signal
// would be lost, silently, for that whole category.
func TestCanonicalizeKeepsEveryCanonicalValue(t *testing.T) {
	canonical := []Category{
		CategorySexual, CategorySuggestiveRacy, CategoryViolence,
		CategoryGoreGraphic, CategoryWeapons, CategorySelfHarm,
		CategoryHate, CategoryDrugs, CategoryAlcoholTobacco,
		CategoryGambling, CategoryOffensiveGesture, CategoryMedical,
		CategorySpoof, CategoryAnimatedSynthetic, CategoryOther,
	}
	for _, c := range canonical {
		if got := Canonicalize(c); got != c {
			t.Errorf("Canonicalize(%q) = %q, want unchanged", c, got)
		}
	}
}

// TestCanonicalizeFoldsUnknownToOther pins the forward-compatibility rule
// stated on Category: consumers must tolerate unknown future values by
// treating them as OTHER, never by erroring or passing them through.
func TestCanonicalizeFoldsUnknownToOther(t *testing.T) {
	for _, in := range []Category{
		"", "sexual", "SEXUAL_V2", "FUTURE_CATEGORY", "other",
	} {
		if got := Canonicalize(in); got != CategoryOther {
			t.Errorf("Canonicalize(%q) = %q, want %q", in, got, CategoryOther)
		}
	}
}

// TestNullableFieldsSerializeAsNull pins invariant 2 (null discipline): a
// nil score/max_score/confidence must serialize as JSON null, never be
// omitted and never collapse to 0. A consumer that sees a missing key
// cannot distinguish "unknown" from "confidently safe".
func TestNullableFieldsSerializeAsNull(t *testing.T) {
	res := NormalizedResult{
		SchemaVersion: SchemaVersion,
		Frames: []FrameResult{{
			Status:     FrameOK,
			Categories: []CategoryResult{{Category: CategorySexual}},
		}},
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	overall, ok := decoded["overall"].(map[string]any)
	if !ok {
		t.Fatalf("overall missing from %s", b)
	}
	for _, key := range []string{"max_score", "confidence", "top_category"} {
		v, present := overall[key]
		if !present {
			t.Errorf("overall.%s omitted; nullable fields must serialize as null: %s", key, b)
			continue
		}
		if v != nil {
			t.Errorf("overall.%s = %v, want null", key, v)
		}
	}

	frame := decoded["frames"].([]any)[0].(map[string]any)
	if v, present := frame["timestamp_sec"]; !present || v != nil {
		t.Errorf("frames[0].timestamp_sec = %v (present=%v), want null", v, present)
	}
	cat := frame["categories"].([]any)[0].(map[string]any)
	for _, key := range []string{"score", "threshold"} {
		v, present := cat[key]
		if !present || v != nil {
			t.Errorf("categories[0].%s = %v (present=%v), want null", key, v, present)
		}
	}
}

// TestRawIsOmittedWhenAbsent: Raw is the one optional field. It must not
// serialize as a null key, because an operator grepping envelopes for
// "raw" should only ever hit envelopes that actually carry one.
func TestRawIsOmittedWhenAbsent(t *testing.T) {
	b, err := json.Marshal(NormalizedResult{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["raw"]; present {
		t.Errorf("raw present on a result that has none: %s", b)
	}
}
