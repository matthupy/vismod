package google

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files")

// TestNormalizeGolden is the executable normalization contract (spec §E). It
// captures raw Vision images:annotate responses, runs normalize, and compares
// against golden files. Run with -update to regenerate.
//
// Fixtures:
//   - mixed: a response exercising all five SafeSearch fields across the
//     likelihood scale, including VERY_UNLIKELY(0.0) vs an UNLIKELY/POSSIBLE row.
//   - all_unknown: every field UNKNOWN -> five rows, every Score null (the
//     could-not-evaluate shape the asset rollup turns into Verdict=error).
func TestNormalizeGolden(t *testing.T) {
	for _, name := range []string{"mixed", "all_unknown"} {
		t.Run(name, func(t *testing.T) {
			raw := readFile(t, filepath.Join("testdata", name+".json"))
			var resp annotateResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			ann, ok := firstAnnotation(resp)
			if !ok {
				t.Fatalf("fixture %s has no safeSearchAnnotation", name)
			}
			got := normalize(ann)

			pretty, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", name+".golden")
			if *update {
				writeFile(t, golden, pretty)
				return
			}
			if want := readFile(t, golden); string(pretty) != string(want) {
				t.Errorf("normalize mismatch for %s\n got: %s\nwant: %s", name, pretty, want)
			}
		})
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
