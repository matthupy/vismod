# Pass-through Metadata Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a caller attach opaque JSON to a job and have it returned untouched on the result envelope, so a webhook receiver can correlate a verdict with the caller's own record.

**Architecture:** A new `Metadata json.RawMessage` field on `queue.Job` and `result.ResultEnvelope`. One validator, `queue.ValidateMetadata`, enforces object-shape and a 4 KiB compacted cap, and is called at all three entry points (HTTP intake, `scan` flag, pipeline execution). The pipeline copies the validated bytes onto the envelope; the audit log, slog output, and operator UI never receive it.

**Tech Stack:** Go 1.x, stdlib `encoding/json`, cobra (CLI flags), existing `internal/queue` / `internal/pipeline` / `internal/result` packages. No new module dependencies.

**Spec:** `docs/superpowers/specs/2026-08-03-passthrough-metadata-design.md`

## Global Constraints

- Done gate for every task: `go build ./... && go vet ./... && go test ./...` — all three exit 0.
- No new module imports. Everything here is stdlib or already-imported packages.
- Metadata is permitted **only** in the `queue.Job` payload and the `result.ResultEnvelope`. It must never appear in the audit log, any log line, or `internal/ui`.
- Metadata must never influence a verdict: no rollup, threshold, or `ConfigHash` input changes.
- Compacted cap is **4096 bytes** exactly (`queue.MaxMetadataBytes`).
- Absent metadata serializes as an omitted field (`omitempty`), never as `null`.
- Fail safe: invalid metadata discovered at execution time yields `verdict:"error"` + dead-letter, never `allow`.
- Windows/PowerShell 5.1: multi-line commit messages break with here-strings — write the message to a file and `git commit -F <file>`.

### Deviation from the spec (deliberate, carry it into the spec in Task 7)

The spec places `validateMetadata` in `internal/cli`. That creates an import cycle: `internal/cli` imports `internal/pipeline`, so `internal/pipeline` cannot import `internal/cli` to re-validate at execution time. The validator therefore lives in **`internal/queue`**, exported as `queue.ValidateMetadata`, which both `internal/cli` and `internal/pipeline` already import. It also **returns the compacted bytes**, so compaction cannot be forgotten at a call site.

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `internal/queue/queue.go` | Modify | `Job.Metadata` field; `MaxMetadataBytes`; `ValidateMetadata` |
| `internal/queue/metadata_test.go` | Create | Validator table tests |
| `internal/result/sink.go` | Modify | `ResultEnvelope.Metadata` field |
| `internal/result/sink_test.go` | Modify | `omitempty` regression guard |
| `internal/pipeline/pipeline.go` | Modify | Execution-time validation; stamp both envelope construction sites |
| `internal/pipeline/metadata_test.go` | Create | Pass-through, override path, invalid-at-execution, log non-leak |
| `internal/audit/metadata_test.go` | Create | Audit-chain non-leak guard |
| `internal/cli/serve.go` | Modify | `intakeRequest.Metadata`; `400` on invalid |
| `internal/cli/serve_test.go` | Modify | Intake accept/reject tests |
| `internal/cli/scan.go` | Modify | `--metadata` flag |
| `internal/cli/scan_test.go` | Modify | Flag validation test |
| `docs/rest-api.md` | Modify | Request-body row; `400` cause |
| `docs/result-envelope.md` | Modify | Field docs |
| `AGENTS.md` | Modify | Free-text done-gate carve-out |

---

### Task 1: The validator and the Job field

**Files:**
- Modify: `internal/queue/queue.go` (add to the `Job` struct at line 38; add validator after the `Job`/`Handler` declarations)
- Test: `internal/queue/metadata_test.go` (create)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `queue.Job.Metadata json.RawMessage` (JSON tag `metadata,omitempty`)
  - `const queue.MaxMetadataBytes = 4096`
  - `func queue.ValidateMetadata(m json.RawMessage) (json.RawMessage, error)` — returns the **compacted** bytes on success, `nil, nil` for empty input, and a non-nil error for anything invalid.

- [ ] **Step 1: Write the failing test**

Create `internal/queue/metadata_test.go`:

