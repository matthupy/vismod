package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/audit"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// The audit record binds a verdict to its inputs by hash: SHA-256(Raw).
// For every shipped adapter that binding was empty — the pipeline read
// res.Frames[0] out of the adapter's result and dropped Raw with the
// rest. These tests pin the binding down to the byte, because a digest
// that is merely non-empty proves nothing about what it attests to.

// fakeRawFor recomputes the fake adapter's raw response independently of
// the pipeline under test.
func fakeRawFor(t *testing.T, content string, score float64) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(fakeRawBody{Frame: content, Score: score})
	if err != nil {
		t.Fatalf("marshal fake raw: %v", err)
	}
	return b
}

func digestOf(t *testing.T, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// arrayDigest is the expected video binding: a JSON array with one entry
// per scanned frame, in timestamp order.
func arrayDigest(t *testing.T, raws ...json.RawMessage) string {
	t.Helper()
	b, err := json.Marshal(raws)
	if err != nil {
		t.Fatalf("marshal raw array: %v", err)
	}
	return digestOf(t, b)
}

func TestImageJobBindsTheProviderResponseByHash(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, _ := newTestPipeline(t, mod, nil)

	env, disp, err := p.ProcessJob(context.Background(), imageJob(writeInput(t, "benign")))
	if err != nil || disp != queue.Ack {
		t.Fatalf("disp=%v err=%v", disp, err)
	}

	want := digestOf(t, fakeRawFor(t, "benign", 0.05))
	if env.RawSHA256 != want {
		t.Errorf("RawSHA256 = %q, want %q", env.RawSHA256, want)
	}
	// The evidence is hashed and dropped; it must not ride along.
	if len(env.Result.Raw) != 0 {
		t.Errorf("envelope carries provider Raw (%d bytes); it must stop at the pipeline", len(env.Result.Raw))
	}
}

func TestVideoJobHashesEveryFrameInTimestampOrder(t *testing.T) {
	dir := t.TempDir()
	fs := &fakeFrameSource{dir: dir, contents: []string{"f0", "f1", "f2"}}
	mod := &fakeModerator{scores: map[string]float64{"f0": 0.1, "f1": 0.2, "f2": 0.9}}
	p, _ := newTestPipeline(t, mod, fs)

	env, _, _ := p.ProcessJob(context.Background(), videoJob(filepath.Join(dir, "v.mp4")))

	want := arrayDigest(t,
		fakeRawFor(t, "f0", 0.1),
		fakeRawFor(t, "f1", 0.2),
		fakeRawFor(t, "f2", 0.9),
	)
	if env.RawSHA256 != want {
		t.Errorf("RawSHA256 = %q, want %q (one array entry per frame, timestamp order)", env.RawSHA256, want)
	}
	if len(env.Result.Frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(env.Result.Frames))
	}
}

// TestRawEvidenceStaysPairedWithItsFrame is the regression guard for the
// shape that regresses quietly: the fan-out writes by EXTRACTION index
// and the sort reorders by TIMESTAMP. Here the two disagree, so a digest
// built over extraction order — the bug a second, independently-sorted
// slice would produce — is a different value, and this test names it.
func TestRawEvidenceStaysPairedWithItsFrameAfterTheSort(t *testing.T) {
	dir := t.TempDir()
	fs := &fakeFrameSource{
		dir:        dir,
		contents:   []string{"late", "early", "middle"},
		timestamps: []float64{9, 1, 5},
	}
	mod := &fakeModerator{scores: map[string]float64{"late": 0.1, "early": 0.2, "middle": 0.3}}
	p, _ := newTestPipeline(t, mod, fs)

	env, _, _ := p.ProcessJob(context.Background(), videoJob(filepath.Join(dir, "v.mp4")))

	sorted := arrayDigest(t,
		fakeRawFor(t, "early", 0.2),
		fakeRawFor(t, "middle", 0.3),
		fakeRawFor(t, "late", 0.1),
	)
	extraction := arrayDigest(t,
		fakeRawFor(t, "late", 0.1),
		fakeRawFor(t, "early", 0.2),
		fakeRawFor(t, "middle", 0.3),
	)
	if sorted == extraction {
		t.Fatal("test is not discriminating: pick timestamps that reorder the frames")
	}
	if env.RawSHA256 == extraction {
		t.Fatal("raw evidence kept extraction order while frames were sorted by timestamp: the two have desynchronized")
	}
	if env.RawSHA256 != sorted {
		t.Errorf("RawSHA256 = %q, want %q", env.RawSHA256, sorted)
	}
	// And the frames really did move, so the comparison above means what
	// it says.
	var gotTS []float64
	for _, fr := range env.Result.Frames {
		gotTS = append(gotTS, *fr.TimestampSec)
	}
	if gotTS[0] != 1 || gotTS[1] != 5 || gotTS[2] != 9 {
		t.Errorf("frame timestamps = %v, want [1 5 9]", gotTS)
	}
}

