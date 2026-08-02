# AGENTS.md — working agreement for coding agents

Canonical guide for changing this repo. For what vismod *is*, read
[CLAUDE.md](CLAUDE.md) first — it is the landing page and links
everything else.

Public-good trust & safety tooling: **fail-safe behavior and
auditability outrank convenience in every design decision.**

## Commands

```sh
go build ./...
go vet ./...
go test ./...            # full suite: NO network, NO credentials needed
go test -update ./internal/moderate/...   # regenerate golden files
go build -o vismod ./cmd/vismod
```

- Tests use fakes (`fakeModerator`, `fakeFrameSource`), `httptest`, and
  miniredis. FFmpeg integration tests skip automatically when
  ffmpeg/ffprobe are absent (synthesized `lavfi testsrc` clips; never
  real media).
- Windows/PowerShell 5.1: multi-line git commit messages break with
  here-strings — write the message to a file and `git commit -F <file>`.

## Loop protocol

One iteration = one task, landed green.

1. Read `docs/agent/STATUS.md`, then take the top unblocked entry in
   `docs/agent/TASKS.md`.
2. Do that task at the scope written. Deliver what the task asks —
   don't quietly narrow, widen, or transform it. If the task looks
   mistaken or a better approach exists, say so in a sentence and
   continue with it as written.
3. Run the done gate below. Land the change and its tests as one commit.
4. Update `docs/agent/STATUS.md`. Remove the finished entry from
   `docs/agent/TASKS.md`. Append to `docs/agent/UNVERIFIED.md` anything
   you asserted but could not run locally, with what would prove it.

Delegate to a subagent only for large, genuinely independent
investigation spanning many files. Do not delegate work finishable in a
handful of tool calls, and do not spawn agents to re-check your own work.

## Done gate

A task is done when all of these hold:

```sh
go build ./... && go vet ./... && go test ./...
```

- All three exit 0. A skipped ffmpeg test is a pass; a failing one is not.
- Every doc under "Docs that must stay true" whose described behavior
  changed is updated in the SAME commit.
- No new module import unless the commit message justifies it.
- No secret, media byte, provider `Raw`, or free text added to any
  envelope, log, audit record, queue payload, or UI surface.
- If the change touched rollup, thresholds, or null handling: the
  existing rollup tests still pass UNMODIFIED. Making them pass by
  weakening them is a failed gate, not a fix.

## Invariants — do not weaken these

1. **Never `allow` on failure.** Provider error, unreadable input,
   extraction failure, zero frames, all-null scores → `verdict:"error"`
   + dead-letter. Verdict precedence is strict:
   `block > error > flag > allow` (`internal/pipeline/rollup.go`).
2. **Null discipline.** `score: null` = could-not-evaluate. Never emit
   `0` for unknown (0 means "confidently safe"). Nullable envelope
   fields serialize as JSON `null`, never omitted. `max_score` /
   `confidence` are null when no non-null score exists.
3. **Never persist or transmit media downstream of analysis.** Queue
   payloads, sink envelopes, audit records, logs, and the web UI carry
   refs, hashes, verdicts, and counts only — no media bytes, no provider
   `Raw`, no OCR/captions/PII. Audit stores `SHA-256(Raw)`, never `Raw`.
4. **Secrets are env-only** (`VISMOD_*`, e.g. `VISMOD_MICROSOFT_API_KEY`),
   accessed via `config.Secret()` — never in yaml, code, tests, logs, or
   envelopes.
5. **No shell in the extraction path.** FFmpeg runs via
   `exec.CommandContext` with a rendered arg slice. Workflow guardrails
   (`internal/frames/workflow.go`): placeholder allow-list
   (`{{.Input}}/{{.WorkDir}}/{{.MaxFrames}}/{{.MaxWidth}}`), exactly one
   `-i {{.Input}}`, protocol deny-list (incl. `subfile,,opts:` comma
   forms and `://`), output confined to WorkDir, `-nostdin` enforced.
6. **Idempotency per JobID.** Queue delivery is at-least-once (redisq);
   `Sink.Write` and the audit append dedupe per JobID so redelivery
   never double-writes.
7. **Detection scope belongs to the vendor.** vismod performs no content
   detection of its own; special-category detection and protections are
   handled by each scanning vendor under that vendor's terms. Do not
   reintroduce category-specific detection logic.
8. **One Moderator per process**, chosen by `adapter.name` at startup;
   restart to change. Never add `AnalyzeVideo` to the `Moderator`
   interface — video-native providers implement the separate
   `VideoModerator` interface (pipeline type-asserts).

## Architecture map

