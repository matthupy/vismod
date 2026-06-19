package azure

import "github.com/matthupy/vismod/pkg/moderation"

// analyzeResponse is the exact shape of a successful image:analyze response.
// Top-level carries ONLY categoriesAnalysis (no score, no decision field — the
// pipeline applies thresholds).
type analyzeResponse struct {
	CategoriesAnalysis []categoryAnalysis `json:"categoriesAnalysis"`
}

type categoryAnalysis struct {
	Category string `json:"category"` // Hate | SelfHarm | Sexual | Violence
	Severity int    `json:"severity"` // trimmed image scale: 0,2,4,6
}

// azureErrorResponse is the error envelope. The machine-readable code also
// appears in the x-ms-error-code header.
type azureErrorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Target  string `json:"target"`
	} `json:"error"`
}

// severityScale is the denominator for the trimmed IMAGE severity scale
// (0=Safe, 2=Low, 4=Medium, 6=High) — normalize as severity/6.0.
const severityScale = 6.0

// categoryMap maps Azure's native category names onto the canonical taxonomy.
// Any native label NOT present here falls back to OTHER (raw label preserved,
// score carried) — a result is never dropped (spec §E fallback discipline).
var categoryMap = map[string]moderation.Category{
	"Hate":     moderation.CategoryHate,
	"SelfHarm": moderation.CategorySelfHarm,
	"Sexual":   moderation.CategorySexual,
	"Violence": moderation.CategoryViolence,
}

// normalize maps an Azure image:analyze response into canonical CategoryResults.
// Score = severity/6.0, ScoreOrigin = "severity". The pipeline stamps Threshold
// and Flagged afterward; this layer only emits raw per-category scores.
func normalize(resp analyzeResponse) []moderation.CategoryResult {
	cats := make([]moderation.CategoryResult, 0, len(resp.CategoriesAnalysis))
	for _, ca := range resp.CategoriesAnalysis {
		cat, ok := categoryMap[ca.Category]
		if !ok {
			// Defensive: image:analyze only ever returns the 4 safety categories
			// (azure.go Capabilities), so this branch is unreachable in practice.
			// If a future api-version adds a native label, it maps to OTHER and
			// the pipeline runs Thresholds.For(Other) — meaning OTHER is subject
			// to the default block threshold like any category. Intended: an
			// unmapped signal is never silently dropped. If OTHER should be
			// exempt or carry a dedicated threshold, that is a rollup-layer policy
			// decision, not a normalization concern.
			cat = moderation.CategoryOther
		}
		score := float64(ca.Severity) / severityScale
		cats = append(cats, moderation.CategoryResult{
			Category:      cat,
			ProviderLabel: ca.Category,
			Score:         moderation.Ptr(score),
			ScoreOrigin:   moderation.ScoreOriginSeverity,
		})
	}
	return cats
}
