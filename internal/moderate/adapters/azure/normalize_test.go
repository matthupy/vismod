package azure

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/matthupy/vismod/pkg/moderation"
)

var update = flag.Bool("update", false, "regenerate golden files")

// TestNormalizeGolden captures Azure's raw response fixtures, normalizes them,
// and compares against golden files. Run with -update to regenerate.
func TestNormalizeGolden(t *testing.T) {
	cases := []string{"mixed_severity", "all_safe", "unknown_category"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			raw := readFile(t, filepath.Join("testdata", name+".json"))
			var resp analyzeResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			got := normalize(resp)

			golden := filepath.Join("testdata", name+".golden")
			pretty, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if *update {
				writeFile(t, golden, pretty)
				return
			}
			want := readFile(t, golden)
			if string(pretty) != string(want) {
				t.Errorf("normalize mismatch for %s\n got: %s\nwant: %s", name, pretty, want)
			}
		})
	}
}

// TestNormalizeSeverityMapping locks the 0/2/4/6 -> severity/6.0 contract.
func TestNormalizeSeverityMapping(t *testing.T) {
	resp := analyzeResponse{CategoriesAnalysis: []categoryAnalysis{
		{Category: "Hate", Severity: 0},
		{Category: "Sexual", Severity: 2},
		{Category: "Violence", Severity: 4},
		{Category: "SelfHarm", Severity: 6},
	}}
	got := normalize(resp)
	want := map[moderation.Category]float64{
		moderation.CategoryHate:     0.0,
		moderation.CategorySexual:   2.0 / 6.0,
		moderation.CategoryViolence: 4.0 / 6.0,
		moderation.CategorySelfHarm: 1.0,
	}
	for _, c := range got {
		if c.ScoreOrigin != moderation.ScoreOriginSeverity {
			t.Errorf("%s: ScoreOrigin = %q, want severity", c.Category, c.ScoreOrigin)
		}
		if c.Score == nil {
			t.Fatalf("%s: score is nil", c.Category)
		}
		if w := want[c.Category]; *c.Score != w {
			t.Errorf("%s: score = %v, want %v", c.Category, *c.Score, w)
		}
	}
}

// TestNormalizeUnknownCategoryFallsToOther proves an unmapped native label
// becomes OTHER with the raw label preserved and the score carried (never dropped).
func TestNormalizeUnknownCategoryFallsToOther(t *testing.T) {
	resp := analyzeResponse{CategoriesAnalysis: []categoryAnalysis{
		{Category: "FutureCategory", Severity: 4},
	}}
	got := normalize(resp)
	if len(got) != 1 {
		t.Fatalf("want 1 result, got %d", len(got))
	}
	c := got[0]
	if c.Category != moderation.CategoryOther {
		t.Errorf("Category = %q, want OTHER", c.Category)
	}
	if c.ProviderLabel != "FutureCategory" {
		t.Errorf("ProviderLabel = %q, want raw label preserved", c.ProviderLabel)
	}
	if c.Score == nil || *c.Score != 4.0/6.0 {
		t.Errorf("score not carried for OTHER fallback: %v", c.Score)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
