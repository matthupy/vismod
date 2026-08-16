package result

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/vismod/vismod/pkg/moderation"
)

// TestPredicateIsEmpty pins the boot-time fail-safe hook. config.
// validateOutput and cli.buildSinks use IsEmpty to prove at least one sink
// is unconditional, so a predicate that constrains ANY group must report
// false — a false "empty" would let a fully-predicated sink set pass boot
// and route some envelopes nowhere.
func TestPredicateIsEmpty(t *testing.T) {
	cases := []struct {
		name string
		p    Predicate
		want bool
	}{
		{"zero value", Predicate{}, true},
		{"explicitly nil groups", Predicate{Verdicts: nil, Categories: nil, SourceKinds: nil}, true},
		{"allocated but empty groups", Predicate{
			Verdicts:    []moderation.Verdict{},
			Categories:  map[string]float64{},
			SourceKinds: []string{},
		}, true},
		{"verdicts only", Predicate{Verdicts: []moderation.Verdict{moderation.VerdictBlock}}, false},
		{"categories only", Predicate{Categories: map[string]float64{"SEXUAL": 0.8}}, false},
		{"source kinds only", Predicate{SourceKinds: []string{"url"}}, false},
		{"every group set", Predicate{
			Verdicts:    []moderation.Verdict{moderation.VerdictBlock},
			Categories:  map[string]float64{"SEXUAL": 0.8},
			SourceKinds: []string{"url"},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.IsEmpty(); got != tc.want {
				t.Errorf("IsEmpty() = %v, want %v for %+v", got, tc.want, tc.p)
			}
		})
	}
}

// TestPredicateNormalizeCanonicalizesValidValues covers the accept side of
// every Normalize branch: verdicts and source kinds fold DOWN, category
// keys fold UP (the viper gotcha), and surrounding whitespace never
// produces a rule that silently never fires.
func TestPredicateNormalizeCanonicalizesValidValues(t *testing.T) {
	cases := []struct {
		name string
		in   Predicate
		want Predicate
	}{
		{
			"nothing to canonicalize",
			Predicate{},
			Predicate{},
		},
		{
			"every verdict, mixed case and padded",
			Predicate{Verdicts: []moderation.Verdict{"ALLOW", "Flag", " block ", "\tERROR\n"}},
			Predicate{Verdicts: []moderation.Verdict{
				moderation.VerdictAllow, moderation.VerdictFlag,
				moderation.VerdictBlock, moderation.VerdictError,
			}},
		},
		{
			"already-canonical verdicts survive unchanged",
			Predicate{Verdicts: []moderation.Verdict{moderation.VerdictBlock}},
			Predicate{Verdicts: []moderation.Verdict{moderation.VerdictBlock}},
		},
		{
			"both source kinds, mixed case and padded",
			Predicate{SourceKinds: []string{"FILE", " Url "}},
			Predicate{SourceKinds: []string{"file", "url"}},
		},
		{
			"category key folds up and keeps its minimum",
			Predicate{Categories: map[string]float64{" suggestive_racy ": 0.25}},
			Predicate{Categories: map[string]float64{"SUGGESTIVE_RACY": 0.25}},
		},
		{
			"minimum of 0 is a legal bound",
			Predicate{Categories: map[string]float64{"violence": 0}},
			Predicate{Categories: map[string]float64{"VIOLENCE": 0}},
		},
		{
			"minimum of 1 is a legal bound",
			Predicate{Categories: map[string]float64{"violence": 1}},
			Predicate{Categories: map[string]float64{"VIOLENCE": 1}},
		},
		{
			"all three groups at once",
			Predicate{
				Verdicts:    []moderation.Verdict{"BLOCK"},
				Categories:  map[string]float64{"gore_graphic": 0.5},
				SourceKinds: []string{"URL"},
			},
			Predicate{
				Verdicts:    []moderation.Verdict{moderation.VerdictBlock},
				Categories:  map[string]float64{"GORE_GRAPHIC": 0.5},
				SourceKinds: []string{"url"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.in
			if err := p.Normalize(); err != nil {
				t.Fatalf("Normalize() = %v, want nil", err)
			}
			if !reflect.DeepEqual(p, tc.want) {
				t.Errorf("Normalize() produced %+v, want %+v", p, tc.want)
			}
			// Normalizing an already-normalized predicate must be a
			// no-op: config can be reloaded, and a second pass that
			// changed the rule (or tripped the duplicate check) would
			// make routing depend on how many times boot ran.
			if err := p.Normalize(); err != nil {
				t.Fatalf("second Normalize() = %v, want nil", err)
			}
			if !reflect.DeepEqual(p, tc.want) {
				t.Errorf("Normalize() is not idempotent: %+v, want %+v", p, tc.want)
			}
		})
	}
}

// TestPredicateNormalizeRejectsDuplicateCategoryAfterCanonicalization is
// the last-writer-wins defense. Two yaml keys that fold to the same
// canonical category would collapse into one entry whose minimum depends
// on Go's randomized map iteration order — a routing rule that differs
// between boots of identical config. It has to be a boot refusal.
//
// The error names one of the two colliding keys, and which one is
// genuinely nondeterministic, so only the stable part of the message is
// asserted here.
func TestPredicateNormalizeRejectsDuplicateCategoryAfterCanonicalization(t *testing.T) {
	cases := map[string]Predicate{
		"case variants":       {Categories: map[string]float64{"sexual": 0.2, "SEXUAL": 0.9}},
		"whitespace variants": {Categories: map[string]float64{"violence": 0.2, " violence ": 0.9}},
		"mixed case variant":  {Categories: map[string]float64{"Hate": 0.2, "hate": 0.9}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			err := p.Normalize()
			if err == nil {
				t.Fatalf("want a boot refusal for colliding category keys, got nil (%+v)", p.Categories)
			}
			if !strings.Contains(err.Error(), "listed twice") {
				t.Errorf("error must explain the collision, got %q", err)
			}
			if len(p.Categories) != 2 {
				t.Errorf("a refused predicate must not be partially canonicalized, got %+v", p.Categories)
			}
		})
	}
}

