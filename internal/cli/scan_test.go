package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// scriptedModerator is a registered adapter whose verdict is decided by the
// CONTENT of the input file, so one registration covers allow, block and
// provider-failure inputs in the same scan — which is what the exit-code
// precedence rules need (2 outranks 1 outranks 0).
type scriptedModerator struct{}

func (scriptedModerator) Name() string         { return "cli-test" }
func (scriptedModerator) ModelVersion() string { return "cli-test-v1" }
func (scriptedModerator) Close() error         { return nil }

func (scriptedModerator) Capabilities() moderation.Caps {
	return moderation.Caps{
		MaxImageBytes: 32 << 20,
		Categories:    []moderation.Category{moderation.CategoryViolence},
	}
}

func (scriptedModerator) AnalyzeImage(_ context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	body := string(img.Bytes)
	if strings.HasPrefix(body, "ERROR") {
		return moderation.NormalizedResult{}, errors.New("scripted provider failure")
	}
	score := 0.01
	if strings.HasPrefix(body, "BLOCK") {
		score = 0.99 // above the 0.8 default block_at
	}
	return moderation.NormalizedResult{
		Provider: "cli-test",
		Frames: []moderation.FrameResult{{
			Status: moderation.FrameOK,
			Categories: []moderation.CategoryResult{{
				Category:      moderation.CategoryViolence,
				ProviderLabel: "cli-test/violence",
				Score:         &score,
				ScoreOrigin:   moderation.OriginProbability,
			}},
		}},
	}, nil
}

func init() {
	moderate.Register("cli-test", func(moderate.AdapterConfig) (moderation.Moderator, error) {
		return scriptedModerator{}, nil
	})
}

// scanConfig is a credential-free scan setup: the scripted adapter, audit
// off, stdout sink. Callers enable audit or change the adapter as needed.
func scanConfig() config.Config {
	c := config.Defaults()
	c.Adapter = config.AdapterSection{Name: "cli-test"}
	c.Audit = config.AuditConfig{Enabled: false}
	c.LogLevel = "error"
	return c
}

// withConfig installs cfg for the duration of a test. runScan and runServe
// read the package-level config the cobra PersistentPreRunE fills in, so a
// test has to stand in for that step and put it back afterwards.
func withConfig(t *testing.T, c config.Config) {
	t.Helper()
	prev := cfg
	cfg = c
	t.Cleanup(func() { cfg = prev })
}

// writeInput creates an input file whose first bytes drive the scripted
// adapter's verdict. The extension decides image vs video, so keep it .jpg
// to stay off the ffmpeg path.
func writeInput(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	return p
}

func scan(t *testing.T, args ...string) (int, string, error) {
	t.Helper()
	var out bytes.Buffer
	code, err := runScan(t.Context(), &out, args, scanOptions{})
	return code, out.String(), err
}

