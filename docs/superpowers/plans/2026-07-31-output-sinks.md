# Configurable Output Sinks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator send result envelopes to any combination of stdout, a JSONL file, and an HTTP webhook, instead of only stdout.

**Architecture:** `result.Sink` is already a one-method interface with one implementation (`JSONLSink`, hardwired to `os.Stdout` at `internal/cli/serve.go:81`). This adds three siblings — `MultiSink`, `FileSink`, `WebhookSink` — plus an `output.sinks` config block and a `buildSinks` constructor in the composition root. No pipeline changes: `Pipeline.Sink` keeps its type.

**Tech Stack:** Go 1.x, viper (config), `internal/moderate.DoJSON` (retry classification), `net/http/httptest` (tests), Prometheus client.

## Global Constraints

Copied verbatim from `AGENTS.md` and the spec. Every task's requirements implicitly include these.

- Done gate: `go build ./... && go vet ./... && go test ./...` all exit 0.
- Full suite runs with NO network and NO credentials. Use `httptest`, never a real host.
- No secret, media byte, provider `Raw`, or free text may be added to any envelope, log, audit record, queue payload, or UI surface.
- `Sink.Write` MUST be idempotent per JobID — at-least-once delivery means redelivery must never double-write.
- Every doc under "Docs that must stay true" whose behavior changed is updated in the SAME commit.
- No new Go module import unless the commit message justifies it. **This plan adds none** — everything used is already in `go.mod`.
- Existing rollup tests must pass UNMODIFIED.
- Default behavior must not change: a config with no `output` block emits to stdout exactly as today.

## File Structure

| Path | Responsibility |
|---|---|
| `internal/result/sink.go` (modify) | Unchanged `Sink` interface + `JSONLSink`, refactored onto the shared dedupe helper. |
| `internal/result/dedupe.go` (create) | Per-JobID idempotency state (`Claim`/`Release`) shared by every sink. |
| `internal/result/multi.go` (create) | `MultiSink` — fan-out, all-attempted, first-error-returned. |
| `internal/result/file.go` (create) | `FileSink` — append-only JSONL to a path. |
| `internal/result/webhook.go` (create) | `WebhookSink` — POST envelope JSON with retry classification. |
| `internal/result/multi_test.go`, `file_test.go`, `webhook_test.go` (create) | One test file per sink. |
| `internal/config/config.go` (modify) | `OutputConfig` / `SinkConfig` types, defaults, validation. |
| `internal/config/config_test.go` (modify) | Positive and negative parse tests. |
| `internal/cli/sinks.go` (create) | `buildSinks` — the one place config becomes sinks. Keeps `serve.go` from growing. |
| `internal/cli/sinks_test.go` (create) | Boot-validation tests. |
| `internal/cli/serve.go:81-82`, `internal/cli/scan.go:95` (modify) | Call `buildSinks`. |
| `internal/observe/observe.go` (modify) | `SinkWriteFailuresTotal` counter. |

---

### Task 1: Config types and validation

**Files:**
- Modify: `internal/config/config.go` (add types near `UIConfig` ~line 264; add field to `Config` ~line 286; add to `Defaults()` ~line 342; add validation in `Validate` ~line 415)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `config.SinkConfig{Type, Path, URL string; Timeout time.Duration; MaxAttempts int}`, `config.OutputConfig{Sinks []SinkConfig}`, and `Config.Output OutputConfig`. Task 5 consumes these.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestOutputDefaultsToStdout(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Output.Sinks) != 1 || cfg.Output.Sinks[0].Type != "stdout" {
		t.Errorf("absent output block must default to stdout, got %+v", cfg.Output.Sinks)
	}
}

func TestOutputSinksParse(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
output:
  sinks:
    - type: stdout
    - type: file
      path: /tmp/results.jsonl
    - type: webhook
      url: https://collector.internal/results
      timeout: 5s
      max_attempts: 4
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Output.Sinks) != 3 {
		t.Fatalf("want 3 sinks, got %d", len(cfg.Output.Sinks))
	}
	if cfg.Output.Sinks[1].Path != "/tmp/results.jsonl" {
		t.Errorf("file path: got %q", cfg.Output.Sinks[1].Path)
	}
	if cfg.Output.Sinks[2].Timeout != 5*time.Second || cfg.Output.Sinks[2].MaxAttempts != 4 {
		t.Errorf("webhook opts: got %+v", cfg.Output.Sinks[2])
	}
}