// TestPredicateNormalizeRejectsUnroutableValues covers the reject side of
// every Normalize branch, including the empty-string inputs a yaml list
// item can produce. Each of these would otherwise become a rule that is
// syntactically fine and never fires, which is the exact failure mode the
// closed struct exists to make impossible.
func TestPredicateNormalizeRejectsUnroutableValues(t *testing.T) {
	cases := []struct {
		name string
		p    Predicate
		want string
	}{
		{"empty verdict", Predicate{Verdicts: []moderation.Verdict{""}}, "unknown verdict"},
		{"whitespace-only verdict", Predicate{Verdicts: []moderation.Verdict{"   "}}, "unknown verdict"},
		{"verdict typo", Predicate{Verdicts: []moderation.Verdict{"allowed"}}, "unknown verdict"},
		{"deny is not a verdict", Predicate{Verdicts: []moderation.Verdict{"deny"}}, "unknown verdict"},
		{"one bad verdict among good ones", Predicate{Verdicts: []moderation.Verdict{
			moderation.VerdictBlock, "warn", moderation.VerdictError,
		}}, "unknown verdict"},
		{"empty source kind", Predicate{SourceKinds: []string{""}}, "unknown source kind"},
		{"whitespace-only source kind", Predicate{SourceKinds: []string{" "}}, "unknown source kind"},
		{"documented-but-unimplemented s3 kind", Predicate{SourceKinds: []string{"S3"}}, "unknown source kind"},
		{"one bad source kind among good ones", Predicate{SourceKinds: []string{"file", "gcs"}}, "unknown source kind"},
		{"empty category key", Predicate{Categories: map[string]float64{"": 0.5}}, "unknown category"},
		{"whitespace-only category key", Predicate{Categories: map[string]float64{"  ": 0.5}}, "unknown category"},
		{"category typo", Predicate{Categories: map[string]float64{"sexual_content": 0.5}}, "unknown category"},
		{"score just above 1", Predicate{Categories: map[string]float64{"sexual": 1.0000001}}, "outside [0,1]"},
		{"score far above 1", Predicate{Categories: map[string]float64{"sexual": 100}}, "outside [0,1]"},
		{"score just below 0", Predicate{Categories: map[string]float64{"sexual": -0.0000001}}, "outside [0,1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Normalize()
			if err == nil {
				t.Fatalf("want a boot refusal, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must contain %q", err, tc.want)
			}
		})
	}
}

