package hive

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files")

// TestNormalizeGolden is the executable normalization contract (spec §E). It
// captures raw Hive /task/sync responses, runs the full flat-class -> canonical
// reduction, and compares against golden files. Run with -update to regenerate.
//
// Fixtures:
//   - mixed_heads: a dense response exercising the head-split (NSFW -> SEXUAL +
//     SUGGESTIVE_RACY), per-head positive-mass sum (gun head), cross-head max
//     (gun vs knife -> WEAPONS), a single head mapping to two categories
//     (injectables -> DRUGS + MEDICAL), the OTHER fallback for an unknown class,
//     descriptive-head skipping, and negative-class dropping.
//   - clean: every head's negative class dominates -> NO categories emitted
//     (absence == not-detected, never score 0).
func TestNormalizeGolden(t *testing.T) {
	for _, name := range []string{"mixed_heads", "clean"} {
		t.Run(name, func(t *testing.T) {
			raw := readFile(t, filepath.Join("testdata", name+".json"))
			var resp hiveResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			classes, ok := firstOutputClasses(resp)
			if !ok {
				t.Fatalf("fixture %s has no output frame", name)
			}
			got := normalize(classes)

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
