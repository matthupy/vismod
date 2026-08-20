// Package corpus loads and validates the evaluation corpus manifest the
// eval harness measures a configured vismod against.
//
// The repo ships the schema, the validator and the digest rule — never a
// corpus. This is CSAM-adjacent trust & safety tooling, so committed media
// (or a committed index of media) is a liability that would outlive any
// benefit. The operator brings the corpus and points at it.
//
// Two shapes reach one code path. A v1 `corpus.yaml` manifest is the full
// format. A flat `ref,expected_flagged` CSV is an on-ramp that DESUGARS
// into a v1 manifest (see csv.go): the generated manifest is materialized
// next to the run output and loaded through this same loader, so there is
// one loader, one validator and one digest rule. A digest taken over the
// CSV bytes instead would give two different files a claim on one corpus
// identity, and a run record that named it could not be reproduced.
//
// Two absences are load-bearing and are modelled as absences, never as
// values:
//
//   - A category the manifest does not mention is NOT asserted. It is not
//     "asserted as allow". Collapsing the two inflates every later
//     precision/recall denominator with categories the operator never
//     labelled, and the harness reports recall it never earned. Hence
//     Expectation.Category returns an explicit ok, and the map behind it is
//     unexported so no caller can index a zero value out of it.
//   - A case with no expectation at all (an empty expected_flagged cell) is
//     observe-only: scanned and reported, never scored.
package corpus

import (
	"sort"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/pkg/moderation"
)

// SchemaVersion is the only manifest version this loader accepts. An
// unknown version is a load error rather than a best-effort parse: a
// corpus that half-loaded would score against expectations nobody wrote.
const SchemaVersion = 1

// Kind is a case's source kind. The values match queue.Job.Source.Kind.
type Kind string

const (
	KindFile Kind = "file"
	KindURL  Kind = "url"
)

// The media types intake understands. A case is one or the other.
const (
	MediaTypeImage = "image"
	MediaTypeVideo = "video"
)

// Provenance records where a case's expected label came from.
type Provenance string

const (
	ProvenanceHuman  Provenance = "human"
	ProvenanceVendor Provenance = "vendor"
	// ProvenanceDerived is a label produced by a previous vismod run. Such
	// a case loads and is scanned, but is never scored: grading a pipeline
	// against its own past output reports 100% F1 forever.
	ProvenanceDerived Provenance = "derived"
)

// Grade is the outcome of comparing one case's expectation to the verdict
// a run actually produced. It is a per-case classification, not a metric;
// the scorer aggregates these.
type Grade string

const (
	// GradeNotScored means the case asserts nothing gradeable — derived
	// provenance, or no expectation at all.
	GradeNotScored Grade = "not_scored"
	// GradeAbstained means the pipeline returned "error". There is no
	// decision to grade, so the case leaves the precision/recall
	// denominators and lands in the gated abstention rate. It is never
	// folded into a match, or a totally dead pipeline scores perfectly on
	// the cases it happened to fail at.
	GradeAbstained Grade = "abstained"
	GradeMatch     Grade = "match"
	GradeMismatch  Grade = "mismatch"
)

// Options are the operator-configuration facts a manifest is validated
// against.
type Options struct {
	// DedupCeiling is frames.dedup.hamming_threshold: the largest
	// per-case dedup_threshold a manifest may ask for. A case may TIGHTEN
	// dedup or turn it off (-1), never loosen it. Every pair of 64-bit
	// dHashes is within distance 64, so a loosened threshold collapses a
	// video into its first frame and lets that one frame decide the
	// verdict — a fail-open.
	//
	// The zero value permits only 0 and -1, which is the strict reading of
	// "no ceiling was supplied": a forgetful caller gets refusals, never a
	// silently widened corpus.
	DedupCeiling int
}

// OptionsFromConfig takes the manifest's validation bounds from the
// operator configuration the corpus will actually run under, so the
// loader and the pipeline enforce one number.
func OptionsFromConfig(cfg config.Config) Options {
	return Options{DedupCeiling: cfg.Frames.Dedup.HammingThreshold}
}

// Source names the media a case points at. Ref for a file case is resolved
// against the manifest's own directory at load; for a url case it is the
// url as written.
type Source struct {
	Kind      Kind
	Ref       string
	MediaType string
}

// Expectation is what the operator asserts about one asset.
//
// Verdict and Flagged are mutually exclusive and both nullable: nil means
// "not asserted", never a default. Flagged is the shorthand and maps onto
// moderation.OverallVerdict.Flagged — true means the verdict is flag or
// block, false means allow. It deliberately cannot express "error", and
// it cannot separate flag from block, so it asserts the flag boundary
// only. It is a narrower instrument than Verdict, not a looser one.
type Expectation struct {
	Verdict *moderation.Verdict
	Flagged *bool

	// categories is unexported on purpose. A map[Category]Verdict handed
	// out directly invites m[VIOLENCE] on a category nobody labelled,
	// which reads as an assertion that was never made. Callers go through
	// Category, which reports absence explicitly.
	categories map[moderation.Category]moderation.Verdict
}

