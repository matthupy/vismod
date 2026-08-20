package corpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/vismod/vismod/pkg/moderation"
)

// manifestDoc is the on-disk v1 shape. It is decoded with KnownFields set,
// so a misspelled key is a load error rather than a silently dropped
// expectation — a corpus that loses an assertion still reports a number,
// and that number is wrong in a flattering direction.
type manifestDoc struct {
	// Version is a pointer so "absent" is distinguishable from 0.
	Version *int      `yaml:"version"`
	Cases   []caseDoc `yaml:"cases"`
}

type caseDoc struct {
	ID              string     `yaml:"id"`
	Source          sourceDoc  `yaml:"source"`
	Expect          *expectDoc `yaml:"expect,omitempty"`
	LabelProvenance string     `yaml:"label_provenance"`
	Workflows       []string   `yaml:"workflows,omitempty"`
	DedupThreshold  *int       `yaml:"dedup_threshold,omitempty"`
	Notes           string     `yaml:"notes,omitempty"`
}

type sourceDoc struct {
	Kind      string `yaml:"kind"`
	Ref       string `yaml:"ref"`
	MediaType string `yaml:"media_type"`
}

type expectDoc struct {
	Verdict *string `yaml:"verdict,omitempty"`
	// Flagged carries no omitempty: a generated manifest writes
	// `flagged: null` rather than dropping the key, so "not asserted" is
	// visible in the artifact instead of being an absence a reader has to
	// infer.
	Flagged    *bool             `yaml:"flagged"`
	Categories map[string]string `yaml:"categories,omitempty"`

	// TopCategory and Confidence exist here only to be refused. Declaring
	// them beats KnownFields' generic "field not found": an operator who
	// asserts on them gets told WHY the field is not a scoring target.
	// They are `any` so that refusing them never depends on the shape of
	// the value someone wrote.
	TopCategory any `yaml:"top_category,omitempty"`
	Confidence  any `yaml:"confidence,omitempty"`
}

// Load reads, validates and digests a v1 corpus manifest.
//
// Relative file refs resolve against the MANIFEST's own directory, not the
// process working directory: a corpus that loads differently depending on
// where the operator was standing is not an artifact anyone can reproduce.
//
// The returned Digest is the SHA-256 of the manifest file's bytes. That is
// the corpus identity a run record names, and it is the only digest rule
// in this package.
func Load(path string, opts Options) (*Corpus, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("corpus: %w", err)
	}
	doc, err := decodeManifest(raw)
	if err != nil {
		return nil, fmt.Errorf("corpus %s: %w", path, err)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("corpus %s: resolve manifest directory: %w", path, err)
	}
	cases, err := validate(doc, dir, opts)
	if err != nil {
		return nil, fmt.Errorf("corpus %s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return &Corpus{
		Version: SchemaVersion,
		Path:    path,
		Digest:  hex.EncodeToString(sum[:]),
		Cases:   cases,
	}, nil
}

func decodeManifest(raw []byte) (manifestDoc, error) {
	var doc manifestDoc
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return doc, errors.New("manifest is empty")
		}
		return doc, fmt.Errorf("parse: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return doc, errors.New("manifest holds more than one YAML document; a corpus is exactly one")
	}
	return doc, nil
}