func TestOutputSinkNegativeCases(t *testing.T) {
	for name, body := range map[string]string{
		"unknown type": `
output:
  sinks:
    - type: carrier-pigeon
`,
		"file without path": `
output:
  sinks:
    - type: file
`,
		"webhook without url": `
output:
  sinks:
    - type: webhook
`,
		"webhook with userinfo": `
output:
  sinks:
    - type: webhook
      url: https://user:pw@collector.internal/results
`,
		"webhook on metadata range": `
output:
  sinks:
    - type: webhook
      url: http://169.254.169.254/results
`,
		"negative max_attempts": `
output:
  sinks:
    - type: webhook
      url: https://collector.internal/results
      max_attempts: -1
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTempYAML(t, "ffmpeg:\n  max_frames: 8\n"+body)); err == nil {
				t.Fatal("want boot refusal, got nil error")
			}
		})
	}
}
```

If `writeTempYAML` does not already exist in `config_test.go`, add it:

```go
func writeTempYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run TestOutput -v`
Expected: FAIL — `cfg.Output` undefined.

- [ ] **Step 3: Add the types**

In `internal/config/config.go`, after `UIConfig`:

```go
// SinkConfig is one entry in output.sinks. Fields not relevant to the
// chosen Type are ignored; Validate rejects a Type whose required field
// is missing rather than silently emitting nowhere.
type SinkConfig struct {
	Type string `mapstructure:"type"` // "stdout" | "file" | "webhook"
	Path string `mapstructure:"path"` // file
	// URL is OPERATOR configuration, never job input. It is the same
	// trust class as a provider endpoint (SECURITY.md): private ranges
	// are expected and allowed. Do NOT apply the media-source deny-list.
	URL         string        `mapstructure:"url"`          // webhook
	Timeout     time.Duration `mapstructure:"timeout"`      // webhook
	MaxAttempts int           `mapstructure:"max_attempts"` // webhook
}

// OutputConfig selects where result envelopes go. An absent block means
// stdout. A present block with an empty list refuses to boot: emitting
// nothing silently is the failure mode this project exists to prevent.
type OutputConfig struct {
	Sinks []SinkConfig `mapstructure:"sinks"`
}
```

Add to `Config`, after `UI`:

```go
	Output       OutputConfig       `mapstructure:"output"`
```

Add to `Defaults()`, after `UI:`:

```go
		Output:       OutputConfig{Sinks: []SinkConfig{{Type: "stdout"}}},
```

- [ ] **Step 4: Add validation**

In `Validate`, before the final `return nil`:

```go
	if err := validateOutput(cfg.Output); err != nil {
		return err
	}
```

New function at the end of the file:

```go
// validateOutput fails closed on every ambiguous sink definition. A sink
// that cannot be built is a boot error, never a silently dropped output.
func validateOutput(o OutputConfig) error {
	if len(o.Sinks) == 0 {
		return fmt.Errorf("config: output.sinks is present but empty — vismod would emit no results anywhere; remove the output block to use stdout, or list at least one sink")
	}
	for i, s := range o.Sinks {
		switch strings.ToLower(strings.TrimSpace(s.Type)) {
		case "stdout":
		case "file":
			if strings.TrimSpace(s.Path) == "" {
				return fmt.Errorf("config: output.sinks[%d] type=file requires a path", i)
			}
		case "webhook":
			if err := validateWebhookURL(s.URL); err != nil {
				return fmt.Errorf("config: output.sinks[%d]: %w", i, err)
			}
			if s.MaxAttempts < 0 {
				return fmt.Errorf("config: output.sinks[%d].max_attempts must be >= 0, got %d", i, s.MaxAttempts)
			}
			if s.Timeout < 0 {
				return fmt.Errorf("config: output.sinks[%d].timeout must be >= 0, got %s", i, s.Timeout)
			}
		default:
			return fmt.Errorf("config: output.sinks[%d].type must be \"stdout\", \"file\" or \"webhook\", got %q", i, s.Type)
		}
	}
	return nil
}

// validateWebhookURL applies the operator-endpoint rules (SECURITY.md
// class 2/3): http or https, no userinfo, and the metadata range refused
// unconditionally because no legitimate receiver lives there and a
// misconfiguration there turns into cloud-credential exposure.
func validateWebhookURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("type=webhook requires a url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhook url is not parseable: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhook url scheme must be http or https, got %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("webhook url must not contain userinfo — credentials are env-only")
	}
	if u.Host == "" {
		return fmt.Errorf("webhook url has no host")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		if ip.IsLinkLocalUnicast() {
			return fmt.Errorf("webhook url host %s is in the link-local/metadata range", ip)
		}
	}
	return nil
}
```

Add `"net/netip"` and `"net/url"` to the import block.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/ -run TestOutput -v`
Expected: PASS.

- [ ] **Step 5a: Determine viper's real behavior for an empty list, then assert it unconditionally**

There is an open question here: does viper preserve `sinks: []` as an empty slice, or drop the key entirely the way it drops maps with no scalar leaf (`AGENTS.md` gotcha)? **Find out first, then write one unconditional test for whichever it is.** Do not write a test that skips — a test that can pass while asserting nothing defends nothing.

Determine it with a throwaway probe:

```bash
cat > /tmp/probe.yaml <<'EOF'
ffmpeg:
  max_frames: 8
output:
  sinks: []
EOF
```

then call `Load("/tmp/probe.yaml")` from a scratch test and print `len(cfg.Output.Sinks)` and `err`.

**If Load returns an error** (viper preserved the empty slice), write:

```go
func TestOutputEmptySinkListRefusesBoot(t *testing.T) {
	_, err := Load(writeTempYAML(t, "ffmpeg:\n  max_frames: 8\noutput:\n  sinks: []\n"))
	if err == nil {
		t.Fatal("an explicitly empty sinks list must refuse to boot")
	}
	if !strings.Contains(err.Error(), "output.sinks") {
		t.Errorf("error must name the offending key, got %v", err)
	}
}
```

**If Load succeeds** (viper dropped the key and `Defaults()` survived), write:

```go
func TestOutputEmptySinkListFallsBackToDefault(t *testing.T) {
	// viper drops a yaml key whose value has no scalar leaf, so `sinks: []`
	// never reaches the struct and Defaults()' stdout entry survives. The
	// empty-list guard in validateOutput is therefore unreachable from yaml
	// and remains as defense-in-depth for direct struct construction —
	// which the unit test below exercises.
	cfg, err := Load(writeTempYAML(t, "ffmpeg:\n  max_frames: 8\noutput:\n  sinks: []\n"))
	if err != nil {
		t.Fatalf("viper drops the empty list, so Load should succeed: %v", err)
	}
	if len(cfg.Output.Sinks) != 1 || cfg.Output.Sinks[0].Type != "stdout" {
		t.Errorf("want the stdout default to survive, got %+v", cfg.Output.Sinks)
	}
}

func TestValidateOutputRejectsEmptyDirectly(t *testing.T) {
	if err := validateOutput(OutputConfig{}); err == nil {
		t.Fatal("an empty sink list constructed directly must be rejected")
	}
}
```

Either way, keep the guard in `validateOutput`, and record which branch was observed in `docs/agent/UNVERIFIED.md` (Task 6).

- [ ] **Step 6: Run the full gate and commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/config/config.go internal/config/config_test.go
git commit -m "config: add output.sinks block with fail-closed validation"
```

---

### Task 2: MultiSink

**Files:**
- Create: `internal/result/multi.go`
- Test: `internal/result/multi_test.go`

**Interfaces:**
- Consumes: `result.Sink`, `result.ResultEnvelope` (existing, `sink.go`).
- Produces: `result.NewMultiSink(sinks []Sink, names []string, onFail func(sinkType string)) *MultiSink`. Task 5 calls it.

- [ ] **Step 1: Write the failing test**

```go
package result

import (
	"context"
	"errors"
	"testing"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

type stubSink struct {
	calls int
	err   error
}

func (s *stubSink) Write(context.Context, ResultEnvelope) error {
	s.calls++
	return s.err
}

func envFixture(id string) ResultEnvelope {
	return ResultEnvelope{
		JobID:  queue.JobID(id),
		Source: moderation.Source{Kind: "file", Ref: "x.png", MediaType: "image"},
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow},
		},
	}
}

func TestMultiSinkAttemptsAllSinksEvenWhenOneFails(t *testing.T) {
	boom := errors.New("webhook down")
	a, b, c := &stubSink{}, &stubSink{err: boom}, &stubSink{}
	m := NewMultiSink([]Sink{a, b, c}, []string{"stdout", "webhook", "file"}, nil)

	err := m.Write(context.Background(), envFixture("job-1"))
	if err == nil {
		t.Fatal("want error from the failing sink, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("want wrapped %v, got %v", boom, err)
	}
	// The point of the design: a webhook outage must not suppress the
	// local record. c is AFTER the failure and must still have been called.
	if a.calls != 1 || b.calls != 1 || c.calls != 1 {
		t.Errorf("all sinks must be attempted: a=%d b=%d c=%d", a.calls, b.calls, c.calls)
	}
}

func TestMultiSinkReturnsFirstError(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	m := NewMultiSink(
		[]Sink{&stubSink{err: first}, &stubSink{err: second}},
		[]string{"file", "webhook"}, nil)

	err := m.Write(context.Background(), envFixture("job-1"))
	if !errors.Is(err, first) {
		t.Errorf("want first error %v, got %v", first, err)
	}
	if errors.Is(err, second) {
		t.Errorf("must not report the second error: %v", err)
	}
}

func TestMultiSinkSuccessPathWritesAll(t *testing.T) {
	a, b := &stubSink{}, &stubSink{}
	m := NewMultiSink([]Sink{a, b}, []string{"stdout", "file"}, nil)
	if err := m.Write(context.Background(), envFixture("job-1")); err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("a=%d b=%d, want 1 each", a.calls, b.calls)
	}
}

func TestMultiSinkReportsFailingSinkType(t *testing.T) {
	var got []string
	m := NewMultiSink(
		[]Sink{&stubSink{}, &stubSink{err: errors.New("x")}},
		[]string{"stdout", "webhook"},
		func(sinkType string) { got = append(got, sinkType) })

	_ = m.Write(context.Background(), envFixture("job-1"))
	if len(got) != 1 || got[0] != "webhook" {
		t.Errorf("onFail must name only the failing sink, got %v", got)
	}
}
```

`envFixture` lives in this file and is reused by `file_test.go` and `webhook_test.go` in Tasks 3 and 4 — same package, so do not redefine it there.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/result/ -run TestMultiSink -v`
Expected: FAIL — `NewMultiSink` undefined.

- [ ] **Step 3: Implement MultiSink**

```go
package result

import (
	"context"
	"fmt"
)

// MultiSink fans one envelope out to every configured sink.
//
// Failure policy: EVERY sink is attempted and the FIRST error is
// returned. Not fail-fast — a webhook outage must never suppress the
// local JSONL record. The returned error reaches the pipeline's queue
// Retry disposition, so the job redelivers; the sinks that already
// succeeded are no-ops on the second pass because each is idempotent per
// JobID.
type MultiSink struct {
	sinks  []Sink
	names  []string
	onFail func(sinkType string)
}

// NewMultiSink pairs each sink with its config type name (used for error
// messages and the failure metric). names must be the same length as
// sinks. onFail may be nil.
func NewMultiSink(sinks []Sink, names []string, onFail func(sinkType string)) *MultiSink {
	return &MultiSink{sinks: sinks, names: names, onFail: onFail}
}

func (m *MultiSink) Write(ctx context.Context, env ResultEnvelope) error {
	var firstErr error
	for i, s := range m.sinks {
		if err := s.Write(ctx, env); err != nil {
			if m.onFail != nil {
				m.onFail(m.name(i))
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("result: sink %s: %w", m.name(i), err)
			}
		}
	}
	return firstErr
}

func (m *MultiSink) name(i int) string {
	if i < len(m.names) {
		return m.names[i]
	}
	return "unknown"
}

var _ Sink = (*MultiSink)(nil)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/result/ -run TestMultiSink -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/result/multi.go internal/result/multi_test.go
git commit -m "result: add MultiSink fan-out (all attempted, first error returned)"
```

---

### Task 3: The shared dedupe helper, then FileSink

**Files:**
- Create: `internal/result/dedupe.go`
- Create: `internal/result/dedupe_test.go`
- Create: `internal/result/file.go`
- Test: `internal/result/file_test.go`
- Modify: `internal/result/sink.go` (JSONLSink adopts the helper)

**Interfaces:**
- Consumes: `result.Sink`, `result.ResultEnvelope`.
- Produces:
  - `result.dedupe` with `Claim(queue.JobID) bool` and `Release(queue.JobID)` (unexported — internal to the package)
  - `result.NewFileSink(path string) (*FileSink, error)` and `(*FileSink).Close() error`. Task 5 calls both.

**Why the helper comes first:** every sink needs per-JobID idempotency, and writing the mutex-plus-map block three times is duplication a reviewer will rightly flag. `WebhookSink` (Task 4) additionally needs to *release* a claim when a send fails, so the helper carries both operations and the semantics become explicit rather than an ad-hoc variant per sink.

- [ ] **Step 1a: Write the failing helper test**

Create `internal/result/dedupe_test.go`:

```go
package result

import (
	"sync"
	"testing"

	"github.com/vismod/vismod/internal/queue"
)

func TestDedupeClaimIsOncePerJobID(t *testing.T) {
	var d dedupe
	if !d.Claim("job-1") {
		t.Fatal("first claim must succeed")
	}
	if d.Claim("job-1") {
		t.Error("second claim of the same id must fail")
	}
	if !d.Claim("job-2") {
		t.Error("a distinct id must claim independently")
	}
}

func TestDedupeReleaseAllowsReclaim(t *testing.T) {
	var d dedupe
	if !d.Claim("job-1") {
		t.Fatal("first claim must succeed")
	}
	d.Release("job-1")
	if !d.Claim("job-1") {
		t.Error("after Release the id must be claimable again — a failed send must be retriable on redelivery")
	}
}

func TestDedupeReleaseOfUnclaimedIsSafe(t *testing.T) {
	var d dedupe
	d.Release("never-claimed") // must not panic
}

func TestDedupeConcurrentClaimYieldsExactlyOneWinner(t *testing.T) {
	var d dedupe
	const n = 100
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.Claim(queue.JobID("job-1")) {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("exactly one goroutine must win the claim, got %d", wins)
	}
}
```

- [ ] **Step 1b: Run it to verify it fails**

Run: `go test ./internal/result/ -run TestDedupe -v`
Expected: FAIL — `dedupe` undefined.

- [ ] **Step 1c: Implement the helper**

Create `internal/result/dedupe.go`:

```go
package result

import (
	"sync"

	"github.com/vismod/vismod/internal/queue"
)

// dedupe is the per-JobID idempotency state every Sink needs.
//
// Queue delivery is at-least-once, so the same job can arrive more than
// once and a sink must not write it twice. Claim reports whether THIS
// call owns the write.
//
// Release exists for sinks whose write can fail after the claim (the
// webhook): without it, a failed send would mark the job written and the
// queue's redelivery would silently skip that sink forever.
//
// The zero value is ready to use.
type dedupe struct {
	mu      sync.Mutex
	written map[queue.JobID]bool
}

// Claim reports whether the caller owns the write for id. It returns
// false if the id was already claimed.
func (d *dedupe) Claim(id queue.JobID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.written[id] {
		return false
	}
	if d.written == nil {
		d.written = map[queue.JobID]bool{}
	}
	d.written[id] = true
	return true
}

// Release undoes a Claim so a later delivery can retry. Releasing an
// unclaimed id is a no-op.
func (d *dedupe) Release(id queue.JobID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.written, id)
}
```

- [ ] **Step 1d: Run helper tests and the race detector**

Run: `go test -race ./internal/result/ -run TestDedupe -v`
Expected: PASS, no race.

- [ ] **Step 1e: Adopt the helper in JSONLSink**

In `internal/result/sink.go`, replace `JSONLSink`'s own mutex and map:

```go
// JSONLSink writes one JSON line per envelope to an io.Writer (stdout or a
// file). Idempotency is per-process: dedupe suppresses duplicate writes
// within this process's lifetime. Cross-restart dedupe is the durable
// queue's completion marker (redisq) — documented in README.
type JSONLSink struct {
	mu sync.Mutex // serializes writes to w
	w  io.Writer
	d  dedupe
}

func NewJSONLSink(w io.Writer) *JSONLSink {
	return &JSONLSink{w: w}
}

func (s *JSONLSink) Write(_ context.Context, env ResultEnvelope) error {
	if !s.d.Claim(env.JobID) {
		return nil // idempotent per JobID
	}
	b, err := json.Marshal(env)
	if err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: marshal envelope: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: write envelope: %w", err)
	}
	return nil
}
```

Run: `go test ./internal/result/ -v`
Expected: PASS — the two pre-existing tests in `sink_test.go` must pass UNMODIFIED. If either fails, the refactor changed behavior; fix the code, not the test.

- [ ] **Step 1f: Commit the helper**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/result/dedupe.go internal/result/dedupe_test.go internal/result/sink.go
git commit -m "result: extract the per-JobID dedupe helper shared by all sinks"
```

