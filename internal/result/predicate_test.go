package result

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vismod/vismod/pkg/moderation"
)

// envWith builds a minimal envelope for predicate matching. score of nil
// means could-not-evaluate, which is NOT the same as 0.0 (invariant 2).
func envWith(kind string, v moderation.Verdict, cat moderation.Category, score *float64) ResultEnvelope {
	return ResultEnvelope{
		Source: moderation.Source{Kind: kind, Ref: "x", MediaType: "image"},
		Result: &moderation.NormalizedResult{
			Frames: []moderation.FrameResult{{
				Status: moderation.FrameOK,
				Categories: []moderation.CategoryResult{{
					Category: cat,
					Score:    score,
				}},
			}},
			Overall: moderation.OverallVerdict{Verdict: v},
		},
	}
}

func f64(v float64) *float64 { return &v }

// TestZeroPredicateMatchesEverything pins the backward-compatible default:
// a sink with no predicate configured must keep receiving every envelope,
// so today's fan-out behavior is the zero value.
func TestZeroPredicateMatchesEverything(t *testing.T) {
	var p Predicate
	for _, v := range []moderation.Verdict{
		moderation.VerdictAllow, moderation.VerdictFlag,
		moderation.VerdictBlock, moderation.VerdictError,
	} {
		if !p.Match(envWith("file", v, moderation.CategorySexual, f64(0.1))) {
			t.Errorf("zero predicate must match verdict %q", v)
		}
	}
}

func TestPredicateVerdictsFilter(t *testing.T) {
	p := Predicate{Verdicts: []moderation.Verdict{moderation.VerdictBlock}}
	if !p.Match(envWith("file", moderation.VerdictBlock, moderation.CategorySexual, f64(0.9))) {
		t.Error("block envelope must match a block predicate")
	}
	if p.Match(envWith("file", moderation.VerdictAllow, moderation.CategorySexual, f64(0.1))) {
		t.Error("allow envelope must not match a block-only predicate")
	}
}

// TestPredicateVerdictsAreORedWithinTheGroup pins the combining rule:
// OR within one group, so [block, error] matches either.
func TestPredicateVerdictsAreORedWithinTheGroup(t *testing.T) {
	p := Predicate{Verdicts: []moderation.Verdict{moderation.VerdictBlock, moderation.VerdictError}}
	if !p.Match(envWith("file", moderation.VerdictError, moderation.CategorySexual, nil)) {
		t.Error("error envelope must match a [block, error] predicate")
	}
	if p.Match(envWith("file", moderation.VerdictFlag, moderation.CategorySexual, f64(0.5))) {
		t.Error("flag envelope must not match a [block, error] predicate")
	}
}

// TestNilResultIsTreatedAsErrorVerdict is the fail-safe read of a
// result-less envelope. A job that never produced a NormalizedResult
// carries Error instead; routing it as anything other than "error" (or
// dropping it) would let a failed scan miss the destination watching for
// failures.
func TestNilResultIsTreatedAsErrorVerdict(t *testing.T) {
	env := ResultEnvelope{
		Source: moderation.Source{Kind: "url", MediaType: "image"},
		Error:  "provider timeout",
	}
	if !(Predicate{Verdicts: []moderation.Verdict{moderation.VerdictError}}).Match(env) {
		t.Error("an envelope with a nil Result must match verdict error")
	}
	if (Predicate{Verdicts: []moderation.Verdict{moderation.VerdictAllow}}).Match(env) {
		t.Error("an envelope with a nil Result must never match verdict allow")
	}
}

func TestPredicateCategoryMinScore(t *testing.T) {
	p := Predicate{Categories: map[string]float64{"SEXUAL": 0.8}}
	if !p.Match(envWith("file", moderation.VerdictFlag, moderation.CategorySexual, f64(0.8))) {
		t.Error("score at the threshold must match (>=, not >)")
	}
	if p.Match(envWith("file", moderation.VerdictFlag, moderation.CategorySexual, f64(0.79))) {
		t.Error("score below the threshold must not match")
	}
	if p.Match(envWith("file", moderation.VerdictFlag, moderation.CategoryViolence, f64(0.99))) {
		t.Error("a different category must not satisfy a SEXUAL constraint")
	}
}

// TestNilScoreNeverSatisfiesAMinScore is invariant 2 at the routing
// boundary: a nil score means could-not-evaluate. Reading it as 0 (never
// matches) or as "unknown so allow through" both silently misroute; the
// rule is that only a real score can satisfy a real threshold, and a
// min_score of 0 must not turn a null into a match.
func TestNilScoreNeverSatisfiesAMinScore(t *testing.T) {
	for _, min := range []float64{0, 0.5} {
		p := Predicate{Categories: map[string]float64{"SEXUAL": min}}
		if p.Match(envWith("file", moderation.VerdictError, moderation.CategorySexual, nil)) {
			t.Errorf("nil score must not satisfy min_score %v", min)
		}
	}
}

