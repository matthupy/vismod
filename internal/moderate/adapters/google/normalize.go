package google

import (
	"sort"

	"github.com/matthupy/vismod/pkg/moderation"
)

// annotateResponse is the Vision v1 images:annotate envelope. One image -> one
// response entry. Only the fields the normalizer needs are decoded; everything
// else (label/face/text annotations) is ignored. A per-response error sits at
// responses[].error (HTTP 200 with a per-image failure — see client.go).
type annotateResponse struct {
	Responses []annotateResult `json:"responses"`
}

type annotateResult struct {
	SafeSearch *safeSearchAnnotation `json:"safeSearchAnnotation"`
	Error      *annotateError        `json:"error"`
}

// safeSearchAnnotation is the five ordinal likelihood fields SafeSearch returns.
// Each is one of UNKNOWN/VERY_UNLIKELY/UNLIKELY/POSSIBLE/LIKELY/VERY_LIKELY.
type safeSearchAnnotation struct {
	Adult    string `json:"adult"`
	Spoof    string `json:"spoof"`
	Medical  string `json:"medical"`
	Violence string `json:"violence"`
	Racy     string `json:"racy"`
}

// annotateError is the per-response error object (responses[].error). The
// machine-readable code is an int google.rpc.Code; we surface message + code.
type annotateError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// field couples a SafeSearch likelihood field to its raw label and canonical
// category. The ordered slice gives normalize deterministic, golden-stable
// output without a post-sort over a map.
type field struct {
	label string
	value func(*safeSearchAnnotation) string
	cat   moderation.Category
}

// fields is the fixed SafeSearch -> canonical taxonomy mapping (spec §E):
//
//	adult    -> SEXUAL
//	racy     -> SUGGESTIVE_RACY
//	violence -> VIOLENCE
//	medical  -> MEDICAL  (provenance-only, NOT a harm signal)
//	spoof    -> SPOOF    (provenance-only, NOT a harm signal)
//
// MEDICAL/SPOOF exist only to carry SafeSearch's medical/spoof; pkg/moderation
// documents that consumers must not read them as harm. The mapping is fixed in
// v1 (not a configurable adapter.option) so it adds no verdict-affecting key to
// the §L model-fingerprint guard — a configurable lookup is a documented future
// step.
var fields = []field{
	{"adult", func(a *safeSearchAnnotation) string { return a.Adult }, moderation.CategorySexual},
	{"racy", func(a *safeSearchAnnotation) string { return a.Racy }, moderation.CategorySuggestiveRacy},
	{"violence", func(a *safeSearchAnnotation) string { return a.Violence }, moderation.CategoryViolence},
	{"medical", func(a *safeSearchAnnotation) string { return a.Medical }, moderation.CategoryMedical},
	{"spoof", func(a *safeSearchAnnotation) string { return a.Spoof }, moderation.CategorySpoof},
}

// likelihoodTable is the §E ordinal-enum -> score lookup. VERY_UNLIKELY is a
// genuine "evaluated, very unlikely" signal (0.0), distinct from UNKNOWN
// ("could not evaluate" -> nil, see likelihoodScore). Scores are
// within-provider comparable ONLY; thresholds are re-tuned per adapter and are
// not portable (MODEL_AND_HASH_LIMITATIONS.md).
var likelihoodTable = map[string]float64{
	"VERY_UNLIKELY": 0.0,
	"UNLIKELY":      0.25,
	"POSSIBLE":      0.5,
	"LIKELY":        0.75,
	"VERY_LIKELY":   1.0,
}

// likelihoodScore maps a SafeSearch likelihood enum to a normalized [0,1] score.
// UNKNOWN, an empty/missing field, and any unrecognized future enum all return
// nil (could-not-evaluate) — never a guessed score. A nil Score per the §E
// rollup contributes no "allow"; an all-nil asset becomes Verdict=error.
func likelihoodScore(enum string) *float64 {
	if s, ok := likelihoodTable[enum]; ok {
		return moderation.Ptr(s)
	}
	return nil
}

// firstAnnotation extracts the first response's SafeSearch annotation, reporting
// ok=false when the response carries no annotation (missing or per-response
// error — the caller treats that as could-not-evaluate, never a clean frame).
func firstAnnotation(resp annotateResponse) (*safeSearchAnnotation, bool) {
	if len(resp.Responses) == 0 || resp.Responses[0].SafeSearch == nil {
		return nil, false
	}
	return resp.Responses[0].SafeSearch, true
}

// normalize maps a SafeSearch annotation into canonical CategoryResults. Every
// field emits a row (including VERY_UNLIKELY=0.0 and UNKNOWN=nil) so the
// envelope records what was evaluated; absence of a row would wrongly read as
// "not analyzed". ScoreOrigin is likelihood_enum. The pipeline stamps
// Threshold/Flagged and the asset rollup afterward.
func normalize(ann *safeSearchAnnotation) []moderation.CategoryResult {
	out := make([]moderation.CategoryResult, 0, len(fields))
	for _, f := range fields {
		out = append(out, moderation.CategoryResult{
			Category:      f.cat,
			ProviderLabel: f.label,
			Score:         likelihoodScore(f.value(ann)),
			ScoreOrigin:   moderation.ScoreOriginLikelihoodEnum,
		})
	}
	sortResults(out)
	return out
}

// sortResults gives deterministic, golden-stable output ordered by category.
func sortResults(rs []moderation.CategoryResult) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Category < rs[j].Category })
}