// validate turns the decoded document into cases, refusing anything that
// would make a later measurement dishonest.
func validate(doc manifestDoc, dir string, opts Options) ([]Case, error) {
	if doc.Version == nil {
		return nil, fmt.Errorf("missing version: this loader accepts version %d only", SchemaVersion)
	}
	if *doc.Version != SchemaVersion {
		return nil, fmt.Errorf("unknown version %d: this loader accepts version %d only", *doc.Version, SchemaVersion)
	}
	if len(doc.Cases) == 0 {
		return nil, errors.New("cases is empty: a corpus with no cases measures nothing")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	out := make([]Case, 0, len(doc.Cases))
	for i, cd := range doc.Cases {
		c, err := convertCase(cd, i, dir, opts)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[c.ID]; dup {
			return nil, fmt.Errorf("case %q: duplicate id", c.ID)
		}
		seen[c.ID] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}

func convertCase(cd caseDoc, index int, dir string, opts Options) (Case, error) {
	id := strings.TrimSpace(cd.ID)
	if id == "" {
		return Case{}, fmt.Errorf("cases[%d]: id is required", index)
	}
	fail := func(format string, a ...any) (Case, error) {
		return Case{}, fmt.Errorf("case %q: %s", id, fmt.Sprintf(format, a...))
	}

	src, err := convertSource(cd.Source, dir)
	if err != nil {
		return fail("%v", err)
	}

	prov, err := parseProvenance(cd.LabelProvenance)
	if err != nil {
		return fail("%v", err)
	}

	if cd.DedupThreshold != nil {
		if v := *cd.DedupThreshold; v < -1 || v > opts.DedupCeiling {
			return fail("dedup_threshold must be -1 (disable) or 0..%d (frames.dedup.hamming_threshold), got %d — a loosened threshold collapses a video into one frame, which is a fail-open", opts.DedupCeiling, v)
		}
	}
	for i, w := range cd.Workflows {
		if strings.TrimSpace(w) == "" {
			return fail("workflows[%d] is empty", i)
		}
	}

	expect, err := convertExpect(cd.Expect)
	if err != nil {
		return fail("%v", err)
	}

	return Case{
		ID:              id,
		Source:          src,
		Expect:          expect,
		LabelProvenance: prov,
		Workflows:       cd.Workflows,
		DedupThreshold:  cd.DedupThreshold,
		Notes:           cd.Notes,
	}, nil
}

func convertSource(sd sourceDoc, dir string) (Source, error) {
	kind := Kind(sd.Kind)
	if kind != KindFile && kind != KindURL {
		return Source{}, fmt.Errorf("unknown source.kind %q: want %q or %q", sd.Kind, KindFile, KindURL)
	}
	ref := strings.TrimSpace(sd.Ref)
	if ref == "" {
		return Source{}, errors.New("source.ref is required")
	}
	if sd.MediaType != MediaTypeImage && sd.MediaType != MediaTypeVideo {
		return Source{}, fmt.Errorf("unknown source.media_type %q: want %q or %q", sd.MediaType, MediaTypeImage, MediaTypeVideo)
	}
	if kind == KindURL {
		if err := validateURLRef(ref); err != nil {
			return Source{}, err
		}
		return Source{Kind: kind, Ref: ref, MediaType: sd.MediaType}, nil
	}
	return Source{Kind: kind, Ref: resolveRef(dir, ref), MediaType: sd.MediaType}, nil
}

// validateURLRef refuses a url the fetcher could never dial. It is
// deliberately no narrower than internal/fetch: https always, and http
// only because fetch permits it for an operator-allow-listed private host.
func validateURLRef(ref string) error {
	u, err := url.Parse(ref)
	if err != nil {
		return fmt.Errorf("source.ref is not a url: %v", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("source.ref scheme must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("source.ref %q has no host", ref)
	}
	return nil
}

// resolveRef anchors a relative ref to the manifest's directory.
//
// An already-rooted ref is left alone. filepath.IsAbs is not enough on
// Windows, where neither "/mnt/corpus/a.png" nor its cleaned form
// "\mnt\corpus\a.png" counts as absolute yet neither is relative to the
// manifest. Resolution has to be idempotent: a generated manifest records
// resolved refs and is then loaded through this same path, so a rooted ref
// that got re-anchored would land under the manifest's directory.
func resolveRef(dir, ref string) string {
	if filepath.IsAbs(ref) || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, `\`) {
		return filepath.Clean(ref)
	}
	return filepath.Join(dir, ref)
}

func parseProvenance(s string) (Provenance, error) {
	switch p := Provenance(s); p {
	case ProvenanceHuman, ProvenanceVendor, ProvenanceDerived:
		return p, nil
	case "":
		return "", fmt.Errorf("label_provenance is required (%q, %q or %q): a case whose provenance is unknown cannot be safely scored, because %q labels must be excluded",
			ProvenanceHuman, ProvenanceVendor, ProvenanceDerived, ProvenanceDerived)
	default:
		return "", fmt.Errorf("unknown label_provenance %q: want %q, %q or %q", s, ProvenanceHuman, ProvenanceVendor, ProvenanceDerived)
	}
}

func convertExpect(ed *expectDoc) (Expectation, error) {
	var out Expectation
	if ed == nil {
		return out, nil
	}
	if ed.TopCategory != nil {
		return out, errors.New("expect.top_category is not a scoring target: top_category is decided by adapter emission order when scores tie, so asserting on it measures a known defect")
	}
	if ed.Confidence != nil {
		return out, errors.New("expect.confidence is not a scoring target: confidence is a copy of max_score, so asserting on it measures a known defect")
	}
	if ed.Verdict != nil && ed.Flagged != nil {
		return out, errors.New("expect.verdict and expect.flagged are mutually exclusive within one case: keep the verdict, or keep the boolean shorthand")
	}
	if ed.Verdict != nil {
		v, err := parseExpectedVerdict(*ed.Verdict)
		if err != nil {
			return out, fmt.Errorf("expect.verdict: %v", err)
		}
		out.Verdict = &v
	}
	out.Flagged = ed.Flagged

	if len(ed.Categories) == 0 {
		return out, nil
	}
	names := make([]string, 0, len(ed.Categories))
	for name := range ed.Categories {
		names = append(names, name)
	}
	sort.Strings(names)
	out.categories = make(map[moderation.Category]moderation.Verdict, len(names))
	for _, name := range names {
		cat := moderation.Category(name)
		if moderation.Canonicalize(cat) != cat {
			return Expectation{}, fmt.Errorf("expect.categories: unknown category %q", name)
		}
		v, err := parseExpectedVerdict(ed.Categories[name])
		if err != nil {
			return Expectation{}, fmt.Errorf("expect.categories.%s: %v", name, err)
		}
		out.categories[cat] = v
	}
	return out, nil
}

// parseExpectedVerdict accepts the three verdicts an asset can be expected
// to earn. "error" is refused: it is an outcome of the environment — a
// provider outage, an unreadable file — not a property of the asset, and a
// corpus that expects it would grade an outage as a pass.
func parseExpectedVerdict(s string) (moderation.Verdict, error) {
	switch v := moderation.Verdict(s); v {
	case moderation.VerdictAllow, moderation.VerdictFlag, moderation.VerdictBlock:
		return v, nil
	case moderation.VerdictError:
		return "", errors.New(`"error" is not an expectation: it is an outcome of the environment, not a property of an asset. Errored cases land in the abstention rate instead`)
	default:
		return "", fmt.Errorf("unknown verdict %q: want %q, %q or %q", s, moderation.VerdictAllow, moderation.VerdictFlag, moderation.VerdictBlock)
	}
}
