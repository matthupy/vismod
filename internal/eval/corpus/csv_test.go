package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vismod/vismod/pkg/moderation"
)

const flatCSV = `ref,expected_flagged
https://media.example.com/a.jpg,true
/mnt/corpus/b.png,false
./local/c.mp4,
`

// writeCSV drops body at dir/name and returns the path.
func writeCSV(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// loadFlatCSV desugars body into a manifest under a separate run-output
// directory and loads it, returning the corpus and the manifest path.
func loadFlatCSV(t *testing.T, body string) (*Corpus, string) {
	t.Helper()
	csvPath := writeCSV(t, t.TempDir(), "list.csv", body)
	manifestPath := filepath.Join(t.TempDir(), "run-1", "corpus.yaml")
	c, err := LoadCSV(csvPath, manifestPath, testOptions())
	if err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}
	return c, manifestPath
}

func TestCSVDesugarsInferringKindMediaTypeAndIDs(t *testing.T) {
	csvDir := t.TempDir()
	csvPath := writeCSV(t, csvDir, "list.csv", flatCSV)
	manifestPath := filepath.Join(t.TempDir(), "corpus.yaml")

	c, err := LoadCSV(csvPath, manifestPath, testOptions())
	if err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}
	if len(c.Cases) != 3 {
		t.Fatalf("len(Cases) = %d, want 3", len(c.Cases))
	}

	wantTrue, wantFalse := true, false
	for i, want := range []struct {
		id        string
		kind      Kind
		ref       string
		mediaType string
		flagged   *bool
	}{
		{"case-0001", KindURL, "https://media.example.com/a.jpg", MediaTypeImage, &wantTrue},
		{"case-0002", KindFile, filepath.Clean("/mnt/corpus/b.png"), MediaTypeImage, &wantFalse},
		{"case-0003", KindFile, filepath.Join(csvDir, "local", "c.mp4"), MediaTypeVideo, nil},
	} {
		got := c.Cases[i]
		if got.ID != want.id {
			t.Errorf("case %d: ID = %q, want %q", i, got.ID, want.id)
		}
		if got.Source.Kind != want.kind {
			t.Errorf("%s: Kind = %q, want %q", want.id, got.Source.Kind, want.kind)
		}
		if got.Source.Ref != want.ref {
			t.Errorf("%s: Ref = %q, want %q", want.id, got.Source.Ref, want.ref)
		}
		if got.Source.MediaType != want.mediaType {
			t.Errorf("%s: MediaType = %q, want %q", want.id, got.Source.MediaType, want.mediaType)
		}
		if want.flagged == nil {
			if got.Expect.Flagged != nil {
				t.Errorf("%s: Flagged = %v, want nil (an empty cell is not an assertion)", want.id, *got.Expect.Flagged)
			}
		} else if got.Expect.Flagged == nil || *got.Expect.Flagged != *want.flagged {
			t.Errorf("%s: Flagged = %v, want %v", want.id, got.Expect.Flagged, *want.flagged)
		}
		if got.Expect.Verdict != nil {
			t.Errorf("%s: a CSV cannot express expect.verdict, got %q", want.id, *got.Expect.Verdict)
		}
		// A CSV cannot express derived, and derived is the excluded value:
		// a row someone typed by hand is a human label.
		if got.LabelProvenance != ProvenanceHuman {
			t.Errorf("%s: LabelProvenance = %q, want human", want.id, got.LabelProvenance)
		}
	}
}

// An empty expected_flagged means NOT ASSERTED: the case is scanned and
// reported, never scored. That is the observe-only mode a one-off scan
// actually wants, and it must not be collapsed into "expected allow".
func TestCSVEmptyExpectationIsObserveOnly(t *testing.T) {
	c, _ := loadFlatCSV(t, flatCSV)

	observe := c.Cases[2]
	if observe.Expect.Asserted() {
		t.Error("an empty expected_flagged read back as an assertion")
	}
	if observe.Scoreable() {
		t.Error("an observe-only case must not be scoreable")
	}
	if observe.AssertsFlagBoundary() || observe.AssertsBlockBoundary() {
		t.Error("an observe-only case must be in no boundary denominator")
	}
	for _, v := range []moderation.Verdict{
		moderation.VerdictAllow, moderation.VerdictFlag,
		moderation.VerdictBlock, moderation.VerdictError,
	} {
		if g := observe.GradeVerdict(v); g != GradeNotScored {
			t.Errorf("GradeVerdict(%q) = %q, want %q", v, g, GradeNotScored)
		}
	}
	counts := c.Counts()
	if counts.Cases != 3 {
		t.Errorf("Cases = %d, want 3 — an observe-only case is still scanned and reported", counts.Cases)
	}
	if counts.ObserveOnly != 1 {
		t.Errorf("ObserveOnly = %d, want 1", counts.ObserveOnly)
	}
	if counts.Scoreable != 2 {
		t.Errorf("Scoreable = %d, want 2", counts.Scoreable)
	}
	if counts.ExcludedDerived != 0 {
		t.Errorf("ExcludedDerived = %d, want 0 — observe-only is not a provenance exclusion", counts.ExcludedDerived)
	}
}