Now build FileSink on top of it.

- [ ] **Step 1: Write the failing test**

```go
package result

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vismod/vismod/internal/queue"
)

func TestFileSinkAppendsOneLinePerEnvelope(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, id := range []string{"job-1", "job-2", "job-3"} {
		env := envFixture(id)
		if err := s.Write(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "\n"); got != 3 {
		t.Errorf("want 3 lines, got %d: %s", got, b)
	}
}

func TestFileSinkIdempotentPerJobID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	env := envFixture("job-1")
	for range 3 {
		if err := s.Write(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Errorf("redelivery double-wrote: %d lines, want 1", got)
	}
}

func TestFileSinkAppendsToExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	if err := os.WriteFile(p, []byte("{\"pre\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Write(context.Background(), envFixture("job-1")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "{\"pre\":true}") {
		t.Error("existing content was truncated; sink must O_APPEND")
	}
	if got := strings.Count(string(b), "\n"); got != 2 {
		t.Errorf("want 2 lines, got %d", got)
	}
}

func TestFileSinkConcurrentWritesDoNotInterleave(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Write(context.Background(), envFixture("job-"+string(rune('a'+i%26))+string(rune('0'+i/26))))
		}()
	}
	wg.Wait()

	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	lines := 0
	for sc.Scan() {
		lines++
		var env ResultEnvelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			t.Fatalf("line %d is not well-formed JSON (writes interleaved): %v", lines, err)
		}
		if env.JobID == queue.JobID("") {
			t.Fatalf("line %d decoded but has no job_id", lines)
		}
	}
	if lines != n {
		t.Errorf("want %d lines, got %d", n, lines)
	}
}

func TestFileSinkUnwritablePathFailsAtConstruction(t *testing.T) {
	if _, err := NewFileSink(filepath.Join(t.TempDir(), "no-such-dir", "r.jsonl")); err == nil {
		t.Fatal("want construction error for an unwritable path, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/result/ -run TestFileSink -v`
