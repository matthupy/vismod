package hive

import (
	"slices"
	"sort"

	"github.com/matthupy/vismod/pkg/moderation"
)

// hiveResponse is the v2 /task/sync envelope. The model output lives at
// status[].response.output[]; for a single image there is one status entry and
// one output frame (time 0). Only the fields the normalizer needs are decoded;
// everything else (task id, timing, model metadata) is ignored.
type hiveResponse struct {
	Status []hiveStatus `json:"status"`
}

type hiveStatus struct {
	Response hiveModelResponse `json:"response"`
}

type hiveModelResponse struct {
	Output []hiveOutput `json:"output"`
}

// hiveOutput is one analyzed frame. Time is the frame timestamp in seconds (0
// for a still image). Classes is the FLAT list of every head's classes — the
// wire format carries no head grouping, so taxonomy.go reconstructs it.
type hiveOutput struct {
	Time    float64     `json:"time"`
	Classes []hiveClass `json:"classes"`
}

type hiveClass struct {
	Class string  `json:"class"`
	Score float64 `json:"score"`
}

// epsilon is the floor below which a category's accumulated mass counts as
// zero-evidence and is omitted (so absence reads as "not detected", never 0).
const epsilon = 1e-9

// headCatKey identifies a (head, canonical category) pair. Positive classes that
// share both are summed (mutually-exclusive sub-states of one signal); distinct
// heads mapping to the same category are then reduced by max.
type headCatKey struct {
	head string
	cat  moderation.Category
}

// catAcc accumulates one category's evidence: mass is the running score, label
// is the single highest-scoring contributing class (used as ProviderLabel so the
// emitted row points at a real Hive class string, per §E "raw native class").
type catAcc struct {
	mass     float64
	topLabel string
	topScore float64
}

// normalize maps Hive's flat class list for ONE frame into canonical
// CategoryResults. The reduction has three stages:
//
//  1. Per (head, category): SUM positive class scores. Within a head the
//     positive classes are mutually-exclusive sub-states of "the thing is
//     present", so their probabilities add up to P(present) = 1 - negative.
//  2. Per category: take the MAX head mass across heads mapping to it (the most
//     confident independent signal — gun vs knife both say WEAPONS).
//  3. Unknown classes bypass both stages and emit directly as OTHER so a future
//     Hive class is never silently dropped (spec §E fallback discipline).
//
// Negative ("safe complement") classes and descriptive heads are dropped — see
// taxonomy.go. Every emitted Score carries ScoreOrigin=probability. The pipeline
// stamps Threshold/Flagged and the asset rollup afterward.
func normalize(classes []hiveClass) []moderation.CategoryResult {
	headSums := map[headCatKey]*catAcc{}
	var out []moderation.CategoryResult

	for _, c := range classes {
		info, known := classTaxonomy[c.Class]
		if !known {
			// Unknown future class: never drop a real signal. Emit straight to
			// OTHER with the raw label and its score. A zero-evidence unknown is
			// omitted like any zero-evidence category (see epsilon below).
			if c.Score > epsilon {
				out = append(out, categoryResult(moderation.CategoryOther, c.Class, c.Score))
			}
			continue
		}
		if info.pol == negative || info.cat == catSkip {
			continue // safe complement, or descriptive non-harm head
		}
		key := headCatKey{head: info.head, cat: info.cat}
		acc := headSums[key]
		if acc == nil {
			acc = &catAcc{}
			headSums[key] = acc
		}
		acc.mass += c.Score
		if c.Score > acc.topScore {
			acc.topScore = c.Score
			acc.topLabel = c.Class
		}
	}

	// Stage 2: reduce heads to one row per category by max mass.
	perCat := map[moderation.Category]*catAcc{}
	for key, acc := range headSums {
		cur := perCat[key.cat]
		if cur == nil || acc.mass > cur.mass {
			// Copy so later mutation of the source map entry can't alias.
			a := *acc
			perCat[key.cat] = &a
		}
	}
	for cat, acc := range perCat {
		// A zero-evidence category ("not present") is OMITTED, not emitted as
		// score 0 — non-emitted == absent (§E worked-example-1). Consumers must
		// never read an absent category as a 0 harm signal.
		if acc.mass > epsilon {
			out = append(out, categoryResult(cat, acc.topLabel, acc.mass))
		}
	}

	sortResults(out)
	return out
}

// categoryResult builds a probability-origin CategoryResult, clamping the score
// to [0,1] (floating-point accumulation of summed positives can drift past 1.0).
// Threshold/Flagged are left zero — the pipeline stamps them.
func categoryResult(cat moderation.Category, label string, raw float64) moderation.CategoryResult {
	if raw > 1.0 {
		raw = 1.0
	}
	if raw < 0 {
		raw = 0
	}
	return moderation.CategoryResult{
		Category:      cat,
		ProviderLabel: label,
		Score:         moderation.Ptr(raw),
		ScoreOrigin:   moderation.ScoreOriginProbability,
	}
}

// sortResults gives deterministic output (golden-stable) ordered by category
// then provider label.
func sortResults(rs []moderation.CategoryResult) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Category != rs[j].Category {
			return rs[i].Category < rs[j].Category
		}
		return rs[i].ProviderLabel < rs[j].ProviderLabel
	})
}

// sortCategories sorts a canonical-category slice in place (used for Caps).
func sortCategories(cs []moderation.Category) {
	slices.Sort(cs)
}