// One loader, one validator, one digest rule: the CSV is desugared into a
// v1 manifest, that manifest is materialized next to the run output, and
// ITS SHA-256 is the corpus digest. A digest over the CSV bytes would give
// two corpora the same claim to one identity.
func TestCSVMaterializesManifestAndDigestsIt(t *testing.T) {
	csvDir := t.TempDir()
	csvPath := writeCSV(t, csvDir, "list.csv", flatCSV)
	manifestPath := filepath.Join(t.TempDir(), "run-1", "corpus.yaml")

	c, err := LoadCSV(csvPath, manifestPath, testOptions())
	if err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}

	written, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("the generated manifest was not materialized: %v", err)
	}
	sum := sha256.Sum256(written)
	if want := hex.EncodeToString(sum[:]); c.Digest != want {
		t.Errorf("Digest = %q, want the materialized manifest's SHA-256 %q", c.Digest, want)
	}
	csvSum := sha256.Sum256([]byte(flatCSV))
	if c.Digest == hex.EncodeToString(csvSum[:]) {
		t.Error("the digest was taken over the CSV bytes, not over the generated manifest")
	}
	if c.Path != manifestPath {
		t.Errorf("Path = %q, want the materialized manifest %q", c.Path, manifestPath)
	}

	// The materialized artifact is the corpus of record: loading it
	// directly must reproduce the same corpus and the same digest.
	again, err := Load(manifestPath, testOptions())
	if err != nil {
		t.Fatalf("the generated manifest does not load: %v", err)
	}
	if again.Digest != c.Digest {
		t.Errorf("reloading the materialized manifest changed the digest: %q vs %q", again.Digest, c.Digest)
	}
	if len(again.Cases) != len(c.Cases) {
		t.Fatalf("reload has %d cases, want %d", len(again.Cases), len(c.Cases))
	}
	for i := range c.Cases {
		if again.Cases[i].Source.Ref != c.Cases[i].Source.Ref {
			t.Errorf("case %d ref drifted on reload: %q vs %q", i, again.Cases[i].Source.Ref, c.Cases[i].Source.Ref)
		}
	}
}

// mediaTypeFor is unexported and lives in internal/cli, so the converter
// reproduces the rule rather than calling it. This test pins the two in
// agreement: it reads intake's extension table out of the source and
// requires this package to answer identically for every entry.
func TestCSVMediaTypeInferenceMatchesIntake(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cli", "scan.go"))
	if err != nil {
		t.Fatalf("read intake source: %v", err)
	}
	block := regexp.MustCompile(`(?s)var videoExts = map\[string\]bool\{(.*?)\n\}`).FindSubmatch(src)
	if block == nil {
		t.Fatal("internal/cli/scan.go no longer declares videoExts — the rule this test pins has moved")
	}
	found := regexp.MustCompile(`"(\.[a-z0-9]+)"`).FindAllStringSubmatch(string(block[1]), -1)
	if len(found) == 0 {
		t.Fatal("no extensions parsed out of intake's videoExts")
	}

	intake := map[string]bool{}
	for _, m := range found {
		intake[m[1]] = true
		if got := MediaTypeFor("clip" + m[1]); got != MediaTypeVideo {
			t.Errorf("MediaTypeFor(%q) = %q, intake says video", m[1], got)
		}
	}
	for ext := range videoExts {
		if !intake[ext] {
			t.Errorf("this package treats %q as video and intake does not", ext)
		}
	}
	if len(videoExts) != len(intake) {
		t.Errorf("extension sets differ: %d here, %d in intake", len(videoExts), len(intake))
	}

	// Everything else is an image, including an extension nobody knows —
	// the same fallback intake uses.
	for _, ref := range []string{"a.jpg", "a.xyz", "a", "https://media.example.com/a"} {
		if got := MediaTypeFor(ref); got != MediaTypeImage {
			t.Errorf("MediaTypeFor(%q) = %q, want image", ref, got)
		}
	}
	// Intake lowercases the extension before the lookup.
	if got := MediaTypeFor("A.MP4"); got != MediaTypeVideo {
		t.Errorf("MediaTypeFor(%q) = %q, want video", "A.MP4", got)
	}
	// A presigned url carries a query string; the extension rule applies
	// to the path, or every signed video reads as an image.
	if got := MediaTypeFor("https://media.example.com/clip.mp4?sig=abc#f"); got != MediaTypeVideo {
		t.Errorf("MediaTypeFor(signed url) = %q, want video", got)
	}
}

