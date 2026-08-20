package corpus

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The flat corpus columns. ref is required; expected_flagged is optional
// per row, and an empty cell means NOT ASSERTED — the case is scanned and
// reported, never scored.
const (
	csvRefColumn     = "ref"
	csvFlaggedColumn = "expected_flagged"
)

// videoExts reproduces intake's extension table
// (internal/cli/scan.go:20). That rule is unexported and lives in
// internal/cli, so the converter cannot call it;
// TestCSVMediaTypeInferenceMatchesIntake reads intake's table out of the
// source and pins the two in agreement.
var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".mkv": true, ".webm": true,
	".avi": true, ".m4v": true, ".mpg": true, ".mpeg": true, ".ts": true,
}

// schemeRef matches a ref that names a url scheme.
var schemeRef = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*://`)

// MediaTypeFor infers a case's media type from its ref, by the same
// extension rule intake applies: a known video extension is video and
// EVERYTHING else — including an extension nobody has heard of — is image.
//
// For a url the rule is applied to the path, not the whole ref: a
// presigned url carries a query string, and every signed video would
// otherwise read as an image.
func MediaTypeFor(ref string) string {
	p := ref
	if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
		p = u.Path
	}
	if videoExts[strings.ToLower(filepath.Ext(p))] {
		return MediaTypeVideo
	}
	return MediaTypeImage
}

// Convert desugars a flat `ref,expected_flagged` CSV into the bytes of a
// v1 manifest.
//
// The CSV is a converter, not a second input format: what it produces is
// an ordinary manifest, which is then loaded, validated and digested by
// the one loader. Nothing here scores, and nothing here digests.
//
// Inference is fixed and small: an https:// ref is kind url and anything
// else is a path; media_type comes from the extension; ids are assigned
// case-0001… in file order; label_provenance is human for every row,
// which is safe only because derived — the value that excludes a case
// from scoring — is one a CSV cannot express. An operator with derived
// labels writes a manifest. Relative refs resolve against the CSV's own
// directory and are recorded resolved, so the generated artifact is
// unambiguous about what ran.
func Convert(csvPath string) ([]byte, error) {
	raw, err := os.ReadFile(csvPath)
	if err != nil {
		return nil, fmt.Errorf("corpus: %w", err)
	}
	dir, err := filepath.Abs(filepath.Dir(csvPath))
	if err != nil {
		return nil, fmt.Errorf("corpus %s: resolve csv directory: %w", csvPath, err)
	}
	doc, err := desugar(raw, dir)
	if err != nil {
		return nil, fmt.Errorf("corpus %s: %w", csvPath, err)
	}
	// Prove the generated document passes the one validator before it is
	// offered to anyone as a manifest.
	if _, err := validate(doc, dir, Options{}); err != nil {
		return nil, fmt.Errorf("corpus %s: generated manifest is invalid: %w", csvPath, err)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := errors.Join(enc.Encode(doc), enc.Close()); err != nil {
		return nil, fmt.Errorf("corpus %s: render manifest: %w", csvPath, err)
	}
	return buf.Bytes(), nil
}

// LoadCSV desugars a flat CSV, materializes the generated v1 manifest at
// manifestPath — next to the run output, where it is the artifact of
// record — and loads it.
//
// The corpus digest is that file's SHA-256, never the CSV's. Two digest
// rules would let two different files claim one corpus identity, and a run
// record naming it could not be reproduced from the artifact it names.
func LoadCSV(csvPath, manifestPath string, opts Options) (*Corpus, error) {
	manifest, err := Convert(csvPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return nil, fmt.Errorf("corpus: create manifest directory: %w", err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		return nil, fmt.Errorf("corpus: materialize manifest: %w", err)
	}
	return Load(manifestPath, opts)
}

// desugar builds the v1 document a CSV stands for. Every refusal names the
// line, because that is the thing the operator has to go and fix.
func desugar(raw []byte, dir string) (manifestDoc, error) {
	var doc manifestDoc
	r := csv.NewReader(bytes.NewReader(raw))
	// Rows are checked here rather than by the reader: a row may omit the
	// trailing expected_flagged column entirely, which means not asserted.
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return doc, errors.New("csv is empty: want a ref,expected_flagged header row")
	}
	if err != nil {
		return doc, fmt.Errorf("csv: %w", err)
	}
	refCol, flaggedCol := -1, -1
	for i, h := range header {
		// A spreadsheet export leads with a byte-order mark.
		switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))) {
		case csvRefColumn:
			refCol = i
		case csvFlaggedColumn:
			flaggedCol = i
		}
	}
	if refCol < 0 {
		return doc, fmt.Errorf("line 1: csv header has no %q column, got %q", csvRefColumn, strings.Join(header, ","))
	}
	if flaggedCol < 0 {
		return doc, fmt.Errorf("line 1: csv header has no %q column, got %q", csvFlaggedColumn, strings.Join(header, ","))
	}

	seen := make(map[string]int)
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return doc, fmt.Errorf("csv: %w", err)
		}
		line, _ := r.FieldPos(0)
		if len(rec) > len(header) {
			return doc, fmt.Errorf("line %d: %d fields, want at most %d (%s,%s)", line, len(rec), len(header), csvRefColumn, csvFlaggedColumn)
		}
		cd, err := caseFromRow(field(rec, refCol), field(rec, flaggedCol), dir, len(doc.Cases)+1)
		if err != nil {
			return doc, fmt.Errorf("line %d: %v", line, err)
		}
		if prev, dup := seen[cd.Source.Ref]; dup {
			return doc, fmt.Errorf("line %d: duplicate ref %q, already on line %d", line, cd.Source.Ref, prev)
		}
		seen[cd.Source.Ref] = line
		doc.Cases = append(doc.Cases, cd)
	}
	if len(doc.Cases) == 0 {
		return doc, errors.New("csv has no cases: a header row and nothing else measures nothing")
	}
	version := SchemaVersion
	doc.Version = &version
	return doc, nil
}