```
cmd/vismod/            thin main -> internal/cli
pkg/moderation/        PUBLIC contract types (no internal deps): Moderator,
                       NormalizedResult, Category, ScoreOrigin, Verdict
internal/cli/          cobra composition root; the ONLY place adapters are
                       blank-imported; wire.go assembles the pipeline
internal/config/       viper loader, thresholds, DefaultWorkflows(), ConfigHash
internal/moderate/     adapter registry + shared Limiter + retrying DoJSON
internal/moderate/adapters/{microsoft,google,hive,shieldgemma}/
                       self-register in init(); shieldgemma is self-hosted
internal/frames/       FrameSource, FFmpegSource (multi-workflow union),
                       workflow guardrails, dHash Dedup
internal/fetch/        kind:"url" media download: parse-time host allow-list,
                       per-dial address deny-list, size cap, transient file
internal/queue/        Queue iface; memq (dev, non-durable) + redisq
                       (durable, at-least-once, strict FIFO via Redis LIST)
internal/pipeline/     per-job flow: frames -> dedup -> fan-out -> thresholds
                       -> rollup -> sink -> audit -> ack/DLQ
internal/result/       ResultEnvelope + Sink implementations (JSONL, file,
                       webhook, multi)
internal/audit/        append-only hash-chained log + verify (JCS canonical)
internal/observe/      slog, Prometheus metrics, backpressure, JobTracker
internal/ui/           embedded read-mostly dashboard (off by default)
```

Job flow specifics that trip people up:

- Video fan-out never cancels on first error: each frame captures its
  own result (`errgroup.SetLimit`, tasks return nil). Panic in a frame
  task or a queue handler dead-letters; the pool survives.
- `defer cleanup()` immediately after `FrameSource.Frames` returns —
  the WorkDir must be deleted on every exit path, before ack.
- Queue `Retry` disposition is only for job-level transient infra (sink
  write failure). Provider retries happen inside `moderate.DoJSON`
  (429/5xx/timeout retryable, other 4xx terminal); exhausted retries →
  frame error → verdict error → DLQ.
