package result

import (
	"fmt"
	"slices"
	"strings"

	"github.com/vismod/vismod/pkg/moderation"
)

// Predicate decides whether a sink receives an envelope.
//
// It is a CLOSED struct on purpose, not an expression language. Every
// field is enumerable and boot-validatable, so a routing rule that can
// never match is a boot refusal rather than a silent runtime drop. Adding
// a parsed grammar here would reintroduce exactly that failure mode: a
// syntactically valid expression that quietly routes nothing.
//
// Combining rule: OR within a group, AND across groups. An empty group is
// unconstrained, so the zero Predicate matches everything — that is the
// backward-compatible default for a sink with no predicate configured.
//
// FAIL-SAFE WARNING: a Predicate can only ever NARROW what a sink
// receives, so a set of predicates that collectively miss an envelope
// routes it NOWHERE. Nothing in this type can detect that; the guarantee
// has to be enforced by config shape (at least one unconditional sink),
// not by hoping every predicate is written correctly.
type Predicate struct {
	// Verdicts is the set of asset-level verdicts this sink accepts.
	// Empty means any verdict.
	Verdicts []moderation.Verdict `json:"verdicts,omitempty" mapstructure:"verdicts"`
	// Categories maps a canonical category name to a minimum score. A
	// category matches when ANY frame carries a non-nil score for it that
	// is >= the minimum. Empty means no category constraint.
	Categories map[string]float64 `json:"categories,omitempty" mapstructure:"categories"`
	// SourceKinds is the set of accepted moderation.Source kinds
	// ("file", "url"). Empty means any kind.
	SourceKinds []string `json:"source_kinds,omitempty" mapstructure:"source_kinds"`
}

// validSourceKinds mirrors moderation.Source.Kind. "s3" is a documented
// future kind and is deliberately NOT accepted until it exists — an
// unreachable rule is a misconfiguration, not a forward declaration.
var validSourceKinds = map[string]bool{"file": true, "url": true}

// IsEmpty reports whether the predicate constrains nothing, i.e. the sink
// holding it is unconditional. config.validateOutput uses this to enforce
// that at least one sink receives every envelope.
func (p Predicate) IsEmpty() bool {
	return len(p.Verdicts) == 0 && len(p.Categories) == 0 && len(p.SourceKinds) == 0
}

// Normalize canonicalizes the predicate in place and validates it. It is
// the boot-time gate: viper lowercases yaml map keys and the canonical
// category taxonomy is uppercase, so keys are folded up here, and any
// value that cannot be canonicalized is an error rather than a rule that
// silently never fires.
//
// moderation.Canonicalize folds every unrecognized category to OTHER, so
// validating AFTER canonicalizing would accept a typo as OTHER. The check
// therefore runs against the raw key.
func (p *Predicate) Normalize() error {
	for i, v := range p.Verdicts {
		lowered := moderation.Verdict(strings.ToLower(strings.TrimSpace(string(v))))
		switch lowered {
		case moderation.VerdictAllow, moderation.VerdictFlag,
			moderation.VerdictBlock, moderation.VerdictError:
			p.Verdicts[i] = lowered
		default:
			return fmt.Errorf("predicate: unknown verdict %q (want allow, flag, block or error)", v)
		}
	}

	for i, k := range p.SourceKinds {
		lowered := strings.ToLower(strings.TrimSpace(k))
		if !validSourceKinds[lowered] {
			return fmt.Errorf("predicate: unknown source kind %q (want file or url)", k)
		}
		p.SourceKinds[i] = lowered
	}

	if len(p.Categories) > 0 {
		canon := make(map[string]float64, len(p.Categories))
		for k, min := range p.Categories {
			upper := moderation.Category(strings.ToUpper(strings.TrimSpace(k)))
			if moderation.Canonicalize(upper) != upper {
				return fmt.Errorf("predicate: unknown category %q — it would fold to OTHER and never match the category you meant", k)
			}
			if min < 0 || min > 1 {
				return fmt.Errorf("predicate: category %q min score %v is outside [0,1]", k, min)
			}
			if _, dup := canon[string(upper)]; dup {
				return fmt.Errorf("predicate: category %q is listed twice after canonicalization", k)
			}
			canon[string(upper)] = min
		}
		p.Categories = canon
	}
	return nil
}

// Match reports whether env should be delivered to the sink holding this
// predicate. Match never mutates env.
func (p Predicate) Match(env ResultEnvelope) bool {
	if len(p.Verdicts) > 0 && !slices.Contains(p.Verdicts, verdictOf(env)) {
		return false
	}
	if len(p.SourceKinds) > 0 && !slices.Contains(p.SourceKinds, strings.ToLower(env.Source.Kind)) {
		return false
	}
	for cat, min := range p.Categories {
		if !anyFrameScoreAtLeast(env, cat, min) {
			return false
		}
	}
	return true
}

// verdictOf reads the asset-level verdict, treating a missing
// NormalizedResult as "error". An envelope with no result is a job that
// could not be evaluated, and invariant 1 says that is never an allow —
// so it must route as an error, not fall through as an unknown value that
// matches nothing.
func verdictOf(env ResultEnvelope) moderation.Verdict {
	if env.Result == nil {
		return moderation.VerdictError
	}
	return env.Result.Overall.Verdict
}

// anyFrameScoreAtLeast is where null discipline lands at the routing
// boundary: a nil Score means could-not-evaluate, so it can never satisfy
// a minimum — not even a minimum of 0, which would otherwise read an
// unknown as "confidently safe and above the bar".
func anyFrameScoreAtLeast(env ResultEnvelope, cat string, min float64) bool {
	if env.Result == nil {
		return false
	}
	want := moderation.Category(cat)
	for _, f := range env.Result.Frames {
		for _, c := range f.Categories {
			if moderation.Canonicalize(c.Category) != want {
				continue
			}
			if c.Score != nil && *c.Score >= min {
				return true
			}
		}
	}
	return false
}
