package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/eval/corpus"
)

const flatCSV = `ref,expected_flagged
https://media.example.com/a.jpg,true
/mnt/corpus/b.png,false
./local/c.mp4,
`

func run(t *testing.T, out *bytes.Buffer, args ...string) error {
	t.Helper()
	cmd := newRootCmd(out)
	cmd.SetArgs(args)
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd.Execute()
}

func writeCSV(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "list.csv")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// The CSV is an on-ramp with a growth path, not a parallel dead end: what
// convert emits is a v1 manifest the loader takes unmodified.
func TestCorpusConvertEmitsLoadableManifest(t *testing.T) {
	csvDir := t.TempDir()
	csvPath := writeCSV(t, csvDir, flatCSV)
	out := filepath.Join(t.TempDir(), "corpus.yaml")

	var buf bytes.Buffer
	if err := run(t, &buf, "corpus", "convert", csvPath, "-o", out); err != nil {
		t.Fatalf("corpus convert: %v (%s)", err, buf.String())
	}

	c, err := corpus.Load(out, corpus.Options{DedupCeiling: 8})
	if err != nil {
		t.Fatalf("the emitted manifest does not load: %v", err)
	}
	if len(c.Cases) != 3 {
		t.Fatalf("len(Cases) = %d, want 3", len(c.Cases))
	}
	if got, want := c.Cases[2].Source.Ref, filepath.Join(csvDir, "local", "c.mp4"); got != want {
		t.Errorf("relative ref resolved to %q, want the CSV's own directory %q", got, want)
	}
	if got := c.Cases[0].Source.Ref; got != "https://media.example.com/a.jpg" {
		t.Errorf("url ref = %q, want it left alone", got)
	}
	if c.Cases[0].Expect.Flagged == nil || !*c.Cases[0].Expect.Flagged {
		t.Errorf("case-0001 expect.flagged = %v, want true", c.Cases[0].Expect.Flagged)
	}
	if c.Cases[2].Expect.Asserted() {
		t.Error("an empty expected_flagged became an assertion on the way through the file")
	}
}

func TestCorpusConvertWritesToStdoutWithoutO(t *testing.T) {
	csvPath := writeCSV(t, t.TempDir(), flatCSV)

	var buf bytes.Buffer
	if err := run(t, &buf, "corpus", "convert", csvPath); err != nil {
		t.Fatalf("corpus convert: %v", err)
	}
	for _, want := range []string{"version: 1", "case-0001", "label_provenance: human"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("stdout does not contain %q:\n%s", want, buf.String())
		}
	}
}

func TestCorpusConvertReportsBadInput(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := run(t, &buf, "corpus", "convert", filepath.Join(dir, "nope.csv")); err == nil {
		t.Error("converting a missing CSV succeeded")
	}

	csvPath := writeCSV(t, dir, "ref,expected_flagged\na.jpg,maybe\n")
	if err := run(t, &buf, "corpus", "convert", csvPath); err == nil {
		t.Error("converting an unparseable expected_flagged succeeded")
	}

	// csvPath is a file, so it cannot also be a parent directory.
	good := writeCSV(t, t.TempDir(), flatCSV)
	if err := run(t, &buf, "corpus", "convert", good, "-o", filepath.Join(csvPath, "corpus.yaml")); err == nil {
		t.Error("writing under a non-directory succeeded")
	}
}