- Rate limiter (`moderate.Limiter`) is owned by the adapter and shared
  across workers and frame fan-out; the `limiter.Wait` sits INSIDE the
  per-attempt request builder so retries take tokens too. Default paces
  BELOW the vendor quota (e.g. 4 RPS vs Azure F0's 5 RPS).
- FIFO = dequeue order only; with >1 worker, completion order is not
  guaranteed. memq is non-durable/single-process — production and
  multi-replica need `queue.driver: redis`.
- Per-job overrides ride `queue.Job`: `Workflows []string` (union of
  extractions) and `DedupThreshold *int` (nil inherit / 0..64 enable /
  negative disable). Validate at intake AND at execution.
- Frame caps are TWO-stage: the extraction budget
  (`max_extract_frames`, default 4×`max_frames`; what `{{.MaxFrames}}`
  renders as) bounds disk during extraction; the `max_frames` scan cap
  is applied by the PIPELINE after dedup, so duplicates never consume
  scan budget. Don't reintroduce a pre-dedup `max_frames` truncation.

## Adding a vendor adapter (the designed extension point)

1. `internal/moderate/adapters/<name>/` with
   `func New(cfg moderate.AdapterConfig) (moderation.Moderator, error)`
   and `moderate.Register("<name>", New)` in `init()`. Fail fast on
   missing credentials (construction IS boot validation).
2. Blank import in `internal/cli/root.go` — the only wiring point; the
   registry never imports adapters.
3. Normalize into `pkg/moderation`: tag every score with the right
   `ScoreOrigin`; unknown → nil never 0; unmapped provider labels →
   `OTHER` with `ProviderLabel` preserved (never drop a signal); `Raw`
   sanitized (no free text).
4. Golden tests (fixture JSON → `.golden`, regen with `-update`) +
   `httptest` retry/terminal classification tests.
5. Zero pipeline changes expected. If `internal/pipeline` needs edits,
   the interface is missing something — stop and reconsider.

Dependencies are deliberately minimal (cobra, viper, errgroup,
prometheus, Vision SDK, go-redis; miniredis test-only). Every new
import needs justification.

## Gotchas

- **Viper lowercases map keys.** Threshold/workflow map keys arrive
  lowercase from yaml; `config.Load` normalizes threshold keys to
  canonical uppercase. Keep `Defaults()` map keys lowercase so yaml
  overrides merge instead of colliding (this was a real map-iteration
  flake).
- **Thresholds are per-adapter, NOT portable** (severity/6 vs likelihood
  lookup vs probability — see MODEL_LIMITATIONS.md). `ConfigHash` in
  every envelope exists to trace which tuning produced a verdict.
- One `config.Thresholds` map holds THREE key namespaces that cannot
  collide: `default`, UPPERCASE categories, and `label:<lowercased
  provider label>`. `provider_thresholds.mode` (off/hybrid/override) is
  resolved away by `Thresholds.Merge` during `config.Load`, so nothing at
  runtime branches on the mode — override mode simply produces a map with
  no category or `default` entries. `ResolveFor(cat, label)` is the ONE
  resolution path; `Resolve(cat)` delegates to it. Both the flag pass and
  the block check must keep calling it, or a label can flag without
  blocking.
- The three standard workflows live in `config.DefaultWorkflows()` as
  ordinary default config (overridable by name in yaml) — don't inject
  them anywhere else.
- dHash quirk: flat/uniform frames all hash to zero and collapse under
  dedup; unhashable frames are always KEPT.
- Google/hive RESPONSE schemas were re-verified against vendor docs on
  2026-07-29 (dated references live in each adapter's package comment).
  Re-check when touching normalization: goldens are built from fixtures
  the repo authored, so a vendor rename passes the tests and breaks
  production. That re-verification found five hive class names that never
  existed (`gore`, `violence`, `yes_drugs`, `yes_gun`, `yes_knife`) —
  dead map keys are silent, since unmapped heads legitimately fall to
  OTHER. The same pass found hive posting an undocumented JSON
  `media_b64` body; it now uploads multipart/form-data via
  `moderate.NewMultipartRequest`. Neither adapter has ever been run
  against a live vendor — see `docs/agent/UNVERIFIED.md`.
- **A url job has TWO `Source` values, and mixing them leaks a
  credential.** `pipeline.resolveSource` returns `resolved{local, env}`:
  `local` is a `kind:"file"` source pointing at the downloaded temp path
  (analysis reads this — no URL ever reaches ffmpeg), `env` is the
  redacted `kind:"url"` source (scheme+host+path plus `RefDigest`) and is
  what every envelope, audit record, log line, and UI row must use.
  `queue.Job.Source.Ref` still holds the FULL url — the fetcher needs it
  — so anything that records `j.Source.Ref` for a url job publishes a
  presigned URL's query string. That was a real bug in `serve.go`'s
  worker handler (the operator-UI job feed); `serve_run_test.go` guards
  it.
- **`fetch.New` returns a typed nil when disabled, and
  `cli.newFetcher` must convert it to an untyped nil.** Returning the
  `*fetch.Fetcher` nil directly gives `pipeline.Fetcher` a non-nil
  interface holding a nil pointer, so the `p.Fetcher == nil` guard is
  false and a url job panics instead of producing `verdict:"error"`.
  `wire.go` has an explicit `if f == nil { return nil, nil }` for this;
  do not "simplify" it away.
- **The host allow-list and the address deny-list are two checks on
  purpose.** `fetch.validateURL` runs at parse time on text; `DenyPrivate`
  runs per-connection from `net.Dialer.Control` against the address
  actually dialed. Only the second closes DNS rebinding, where an
  allow-listed name re-resolves to `169.254.169.254` after validation.
  Merging them into one parse-time check passes every test and silently
  deletes the defense. The deny-list has no config switch, and
  `Fetcher.allowScheme` ("http" only in tests, unexported) is the one
  weakening any test may do.
- UI (`internal/ui`) is read-mostly by design: pause/resume intake is
  the entire control surface; config stays restart-to-apply; never add
  anything that renders media or `Raw`.
- `MultiSink` attempts EVERY sink and returns the FIRST error — not
  fail-fast. A webhook outage must not suppress the local JSONL record.
  The error still reaches the queue's `Retry` disposition, and per-JobID
  idempotency makes the sinks that already succeeded no-ops on redelivery.
  `WebhookSink` claims a JobID before sending and RELEASES it on failure;
  dropping that release would make a failed webhook permanently skipped
  on redelivery.
- **A sink failure costs vendor money and loses the audit record.** The
  sink write happens BEFORE `p.Audit.Record` in
  `internal/pipeline/pipeline.go`, and a sink error returns
  `queue.Retry`. So a third-party webhook receiver being down means: the
  whole job re-runs on each retry — frame extraction and a fresh BILLED
  vendor call included, up to `queue.max_retries` (default 3, i.e. 4
  pipeline runs per job) — and when it finally dead-letters there is NO
  audit entry for it at all, even though stdout and the `file` sink
  already hold the envelope. Compounding it, `moderate.DoJSON` honors
  `Retry-After` up to 120s and the webhook's own `max_attempts` are
  serial, so one job can hold a worker for minutes. This ordering is
  intentional-by-inertia, not proven correct; treat changing it as a
  design change with its own review, and until then budget the webhook's
  `timeout` and `max_attempts` as documented in `config.example.yaml`.
- Sink idempotency is **per process**, not durable. Only the audit log
  replays its file on open (`audit.Open` rebuilds `seen`). A restart
  resets every sink's dedupe set, so a job redelivered after a restart
  gets a second file line and a second webhook POST. Consumers dedupe on
  `job_id`; do not write docs that equate the sinks to the audit log.

## Docs that must stay true

README.md, CLAUDE.md, SECURITY.md (workflow trust boundary, SSRF
posture, audit scope), RESPONSIBLE_USE.md (vendor-scope detection,
human-in-the-loop), MODEL_LIMITATIONS.md, CONTRIBUTING.md,
config.example.yaml, docs/custom-ffmpeg-workflows.md,
docs/rest-api.md (intake contract, url rules), deploy/README.md
(autoscaling contract), deploy/compose/README.md (per-replica volume
constraints). If you change behavior they describe, update them in the
same commit.