// TestPredicateGroupsAreANDed pins the combining rule across groups: a
// verdict constraint and a category constraint must BOTH hold.
func TestPredicateGroupsAreANDed(t *testing.T) {
	p := Predicate{
		Verdicts:   []moderation.Verdict{moderation.VerdictBlock},
		Categories: map[string]float64{"SEXUAL": 0.9},
	}
	if !p.Match(envWith("file", moderation.VerdictBlock, moderation.CategorySexual, f64(0.95))) {
		t.Error("both groups satisfied must match")
	}
	if p.Match(envWith("file", moderation.VerdictBlock, moderation.CategorySexual, f64(0.1))) {
		t.Error("verdict satisfied but category not must NOT match")
	}
	if p.Match(envWith("file", moderation.VerdictFlag, moderation.CategorySexual, f64(0.95))) {
		t.Error("category satisfied but verdict not must NOT match")
	}
}

func TestPredicateSourceKinds(t *testing.T) {
	p := Predicate{SourceKinds: []string{"url"}}
	if !p.Match(envWith("url", moderation.VerdictAllow, moderation.CategorySexual, f64(0.1))) {
		t.Error("url source must match a url predicate")
	}
	if p.Match(envWith("file", moderation.VerdictAllow, moderation.CategorySexual, f64(0.1))) {
		t.Error("file source must not match a url-only predicate")
	}
}

// TestPredicateCategoryMatchesAnyFrame: for video, one bad frame is
// enough. Rollup already uses strict precedence; routing must not be
// narrower than the verdict it routes.
func TestPredicateCategoryMatchesAnyFrame(t *testing.T) {
	env := ResultEnvelope{
		Source: moderation.Source{Kind: "file", MediaType: "video"},
		Result: &moderation.NormalizedResult{
			Frames: []moderation.FrameResult{
				{Status: moderation.FrameOK, Categories: []moderation.CategoryResult{{Category: moderation.CategorySexual, Score: f64(0.1)}}},
				{Status: moderation.FrameOK, Categories: []moderation.CategoryResult{{Category: moderation.CategorySexual, Score: f64(0.95)}}},
			},
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictBlock},
		},
	}
	if !(Predicate{Categories: map[string]float64{"SEXUAL": 0.9}}).Match(env) {
		t.Error("a single frame over the threshold must satisfy the constraint")
	}
}

// TestPredicateNormalizeUppercasesViperLoweredKeys covers the documented
// viper gotcha: yaml map keys arrive lowercased, and the canonical
// category taxonomy is uppercase.
func TestPredicateNormalizeUppercasesViperLoweredKeys(t *testing.T) {
	p := Predicate{Categories: map[string]float64{"sexual": 0.8}}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Categories["SEXUAL"]; !ok {
		t.Fatalf("want canonical key SEXUAL, got %v", p.Categories)
	}
	if !p.Match(envWith("file", moderation.VerdictFlag, moderation.CategorySexual, f64(0.9))) {
		t.Error("normalized predicate must match the canonical category")
	}
}

// TestPredicateValidateRejectsUnknownCategory is the dead-map-key
// defense. Canonicalize folds anything unknown to OTHER, so a typo would
// otherwise become a silently-never-matching rule — the exact failure
// mode that produced five phantom hive class names.
func TestPredicateValidateRejectsUnknownCategory(t *testing.T) {
	p := Predicate{Categories: map[string]float64{"sexuall": 0.8}}
	err := p.Normalize()
	if err == nil {
		t.Fatal("want a boot refusal for an unknown category name, got nil")
	}
	if !strings.Contains(err.Error(), "sexuall") {
		t.Errorf("error must name the offending key, got %q", err)
	}
}

func TestPredicateValidateRejectsBadValues(t *testing.T) {
	cases := map[string]Predicate{
		"unknown verdict":     {Verdicts: []moderation.Verdict{"blocked"}},
		"unknown source kind": {SourceKinds: []string{"s3"}},
		"score above 1":       {Categories: map[string]float64{"sexual": 1.5}},
		"score below 0":       {Categories: map[string]float64{"sexual": -0.1}},
	}
	for name, p := range cases {
		if err := p.Normalize(); err == nil {
			t.Errorf("%s: want a boot refusal, got nil", name)
		}
	}
}

// TestPredicateJSONRoundTrip: the predicate is config, and config is
// hashed into ConfigHash and echoed by tooling, so it must survive a
// JSON round trip unchanged and omit empty groups entirely.
func TestPredicateJSONRoundTrip(t *testing.T) {
	if b, err := json.Marshal(Predicate{}); err != nil || string(b) != "{}" {
		t.Fatalf("empty predicate must marshal to {}, got %q err %v", b, err)
	}
	in := Predicate{
		Verdicts:    []moderation.Verdict{moderation.VerdictBlock},
		Categories:  map[string]float64{"SEXUAL": 0.8},
		SourceKinds: []string{"url"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Predicate
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Verdicts) != 1 || out.Verdicts[0] != moderation.VerdictBlock ||
		out.Categories["SEXUAL"] != 0.8 || len(out.SourceKinds) != 1 || out.SourceKinds[0] != "url" {
		t.Errorf("round trip changed the predicate: %+v", out)
	}
}
