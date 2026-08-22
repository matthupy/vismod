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
gofmt -l .    # must print NOTHING
go build ./... && go vet ./... && go test ./...
```

- All four pass. A skipped ffmpeg test is a pass; a failing one is not.
- **`gofmt -l .` is first because it is the one CI runs that nothing else
  catches.** `go vet` does not check formatting and `go test` does not
  care, so a build-vet-test gate is green on code CI rejects. The usual
  way in is a scripted edit (`sed` over call sites) that produces valid
  but unformatted Go — `2*time.Second` rather than `2 * time.Second`.
- **Every line the change adds is covered, or the commit says why not.**
  The target is 100% patch coverage; >=90% is accepted, and the gap is
  reserved for lines that cannot be covered — branches unreachable by
  construction, kept as guards against a future edit. Below 80% the
  change is undertested and not done. Name the uncovered lines and the
  reason in the commit message; "hard to test" is a reason to restructure
  the code, not to skip the test. Failure paths come first: a sink that
  cannot render, a provider that ran out of retries, a config that must
  refuse to boot. Those are where the fail-safe posture lives, and an
  untested failure arm is how a silent `allow` ships.
- Every doc under "Docs that must stay true" whose described behavior
  changed is updated in the SAME commit.
- No new module import unless the commit message justifies it.
- No secret, media byte, provider `Raw`, or free text added to any
  envelope, log, audit record, queue payload, or UI surface. ONE
  carve-out: caller-supplied `metadata` (`queue.Job.Metadata` /
  `ResultEnvelope.Metadata`, validated by `queue.ValidateMetadata`) is
  opaque JSON permitted in the queue payload, the result envelope, and —
  only for keys an operator NAMED in `output.sinks[].metadata_fields` — a
  non-JSON formatter's rendered output. It is still forbidden in the audit
  log, in logs, and in the UI, and it never influences a verdict. Widening
  it beyond an operator-named allow-list needs its own review: the point
  of the allow-list is that a caller adding a metadata key cannot start
  publishing it anywhere new without an operator naming it first.
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
cmd/vismod-eval/       eval harness CLI, separate binary on purpose: the
                       harness never moderates and is not in the prod image
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
                       (durable, at-least-once, strict FIFO via Redis LIST;
                       PER-REPLICA processing lists + instance heartbeats)
internal/pipeline/     per-job flow: frames -> dedup -> fan-out -> thresholds
                       -> rollup -> sink -> audit -> ack/DLQ
internal/result/       ResultEnvelope + Sink implementations (JSONL, file,
                       webhook, multi) + routing (Predicate, RoutedSink)
                       and rendering (Formatter: json, discord)
internal/audit/        append-only hash-chained log + verify (JCS canonical)
internal/observe/      slog, Prometheus metrics, backpressure, JobTracker
internal/ui/           embedded read-mostly dashboard (off by default)
internal/eval/         evaluation harness, NOT reachable from internal/cli:
                       corpus/ loads the v1 manifest and desugars the CSV
                       on-ramp into one, then digests the manifest it wrote
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
  extractions) and `DedupThreshold *int` (nil inherit / 0..ceiling enable
  / negative disable). Validate at intake AND at execution. The ceiling
  is `frames.dedup.hamming_threshold`: a job may TIGHTEN dedup or turn it
  off, never loosen it. Every pair of 64-bit dHashes is within distance
  64, so an unbounded override collapses a video into frame 0 and that
  frame decides the verdict — a fail-open. Intake rejects above the
  ceiling; the pipeline re-clamps because a job can reach Redis without
  passing intake.
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
- **`fetch.New` returns a typed nil when `allow_hosts` is empty, and
  `cli.newFetcher` must convert it to an untyped nil.** Returning the
  `*fetch.Fetcher` nil directly gives `pipeline.Fetcher` a non-nil
  interface holding a nil pointer, so the `p.Fetcher == nil` guard is
  false and a url job panics instead of producing `verdict:"error"`.
  `wire.go` has an explicit `if f == nil { return nil, nil }` for this;
  do not "simplify" it away.
- **The in-process boot seam lives in `internal/cli/boot.go`.**
  `cli.Serve(ctx, cfg, cli.WithModeratorDecorator(fn))` is the exported
  entry point for a caller running the serve stack in-process — the eval
  harness, whose capture/replay wrappers sit on the `moderation.Moderator`
  and have nowhere to install themselves in a separately launched `vismod
  serve`. The hook's type is `func(moderation.Moderator)
  moderation.Moderator` and nothing more: naming a concrete capturing type
  here would make the eval binary reachable from the production image.
  Three placement rules, each with a test that fails when it moves — the
  hook runs BEFORE `observe.InstrumentModerator` and `buildPipeline`
  (outside them, a capture counts the wrapper and the pipeline analyzes
  through the undecorated moderator) and AFTER `validateProviderLabelBoot`
  (a boot check reading an optional interface off a wrapper is a check that
  silently stops checking). A nil hook, a nil return (including a typed
  nil), or a decorator that DROPS `ModelVersion()` or `AnalyzeVideo()` the
  adapter satisfied is a boot failure: the capability just disappears with
  nothing logged, stamping `model_version:"unversioned"` on every envelope
  forever or silently falling back to frame extraction. The guard requires
  forwarding, not invention. The MANDATORY methods — `Close()`, `Name()`
  and `Capabilities()` — cannot be guarded at all: every decorator has
  them, so an assertion proves presence and never forwarding. They are
  contract, not check. A decorator that overrides `Name()` to `""` poisons
  `ConfigHash` and the `Provider` on every record; one that returns a zero
  `Caps` kills the video branch even with `AnalyzeVideo()` forwarded (it
  gates on both) and drops the oversize pre-flight. Embedding
  `moderation.Moderator` forwards all three by construction — do not
  override one without forwarding. Rationale in `boot.go`'s godoc; guards
  pinned by
  `internal/cli/architecture_test.go` and the decorator tests.
- **The address policy is chosen from the hostname, before resolution.**
  `Fetcher.dial` picks `DenyMetadata` over `DenyPrivate` only when the
  dialed hostname is in `allow_private_hosts`. Selecting it from the
  resolved IP instead would invert the check: any host that resolved into
  RFC 1918 would get the weaker policy, which is exactly the rebinding
  attack.
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
- **A sink predicate can only NARROW, so the fail-safe lives in the config
  SHAPE, not in the predicate.** `result.Predicate` is a closed struct
  (verdicts / categories+min score / source kinds; OR within a group, AND
  across groups) precisely so an unfireable rule is a boot refusal. But
  no per-predicate check can catch the real hazard: if EVERY sink is
  predicated, an envelope matching none of them is emitted nowhere, and a
  rule that is syntactically perfect and simply never matches passes every
  static check. `config.validateOutput` and `cli.buildSinks` therefore
  both require at least one sink with `Predicate.IsEmpty()`, overridable
  only by `output.allow_unrouted`. Do not add an expression language here
  — the moment a predicate needs a parser, that boot-time guarantee is
  gone. Two more rules that look like details and are not: a predicate
  MISS returns nil (an error would reach `queue.Retry` and re-bill the
  vendor for every allow verdict), and a nil `Score` never satisfies a
  minimum, including a minimum of 0 — could-not-evaluate is not a value.
- **A non-JSON `Formatter` is a disclosure boundary.** `format` (json,
  discord) is orthogonal to `type` (stdout, file, webhook): transport and
  rendering are separate axes, so a chat integration is a formatter, never
  a new sink type. Chat formatters are allow-list renderers that name
  every field they emit, so adding a field to `ResultEnvelope` can never
  silently start publishing it to Discord;
  `TestDiscordFormatterNeverLeaksMetadata` fails on exactly that, and
  `Source.RefDigest` and the provider raw digest stay out permanently.
  Caller `metadata` is the ONE opt-in exception, because passthrough that
  dies before the notification a human reads is not passthrough: a
  correlation id is the field operators most need in an alert.
  `output.sinks[].metadata_fields` names the keys a sink may publish (`*`
  for all), defaulting to none. Keep it an allow-list rather than a
  boolean — the caller who WRITES metadata is not the operator who
  configures the sink, so an enumerated key is a reviewable disclosure
  decision while `include_metadata: true` is a blank cheque against every
  future caller. Note the limit honestly: only key NAMES are knowable at
  boot, so vismod can promise "only the fields you named" and never "only
  safe content". `metadata_fields` on `format: json` is a boot refusal,
  not a no-op — that envelope already carries everything. Null scores
  render as `unknown`, never `0.00`, for the same reason they serialize as
  `null`.
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
- **A webhook URL is a CREDENTIAL, so nothing on the delivery path may log
  it.** A Discord webhook URL carries its token in the PATH
  (`/api/webhooks/<id>/<token>`), and `url.Redacted()` does not help — it
  strips userinfo and leaves the path intact. Every log field and error
  message on the retry/failure path therefore names an operator-facing
  label (`webhook[1]`, from `WebhookOptions.Name`) and never the URL;
  `moderate.WithRetryLog` takes that label for the same reason. **Its
  parameter is called `target`, which reads like an address and is not
  one** — it is the sink's zero-based position in `output.sinks`, or
  `adapter:hive` for an adapter call. That misreading is the likely way
  this invariant gets broken, so the meaning is spelled out in three
  places (the `WithRetryLog` godoc, `WebhookOptions.Name`, and
  docs/result-envelope.md) and must stay spelled out. One value, two field
  names: `target` on the retry warning (shared with adapter calls) and
  `sink` on the give-up error (matching `output.sinks` and the
  `vismod_sink_*` metrics); they must remain equal or the two records
  cannot be joined, which `TestWebhookRetryTargetMatchesGiveUpSink`
  asserts. The
  redirect refusal in `NewWebhookSink` also stops naming its target: that
  string is attacker-chosen text on a path that gets logged.
  `TestWebhookDeliveryLogNeverCarriesTheURL` and
  `TestDoJSONRetryLogNeverCarriesTheURL` fail on any regression. The same
  rule bounds the log/metric `reason` and `status` fields to closed sets —
  an error string there is both a cardinality bomb and a way for a URL to
  reach a label.
- **"Gave up after N attempts" and "the receiver said no" are different
  errors.** `moderate.DoJSON` retries 429/5xx/timeout with exponential
  backoff and returns `ErrAttemptsExhausted`; every other 4xx returns
  immediately with an `HTTPError` and no retry at all. Both mean the
  envelope arrived nowhere, so every sink wraps `result.ErrDeliveryFailed`
  — one `errors.Is` answers "did this destination get it?" regardless of
  transport — but the bounded `reason` (`exhausted` / `rejected` /
  `format` / `write`) is what tells an operator whether to look for an
  outage or a bad payload. Do not collapse them, and do not replace the
  underlying error when wrapping: `moderation.IsRetryable` still decides
  the QUEUE-level disposition and must survive. Retries are counted in
  `vismod_sink_retries_total` and give-ups in
  `vismod_sink_write_failures_total`; rising retries with flat failures is
  a destination that is throttling but still delivering.
- **The audit digest travels out of band, and that is not an
  optimization.** `evaluateFrame` returns the adapter's `Raw` alongside
  the frame result; `processImage`/`processVideo` hand it up as evidence;
  `ProcessJob` hashes it into `ResultEnvelope.RawSHA256` (`json:"-"`) and
  drops it. `Raw` is never written onto the `NormalizedResult` the
  envelope carries. Two reasons, both load-bearing: invariant 3 forbids
  `Raw` in an envelope, and `env.Result` is a POINTER the sink already
  holds by the time `p.Audit.Record` runs — so "populate, then clear
  after the sink" is a mutation race, not a boundary. `payloadFor` keeps a
  fallback that hashes `env.Result.Raw` for envelopes built outside the
  pipeline; that is a safety net, not a second supported path. This field
  was the empty string on every record a shipped adapter ever produced
  until it was fixed, because `evaluateFrame` kept `res.Frames[0]` and
  discarded the rest.
- **Video raw evidence is sorted in lockstep with its frames.** The
  fan-out writes by extraction index; `sort.SliceStable` reorders by
  timestamp afterwards. `frameOutcome` holds the result and its raw
  response in ONE struct so the sort moves both — a second slice sorted
  separately (or not at all) misattributes every response and yields a
  digest that is wrong and unstable across runs of identical input, with
  no visible symptom. `TestRawEvidenceStaysPairedWithItsFrameAfterTheSort`
  fails on exactly that. Do not split them back apart.
- Sink idempotency is **per process and time-bounded**, not durable. Only
  the audit log replays its file on open (`audit.Open` rebuilds `seen`).
  A restart resets every sink's dedupe set, so a job redelivered after a
  restart gets a second file line and a second webhook POST. Claims also
  expire after `result.dedupeRetention` (1h) — the map used to grow for
  the life of the process and OOM-killed long-running pods, and that
  crash caused the very duplicates the map prevents. The window is far
  wider than any real redelivery (max_retries 3; Retry-After honored only
  to 120s). Consumers dedupe on `job_id`; do not write docs that equate
  the sinks to the audit log.
- The Redis processing list is **per replica**
  (`<prefix>:processing:<instance>`) with liveness in
  `<prefix>:instances`. A replica reclaims only its own list on Start;
  another replica's work is returned by the reaper once its heartbeat
  goes stale (~60s). It was one shared key, and every Start drained the
  whole thing — so any scale-up, rolling deploy, or crashloop re-ran jobs
  live replicas were processing, double-billing the vendor. Do not
  "simplify" it back to a shared key, and keep `ProcessingDepth` out of
  `QueueDepth` (the autoscaling signal) while keeping it exported as
  `vismod_processing_depth`.

## Docs that must stay true

README.md, CLAUDE.md, SECURITY.md (workflow trust boundary, SSRF
posture, audit scope), RESPONSIBLE_USE.md (vendor-scope detection,
human-in-the-loop), MODEL_LIMITATIONS.md, CONTRIBUTING.md,
config.example.yaml, docs/custom-ffmpeg-workflows.md,
docs/rest-api.md (intake contract, url rules), docs/result-envelope.md
(envelope field contract), deploy/README.md (autoscaling contract),
deploy/compose/README.md (per-replica volume constraints). If you
change behavior they describe, update them in the same commit.