Expected: FAIL — `NewFileSink` undefined.

- [ ] **Step 3: Implement FileSink**

```go
package result

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/vismod/vismod/internal/queue"
)

// FileSink appends one JSON line per envelope to a file.
//
// Opening happens at construction so an unwritable path is a BOOT error,
// not a surprise on the first verdict.
//
// One process per file. Two replicas appending to one file interleave in
// exactly the way the audit log does — compose gives each replica its own
// volume for that reason, and the same applies here.
type FileSink struct {
	mu sync.Mutex // serializes appends so concurrent lines never interleave
	f  *os.File
	d  dedupe
}

func NewFileSink(path string) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("result: open file sink %s: %w", path, err)
	}
	return &FileSink{f: f}, nil
}

func (s *FileSink) Write(_ context.Context, env ResultEnvelope) error {
	if !s.d.Claim(env.JobID) {
		return nil // idempotent per JobID
	}
	b, err := json.Marshal(env)
	if err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: marshal envelope: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: write envelope: %w", err)
	}
	return nil
}

func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

var _ Sink = (*FileSink)(nil)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/result/ -run TestFileSink -v`
Expected: PASS.

- [ ] **Step 5: Run the race detector**

Run: `go test -race ./internal/result/ -run TestFileSinkConcurrent -v`
Expected: PASS with no race reported. This test exists specifically to catch an unguarded write.

