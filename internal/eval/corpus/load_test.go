package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/pkg/moderation"
)

// testOptions is the operator configuration these tests validate against:
// a dedup ceiling of 8, which is what config.Defaults() ships.
func testOptions() Options { return Options{DedupCeiling: 8} }

// writeManifest drops body at dir/name and returns the path.
func writeManifest(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

const validManifest = `version: 1
cases:
  - id: case-0001
    source:
      kind: file
      ref: ./media/a.jpg
      media_type: image
    expect:
      verdict: block
      categories:
        SEXUAL: block
        VIOLENCE: allow
    label_provenance: human
    notes: "known-bad still"
  - id: case-0002
    source: {kind: url, ref: "https://media.example.com/clip.mp4", media_type: video}
    workflows: [scene-detect, keyframes]
    dedup_threshold: 6
    expect:
      verdict: allow
    label_provenance: vendor
`

func TestLoadValidManifestV1(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, "corpus.yaml", validManifest)

	c, err := Load(path, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Version != SchemaVersion {
		t.Errorf("Version = %d, want %d", c.Version, SchemaVersion)
	}
	if c.Path != path {
		t.Errorf("Path = %q, want %q", c.Path, path)
	}
	if len(c.Cases) != 2 {
		t.Fatalf("len(Cases) = %d, want 2", len(c.Cases))
	}

	one := c.Cases[0]
	if one.ID != "case-0001" {
		t.Errorf("ID = %q, want case-0001", one.ID)
	}
	if one.Source.Kind != KindFile {
		t.Errorf("Source.Kind = %q, want %q", one.Source.Kind, KindFile)
	}
	if want := filepath.Join(dir, "media", "a.jpg"); one.Source.Ref != want {
		t.Errorf("Source.Ref = %q, want %q", one.Source.Ref, want)
	}
	if one.Source.MediaType != MediaTypeImage {
		t.Errorf("Source.MediaType = %q, want %q", one.Source.MediaType, MediaTypeImage)
	}
	if one.Expect.Verdict == nil || *one.Expect.Verdict != moderation.VerdictBlock {
		t.Fatalf("Expect.Verdict = %v, want block", one.Expect.Verdict)
	}
	if got, ok := one.Expect.Category(moderation.CategorySexual); !ok || got != moderation.VerdictBlock {
		t.Errorf("Category(SEXUAL) = %q,%v want block,true", got, ok)
	}
	if got, ok := one.Expect.Category(moderation.CategoryViolence); !ok || got != moderation.VerdictAllow {
		t.Errorf("Category(VIOLENCE) = %q,%v want allow,true", got, ok)
	}
	if one.LabelProvenance != ProvenanceHuman {
		t.Errorf("LabelProvenance = %q, want human", one.LabelProvenance)
	}
	if one.Notes != "known-bad still" {
		t.Errorf("Notes = %q", one.Notes)
	}
	if one.DedupThreshold != nil {
		t.Errorf("DedupThreshold = %v, want nil (inherit the configured behavior)", *one.DedupThreshold)
	}
	if len(one.Workflows) != 0 {
		t.Errorf("Workflows = %v, want none", one.Workflows)
	}

	two := c.Cases[1]
	if two.Source.Kind != KindURL {
		t.Errorf("Source.Kind = %q, want %q", two.Source.Kind, KindURL)
	}
	if two.Source.Ref != "https://media.example.com/clip.mp4" {
		t.Errorf("a url ref must not be path-resolved, got %q", two.Source.Ref)
	}
	if two.Source.MediaType != MediaTypeVideo {
		t.Errorf("Source.MediaType = %q, want video", two.Source.MediaType)
	}
	if got := strings.Join(two.Workflows, ","); got != "scene-detect,keyframes" {
		t.Errorf("Workflows = %q", got)
	}
	if two.DedupThreshold == nil || *two.DedupThreshold != 6 {
		t.Fatalf("DedupThreshold = %v, want 6", two.DedupThreshold)
	}
	if two.LabelProvenance != ProvenanceVendor {
		t.Errorf("LabelProvenance = %q, want vendor", two.LabelProvenance)
	}
}

// The manifest names the media, and the manifest's own directory is what
// those names are relative to. Resolving against the process working
// directory makes one corpus load differently depending on where the
// operator happened to be standing.
func TestRefResolvesRelativeToManifestDir(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	path := writeManifest(t, home, "corpus.yaml", validManifest)

	t.Chdir(elsewhere)

	c, err := Load(path, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.Cases[0].Source.Ref
	if want := filepath.Join(home, "media", "a.jpg"); got != want {
		t.Errorf("Source.Ref = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Source.Ref = %q, want an absolute path", got)
	}
	if strings.HasPrefix(got, elsewhere) {
		t.Errorf("Source.Ref = %q resolved against the working directory %q", got, elsewhere)
	}
}

// The corpus digest is the manifest file's own SHA-256: what a run record
// names must be reproducible from the artifact itself.
func TestLoadReturnsManifestSHA256(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, "corpus.yaml", validManifest)

	c, err := Load(path, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sum := sha256.Sum256([]byte(validManifest))
	if want := hex.EncodeToString(sum[:]); c.Digest != want {
		t.Errorf("Digest = %q, want %q", c.Digest, want)
	}

	other := writeManifest(t, dir, "other.yaml", validManifest+"# a comment changes the bytes\n")
	c2, err := Load(other, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c2.Digest == c.Digest {
		t.Error("two manifests with different bytes share a digest")
	}
}

// A category nobody labelled is NOT an assertion that the asset is safe in
// that category. Collapsing "absent" into "allow" inflates every later
// denominator with categories the operator never asserted on, and the
// harness reports recall it never earned.
func TestManifestOmittedCategoryIsNotAsserted(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, "corpus.yaml", `version: 1
cases:
  - id: sexual-only
    source: {kind: file, ref: a.jpg, media_type: image}
    expect:
      verdict: block
      categories: {SEXUAL: block}
    label_provenance: human
  - id: also-violence
    source: {kind: file, ref: b.jpg, media_type: image}
    expect:
      verdict: block
      categories: {SEXUAL: block, VIOLENCE: allow}
    label_provenance: human
`)
	c, err := Load(path, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got, ok := c.Cases[0].Expect.Category(moderation.CategoryViolence)
	if ok {
		t.Errorf("VIOLENCE reported as asserted by a manifest that never mentions it (got %q)", got)
	}
	if got == moderation.VerdictAllow {
		t.Error("an unasserted category read back as allow")
	}
	if got, ok := c.Cases[1].Expect.Category(moderation.CategoryViolence); !ok || got != moderation.VerdictAllow {
		t.Errorf("explicit VIOLENCE: allow = %q,%v want allow,true", got, ok)
	}
	if n := len(c.Cases[0].Expect.AssertedCategories()); n != 1 {
		t.Errorf("AssertedCategories = %d, want 1", n)
	}
	if n := len(c.Cases[1].Expect.AssertedCategories()); n != 2 {
		t.Errorf("AssertedCategories = %d, want 2", n)
	}
}

// Grading a pipeline against labels a previous run of that pipeline
// produced reports 100% F1 forever. Such a case still loads and is still
// scanned; it is excluded from scoring, and the exclusion is counted so a
// run record can say how much of the corpus went ungraded.
func TestDerivedProvenanceLoadsNotScoreableAndCounted(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, "corpus.yaml", `version: 1
cases:
  - id: human-1
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: block}
    label_provenance: human
  - id: derived-1
    source: {kind: file, ref: b.jpg, media_type: image}
    expect: {verdict: block}
    label_provenance: derived
  - id: vendor-1
    source: {kind: file, ref: c.jpg, media_type: image}
    expect: {verdict: allow}
    label_provenance: vendor
`)
	c, err := Load(path, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Cases) != 3 {
		t.Fatalf("len(Cases) = %d, want 3 — a derived case still loads", len(c.Cases))
	}
	derived := c.Cases[1]
	if derived.LabelProvenance != ProvenanceDerived {
		t.Fatalf("LabelProvenance = %q", derived.LabelProvenance)
	}
	if derived.Expect.Verdict == nil {
		t.Error("a derived case keeps its expectation; it is reported, just not scored")
	}
	if derived.Scoreable() {
		t.Error("a derived-provenance case must not be scoreable")
	}
	if g := derived.GradeVerdict(moderation.VerdictBlock); g != GradeNotScored {
		t.Errorf("GradeVerdict = %q, want %q even on a perfect match", g, GradeNotScored)
	}
	if derived.AssertsFlagBoundary() || derived.AssertsBlockBoundary() {
		t.Error("a derived case must be in no boundary denominator")
	}
	counts := c.Counts()
	if counts.Cases != 3 || counts.Scoreable != 2 || counts.ExcludedDerived != 1 {
		t.Errorf("Counts = %+v, want Cases 3, Scoreable 2, ExcludedDerived 1", counts)
	}
}

func TestExpectFlaggedShorthandLoads(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, "corpus.yaml", `version: 1
cases:
  - id: harmful
    source: {kind: url, ref: "https://media.example.com/a.jpg", media_type: image}
    expect: {flagged: true}
    label_provenance: human
  - id: benign
    source: {kind: url, ref: "https://media.example.com/b.jpg", media_type: image}
    expect: {flagged: false}
    label_provenance: human
`)
	c, err := Load(path, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	harmful, benign := c.Cases[0], c.Cases[1]
	if harmful.Expect.Flagged == nil || !*harmful.Expect.Flagged {
		t.Fatalf("Expect.Flagged = %v, want true", harmful.Expect.Flagged)
	}
	if benign.Expect.Flagged == nil || *benign.Expect.Flagged {
		t.Fatalf("Expect.Flagged = %v, want false", benign.Expect.Flagged)
	}
	if harmful.Expect.Verdict != nil {
		t.Error("the shorthand must not synthesize an expect.verdict")
	}
	if !harmful.Scoreable() || !benign.Scoreable() {
		t.Error("a flagged shorthand case is scoreable")
	}

	// flagged maps onto OverallVerdict.Flagged: the verdict is flag or block.
	for _, tc := range []struct {
		name   string
		c      Case
		actual moderation.Verdict
		want   Grade
	}{
		{"true vs flag", harmful, moderation.VerdictFlag, GradeMatch},
		{"true vs block", harmful, moderation.VerdictBlock, GradeMatch},
		{"true vs allow", harmful, moderation.VerdictAllow, GradeMismatch},
		{"false vs allow", benign, moderation.VerdictAllow, GradeMatch},
		{"false vs flag", benign, moderation.VerdictFlag, GradeMismatch},
		{"false vs block", benign, moderation.VerdictBlock, GradeMismatch},
	} {
		if got := tc.c.GradeVerdict(tc.actual); got != tc.want {
			t.Errorf("%s: GradeVerdict = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A boolean cannot separate flag from block — two different operator
// actions — so it asserts the flag boundary and says nothing about the
// block boundary. The block-boundary denominator must exclude it.
func TestExpectFlaggedScoresFlagBoundaryOnly(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, "corpus.yaml", `version: 1
cases:
  - id: shorthand
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {flagged: true}
    label_provenance: human
  - id: verdict
    source: {kind: file, ref: b.jpg, media_type: image}
    expect: {verdict: block}
    label_provenance: human
`)
	c, err := Load(path, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	shorthand, verdict := c.Cases[0], c.Cases[1]

	if !shorthand.AssertsFlagBoundary() {
		t.Error("expect.flagged must assert the flag boundary")
	}
	if shorthand.AssertsBlockBoundary() {
		t.Error("expect.flagged must NOT assert the block boundary")
	}
	if !verdict.AssertsFlagBoundary() || !verdict.AssertsBlockBoundary() {
		t.Error("expect.verdict asserts both boundaries")
	}
	counts := c.Counts()
	if counts.AssertsFlagBoundary != 2 {
		t.Errorf("flag-boundary denominator = %d, want 2", counts.AssertsFlagBoundary)
	}
	if counts.AssertsBlockBoundary != 1 {
		t.Errorf("block-boundary denominator = %d, want 1 — the shorthand case must be excluded", counts.AssertsBlockBoundary)
	}
}

// An expect.verdict case is graded by exact verdict: it asserts which of
// the three operator actions the asset should earn, not merely that
// something happened.
func TestGradeVerdictAgainstAnExpectedVerdict(t *testing.T) {
	path := writeManifest(t, t.TempDir(), "corpus.yaml", `version: 1
cases:
  - id: blocked
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: block}
    label_provenance: human
`)
	c, err := Load(path, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct {
		actual moderation.Verdict
		want   Grade
	}{
		{moderation.VerdictBlock, GradeMatch},
		{moderation.VerdictFlag, GradeMismatch},
		{moderation.VerdictAllow, GradeMismatch},
	} {
		if got := c.Cases[0].GradeVerdict(tc.actual); got != tc.want {
			t.Errorf("GradeVerdict(%q) = %q, want %q", tc.actual, got, tc.want)
		}
	}
}

// A case may assert nothing at all, or assert only per-category labels.
// Neither asserts anything at asset level, so neither is graded there —
// and a case that asserts nothing is not scored at all.
func TestCaseWithoutAnAssetLevelExpectationIsNotGradedThere(t *testing.T) {
	path := writeManifest(t, t.TempDir(), "corpus.yaml", `version: 1
cases:
  - id: observe-only
    source: {kind: file, ref: a.jpg, media_type: image}
    label_provenance: human
  - id: categories-only
    source: {kind: file, ref: b.jpg, media_type: image}
    expect:
      categories: {SEXUAL: block}
    label_provenance: human
`)
	c, err := Load(path, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	observe, cats := c.Cases[0], c.Cases[1]

	if observe.Expect.Asserted() {
		t.Error("a case with no expect asserted something")
	}
	if observe.Scoreable() {
		t.Error("a case with no expect must not be scoreable")
	}
	if g := observe.GradeVerdict(moderation.VerdictBlock); g != GradeNotScored {
		t.Errorf("GradeVerdict = %q, want %q", g, GradeNotScored)
	}

	if !cats.Expect.Asserted() || !cats.Scoreable() {
		t.Error("a category-only expectation is an assertion")
	}
	if got, ok := cats.Expect.Category(moderation.CategorySexual); !ok || got != moderation.VerdictBlock {
		t.Errorf("Category(SEXUAL) = %q,%v want block,true", got, ok)
	}
	if cats.AssertsFlagBoundary() || cats.AssertsBlockBoundary() {
		t.Error("a category-only case asserts no asset-level boundary")
	}
	if g := cats.GradeVerdict(moderation.VerdictBlock); g != GradeNotScored {
		t.Errorf("GradeVerdict = %q, want %q — nothing was asserted at asset level", g, GradeNotScored)
	}
	if counts := c.Counts(); counts.ObserveOnly != 1 || counts.Scoreable != 1 {
		t.Errorf("Counts = %+v, want ObserveOnly 1, Scoreable 1", counts)
	}
}

// error is an outcome of the environment, not a property of an asset.
func TestExpectErrorVerdictRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, "corpus.yaml", `version: 1
cases:
  - id: case-0007
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: error}
    label_provenance: human
`)
	_, err := Load(path, testOptions())
	if err == nil {
		t.Fatal("expect.verdict: error accepted")
	}
	for _, want := range []string{"case-0007", "error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// The same rule holds per category.
	path = writeManifest(t, dir, "cat.yaml", `version: 1
cases:
  - id: case-0008
    source: {kind: file, ref: a.jpg, media_type: image}
    expect:
      verdict: block
      categories: {SEXUAL: error}
    label_provenance: human
`)
	if _, err := Load(path, testOptions()); err == nil || !strings.Contains(err.Error(), "case-0008") {
		t.Errorf("categories: {SEXUAL: error} = %v, want a load error naming the case", err)
	}
}

// top_category is decided by adapter emission order when scores tie, and
// confidence is a copy of max_score. Both are known-defective, so a corpus
// asserting on them is measuring a defect and gets refused.
func TestAssertingOnDefectiveFieldsRejected(t *testing.T) {
	for _, tc := range []struct {
		field string
		body  string
	}{
		{"top_category", `version: 1
cases:
  - id: case-0009
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: block, top_category: SEXUAL}
    label_provenance: human
`},
		{"confidence", `version: 1
cases:
  - id: case-0009
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: block, confidence: 0.9}
    label_provenance: human
`},
	} {
		t.Run(tc.field, func(t *testing.T) {
			path := writeManifest(t, t.TempDir(), "corpus.yaml", tc.body)
			_, err := Load(path, testOptions())
			if err == nil {
				t.Fatalf("expect.%s accepted", tc.field)
			}
			for _, want := range []string{"case-0009", tc.field} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestUnknownEnumsAndDuplicateIDsAreLoadErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"version", `version: 2
cases:
  - id: case-0001
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: allow}
    label_provenance: human
`, []string{"version", "2"}},
		{"source kind", `version: 1
cases:
  - id: case-0001
    source: {kind: s3, ref: a.jpg, media_type: image}
    expect: {verdict: allow}
    label_provenance: human
`, []string{"case-0001", "s3"}},
		{"media type", `version: 1
cases:
  - id: case-0001
    source: {kind: file, ref: a.wav, media_type: audio}
    expect: {verdict: allow}
    label_provenance: human
`, []string{"case-0001", "audio"}},
		{"label provenance", `version: 1
cases:
  - id: case-0001
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: allow}
    label_provenance: guessed
`, []string{"case-0001", "guessed"}},
		{"missing label provenance", `version: 1
cases:
  - id: case-0001
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: allow}
`, []string{"case-0001", "label_provenance"}},
		{"unknown category", `version: 1
cases:
  - id: case-0001
    source: {kind: file, ref: a.jpg, media_type: image}
    expect:
      verdict: allow
      categories: {ADULT: allow}
    label_provenance: human
`, []string{"case-0001", "ADULT"}},
		{"unknown category verdict", `version: 1
cases:
  - id: case-0001
    source: {kind: file, ref: a.jpg, media_type: image}
    expect:
      verdict: allow
      categories: {SEXUAL: maybe}
    label_provenance: human
`, []string{"case-0001", "maybe"}},
		{"duplicate id", `version: 1
cases:
  - id: dupe
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: allow}
    label_provenance: human
  - id: dupe
    source: {kind: file, ref: b.jpg, media_type: image}
    expect: {verdict: block}
    label_provenance: human
`, []string{"dupe", "duplicate"}},
		{"missing id", `version: 1
cases:
  - source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: allow}
    label_provenance: human
`, []string{"id"}},
		{"missing ref", `version: 1
cases:
  - id: case-0001
    source: {kind: file, media_type: image}
    expect: {verdict: allow}
    label_provenance: human
`, []string{"case-0001", "ref"}},
		{"non-http url ref", `version: 1
cases:
  - id: case-0001
    source: {kind: url, ref: "gs://bucket/a.jpg", media_type: image}
    expect: {verdict: allow}
    label_provenance: human
`, []string{"case-0001", "gs"}},
		{"unknown field", `version: 1
cases:
  - id: case-0001
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: allow}
    label_provenance: human
    expected: block
`, []string{"expected"}},
		{"no cases", `version: 1
cases: []
`, []string{"cases"}},
		{"not yaml", "\tversion: 1\n", []string{"corpus"}},
		{"empty file", "", []string{"empty"}},
		{"two documents", validManifest + "---\nversion: 1\n", []string{"document"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeManifest(t, t.TempDir(), "corpus.yaml", tc.body)
			_, err := Load(path, testOptions())
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

func TestLoadMissingFileIsAnError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), testOptions())
	if err == nil {
		t.Fatal("a missing manifest loaded")
	}
	if !strings.Contains(err.Error(), "nope.yaml") {
		t.Errorf("error %q does not name the path", err)
	}
}

// A per-case dedup override may tighten dedup or turn it off, never loosen
// it: every pair of 64-bit dHashes is within distance 64, so a wide-open
// threshold collapses a video into its first frame and lets that one frame
// decide the verdict. That is a fail-open, and it is refused at load.
func TestDedupThresholdAboveCeilingRejected(t *testing.T) {
	manifest := func(v string) string {
		return `version: 1
cases:
  - id: clip-1
    source: {kind: file, ref: a.mp4, media_type: video}
    dedup_threshold: ` + v + `
    expect: {verdict: block}
    label_provenance: human
`
	}
	path := writeManifest(t, t.TempDir(), "corpus.yaml", manifest("9"))
	_, err := Load(path, testOptions())
	if err == nil {
		t.Fatal("dedup_threshold 9 accepted against a ceiling of 8")
	}
	for _, want := range []string{"clip-1", "dedup_threshold", "8"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if _, err := Load(writeManifest(t, t.TempDir(), "c.yaml", manifest("-2")), testOptions()); err == nil {
		t.Error("dedup_threshold -2 accepted; only -1 disables")
	}
	for _, ok := range []string{"8", "0", "-1"} {
		if _, err := Load(writeManifest(t, t.TempDir(), "c.yaml", manifest(ok)), testOptions()); err != nil {
			t.Errorf("dedup_threshold %s rejected: %v", ok, err)
		}
	}
}

// The two are mutually exclusive within one case: a case that carries both
// holds two expectations that can disagree, and no rule picks a winner.
func TestExpectFlaggedAndVerdictTogetherRejected(t *testing.T) {
	path := writeManifest(t, t.TempDir(), "corpus.yaml", `version: 1
cases:
  - id: case-0042
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {verdict: block, flagged: true}
    label_provenance: human
`)
	_, err := Load(path, testOptions())
	if err == nil {
		t.Fatal("a case carrying both expect.verdict and expect.flagged was accepted")
	}
	for _, want := range []string{"case-0042", "verdict", "flagged"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A boolean corpus must still fail against a dead pipeline. An error
// outcome leaves the precision/recall denominators and lands in the gated
// abstention rate — it is never quietly counted as "not harmful".
func TestFlaggedFalseDoesNotMatchErrorOutcome(t *testing.T) {
	path := writeManifest(t, t.TempDir(), "corpus.yaml", `version: 1
cases:
  - id: shorthand-false
    source: {kind: file, ref: a.jpg, media_type: image}
    expect: {flagged: false}
    label_provenance: human
  - id: verdict-allow
    source: {kind: file, ref: b.jpg, media_type: image}
    expect: {verdict: allow}
    label_provenance: human
  - id: shorthand-true
    source: {kind: file, ref: c.jpg, media_type: image}
    expect: {flagged: true}
    label_provenance: human
`)
	c, err := Load(path, testOptions())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, cs := range c.Cases {
		got := cs.GradeVerdict(moderation.VerdictError)
		if got == GradeMatch {
			t.Errorf("%s: an error outcome graded as a match", cs.ID)
		}
		if got != GradeAbstained {
			t.Errorf("%s: GradeVerdict(error) = %q, want %q", cs.ID, got, GradeAbstained)
		}
	}
	if !Abstains(moderation.VerdictError) {
		t.Error("Abstains(error) = false")
	}
	for _, v := range []moderation.Verdict{moderation.VerdictAllow, moderation.VerdictFlag, moderation.VerdictBlock} {
		if Abstains(v) {
			t.Errorf("Abstains(%q) = true", v)
		}
	}
}

// The ceiling a manifest is validated against is the operator's configured
// frames.dedup.hamming_threshold, not a constant this package invents.
func TestOptionsFromConfigUsesTheConfiguredCeiling(t *testing.T) {
	cfg := config.Defaults()
	cfg.Frames.Dedup.HammingThreshold = 3
	if got := OptionsFromConfig(cfg); got.DedupCeiling != 3 {
		t.Errorf("DedupCeiling = %d, want 3", got.DedupCeiling)
	}
}