// TestPredicateCategoryConstraintNeverMatchesWithoutAScore walks every way
// an envelope can fail to carry an evaluable score for the constrained
// category — no result at all, no frames, no categories, the wrong
// category, a null score, a score under the bar. Every one must be a MISS.
// Routing is narrow-only, so the dangerous direction here is a match: it
// would put unscorable content into a destination an operator configured
// for confirmed hits.
func TestPredicateCategoryConstraintNeverMatchesWithoutAScore(t *testing.T) {
	sexualAtHalf := Predicate{Categories: map[string]float64{"SEXUAL": 0.5}}
	cases := []struct {
		name string
		p    Predicate
		env  ResultEnvelope
	}{
		{
			"nil result",
			sexualAtHalf,
			ResultEnvelope{Source: moderation.Source{Kind: "file"}, Error: "provider timeout"},
		},
		{
			// min 0 is the trap: a nil Result is could-not-evaluate, not
			// "0.0 and therefore over a bar of 0".
			"nil result against a minimum of 0",
			Predicate{Categories: map[string]float64{"SEXUAL": 0}},
			ResultEnvelope{Source: moderation.Source{Kind: "file"}, Error: "provider timeout"},
		},
		{
			"zero envelope",
			sexualAtHalf,
			ResultEnvelope{},
		},
		{
			"result with no frames",
			sexualAtHalf,
			ResultEnvelope{
				Source: moderation.Source{Kind: "file"},
				Result: &moderation.NormalizedResult{
					Overall: moderation.OverallVerdict{Verdict: moderation.VerdictError},
				},
			},
		},
		{
			"frame with no categories",
			sexualAtHalf,
			ResultEnvelope{
				Source: moderation.Source{Kind: "file"},
				Result: &moderation.NormalizedResult{
					Frames:  []moderation.FrameResult{{Status: moderation.FrameError, Error: "unscorable"}},
					Overall: moderation.OverallVerdict{Verdict: moderation.VerdictError},
				},
			},
		},
		{
			"only a different category, scored high",
			sexualAtHalf,
			envWith("file", moderation.VerdictBlock, moderation.CategoryViolence, f64(0.99)),
		},
		{
			"the right category with a null score",
			sexualAtHalf,
			envWith("file", moderation.VerdictError, moderation.CategorySexual, nil),
		},
		{
			"the right category just under the bar",
			sexualAtHalf,
			envWith("file", moderation.VerdictFlag, moderation.CategorySexual, f64(0.4999999)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.p.Match(tc.env) {
				t.Error("an envelope with no qualifying score must not match a category constraint")
			}
		})
	}
}