- [ ] **Step 6: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/result/file.go internal/result/file_test.go
git commit -m "result: add FileSink (append-only JSONL, idempotent per JobID)"
```

---

### Task 4: WebhookSink

**Files:**
- Create: `internal/result/webhook.go`
- Test: `internal/result/webhook_test.go`

**Interfaces:**
- Consumes: `result.Sink`, `result.ResultEnvelope`, `moderate.DoJSON` (`internal/moderate/httpx.go:43`).
- Produces: `result.NewWebhookSink(url string, timeout time.Duration, maxAttempts int) *WebhookSink`. Task 5 calls it.

**Background the implementer needs:** `moderate.DoJSON(ctx, client, build, maxAttempts, baseBackoff, errCodeHeader)` already implements the exact retry classification this sink needs — 429/5xx/timeout retryable with `Retry-After` honored, other 4xx terminal. Reuse it rather than writing a second copy. `internal/result` importing `internal/moderate` introduces no cycle (`moderate` does not import `result`).

- [ ] **Step 1: Write the failing test**

```go
package result

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vismod/vismod/pkg/moderation"
)

func TestWebhookSinkPostsEnvelope(t *testing.T) {
	var gotMethod, gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 3)
	sent := envFixture("job-1")
	if err := s.Write(context.Background(), sent); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %s want POST", gotMethod)
	}
	if !strings.HasPrefix(gotType, "application/json") {
		t.Errorf("content-type: got %q", gotType)
	}
	var round ResultEnvelope
	if err := json.Unmarshal(gotBody, &round); err != nil {
		t.Fatalf("body is not a decodable envelope: %v", err)
	}
	if round.JobID != sent.JobID {
		t.Errorf("job_id must reach the receiver for its own dedupe: got %q", round.JobID)
	}
	if round.Result == nil || round.Result.Overall.Verdict != moderation.VerdictAllow {
		t.Errorf("envelope did not round-trip: %+v", round)
	}
}

func TestWebhookSinkRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 3)
	if err := s.Write(context.Background(), envFixture("job-1")); err != nil {
		t.Fatalf("want success after retry, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("want 2 attempts, got %d", calls.Load())
	}
}

func TestWebhookSinkRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 5*time.Second, 3)
	if err := s.Write(context.Background(), envFixture("job-1")); err != nil {
		t.Fatalf("want success after 429 retry, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("want 2 attempts, got %d", calls.Load())
	}
}

func TestWebhookSinkTerminalOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 3)
	if err := s.Write(context.Background(), envFixture("job-1")); err == nil {
		t.Fatal("want error on 400, got nil")
	}
	if calls.Load() != 1 {
		t.Errorf("400 is terminal: want 1 attempt, got %d", calls.Load())
	}
}

func TestWebhookSinkCapsAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 2)
	if err := s.Write(context.Background(), envFixture("job-1")); err == nil {
		t.Fatal("want error after exhausting attempts, got nil")
	}
	if calls.Load() != 2 {
		t.Errorf("want exactly max_attempts=2 calls, got %d", calls.Load())
	}
}

func TestWebhookSinkIdempotentPerJobID(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 3)
	env := envFixture("job-1")
	for range 3 {
		if err := s.Write(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("redelivery double-posted: %d calls, want 1", calls.Load())
	}
}

func TestWebhookSinkFailedWriteIsRetriableLater(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewWebhookSink(srv.URL, 2*time.Second, 1)
	env := envFixture("job-1")
	if err := s.Write(context.Background(), env); err == nil {
		t.Fatal("want first write to fail")
	}
	// A failed send must NOT be recorded as written, or the queue's
	// redelivery of this job would silently skip the webhook forever.
	fail.Store(false)
	if err := s.Write(context.Background(), env); err != nil {
		t.Fatalf("redelivery after failure must retry the send, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("want 2 calls, got %d", calls.Load())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/result/ -run TestWebhookSink -v`
Expected: FAIL — `NewWebhookSink` undefined.

- [ ] **Step 3: Implement WebhookSink**

```go
package result

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/internal/queue"
)

const (
	defaultWebhookTimeout  = 5 * time.Second
	defaultWebhookAttempts = 3
	webhookBaseBackoff     = 500 * time.Millisecond
)

// WebhookSink POSTs each envelope as JSON to an operator-configured
// receiver.
//
// Retry classification is delegated to moderate.DoJSON — 429/5xx/timeout
// retryable with Retry-After honored, other 4xx terminal — so there is
// exactly one copy of that policy in the codebase.
//
// The receiver gets JobID in the body and is expected to dedupe on it:
// the in-process `written` set below cannot survive a worker restart,
// and at-least-once delivery means a restart can resend.
type WebhookSink struct {
	url         string
	client      *http.Client
	maxAttempts int
	d           dedupe
}

func NewWebhookSink(url string, timeout time.Duration, maxAttempts int) *WebhookSink {
	if timeout <= 0 {
		timeout = defaultWebhookTimeout
	}
	if maxAttempts <= 0 {
		maxAttempts = defaultWebhookAttempts
	}
	return &WebhookSink{
		url:         url,
		client:      &http.Client{Timeout: timeout},
		maxAttempts: maxAttempts,
	}
}

func (s *WebhookSink) Write(ctx context.Context, env ResultEnvelope) error {
	// Claim the JobID BEFORE sending so two concurrent redeliveries of the
	// same job cannot both POST. The claim is released on failure, or the
	// queue's redelivery would skip this sink forever.
	if !s.d.Claim(env.JobID) {
		return nil
	}
	b, err := json.Marshal(env)
	if err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: marshal envelope: %w", err)
	}
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}
	if _, err := moderate.DoJSON(ctx, s.client, build, s.maxAttempts, webhookBaseBackoff, ""); err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: webhook post: %w", err)
	}
	return nil
}

var _ Sink = (*WebhookSink)(nil)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/result/ -run TestWebhookSink -v`
Expected: PASS. `TestWebhookSinkRetriesOn429` takes ~1s because `Retry-After: 1` is honored.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/result/webhook.go internal/result/webhook_test.go
git commit -m "result: add WebhookSink reusing moderate.DoJSON retry classification"
```

---

### Task 5: Wire sinks into the composition root

**Files:**
- Create: `internal/cli/sinks.go`
- Create: `internal/cli/sinks_test.go`
- Modify: `internal/cli/serve.go:81-82`
- Modify: `internal/cli/scan.go:95`
- Modify: `internal/observe/observe.go` (add `SinkWriteFailuresTotal` to `Metrics` and `NewMetrics`)

**Interfaces:**
- Consumes: `config.OutputConfig` (Task 1), `result.NewMultiSink` (Task 2), `result.NewFileSink` (Task 3), `result.NewWebhookSink` (Task 4).
- Produces: `cli.buildSinks(cfg config.Config, stdout io.Writer, m *observe.Metrics) (result.Sink, func() error, error)`.

- [ ] **Step 1: Add the failure metric**

In `internal/observe/observe.go`, add to the `Metrics` struct:

```go
	SinkWriteFailuresTotal *prometheus.CounterVec
```

In `NewMetrics()`, add to the literal:

```go
		SinkWriteFailuresTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vismod_sink_write_failures_total",
			Help: "Result-sink write failures, by sink type.",
		}, []string{"type"}),
```

and add it to the `reg.MustRegister(...)` call.

- [ ] **Step 2: Write the failing test**

Create `internal/cli/sinks_test.go`:

```go
package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
)