func caseFromRow(rawRef, rawFlagged, dir string, n int) (caseDoc, error) {
	ref := strings.TrimSpace(rawRef)
	if ref == "" {
		return caseDoc{}, fmt.Errorf("%s is required", csvRefColumn)
	}
	kind := KindFile
	switch {
	case strings.HasPrefix(ref, "https://"):
		kind = KindURL
	case schemeRef.MatchString(ref):
		// Silently treating "http://host/a.jpg" as a file path would
		// produce a case pointing at a directory that cannot exist.
		return caseDoc{}, fmt.Errorf("%q: a csv ref is an https url or a path; write a manifest if you need another scheme", ref)
	}
	flagged, err := parseExpectedFlagged(rawFlagged)
	if err != nil {
		return caseDoc{}, err
	}
	resolved := ref
	if kind == KindFile {
		resolved = resolveRef(dir, ref)
	}
	return caseDoc{
		ID: fmt.Sprintf("case-%04d", n),
		Source: sourceDoc{
			Kind:      string(kind),
			Ref:       resolved,
			MediaType: MediaTypeFor(ref),
		},
		Expect:          &expectDoc{Flagged: flagged},
		LabelProvenance: string(ProvenanceHuman),
	}, nil
}

// parseExpectedFlagged reads one expected_flagged cell. An empty cell is
// nil — not asserted — and never false: "nobody said" and "expected not
// harmful" are different claims, and folding them would score cases the
// operator never labelled.
func parseExpectedFlagged(s string) (*bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return nil, fmt.Errorf("%s %q is neither true nor false; leave it empty for not asserted", csvFlaggedColumn, s)
	}
	return &v, nil
}

// field reads one column out of a row that may be short.
func field(rec []string, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return rec[i]
}