// Every CSV load error names the line, because that is what the operator
// has to go and fix.
func TestCSVDuplicateRefMissingColumnOrBadBooleanNamesLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"duplicate ref", `ref,expected_flagged
a.jpg,true
b.jpg,false
a.jpg,true
`, []string{"line 4", "duplicate", "a.jpg"}},
		{"missing ref column", `path,expected_flagged
a.jpg,true
`, []string{"line 1", "ref"}},
		{"unparseable expected_flagged", `ref,expected_flagged
a.jpg,maybe
`, []string{"line 2", "maybe"}},
		{"empty ref", `ref,expected_flagged
,true
`, []string{"line 2", "ref"}},
		{"no rows", "ref,expected_flagged\n", []string{"no cases"}},
		{"empty file", "", []string{"empty"}},
		{"ragged row", `ref,expected_flagged
a.jpg,true,extra
`, []string{"line 2"}},
		{"non-https scheme", `ref,expected_flagged
http://media.example.com/a.jpg,true
`, []string{"line 2", "https"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			csvPath := writeCSV(t, t.TempDir(), "list.csv", tc.body)
			_, err := LoadCSV(csvPath, filepath.Join(t.TempDir(), "corpus.yaml"), testOptions())
			if err == nil {
				t.Fatalf("%s accepted", tc.name)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestCSVRefusesColumnsItCannotExpress is the CSV half of the guarantee
// dec.KnownFields(true) gives the manifest: a corpus that loses an assertion
// still reports a number, and that number is wrong in a flattering direction.
//
// An unknown column is the sharper case of the two. The CSV on-ramp stamps
// label_provenance: human on every row, which is safe ONLY because a CSV
// cannot express "derived" — so an operator who adds a label_provenance
// column and writes "derived" in it believes they excluded those rows from
// scoring, while every one of them stays Scoreable() and the harness grades
// the pipeline against its own past output. A duplicate column is the same
// defect with a smaller blast radius: last-index-wins silently inverts a
// label.
func TestCSVRefusesColumnsItCannotExpress(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"unknown column", `ref,expected_flagged,label_provenance
a.jpg,true,derived
`, []string{"line 1", "label_provenance"}},
		{"duplicate expected_flagged", `ref,expected_flagged,expected_flagged
a.jpg,true,false
`, []string{"line 1", "expected_flagged", "twice"}},
		{"duplicate ref column", `ref,ref,expected_flagged
a.jpg,b.jpg,true
`, []string{"line 1", "ref", "twice"}},
		{"trailing comma leaves an unnamed column", `ref,expected_flagged,
a.jpg,true,
`, []string{"line 1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			csvPath := writeCSV(t, t.TempDir(), "list.csv", tc.body)
			_, err := LoadCSV(csvPath, filepath.Join(t.TempDir(), "corpus.yaml"), testOptions())
			if err == nil {
				t.Fatalf("%s accepted; the expectation it carries is silently dropped", tc.name)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestCSVAcceptsTheBooleanSpellingsEncodingCSVWrites(t *testing.T) {
	c, _ := loadFlatCSV(t, `ref,expected_flagged
a.jpg,TRUE
b.jpg,False
c.jpg, true
d.jpg
`)
	if len(c.Cases) != 4 {
		t.Fatalf("len(Cases) = %d, want 4", len(c.Cases))
	}
	for i, want := range []*bool{boolPtr(true), boolPtr(false), boolPtr(true), nil} {
		got := c.Cases[i].Expect.Flagged
		switch {
		case want == nil && got != nil:
			t.Errorf("case %d: Flagged = %v, want nil", i, *got)
		case want != nil && (got == nil || *got != *want):
			t.Errorf("case %d: Flagged = %v, want %v", i, got, *want)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

func TestCSVMissingFileAndUnwritableManifestAreErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCSV(filepath.Join(dir, "nope.csv"), filepath.Join(dir, "corpus.yaml"), testOptions()); err == nil {
		t.Error("a missing CSV loaded")
	}
	csvPath := writeCSV(t, dir, "list.csv", flatCSV)
	// A directory is not a writable manifest path.
	if _, err := LoadCSV(csvPath, dir, testOptions()); err == nil {
		t.Error("materializing the manifest over a directory succeeded")
	}
}