func TestBuildSinksDefaultsToStdout(t *testing.T) {
	cfg := config.Defaults()
	s, closeFn, err := buildSinks(cfg, io.Discard, observe.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if s == nil {
		t.Fatal("want a sink, got nil")
	}
}

func TestBuildSinksConstructsFileSink(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.jsonl")
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{{Type: "file", Path: p}}

	_, closeFn, err := buildSinks(cfg, io.Discard, observe.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("file sink must create its file at construction: %v", err)
	}
}

func TestBuildSinksUnwritableFilePathRefusesBoot(t *testing.T) {
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{{Type: "file", Path: filepath.Join(t.TempDir(), "nope", "out.jsonl")}}
	if _, _, err := buildSinks(cfg, io.Discard, observe.NewMetrics()); err == nil {
		t.Fatal("want boot refusal for an unwritable path, got nil")
	}
}

func TestBuildSinksUnknownTypeRefusesBoot(t *testing.T) {
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{{Type: "carrier-pigeon"}}
	if _, _, err := buildSinks(cfg, io.Discard, observe.NewMetrics()); err == nil {
		t.Fatal("want boot refusal for an unknown sink type, got nil")
	}
}

func TestBuildSinksClosesFileOnPartialFailure(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jsonl")
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{
		{Type: "file", Path: good},
		{Type: "file", Path: filepath.Join(dir, "nope", "bad.jsonl")},
	}
	if _, _, err := buildSinks(cfg, io.Discard, observe.NewMetrics()); err == nil {
		t.Fatal("want boot refusal, got nil")
	}
	// The already-opened good file must have been closed, not leaked.
	// Nothing portable asserts an fd is closed, so assert the contract
	// holds by re-opening exclusively — on Windows this fails if the
	// handle leaked.
	f, err := os.OpenFile(good, os.O_RDWR, 0o600)
	if err != nil {
		t.Errorf("first sink's file handle leaked after partial failure: %v", err)
	} else {
		f.Close()
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestBuildSinks -v`
Expected: FAIL — `buildSinks` undefined.

- [ ] **Step 4: Implement buildSinks**

Create `internal/cli/sinks.go`:

```go
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/result"
)

// buildSinks turns output.sinks into the single Sink the pipeline holds.
//
// Every sink is constructed eagerly so a bad path or URL is a BOOT
// failure, never a surprise on the first verdict. If any sink fails to
// construct, the ones already built are closed before returning.
//
// The returned close func must be deferred by the caller.
func buildSinks(cfg config.Config, stdout io.Writer, m *observe.Metrics) (result.Sink, func() error, error) {
	sinks := make([]result.Sink, 0, len(cfg.Output.Sinks))
	names := make([]string, 0, len(cfg.Output.Sinks))
	closers := make([]func() error, 0, len(cfg.Output.Sinks))

	closeAll := func() error {
		var firstErr error
		for _, c := range closers {
			if err := c(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	for i, sc := range cfg.Output.Sinks {
		switch strings.ToLower(strings.TrimSpace(sc.Type)) {
		case "stdout":
			sinks = append(sinks, result.NewJSONLSink(stdout))
			names = append(names, "stdout")
		case "file":
			fs, err := result.NewFileSink(sc.Path)
			if err != nil {
				_ = closeAll()
				return nil, nil, fmt.Errorf("output.sinks[%d]: %w", i, err)
			}
			sinks = append(sinks, fs)
			names = append(names, "file")
			closers = append(closers, fs.Close)
		case "webhook":
			sinks = append(sinks, result.NewWebhookSink(sc.URL, sc.Timeout, sc.MaxAttempts))
			names = append(names, "webhook")
		default:
			_ = closeAll()
			return nil, nil, fmt.Errorf("output.sinks[%d]: unknown sink type %q", i, sc.Type)
		}
	}

	onFail := func(sinkType string) {
		if m != nil {
			m.SinkWriteFailuresTotal.WithLabelValues(sinkType).Inc()
		}
	}
	return result.NewMultiSink(sinks, names, onFail), closeAll, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestBuildSinks -v`
Expected: PASS.

- [ ] **Step 6: Call it from serve**

Replace `internal/cli/serve.go:81-83`:

```go
	sinkFile := os.Stdout
	sink := result.NewJSONLSink(sinkFile)
	p := buildPipeline(cfg, mod, sink, auditLog, log)
```

with:

```go
	sink, closeSinks, err := buildSinks(cfg, os.Stdout, metrics)
	if err != nil {
		return err
	}
	defer func() {
		if err := closeSinks(); err != nil {
			log.Error("closing result sinks failed", "err", err)
		}
	}()
	p := buildPipeline(cfg, mod, sink, auditLog, log)
```

**Implementer note:** `metrics` must already be in scope at line 81. Check where `observe.NewMetrics()` is called in `serve.go` — if it is constructed later than line 81, move that construction above this block rather than passing `nil`.

- [ ] **Step 7: Call it from scan**

Replace `internal/cli/scan.go:95-96`:

```go
		sink := result.NewJSONLSink(cmd.OutOrStdout())
		p := buildPipeline(cfg, mod, sink, auditLog, log)
```

with:

```go
		sink, closeSinks, err := buildSinks(cfg, cmd.OutOrStdout(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = closeSinks() }()
		p := buildPipeline(cfg, mod, sink, auditLog, log)
```

`scan` passes `nil` metrics deliberately — the one-shot CLI registers no Prometheus registry.

- [ ] **Step 8: Verify default behavior is unchanged**

Run the existing CLI tests, which assert stdout JSONL output:

Run: `go test ./internal/cli/ -v`
Expected: PASS with no test modified. If a test fails, the default path changed — fix `buildSinks`, do not edit the test.

- [ ] **Step 9: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/cli/sinks.go internal/cli/sinks_test.go internal/cli/serve.go internal/cli/scan.go internal/observe/observe.go
git commit -m "cli: build result sinks from output.sinks config"
```

---

### Task 6: Documentation

**Files:**
- Modify: `config.example.yaml`
- Modify: `README.md`
- Modify: `CLAUDE.md` (the `internal/result/` line in "Shape of the code")
- Modify: `AGENTS.md` (architecture map + a gotcha)
- Modify: `deploy/README.md`
- Modify: `deploy/compose/README.md`
- Modify: `docs/agent/STATUS.md`, `docs/agent/TASKS.md`, `docs/agent/UNVERIFIED.md`

- [ ] **Step 1: Document the config surface**

Append to `config.example.yaml`:

```yaml
# Where result envelopes go. Omit this block entirely for stdout-only
# (the default, and what every release before this one did).
#
# A present block with an empty `sinks` list refuses to boot: emitting
# nothing silently is exactly the failure mode this project avoids.
output:
  sinks:
    - type: stdout

    # Append-only JSONL. ONE PROCESS PER FILE — two replicas sharing a
    # path interleave writes, the same hazard the audit log has.
    # - type: file
    #   path: /var/lib/vismod/results.jsonl

    # POST each envelope to a receiver. This URL is OPERATOR config, not
    # job input, so private/internal addresses are expected and allowed
    # (SECURITY.md's provider-endpoint class). The receiver should dedupe
    # on job_id: delivery is at-least-once and a worker restart can
    # resend.
    # - type: webhook
    #   url: https://collector.internal/results
    #   timeout: 5s
    #   max_attempts: 3
```

- [ ] **Step 2: Add the AGENTS.md gotcha**

Under "Gotchas" in `AGENTS.md`:

```markdown
- `MultiSink` attempts EVERY sink and returns the FIRST error — not
  fail-fast. A webhook outage must not suppress the local JSONL record.
  The error still reaches the queue's `Retry` disposition, and per-JobID
  idempotency makes the sinks that already succeeded no-ops on redelivery.
  `WebhookSink` claims a JobID before sending and RELEASES it on failure;
  dropping that release would make a failed webhook permanently skipped
  on redelivery.
```

Add `internal/result/` to the architecture map entry: `ResultEnvelope + Sink implementations (JSONL, file, webhook, multi)`.

- [ ] **Step 3: Note the per-replica file constraint**

In `deploy/README.md` and `deploy/compose/README.md`, next to the existing per-replica audit-volume explanation, add that a `file` sink has the same constraint: each replica needs its own path or its own volume.

- [ ] **Step 4: Update the agent docs**

- `docs/agent/STATUS.md`: rewrite the "Where things stand" tail with what landed.
- `docs/agent/TASKS.md`: remove any entry this completed.
- `docs/agent/UNVERIFIED.md`: add — no webhook sink has been exercised against a real receiver outside `httptest`; no `file` sink has been run under two live replicas. State what would prove each.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add config.example.yaml README.md CLAUDE.md AGENTS.md deploy/README.md deploy/compose/README.md docs/agent/
git commit -m "docs: document output.sinks and the per-replica file-sink constraint"
```

---

## Self-review notes

Checked against `docs/superpowers/specs/2026-07-31-url-source-and-output-sinks-design.md`, Part 2:

- `MultiSink` all-attempted / first-error → Task 2 ✓
- `WebhookSink` reusing `DoJSON` classification, `max_attempts`, JobID for receiver dedupe → Task 4 ✓
- `FileSink` O_APPEND, mutex, per-replica warning → Tasks 3, 6 ✓
- Boot validation: unknown type, webhook without URL, webhook URL rules, unwritable file path, empty sink list → Tasks 1, 5 ✓
- Absent `output` block = stdout, unchanged → Tasks 1, 5 (Step 8) ✓
- `vismod_sink_write_failures_total{type}` → Task 5 ✓
- SECURITY.md's third URL class → **deferred to the fetcher plan**, where SECURITY.md is edited anyway; Task 1's code comment and Task 6's `config.example.yaml` comment carry the rule in the meantime. If the fetcher plan is not executed, add a SECURITY.md paragraph here.

`ConfigHash` exclusion is asserted in the fetcher plan's config task, which touches the same function. If this plan ships alone, add a test here asserting `ConfigHash` is unchanged when `Output` differs — the hash takes only `(adapterName, modelVersion, thresholds)`, so it is exclusion by construction, but a regression test is cheap.
