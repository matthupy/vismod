package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/matthupy/vismod/internal/result"
)

// runRoot executes the full cobra tree with args and captures stdout.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

func TestScanCommandEndToEnd(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "pic.jpg")
	if err := os.WriteFile(img, []byte("end-to-end-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, "scan", img)
	if err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}

	var env result.ResultEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("scan output is not a valid envelope: %v\n%s", err, out)
	}
	if env.Result == nil {
		t.Fatal("envelope must carry a result")
	}
	if env.Result.SchemaVersion == "" {
		t.Error("schema_version must be stamped")
	}
	if env.ModelID.Adapter != "stub" {
		t.Errorf("adapter = %q, want stub", env.ModelID.Adapter)
	}
	if env.ModelID.ConfigHash == "" {
		t.Error("config_hash must be stamped")
	}
	if env.Result.AssetID != img {
		t.Errorf("asset_id = %q, want %q", env.Result.AssetID, img)
	}
	if env.Result.Overall.Verdict == "" {
		t.Error("overall verdict must be set")
	}
}

func TestScanMissingFileIsErrorNeverAllow(t *testing.T) {
	out, _ := runRoot(t, "scan", "does-not-exist.jpg")
	var env result.ResultEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("expected an envelope even on read failure: %v\n%s", err, out)
	}
	if env.Result == nil || env.Result.Overall.Verdict != "error" {
		t.Fatalf("missing file must yield verdict=error (never allow), got %s", out)
	}
}

func TestAdaptersCommandListsStub(t *testing.T) {
	out, err := runRoot(t, "adapters")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(out), []byte("stub")) {
		t.Fatalf("adapters output must list stub:\n%s", out)
	}
}

func TestVersionCommand(t *testing.T) {
	out, err := runRoot(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("version must print something")
	}
}

func TestAuditVerifyCommandOnEmpty(t *testing.T) {
	// Verifying a non-existent log is an intact (empty) chain.
	path := filepath.Join(t.TempDir(), "nope.log")
	if _, err := runRoot(t, "audit", "verify", path); err != nil {
		t.Fatalf("verify of empty chain should pass: %v", err)
	}
}