// Category reports the verdict expected for cat, and whether the manifest
// asserted on cat at all. An unasserted category is not "allow".
func (e Expectation) Category(cat moderation.Category) (moderation.Verdict, bool) {
	v, ok := e.categories[cat]
	return v, ok
}

// AssertedCategories are the categories this case asserts on, sorted.
func (e Expectation) AssertedCategories() []moderation.Category {
	out := make([]moderation.Category, 0, len(e.categories))
	for cat := range e.categories {
		out = append(out, cat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Asserted reports whether this case asserts anything at all. A case that
// asserts nothing is observe-only: it is scanned and reported, never
// scored.
func (e Expectation) Asserted() bool {
	return e.Verdict != nil || e.Flagged != nil || len(e.categories) > 0
}

// Case is one asset in the corpus and what is expected of it.
type Case struct {
	ID              string
	Source          Source
	Expect          Expectation
	LabelProvenance Provenance
	// Workflows optionally selects the ffmpeg extraction workflows for a
	// video case; nil inherits the configured default.
	Workflows []string
	// DedupThreshold optionally overrides frames.dedup for this case: nil
	// inherits, -1 disables, 0..DedupCeiling tightens.
	DedupThreshold *int
	Notes          string
}

// Scoreable reports whether this case may enter any metric. A derived
// label cannot grade the pipeline that produced it, and a case that
// asserts nothing has nothing to grade.
func (c Case) Scoreable() bool {
	return c.LabelProvenance != ProvenanceDerived && c.Expect.Asserted()
}

// AssertsFlagBoundary reports whether this case belongs in the flag
// boundary's denominator (predicted positive when score >= flag_at).
// Both expect.verdict and the expect.flagged shorthand do.
func (c Case) AssertsFlagBoundary() bool {
	return c.Scoreable() && (c.Expect.Verdict != nil || c.Expect.Flagged != nil)
}

// AssertsBlockBoundary reports whether this case belongs in the block
// boundary's denominator (predicted positive when score >= block_at).
// Only expect.verdict does: a boolean cannot distinguish flag from block,
// which are two different operator actions, so counting a shorthand case
// here would grade a decision it never asserted.
func (c Case) AssertsBlockBoundary() bool {
	return c.Scoreable() && c.Expect.Verdict != nil
}

// GradeVerdict compares the asset-level verdict a run produced against
// this case's expectation.
//
// Order matters: a case that is not scoreable is not graded at all, and an
// "error" outcome abstains before any comparison — including for
// expect.flagged: false, which asserts "allow" and not "nothing bad
// happened". A boolean corpus must still fail against a dead pipeline.
func (c Case) GradeVerdict(actual moderation.Verdict) Grade {
	if !c.Scoreable() {
		return GradeNotScored
	}
	if Abstains(actual) {
		return GradeAbstained
	}
	switch {
	case c.Expect.Flagged != nil:
		flagged := actual == moderation.VerdictFlag || actual == moderation.VerdictBlock
		if flagged == *c.Expect.Flagged {
			return GradeMatch
		}
		return GradeMismatch
	case c.Expect.Verdict != nil:
		if actual == *c.Expect.Verdict {
			return GradeMatch
		}
		return GradeMismatch
	default:
		// Category assertions only: nothing is asserted at asset level.
		return GradeNotScored
	}
}

// Abstains reports whether an actual verdict removes a case from the
// precision/recall denominators. "error" is an outcome of the environment,
// not a property of an asset, and it is gated as an abstention rate
// instead of being folded into any decision.
func Abstains(actual moderation.Verdict) bool {
	return actual == moderation.VerdictError
}

// Corpus is a loaded, validated manifest.
type Corpus struct {
	Version int
	// Path is the manifest that was loaded — for a CSV corpus, the
	// materialized manifest, which is the artifact of record.
	Path string
	// Digest is the hex SHA-256 of that file's bytes. It is the corpus
	// identity stamped on a run record.
	Digest string
	Cases  []Case
}

// Counts summarizes what a run over this corpus can and cannot grade, so
// the run record can report how much of the corpus was excluded and why.
type Counts struct {
	Cases     int
	Scoreable int
	// ExcludedDerived counts cases dropped from scoring because their
	// labels came from a previous vismod run.
	ExcludedDerived int
	// ObserveOnly counts cases that assert nothing: scanned and reported,
	// never scored.
	ObserveOnly int
	// AssertsFlagBoundary and AssertsBlockBoundary are the two boundary
	// denominators. They differ whenever the corpus uses the
	// expect.flagged shorthand.
	AssertsFlagBoundary  int
	AssertsBlockBoundary int
}

// Counts computes the corpus-level tallies.
func (c *Corpus) Counts() Counts {
	out := Counts{Cases: len(c.Cases)}
	for _, cs := range c.Cases {
		if cs.Scoreable() {
			out.Scoreable++
		}
		if cs.LabelProvenance == ProvenanceDerived {
			out.ExcludedDerived++
		}
		if !cs.Expect.Asserted() {
			out.ObserveOnly++
		}
		if cs.AssertsFlagBoundary() {
			out.AssertsFlagBoundary++
		}
		if cs.AssertsBlockBoundary() {
			out.AssertsBlockBoundary++
		}
	}
	return out
}