// TestPredicateMatchAcrossEveryGroup pins AND-across-groups by failing one
// group at a time while the other two hold. Each row is the only reason
// that row misses.
func TestPredicateMatchAcrossEveryGroup(t *testing.T) {
	p := Predicate{
		Verdicts:    []moderation.Verdict{moderation.VerdictBlock},
		Categories:  map[string]float64{"SEXUAL": 0.8},
		SourceKinds: []string{"file"},
	}
	cases := []struct {
		name string
		env  ResultEnvelope
		want bool
	}{
		{"all three groups satisfied", envWith("file", moderation.VerdictBlock, moderation.CategorySexual, f64(0.9)), true},
		{"verdict group fails", envWith("file", moderation.VerdictFlag, moderation.CategorySexual, f64(0.9)), false},
		{"source kind group fails", envWith("url", moderation.VerdictBlock, moderation.CategorySexual, f64(0.9)), false},
		{"category group fails on score", envWith("file", moderation.VerdictBlock, moderation.CategorySexual, f64(0.7)), false},
		{"category group fails on null score", envWith("file", moderation.VerdictBlock, moderation.CategorySexual, nil), false},
		{"category group fails on wrong category", envWith("file", moderation.VerdictBlock, moderation.CategoryHate, f64(0.99)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Match(tc.env); got != tc.want {
				t.Errorf("Match() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPredicateEveryCategoryConstraintMustHold: categories are a group of
// their own but each key is an AND, not an OR. A sink asking for sexual
// AND violence must not fire on sexual alone.
func TestPredicateEveryCategoryConstraintMustHold(t *testing.T) {
	p := Predicate{Categories: map[string]float64{"SEXUAL": 0.5, "VIOLENCE": 0.5}}
	both := ResultEnvelope{
		Source: moderation.Source{Kind: "file", MediaType: "video"},
		Result: &moderation.NormalizedResult{
			Frames: []moderation.FrameResult{
				{Status: moderation.FrameOK, Categories: []moderation.CategoryResult{
					{Category: moderation.CategorySexual, Score: f64(0.9)},
				}},
				{Status: moderation.FrameOK, Categories: []moderation.CategoryResult{
					{Category: moderation.CategoryViolence, Score: f64(0.6)},
					{Category: moderation.CategorySelfHarm, Score: nil},
				}},
			},
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictBlock},
		},
	}
	if !p.Match(both) {
		t.Error("both category constraints satisfied (across different frames) must match")
	}
	if p.Match(envWith("file", moderation.VerdictBlock, moderation.CategorySexual, f64(0.9))) {
		t.Error("only one of two category constraints satisfied must NOT match")
	}
}

// TestPredicateSourceKindComparisonIsCaseInsensitive: the predicate side is
// lowercased by Normalize, the envelope side by Match. An unexpected kind
// must miss rather than fall through.
func TestPredicateSourceKindComparisonIsCaseInsensitive(t *testing.T) {
	p := Predicate{SourceKinds: []string{"url"}}
	if !p.Match(envWith("URL", moderation.VerdictAllow, moderation.CategorySexual, f64(0.1))) {
		t.Error("an upper-case envelope kind must still match a url predicate")
	}
	for _, kind := range []string{"", "file", "s3", "u r l"} {
		if p.Match(envWith(kind, moderation.VerdictAllow, moderation.CategorySexual, f64(0.1))) {
			t.Errorf("source kind %q must not match a url-only predicate", kind)
		}
	}
}

// TestPredicateMatchesFrameCategoriesThroughCanonicalization: adapters
// carry an unmapped provider label as OTHER, so a predicate on OTHER is
// how an operator routes "a signal we do not have a canonical name for".
// It must not leak into the named categories, and they must not absorb it.
func TestPredicateMatchesFrameCategoriesThroughCanonicalization(t *testing.T) {
	env := envWith("file", moderation.VerdictFlag, moderation.Category("SOME_FUTURE_LABEL"), f64(0.9))

	other := Predicate{Categories: map[string]float64{"other": 0.5}}
	if err := other.Normalize(); err != nil {
		t.Fatalf("OTHER must be a routable category: %v", err)
	}
	if !other.Match(env) {
		t.Error("an unmapped provider label folds to OTHER and must satisfy an OTHER constraint")
	}
	if (Predicate{Categories: map[string]float64{"SEXUAL": 0.5}}).Match(env) {
		t.Error("an unmapped label must not satisfy a named category constraint")
	}
}

// TestPredicateNormalizeThenMatch is the end-to-end boot path: yaml-shaped
// input (viper-lowercased keys, operator-cased values) through Normalize
// and into a routing decision.
func TestPredicateNormalizeThenMatch(t *testing.T) {
	p := Predicate{
		Verdicts:    []moderation.Verdict{"BLOCK", "Error"},
		Categories:  map[string]float64{"sexual": 0.8},
		SourceKinds: []string{"URL"},
	}
	if err := p.Normalize(); err != nil {
		t.Fatalf("Normalize() = %v, want nil", err)
	}
	if !p.Match(envWith("url", moderation.VerdictBlock, moderation.CategorySexual, f64(0.85))) {
		t.Error("the normalized predicate must match the envelope it was written for")
	}
	if p.Match(envWith("file", moderation.VerdictBlock, moderation.CategorySexual, f64(0.85))) {
		t.Error("a file source must still miss a url-only predicate after Normalize")
	}
	if p.Match(envWith("url", moderation.VerdictFlag, moderation.CategorySexual, f64(0.85))) {
		t.Error("a flag verdict must still miss a [block, error] predicate after Normalize")
	}
}

// TestVerdictOfNeverInventsAVerdict: an envelope whose result carries an
// empty verdict is not an allow and not a wildcard. It matches no
// configurable verdict, because "" is a boot refusal in Normalize.
func TestVerdictOfNeverInventsAVerdict(t *testing.T) {
	env := envWith("file", moderation.Verdict(""), moderation.CategorySexual, f64(0.1))
	for _, v := range []moderation.Verdict{
		moderation.VerdictAllow, moderation.VerdictFlag,
		moderation.VerdictBlock, moderation.VerdictError,
	} {
		if (Predicate{Verdicts: []moderation.Verdict{v}}).Match(env) {
			t.Errorf("an empty verdict must not match a %q predicate", v)
		}
	}
	if !(Predicate{}).Match(env) {
		t.Error("an unconditional sink must still receive it — that is the fail-safe path")
	}
}

// TestZeroPredicateMatchesTheZeroEnvelope is the nil/empty input case for
// the unconditional sink: the emptiest possible envelope still routes, and
// it reads as error rather than allow.
func TestZeroPredicateMatchesTheZeroEnvelope(t *testing.T) {
	var env ResultEnvelope
	if !(Predicate{}).Match(env) {
		t.Fatal("the zero predicate must match the zero envelope, or an unconditional sink could drop one")
	}
	if !(Predicate{Verdicts: []moderation.Verdict{moderation.VerdictError}}).Match(env) {
		t.Error("a zero envelope has no result, so it must route as error")
	}
	if (Predicate{Verdicts: []moderation.Verdict{moderation.VerdictAllow}}).Match(env) {
		t.Error("a zero envelope must never route as allow")
	}
	if (Predicate{SourceKinds: []string{"file", "url"}}).Match(env) {
		t.Error("a zero envelope has no source kind, so a kind-constrained sink must miss it")
	}
}

// TestPredicateNaNMinimumNarrowsRatherThanWidens documents a value
// Normalize does NOT reject: NaN fails both `min < 0` and `min > 1`, so a
// NaN minimum boots. What matters is the direction it fails in — every
// comparison against NaN is false, so the rule matches nothing and the
// sink receives nothing. A predicate can only narrow, and a degenerate
// one must narrow to zero rather than widen to everything.
func TestPredicateNaNMinimumNarrowsRatherThanWidens(t *testing.T) {
	p := Predicate{Categories: map[string]float64{"sexual": math.NaN()}}
	if err := p.Normalize(); err != nil {
		t.Skipf("Normalize now rejects NaN (%v); the fail-safe direction is moot", err)
	}
	for _, score := range []*float64{nil, f64(0), f64(0.5), f64(1)} {
		if p.Match(envWith("file", moderation.VerdictBlock, moderation.CategorySexual, score)) {
			t.Error("a NaN minimum must match nothing, never everything")
		}
	}
}

// TestPredicateMatchMutatesNothing: Match is documented as read-only over
// the envelope, and the envelope is a POINTER-carrying struct shared with
// every other sink in the fan-out. A mutation here would corrupt what a
// later sink writes.
func TestPredicateMatchMutatesNothing(t *testing.T) {
	build := func() ResultEnvelope {
		return envWith("URL", moderation.VerdictBlock, moderation.CategorySexual, f64(0.9))
	}
	env, wantEnv := build(), build()
	buildPredicate := func() Predicate {
		return Predicate{
			Verdicts:    []moderation.Verdict{moderation.VerdictBlock},
			Categories:  map[string]float64{"SEXUAL": 0.8},
			SourceKinds: []string{"url"},
		}
	}
	p, wantPredicate := buildPredicate(), buildPredicate()

	if !p.Match(env) {
		t.Fatal("setup: this envelope is supposed to match")
	}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Errorf("Match mutated the envelope: %+v, want %+v", env, wantEnv)
	}
	if !reflect.DeepEqual(p, wantPredicate) {
		t.Errorf("Match mutated the predicate: %+v, want %+v", p, wantPredicate)
	}
}
