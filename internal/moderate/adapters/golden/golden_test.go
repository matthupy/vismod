package golden

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sample struct {
	Provider string   `json:"provider"`
	Score    *float64 `json:"score"`
}

// TestCheckWritesThenMatches walks the real workflow: `go test -update`
// writes the golden, and a later plain run compares against it. The test
// chdirs into a temp dir so it never touches a committed golden.
func TestCheckWritesThenMatches(t *testing.T) {
	t.Chdir(t.TempDir())

	got := sample{Provider: "fake"}

	*update = true
	Check(t, "sample", got)
	*update = false
	t.Cleanup(func() { *update = false })

	written, err := os.ReadFile(filepath.Join("testdata", "sample.golden"))
	if err != nil {
		t.Fatalf("-update did not write the golden: %v", err)
	}
	if len(written) == 0 || written[len(written)-1] != '\n' {
		t.Error("golden must end in a newline so diffs stay line-oriented")
	}

	// The same value must now compare clean.
	Check(t, "sample", got)
}

// TestCheckIgnoresLineEndings: goldens are regenerated on Windows and read
// on Linux CI (and the reverse). A CRLF checkout must not be reported as a
// normalization change — that would train people to ignore golden failures.
func TestCheckToleratesCRLFGolden(t *testing.T) {
	t.Chdir(t.TempDir())

	got := sample{Provider: "fake"}

	*update = true
	Check(t, "crlf", got)
	*update = false
	t.Cleanup(func() { *update = false })

	path := filepath.Join("testdata", "crlf.golden")
	lf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	crlf := make([]byte, 0, len(lf)*2)
	for _, b := range lf {
		if b == '\n' {
			crlf = append(crlf, '\r')
		}
		crlf = append(crlf, b)
	}
	if err := os.WriteFile(path, crlf, 0o600); err != nil {
		t.Fatal(err)
	}

	Check(t, "crlf", got) // fails the test if line endings are compared raw
}

// TestCheckSerializesNullScores: the golden is the contract for the wire
// format, and null discipline is the invariant most likely to regress
// silently. A nil score must appear as null, not 0 and not an absent key.
func TestCheckSerializesNullScores(t *testing.T) {
	t.Chdir(t.TempDir())

	*update = true
	Check(t, "nullable", sample{Provider: "fake"})
	*update = false
	t.Cleanup(func() { *update = false })

	b, err := os.ReadFile(filepath.Join("testdata", "nullable.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `"score": null`; !strings.Contains(string(b), want) {
		t.Errorf("golden does not carry %s:\n%s", want, b)
	}
}