// A frame with no provider response holds its place as JSON null, so
// position i in the evidence always names frame i.
func TestFailedFrameIsNullInTheRawEvidence(t *testing.T) {
	dir := t.TempDir()
	fs := &fakeFrameSource{dir: dir, contents: []string{"f0", "boom", "f2"}}
	mod := &fakeModerator{
		scores: map[string]float64{"f0": 0.1, "f2": 0.2},
		errOn:  map[string]error{"boom": errors.New("provider exploded")},
	}
	p, _ := newTestPipeline(t, mod, fs)

	env, _, _ := p.ProcessJob(context.Background(), videoJob(filepath.Join(dir, "v.mp4")))

	want := arrayDigest(t, fakeRawFor(t, "f0", 0.1), nil, fakeRawFor(t, "f2", 0.2))
	if env.RawSHA256 != want {
		t.Errorf("RawSHA256 = %q, want %q (failed frame holds its index as null)", env.RawSHA256, want)
	}
	if env.Result.Frames[1].Status != moderation.FrameError {
		t.Errorf("frames[1].Status = %v, want error", env.Result.Frames[1].Status)
	}
}

// No response from any frame is no evidence. Reporting a digest there
// would attest to nothing while looking exactly like a real binding.
func TestNoProviderResponseYieldsNoDigest(t *testing.T) {
	dir := t.TempDir()
	fs := &fakeFrameSource{dir: dir, contents: []string{"a", "b"}}
	mod := &fakeModerator{errOn: map[string]error{
		"a": errors.New("down"),
		"b": errors.New("down"),
	}}
	p, _ := newTestPipeline(t, mod, fs)

	env, disp, _ := p.ProcessJob(context.Background(), videoJob(filepath.Join(dir, "v.mp4")))

	if env.RawSHA256 != "" {
		t.Errorf("RawSHA256 = %q, want empty when no frame produced a response", env.RawSHA256)
	}
	// Fail-safe is unchanged by any of this.
	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("verdict = %s, want error", env.Result.Overall.Verdict)
	}
	if disp != queue.DeadLetter {
		t.Errorf("disp = %v, want dead-letter", disp)
	}
}

// The whole point of the field: the same input must produce the same
// digest on every run, or it cannot be compared against anything.
func TestRawEvidenceIsDeterministicAcrossRuns(t *testing.T) {
	run := func(jobID queue.JobID) string {
		dir := t.TempDir()
		fs := &fakeFrameSource{dir: dir, contents: []string{"f0", "f1", "f2", "f3"}}
		mod := &fakeModerator{scores: map[string]float64{"f0": 0.1, "f1": 0.2, "f2": 0.3, "f3": 0.4}}
		p, _ := newTestPipeline(t, mod, fs)
		p.Concurrency = 4 // fan out wide: ordering must not come from luck
		j := videoJob(filepath.Join(dir, "v.mp4"))
		j.ID = jobID
		env, _, _ := p.ProcessJob(context.Background(), j)
		return env.RawSHA256
	}

	first := run("v1")
	if first == "" {
		t.Fatal("no digest produced")
	}
	for i := range 5 {
		if got := run(queue.JobID("v" + string(rune('a'+i)))); got != first {
			t.Fatalf("run %d digest = %q, want %q: raw evidence is not deterministic", i, got, first)
		}
	}
}