// TestRunScanAllowExitsZero: a benign input exits 0 AND emits an envelope.
// The exit code is the only thing a shell pipeline sees, so it must agree
// with the verdict that was actually written.
func TestRunScanAllowExitsZero(t *testing.T) {
	withConfig(t, scanConfig())
	code, out, err := scan(t, writeInput(t, "clean.jpg", "OK"))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 for an allowed input", code)
	}

	var env struct {
		Result *struct {
			Overall struct {
				Verdict string `json:"verdict"`
			} `json:"overall"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("scan wrote no parseable envelope (%q): %v", out, err)
	}
	if env.Result == nil || env.Result.Overall.Verdict != string(moderation.VerdictAllow) {
		t.Errorf("envelope verdict = %+v, want allow", env.Result)
	}
}

// TestRunScanBlockExitsOne: a blocked verdict must exit 1. Exiting 0 would
// let a calling pipeline publish content the model refused.
func TestRunScanBlockExitsOne(t *testing.T) {
	withConfig(t, scanConfig())
	code, out, err := scan(t, writeInput(t, "bad.jpg", "BLOCK"))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1 for a blocked input", code)
	}
	if !strings.Contains(out, string(moderation.VerdictBlock)) {
		t.Errorf("envelope does not carry the block verdict: %s", out)
	}
}

// TestRunScanProviderFailureExitsTwo is invariant 1 at the CLI boundary: an
// unscorable input is never an allow. A provider failure has to exit 2 and
// say verdict=error in the envelope, not exit 0 with a benign-looking one.
func TestRunScanProviderFailureExitsTwo(t *testing.T) {
	withConfig(t, scanConfig())
	code, out, err := scan(t, writeInput(t, "boom.jpg", "ERROR"))
	if err != nil {
		t.Fatalf("runScan returned a setup error for a per-job failure: %v", err)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2 for an unscorable input", code)
	}
	if !strings.Contains(out, string(moderation.VerdictError)) {
		t.Errorf("envelope must carry verdict=error, got: %s", out)
	}
	if strings.Contains(out, `"verdict":"allow"`) {
		t.Error("a provider failure reported as allow; invariant 1 is broken")
	}
}

// TestRunScanErrorOutranksFlag: with a mix of inputs the worst outcome
// wins, regardless of order. If a flagged file could mask an errored one
// the operator would never learn a file went unscanned.
func TestRunScanErrorOutranksFlag(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		return p
	}
	blocked := write("bad.jpg", "BLOCK")
	broken := write("boom.jpg", "ERROR")
	clean := write("ok.jpg", "OK")

	for _, order := range [][]string{
		{blocked, broken, clean},
		{broken, blocked, clean},
		{clean, blocked, broken},
	} {
		withConfig(t, scanConfig())
		var out bytes.Buffer
		code, err := runScan(t.Context(), &out, order, scanOptions{})
		if err != nil {
			t.Fatalf("runScan: %v", err)
		}
		if code != 2 {
			t.Errorf("exit code = %d for %v, want 2 (error outranks flag)", code, order)
		}
	}
}

// TestRunScanScansEveryInputAfterAFailure: one bad file must not abort the
// batch. Stopping early would leave later files silently unscanned while
// the process still reports a single failure.
func TestRunScanScansEveryInputAfterAFailure(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		return p
	}
	withConfig(t, scanConfig())

	var out bytes.Buffer
	code, err := runScan(t.Context(), &out, []string{write("boom.jpg", "ERROR"), write("a.jpg", "OK"), write("b.jpg", "OK")}, scanOptions{})
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if got := strings.Count(strings.TrimSpace(out.String()), "\n") + 1; got != 3 {
		t.Errorf("wrote %d envelopes, want 3 (one per input)", got)
	}
}

// TestRunScanMissingInputIsASetupError: a path that does not exist is an
// operator mistake, not a moderation outcome. It must surface as an error
// rather than as a scored verdict.
func TestRunScanMissingInputIsASetupError(t *testing.T) {
	withConfig(t, scanConfig())
	_, _, err := scan(t, filepath.Join(t.TempDir(), "not-here.jpg"))
	if err == nil {
		t.Fatal("a missing input must fail; it must not be scored")
	}
}

func TestRunScanRejectsUnknownWorkflow(t *testing.T) {
	withConfig(t, scanConfig())
	var out bytes.Buffer
	_, err := runScan(t.Context(), &out, []string{writeInput(t, "a.jpg", "OK")},
		scanOptions{Workflows: []string{"no-such-workflow"}})
	if err == nil {
		t.Fatal("an unknown workflow must be refused before scanning")
	}
}

func TestRunScanRejectsOutOfRangeDedupOverride(t *testing.T) {
	withConfig(t, scanConfig())
	bad := 65
	var out bytes.Buffer
	_, err := runScan(t.Context(), &out, []string{writeInput(t, "a.jpg", "OK")},
		scanOptions{DedupThreshold: &bad})
	if err == nil {
		t.Fatal("a dedup threshold above 64 must be refused before scanning")
	}
}

// TestRunScanUnknownAdapterFailsBeforeScanning: the adapter is chosen at
// boot and there is exactly one per process. A bad name must fail loudly
// instead of falling back to anything.
func TestRunScanUnknownAdapterFailsBeforeScanning(t *testing.T) {
	c := scanConfig()
	c.Adapter.Name = "not-a-vendor"
	withConfig(t, c)
	if _, _, err := scan(t, writeInput(t, "a.jpg", "OK")); err == nil {
		t.Fatal("an unregistered adapter must fail boot")
	}
}

// TestRunScanWritesAuditRecords: scan is auditable too, not just serve.
// One record per scanned input, in the same hash chain.
func TestRunScanWritesAuditRecords(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.log")
	c := scanConfig()
	c.Audit = config.AuditConfig{Enabled: true, Path: auditPath}
	withConfig(t, c)

	dir := t.TempDir()
	var inputs []string
	for _, name := range []string{"a.jpg", "b.jpg"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("OK"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		inputs = append(inputs, p)
	}

	var out bytes.Buffer
	if _, err := runScan(t.Context(), &out, inputs, scanOptions{}); err != nil {
		t.Fatalf("runScan: %v", err)
	}

	// The audit log is closed by runScan's defer, which only runs because
	// the exit code is returned rather than passed to os.Exit inside it.
	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(b)), "\n")); got != 2 {
		t.Errorf("audit log has %d records, want one per scanned input (2)", got)
	}
}

// TestRunScanUnwritableAuditPathRefusesToScan: running unaudited when audit
// is enabled would produce decisions no one can later account for.
func TestRunScanUnwritableAuditPathRefusesToScan(t *testing.T) {
	c := scanConfig()
	c.Audit = config.AuditConfig{Enabled: true, Path: t.TempDir()} // a directory
	withConfig(t, c)
	if _, _, err := scan(t, writeInput(t, "a.jpg", "OK")); err == nil {
		t.Fatal("an unopenable audit path must refuse to scan")
	}
}

// TestRunScanRejectsSinkThatGoesNowhere: results must land somewhere. An
// empty sink list would Ack every job into the void.
func TestRunScanRejectsSinkThatGoesNowhere(t *testing.T) {
	c := scanConfig()
	c.Output.Sinks = nil
	withConfig(t, c)
	if _, _, err := scan(t, writeInput(t, "a.jpg", "OK")); err == nil {
		t.Fatal("a scan with no configured sink must refuse to run")
	}
}

// TestRunScanVideoInputValidatesFFmpegFirst: video inputs go through frame
// extraction, so a missing binary has to fail at boot validation rather
// than per job. Image-only scans must NOT require ffmpeg (covered by every
// other test here, which uses .jpg inputs and no real binaries).
func TestRunScanVideoInputValidatesFFmpegFirst(t *testing.T) {
	c := scanConfig()
	c.FFmpeg.FFmpegPath = filepath.Join(t.TempDir(), "no-such-ffmpeg")
	c.FFmpeg.FFprobePath = filepath.Join(t.TempDir(), "no-such-ffprobe")
	withConfig(t, c)

	_, _, err := scan(t, writeInput(t, "clip.mp4", "OK"))
	if err == nil {
		t.Fatal("a video input with no ffmpeg must fail boot validation")
	}
	if !strings.Contains(err.Error(), "boot validation") {
		t.Errorf("error should name boot validation, got: %v", err)
	}
}

// Invalid metadata is a setup error: it fails BEFORE any scanning, so a
// typo never costs a billed vendor call.
func TestScanRejectsInvalidMetadata(t *testing.T) {
	for name, meta := range map[string]string{
		"array":    `["a"]`,
		"scalar":   `42`,
		"bad json": `{"a":`,
		"oversize": `{"k":"` + strings.Repeat("a", queue.MaxMetadataBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			opts := scanOptions{Metadata: json.RawMessage(meta)}
			code, err := runScan(context.Background(), io.Discard, []string{"nonexistent.png"}, opts)
			if err == nil {
				t.Fatalf("invalid metadata must be a setup error, got code=%d", code)
			}
			if !strings.Contains(err.Error(), "metadata") {
				t.Errorf("the error must name metadata, got %v", err)
			}
		})
	}
}