```go
package queue

import (
	"encoding/json"
	"strings"
	"testing"
)

// objectOfSize builds a compacted JSON object of exactly n bytes.
// `{"k":"` is 6 bytes and `"}` is 2, so the value is n-8 bytes.
func objectOfSize(n int) json.RawMessage {
	return json.RawMessage(`{"k":"` + strings.Repeat("a", n-8) + `"}`)
}

func TestValidateMetadataAccepts(t *testing.T) {
	tests := map[string]json.RawMessage{
		"empty is absent":  nil,
		"simple object":    json.RawMessage(`{"ticket":"T-1"}`),
		"nested object":    json.RawMessage(`{"a":{"b":[1,2,{"c":true}]}}`),
		"empty object":     json.RawMessage(`{}`),
		"exactly the cap":  objectOfSize(MaxMetadataBytes),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ValidateMetadata(in)
			if err != nil {
				t.Fatalf("ValidateMetadata(%s) = error %v, want accepted", in, err)
			}
			if len(in) == 0 && got != nil {
				t.Errorf("absent metadata must stay nil, got %s", got)
			}
		})
	}
}

func TestValidateMetadataRejects(t *testing.T) {
	tests := map[string]json.RawMessage{
		"array":            json.RawMessage(`["a","b"]`),
		"string scalar":    json.RawMessage(`"hello"`),
		"number scalar":    json.RawMessage(`42`),
		"null literal":     json.RawMessage(`null`),
		"malformed json":   json.RawMessage(`{"a":`),
		"trailing garbage": json.RawMessage(`{"a":1} oops`),
		"one over the cap": objectOfSize(MaxMetadataBytes + 1),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateMetadata(in); err == nil {
				t.Fatalf("ValidateMetadata(%.40s) = nil error, want rejected", in)
			}
		})
	}
}

// The cap measures CONTENT, not formatting: an object that is oversize
// as submitted but in-bounds once compacted is accepted, and what gets
// stored is the compacted form.
func TestValidateMetadataCompacts(t *testing.T) {
	padded := json.RawMessage("{\n\t\"ticket\"  :  \"T-1\"" +
		strings.Repeat(" ", MaxMetadataBytes) + "\n}")
	got, err := ValidateMetadata(padded)
	if err != nil {
		t.Fatalf("padded object must be accepted after compaction: %v", err)
	}
	if string(got) != `{"ticket":"T-1"}` {
		t.Errorf("metadata must be stored compacted, got %s", got)
	}
	if len(got) > MaxMetadataBytes {
		t.Errorf("compacted metadata must be within the cap, got %d bytes", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run TestValidateMetadata -v`
Expected: FAIL — build error, `undefined: ValidateMetadata` and `undefined: MaxMetadataBytes`.

- [ ] **Step 3: Add the Job field**

In `internal/queue/queue.go`, inside the `Job` struct, after the `DedupThreshold` field and before `SubmittedAt`:

```go
	// Metadata is opaque caller-supplied JSON, passed through untouched
	// to the result envelope. vismod never interprets, indexes, or acts
	// on it, and it can never influence a verdict. It is permitted in
	// this payload and in result.ResultEnvelope ONLY: never logged,
	// never rendered in the UI, never recorded in the audit log, so the
	// audit chain stays free of caller free text.
	Metadata json.RawMessage `json:"metadata,omitempty"`
```

Add `"encoding/json"` to the import block.

- [ ] **Step 4: Implement the validator**

In `internal/queue/queue.go`, after the `Handler` type declaration:

```go
// MaxMetadataBytes caps Job.Metadata after compaction. The cap is small
// because metadata rides EVERY queue payload (durably, under redisq) and
// every webhook POST; the intake's 1 MiB body limit is far too loose for
// a field with that reach.
const MaxMetadataBytes = 4096

// ValidateMetadata checks caller-supplied metadata and returns it
// compacted. Empty input is valid and yields nil (the field is omitted).
//
// Metadata must be a JSON object: that keeps the envelope field shape
// stable for consumers and keeps `.metadata.foo` addressable. Compaction
// happens here so the cap measures content rather than indentation, and
// so no call site can store an unbounded pretty-printed form. Nesting
// depth needs no check of ours — encoding/json's scanner already errors
// past its own maximum depth.
func ValidateMetadata(m json.RawMessage) (json.RawMessage, error) {
	if len(m) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, m); err != nil {
		return nil, fmt.Errorf("metadata is not valid JSON: %w", err)
	}
	c := buf.Bytes()
	if len(c) == 0 || c[0] != '{' {
		return nil, errors.New("metadata must be a JSON object")
	}
	if len(c) > MaxMetadataBytes {
		return nil, fmt.Errorf("metadata must be at most %d bytes once compacted, got %d", MaxMetadataBytes, len(c))
	}
	return json.RawMessage(c), nil
}
```

Add `"bytes"` and `"fmt"` to the import block (`errors` is already imported).

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/queue/ -run TestValidateMetadata -v`
Expected: PASS, all subtests.

- [ ] **Step 6: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all exit 0.

- [ ] **Step 7: Commit**

```bash
git add internal/queue/queue.go internal/queue/metadata_test.go
git commit -m "feat(queue): Add opaque caller metadata to Job with a validated 4 KiB cap"
```

---

### Task 2: The envelope field

**Files:**
- Modify: `internal/result/sink.go:25-33` (the `ResultEnvelope` struct)
- Test: `internal/result/sink_test.go` (append)

**Interfaces:**
- Consumes: nothing (the field is declared independently of `queue.Job`).
- Produces: `result.ResultEnvelope.Metadata json.RawMessage` (JSON tag `metadata,omitempty`).

- [ ] **Step 1: Write the failing test**

Append to `internal/result/sink_test.go`:

```go
// Metadata rides the envelope to every sink. Absent metadata must be
// OMITTED, not null: unlike score, absence here carries no signal, and
// existing consumers must see a byte-identical envelope.
func TestEnvelopeMetadataSerialization(t *testing.T) {
	var withMeta bytes.Buffer
	s := NewJSONLSink(&withMeta)
	if err := s.Write(context.Background(), ResultEnvelope{
		JobID:    "m1",
		Metadata: json.RawMessage(`{"ticket":"T-1"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withMeta.String(), `"metadata":{"ticket":"T-1"}`) {
		t.Errorf("metadata must serialize verbatim, got %s", withMeta.String())
	}

	var without bytes.Buffer
	s2 := NewJSONLSink(&without)
	if err := s2.Write(context.Background(), ResultEnvelope{JobID: "m2"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without.String(), "metadata") {
		t.Errorf("absent metadata must be omitted entirely, got %s", without.String())
	}
}
```

Ensure `bytes`, `context`, `encoding/json`, `strings`, and `testing` are imported in that file; add whichever are missing.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/result/ -run TestEnvelopeMetadataSerialization -v`
Expected: FAIL — `unknown field Metadata in struct literal`.

- [ ] **Step 3: Add the field**

In `internal/result/sink.go`, in `ResultEnvelope`, after `Error` and before `StartedAt`:

```go
	// Metadata is the opaque caller-supplied JSON from queue.Job, passed
	// through untouched. vismod never interprets it and it never
	// influences a verdict. Omitted when the caller supplied none.
	// Present in sink output ONLY — never in the audit log, log lines,
	// or the operator UI.
	Metadata json.RawMessage `json:"metadata,omitempty"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/result/ -run TestEnvelopeMetadataSerialization -v`
Expected: PASS.

- [ ] **Step 5: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all exit 0.

- [ ] **Step 6: Commit**

```bash
git add internal/result/sink.go internal/result/sink_test.go
git commit -m "feat(result): Carry caller metadata on the result envelope"
```

---

### Task 3: Pipeline pass-through and execution-time validation

**Files:**
- Modify: `internal/pipeline/pipeline.go` — `ProcessJob` (starts line 112), the override return (line 153), the main envelope (line 172)
- Test: `internal/pipeline/metadata_test.go` (create)

**Interfaces:**
- Consumes: `queue.ValidateMetadata(json.RawMessage) (json.RawMessage, error)`, `queue.MaxMetadataBytes`, `queue.Job.Metadata`, `result.ResultEnvelope.Metadata`.
- Produces: the guarantee that a validated `j.Metadata` reaches `env.Metadata` on both envelope paths, and that invalid metadata yields `verdict:"error"` + `queue.DeadLetter`.

Existing helpers in `internal/pipeline/pipeline_test.go` that this task's tests use: `newTestPipeline(t, mod, fs) (*Pipeline, *bytes.Buffer)`, `writeInput(t, content) string`, `imageJob(path) queue.Job`, `videoJob(path) queue.Job`, `decodeEnvelope(t, buf) result.ResultEnvelope`, `fakeModerator{scores: map[string]float64}`, `fakeFrameSource{zeroClean: bool, dir: string}`, `capturingEvents{}`.

- [ ] **Step 1: Write the failing test**

Create `internal/pipeline/metadata_test.go`:

```go
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// The happy path: opaque caller JSON reaches the sink envelope byte-for-byte.
func TestMetadataReachesEnvelope(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, buf := newTestPipeline(t, mod, nil)
	j := imageJob(writeInput(t, "benign"))
	j.Metadata = json.RawMessage(`{"ticket":"T-1","tenant":"acme"}`)

	if _, disp, err := p.ProcessJob(context.Background(), j); err != nil || disp != queue.Ack {
		t.Fatalf("disp=%v err=%v", disp, err)
	}
	env := decodeEnvelope(t, buf)
	if string(env.Metadata) != `{"ticket":"T-1","tenant":"acme"}` {
		t.Errorf("metadata must pass through untouched, got %s", env.Metadata)
	}
}

// The gated §F.5 override returns an envelope from a SECOND construction
// site. Missing it there drops the correlation ID on exactly the jobs an
// operator most needs to reconcile.
func TestMetadataSurvivesEmptyVideoSkipOverride(t *testing.T) {
	fs := &fakeFrameSource{zeroClean: true, dir: t.TempDir()}
	p, _ := newTestPipeline(t, &fakeModerator{}, fs)
	p.AllowEmptyVideoSkip = true
	p.Events = &capturingEvents{}
	j := videoJob(writeInput(t, "video-bytes"))
	j.Metadata = json.RawMessage(`{"ticket":"T-2"}`)

	env, disp, err := p.ProcessJob(context.Background(), j)
	if err != nil || disp != queue.Ack {
		t.Fatalf("override must ack: disp=%v err=%v", disp, err)
	}
	if string(env.Metadata) != `{"ticket":"T-2"}` {
		t.Errorf("override envelope must carry metadata, got %s", env.Metadata)
	}
}

// A job can reach redisq without passing through POST /jobs, so the
// pipeline validates too. Fail safe: error verdict + dead-letter, and
// the rejected bytes never reach the envelope.
func TestInvalidMetadataAtExecutionIsErrorVerdict(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, buf := newTestPipeline(t, mod, nil)
	j := imageJob(writeInput(t, "benign"))
	j.Metadata = json.RawMessage(`["not","an","object"]`)

	env, disp, err := p.ProcessJob(context.Background(), j)
	if disp != queue.DeadLetter {
		t.Fatalf("invalid metadata must dead-letter, got disp=%v err=%v", disp, err)
	}
	if env.Result == nil || env.Result.Overall.Verdict != moderation.VerdictError {
		t.Fatalf("invalid metadata must never allow, got %+v", env.Result)
	}
	if env.Metadata != nil {
		t.Errorf("rejected metadata must not reach the envelope, got %s", env.Metadata)
	}
	if !strings.Contains(decodeEnvelope(t, buf).Error, "metadata") {
		t.Error("the envelope error must name metadata as the cause")
	}
}

// Metadata is caller free text: it is carried, never logged.
func TestMetadataNeverReachesLogs(t *testing.T) {
	var logBuf bytes.Buffer
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, _ := newTestPipeline(t, mod, nil)
	p.Log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	j := imageJob(writeInput(t, "benign"))
	j.Metadata = json.RawMessage(`{"secret_marker":"do-not-log-me"}`)

	if _, _, err := p.ProcessJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logBuf.String(), "do-not-log-me") {
		t.Errorf("metadata must never appear in a log line:\n%s", logBuf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pipeline/ -run TestMetadata -v && go test ./internal/pipeline/ -run TestInvalidMetadata -v`
Expected: FAIL — `env.Metadata` is always empty and the invalid-metadata job acks instead of dead-lettering.

- [ ] **Step 3: Validate at the top of ProcessJob**

In `internal/pipeline/pipeline.go`, immediately after `started := time.Now().UTC()` in `ProcessJob`:

```go
	// Validated here as well as at intake: with queue.driver: redis a job
	// can be enqueued without ever passing through POST /jobs. Invalid
	// metadata is could-not-evaluate, not a silent pass — meta stays nil
	// so rejected bytes never reach the envelope.
	meta, metaErr := queue.ValidateMetadata(j.Metadata)
```

- [ ] **Step 4: Route the validation failure into the fail-safe path**

In the same function, add a new first case to the `switch` that currently begins `case resolveErr != nil:`:

```go
	switch {
	case metaErr != nil:
		procErr = fmt.Errorf("invalid metadata: %w", metaErr)
	case resolveErr != nil:
		procErr = resolveErr
```

Leave the remaining cases untouched. `procErr != nil` already routes to `p.errorResult`, and a `VerdictError` rollup already returns `queue.DeadLetter` at the end of `ProcessJob` — no disposition change is needed.

- [ ] **Step 5: Stamp both envelope construction sites**

In the empty-video-skip override return (currently one line, near line 153), add the field:

```go
		return result.ResultEnvelope{JobID: j.ID, Source: rs.env, ModelID: p.ModelID, Metadata: meta, StartedAt: started, FinishedAt: time.Now().UTC()}, queue.Ack, nil
```

In the main envelope literal (near line 172), add `Metadata: meta,` after `Error` is set — i.e. inside the struct literal, after `ModelID: p.ModelID,`:

```go
	env := result.ResultEnvelope{
		JobID:      j.ID,
		Source:     rs.env,
		ModelID:    p.ModelID,
		Result:     &res,
		Metadata:   meta,
		StartedAt:  started,
		FinishedAt: time.Now().UTC(),
	}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/pipeline/ -v -run "Metadata"`
Expected: PASS — all four tests.

- [ ] **Step 7: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all exit 0. In particular the existing rollup and empty-video-skip tests must pass **unmodified**.

- [ ] **Step 8: Commit**

```bash
git add internal/pipeline/pipeline.go internal/pipeline/metadata_test.go
git commit -m "feat(pipeline): Pass caller metadata to the envelope, fail safe when invalid"
```

---

### Task 4: The audit non-leak guard

**Files:**
- Test: `internal/audit/metadata_test.go` (create)
- Modify: none expected — `payloadFor` (`internal/audit/audit.go:159`) builds a `map[string]string` from selected fields, so metadata cannot leak by construction. This task proves it and locks it.

**Interfaces:**
- Consumes: `result.ResultEnvelope.Metadata`, `audit.Open(path, signer)`, `(*audit.Log).Record(ctx, env)`, `audit.Record` (the on-disk line type).
- Produces: nothing consumed by later tasks.

Existing helper: `internal/audit/audit_test.go` has a helper that builds a `result.ResultEnvelope`; read it before writing this test and reuse it if its shape fits, otherwise build the envelope inline as below.

- [ ] **Step 1: Write the test**

Create `internal/audit/metadata_test.go`:

```go
package audit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

func metaEnvelope(jobID string, meta json.RawMessage) result.ResultEnvelope {
	return result.ResultEnvelope{
		JobID:    "job-" + jobID,
		Source:   moderation.Source{Kind: "file", Ref: "/data/a.png", MediaType: "image"},
		ModelID:  result.ModelIdentity{Adapter: "fake", ModelVersion: "v1", ConfigHash: "h"},
		Metadata: meta,
		Result: &moderation.NormalizedResult{
			SchemaVersion: moderation.SchemaVersion,
			AssetID:       "/data/a.png",
			MediaType:     "image",
			Overall:       moderation.Overall{Verdict: moderation.VerdictAllow},
		},
	}
}

// The audit chain must stay free of caller free text. Two logs that
// differ ONLY by envelope metadata must produce identical entry hashes,
// and the metadata must appear nowhere in the file.
func TestAuditIgnoresEnvelopeMetadata(t *testing.T) {
	write := func(t *testing.T, meta json.RawMessage) (string, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "audit.jsonl")
		l, err := Open(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Record(context.Background(), metaEnvelope("1", meta)); err != nil {
			t.Fatal(err)
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var rec Record
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
			t.Fatal(err)
		}
		return string(b), rec.EntryHash
	}

	plainFile, plainHash := write(t, nil)
	metaFile, metaHash := write(t, json.RawMessage(`{"secret_marker":"do-not-audit-me"}`))

	if strings.Contains(metaFile, "do-not-audit-me") || strings.Contains(metaFile, "metadata") {
		t.Errorf("caller metadata must never enter the audit log:\n%s", metaFile)
	}
	if plainHash != metaHash {
		t.Errorf("metadata must not change the chain hash: %s != %s (plain log: %s)", plainHash, metaHash, plainFile)
	}
}
```

Note: `Record.Timestamp` feeds `entryHash`, so the two hashes only match if the timestamps match. If this test proves flaky for that reason, assert the two things that actually matter instead — that the file contains neither `do-not-audit-me` nor a `metadata` key, and that `rec.Payload` has exactly the same key set in both runs — and drop the hash comparison. Do not "fix" it by weakening what the audit log excludes.

- [ ] **Step 2: Run test**

Run: `go test ./internal/audit/ -run TestAuditIgnoresEnvelopeMetadata -v`
Expected: PASS immediately (no production change needed). If it FAILS, metadata is leaking into the audit chain — stop and fix `payloadFor`, do not adjust the test.

- [ ] **Step 3: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all exit 0.

- [ ] **Step 4: Commit**

```bash
git add internal/audit/metadata_test.go
git commit -m "test(audit): Lock caller metadata out of the hash-chained log"
```

---

### Task 5: HTTP intake

**Files:**
- Modify: `internal/cli/serve.go` — `intakeRequest` (line 397), the `POST /jobs` handler validation block (after the `validateDedupThreshold` check at line 483), the `queue.Job` literal (line 496)
- Test: `internal/cli/serve_test.go` (append)

**Interfaces:**
- Consumes: `queue.ValidateMetadata`, `queue.MaxMetadataBytes`, `queue.Job.Metadata`.
- Produces: `POST /jobs` accepts an optional `metadata` object; enqueued jobs carry the compacted bytes.

Read the existing intake tests in `internal/cli/serve_test.go` first and follow their harness (how they build the handler, the fake queue, and assert on the enqueued job). The test below assumes a helper that posts a body and returns the response plus the captured job; adapt names to whatever that file already uses rather than introducing a second harness.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/serve_test.go`:

```go
// Metadata is optional, opaque, and capped. Accepted metadata is stored
// COMPACTED, because the cap must measure content, not indentation.
func TestIntakeMetadata(t *testing.T) {
	t.Run("accepted and compacted", func(t *testing.T) {
		body := `{"kind":"file","ref":"/data/a.png","metadata":{ "ticket" : "T-1" }}`
		rec, jobs := postJob(t, body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("got %d, want 202: %s", rec.Code, rec.Body.String())
		}
		if len(jobs) != 1 {
			t.Fatalf("want 1 enqueued job, got %d", len(jobs))
		}
		if string(jobs[0].Metadata) != `{"ticket":"T-1"}` {
			t.Errorf("metadata must be stored compacted, got %s", jobs[0].Metadata)
		}
	})

	t.Run("omitted stays nil", func(t *testing.T) {
		rec, jobs := postJob(t, `{"kind":"file","ref":"/data/a.png"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("got %d, want 202", rec.Code)
		}
		if jobs[0].Metadata != nil {
			t.Errorf("absent metadata must stay nil, got %s", jobs[0].Metadata)
		}
	})

	for name, meta := range map[string]string{
		"array":    `["a"]`,
		"scalar":   `"a"`,
		"oversize": `{"k":"` + strings.Repeat("a", queue.MaxMetadataBytes) + `"}`,
	} {
		t.Run("rejected: "+name, func(t *testing.T) {
			body := `{"kind":"file","ref":"/data/a.png","metadata":` + meta + `}`
			rec, jobs := postJob(t, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", rec.Code)
			}
			if len(jobs) != 0 {
				t.Errorf("a rejected job must not be enqueued, got %d", len(jobs))
			}
		})
	}
}
```

If `serve_test.go` has no `postJob(t, body) (*httptest.ResponseRecorder, []queue.Job)` helper, write one in that file that mirrors how the existing intake tests build the server and capture enqueued jobs, and use it here.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestIntakeMetadata -v`
Expected: FAIL — metadata is dropped (nil on the enqueued job) and invalid metadata returns `202` instead of `400`.

- [ ] **Step 3: Add the request field**

In `internal/cli/serve.go`, in `intakeRequest`, after `DedupThreshold`:

```go
	// Metadata is opaque caller JSON echoed back on the result envelope.
	// vismod never interprets it. Must be a JSON object, at most
	// queue.MaxMetadataBytes once compacted.
	Metadata json.RawMessage `json:"metadata,omitempty"`
```

- [ ] **Step 4: Validate and attach it**

In the `POST /jobs` handler, immediately after the `validateDedupThreshold` block:

```go
			meta, err := queue.ValidateMetadata(req.Metadata)
			if err != nil {
				http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
				return
			}
```

Then add `Metadata: meta,` to the `queue.Job` literal, after `DedupThreshold: req.DedupThreshold,`.

Note: the handler already declares `err` in inner scopes; if the compiler reports a shadowing or redeclaration problem at this position, name it `metaErr` and adjust the two references.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestIntakeMetadata -v`
Expected: PASS, all subtests.

- [ ] **Step 6: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all exit 0.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/serve.go internal/cli/serve_test.go
git commit -m "feat(intake): Accept optional caller metadata on POST /jobs"
```

---

### Task 6: The `scan --metadata` flag

**Files:**
- Modify: `internal/cli/scan.go` — flag var (line 31), `Long` help text, `RunE` (line 53), `scanOptions` (line 77), the validation block in `runScan` (after line 104), the `queue.Job` literal (line 156), `init()` (line 178)
- Test: `internal/cli/scan_test.go` (append)

**Interfaces:**
- Consumes: `queue.ValidateMetadata`, `queue.Job.Metadata`, existing `scanOptions`, `runScan(ctx, out, args, opts) (int, error)`.
- Produces: `scanOptions.Metadata json.RawMessage`; `vismod scan --metadata '<json>'`.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/scan_test.go`:

```go
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
```

This test asserts the metadata check runs before the input-file check, so `nonexistent.png` is never reached. Place the validation accordingly in Step 4.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestScanRejectsInvalidMetadata -v`
Expected: FAIL — build error, `unknown field Metadata in struct literal of type scanOptions`.

- [ ] **Step 3: Add the flag and the option**

In `internal/cli/scan.go`, extend the var block:

```go
var (
	scanWorkflows      []string
	scanDedupThreshold int
	scanMetadata       string
)
```

Add to `scanOptions`:

```go
	// Metadata is opaque caller JSON echoed back on each envelope.
	Metadata json.RawMessage
```

In `RunE`, after the `dedup-threshold` block:

```go
			if cmd.Flags().Changed("metadata") {
				opts.Metadata = json.RawMessage(scanMetadata)
			}
```

In `init()`:

```go
	scanCmd.Flags().StringVar(&scanMetadata, "metadata", "",
		"opaque JSON object echoed back on each result envelope; vismod never interprets it")
```

Append to the `Long` help text, after the `--dedup-threshold` paragraph:

```
--metadata attaches an opaque JSON object to every envelope this
invocation emits, for correlating verdicts with your own records. vismod
never interprets it; it must be a JSON object and at most 4096 bytes once
compacted. Do not put secrets in it.
```

Add `"encoding/json"` to the imports.

- [ ] **Step 4: Validate and attach it**

In `runScan`, immediately after the `validateDedupThreshold` block — before `buildModerator`, so a bad flag never reaches a billed call:

```go
	meta, err := queue.ValidateMetadata(opts.Metadata)
	if err != nil {
		return 0, err
	}
```

The existing `mod, err := buildModerator(...)` on the following lines must become `mod, err = buildModerator(...)` (or keep `:=` by naming this one `metaErr`) — whichever the compiler accepts cleanly.

Add `Metadata: meta,` to the `queue.Job` literal in the scan loop, after `DedupThreshold: opts.DedupThreshold,`.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestScanRejectsInvalidMetadata -v`
Expected: PASS, all subtests.

- [ ] **Step 6: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all exit 0.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/scan.go internal/cli/scan_test.go
git commit -m "feat(scan): Add --metadata for one-shot scans"
```

---

### Task 7: Documentation

**Files:**
- Modify: `docs/rest-api.md` (request-body table ~line 38, `400` causes table ~line 46)
- Modify: `docs/result-envelope.md` ("Fields that matter downstream", ~line 31)
- Modify: `AGENTS.md` (done gate, line 59)
- Modify: `docs/superpowers/specs/2026-08-03-passthrough-metadata-design.md` (record the validator-placement deviation)

**Interfaces:**
- Consumes: the shipped behavior from Tasks 1–6.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Update the REST intake doc**

In `docs/rest-api.md`, add a row to the request-body field table:

```
| `metadata` | no | An opaque JSON **object** echoed back verbatim on the result envelope, for correlating a verdict with your own records. vismod never interprets it, and it can never affect a verdict. Max 4096 bytes once compacted. Not written to the audit log — **do not put secrets in it** |
```

Update the example request body at the top of that section:

```json
{"kind":"file","ref":"/data/clip.mp4","media_type":"video",
 "workflows":["interval","keyframe"],"dedup_threshold":8,
 "metadata":{"ticket":"T-4417","tenant":"acme"}}
```

Extend the `400` row to name the new cause — append to the existing list in that cell: `, or metadata that is not a JSON object or exceeds 4096 bytes compacted`.

- [ ] **Step 2: Update the result-envelope doc**

In `docs/result-envelope.md`, add a bullet to "Fields that matter downstream":

```markdown
- **`metadata`** is whatever you attached to the job (`POST /jobs` or
  `scan --metadata`), echoed back verbatim and compacted. It is **absent**
  when you supplied none — not `null` — so envelopes from callers who do
  not use it are byte-identical to before. vismod never interprets it, so
  it can never affect a verdict, and it is deliberately **not** written to
  the audit log: the hash chain stays free of caller free text. It is also
  never logged and never shown in the operator UI. Do not put secrets in
  it — it reaches every configured sink, including your webhook receiver.
```

Add a sentence to the paragraph that discusses `result.schema_version` (~line 58): `metadata` rides the **envelope**, not `NormalizedResult`, so it does not move `result.schema_version`; like `source`, it has no version signal of its own, and it is additive — a consumer that ignores it keeps working.

- [ ] **Step 3: Amend the AGENTS.md done gate**

Replace the done-gate line at `AGENTS.md:59`:

```markdown
- No secret, media byte, provider `Raw`, or free text added to any
  envelope, log, audit record, queue payload, or UI surface. ONE
  carve-out: caller-supplied `metadata` (`queue.Job.Metadata` /
  `ResultEnvelope.Metadata`, validated by `queue.ValidateMetadata`) is
  opaque JSON permitted in the queue payload and the result envelope
  ONLY — still forbidden in the audit log, in logs, and in the UI, and it
  never influences a verdict. Do not widen it further.
```

- [ ] **Step 4: Record the deviation in the spec**

In `docs/superpowers/specs/2026-08-03-passthrough-metadata-design.md`, in the "Validation" section, replace the sentence placing the validator in `internal/cli` with:

```markdown
One shared validator, `queue.ValidateMetadata(json.RawMessage)
(json.RawMessage, error)`, in `internal/queue` beside the `Job` type it
guards. It cannot live in `internal/cli` as first sketched: `internal/cli`
imports `internal/pipeline`, so the pipeline could not import it back for
execution-time validation. It returns the **compacted** bytes so no call
site can forget to compact.
```

- [ ] **Step 5: Verify the docs match the shipped behavior**

Re-read each edited passage against the code from Tasks 1–6. Specifically confirm: the cap number is 4096 in all four documents, the `400` message text matches what `queue.ValidateMetadata` actually returns, and the `scan` flag name is `--metadata`.

- [ ] **Step 6: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all exit 0.

- [ ] **Step 7: Commit**

```bash
git add docs/rest-api.md docs/result-envelope.md AGENTS.md docs/superpowers/specs/2026-08-03-passthrough-metadata-design.md
git commit -m "docs: Document caller pass-through metadata and its done-gate carve-out"
```

---

### Task 8: Close out the loop protocol

**Files:**
- Modify: `docs/agent/STATUS.md`
- Modify: `docs/agent/UNVERIFIED.md` (only if something below applies)

**Interfaces:**
- Consumes: everything from Tasks 1–7.
- Produces: nothing.

- [ ] **Step 1: Update STATUS.md**

Read `docs/agent/STATUS.md`, then add an entry in its existing format recording that caller pass-through metadata shipped: opaque JSON object on `queue.Job` and `ResultEnvelope`, 4 KiB compacted cap, validated at intake / `scan` / execution, excluded from audit, logs, and UI.

- [ ] **Step 2: Append anything unproven to UNVERIFIED.md**

If any claim in this work could not be proven locally, append it with what would prove it. Note that this dev box has no C compiler, so `go test -race` cannot run locally — if the metadata field's concurrent access through `MultiSink` is worth a race check, record that CI is the gate for it.

If everything was proven by the test suite, add nothing.

- [ ] **Step 3: Commit**

```bash
git add docs/agent/STATUS.md docs/agent/UNVERIFIED.md
git commit -m "docs(agent): Record pass-through metadata in STATUS"
```

---

## Self-Review

**Spec coverage:**

| Spec requirement | Task |
|---|---|
| `Metadata json.RawMessage` on `queue.Job` | 1 |
| Object-only, compacted, ≤ 4 KiB, no depth check | 1 |
| `Metadata` on `ResultEnvelope`, `omitempty` | 2 |
| Byte-identical envelope when absent | 2 |
| Validate at execution → error verdict + dead-letter | 3 |
| Stamp both envelope construction sites (`:153`, `:172`) | 3 |
| Metadata never logged | 3 |
| Metadata never in the audit chain | 4 |
| Validate at intake → `400` | 5 |
| `scan --metadata` → setup error | 6 |
| DLQ carries metadata (free via `DeadLetterEntry`) | 3 (no code) |
| Idempotency unchanged, keyed on JobID | no code change; nothing touches `dedupe` |
| No redaction / secret scanning; documented | 7 |
| No config surface; UI unchanged | no code change |
| Docs: rest-api, result-envelope, AGENTS.md | 7 |

**Placeholder scan:** none — every code step carries the literal code to write, and every test step carries the test body.

**Type consistency:** `queue.ValidateMetadata(json.RawMessage) (json.RawMessage, error)` and `queue.MaxMetadataBytes = 4096` are declared in Task 1 and used with that exact signature in Tasks 3, 5, and 6. `Metadata` is the field name on `queue.Job` (Task 1), `result.ResultEnvelope` (Task 2), `intakeRequest` (Task 5), and `scanOptions` (Task 6).

**Known deviation from the spec:** validator placement (`internal/queue`, not `internal/cli`) and its return signature. Recorded in Task 7, Step 4.