// Invariant 3: no provider Raw in any envelope. The digest is carried for
// the audit log alone and must not appear in sink output either — nothing
// downstream asked for it, and a field that leaks by default is how Raw
// would follow.
func TestSinkEnvelopeCarriesNeitherRawNorItsDigest(t *testing.T) {
	dir := t.TempDir()
	fs := &fakeFrameSource{dir: dir, contents: []string{"f0", "f1"}}
	mod := &fakeModerator{scores: map[string]float64{"f0": 0.1, "f1": 0.2}}
	p, buf := newTestPipeline(t, mod, fs)

	env, _, _ := p.ProcessJob(context.Background(), videoJob(filepath.Join(dir, "v.mp4")))
	if env.RawSHA256 == "" {
		t.Fatal("no digest produced; the rest of this test would pass vacuously")
	}

	line := buf.String()
	for _, needle := range []string{`"raw"`, `"raw_sha256"`, `"frame":"f0"`} {
		if strings.Contains(line, needle) {
			t.Errorf("sink envelope contains %s:\n%s", needle, line)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &decoded); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if _, present := decoded["raw_sha256"]; present {
		t.Error("raw_sha256 must not be serialized into the sink envelope")
	}
	res, _ := decoded["result"].(map[string]any)
	if _, present := res["raw"]; present {
		t.Error("result.raw must not be serialized into the sink envelope")
	}
}

// End to end: the value the pipeline computed is the value the audit log
// stores, and the chain still verifies over it.
func TestAuditRecordCarriesTheComputedDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	log, err := audit.Open(path, nil)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	defer log.Close()

	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, _ := newTestPipeline(t, mod, nil)
	p.Audit = log

	env, disp, err := p.ProcessJob(context.Background(), imageJob(writeInput(t, "benign")))
	if err != nil || disp != queue.Ack {
		t.Fatalf("disp=%v err=%v", disp, err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	line, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var rec audit.Record
	if err := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(string(line)), "\n", 2)[0]), &rec); err != nil {
		t.Fatalf("decode record: %v", err)
	}

	want := digestOf(t, fakeRawFor(t, "benign", 0.05))
	if rec.Payload["raw_sha256"] != want {
		t.Errorf("audit raw_sha256 = %q, want %q", rec.Payload["raw_sha256"], want)
	}
	if rec.Payload["raw_sha256"] != env.RawSHA256 {
		t.Errorf("audit record (%q) disagrees with the envelope (%q)", rec.Payload["raw_sha256"], env.RawSHA256)
	}
	// The digest is a hash OF the response, never the response.
	if strings.Contains(string(line), `"frame":"benign"`) {
		t.Fatal("audit log stored the provider response itself")
	}
	if n, err := audit.Verify(path); err != nil {
		t.Fatalf("Verify after %d records: %v", n, err)
	}
}

func TestBuildRawEvidence(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }

	tests := []struct {
		name     string
		outcomes []frameOutcome
		want     string
	}{
		{"all present", []frameOutcome{{raw: raw(`{"a":1}`)}, {raw: raw(`{"b":2}`)}}, `[{"a":1},{"b":2}]`},
		{"failure holds its index", []frameOutcome{{raw: raw(`{"a":1}`)}, {}, {raw: raw(`{"c":3}`)}}, `[{"a":1},null,{"c":3}]`},
		{"leading failure", []frameOutcome{{}, {raw: raw(`{"b":2}`)}}, `[null,{"b":2}]`},
		{"no evidence at all", []frameOutcome{{}, {}}, ""},
		{"no frames", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildRawEvidence(tc.outcomes)
			if err != nil {
				t.Fatalf("buildRawEvidence: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("= %s, want %s", got, tc.want)
			}
		})
	}
}
