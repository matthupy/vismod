---
title: Status
nav_order: 20
---

# STATUS

Current state of the work. Rewrite this at the end of every iteration.
Keep it short — it is read cold at the start of the next one.

**Updated:** 2026-08-03

## Where things stand

All milestones M0–M6 are landed. Since M6 the following shipped on
`main`: per-job workflow selection, Hamming-distance frame dedup,
per-job dedup threshold override, rate-limiter fixes (retries take
tokens, 429 backoff floor, F0 headroom), the two-stage frame cap
(`max_extract_frames` budget + post-dedup `max_frames` scan cap), UI and
metrics for frame-extraction volume, and structured job logging.

Latest: google and hive schemas re-verified against current vendor docs
(2026-07-29). Google had no drift. Hive's response envelope was correct,
but five class-map keys named heads that do not exist in Hive's taxonomy;
they were replaced with documented heads and the golden fixture now
exercises the corrected names. That pass also caught the hive REQUEST
encoding posting an undocumented JSON `media_b64` body — now a
multipart/form-data `media` upload via `moderate.NewMultipartRequest`,
with a test that parses the outgoing body.

Then the category taxonomy was audited against all three vendors.
microsoft (4 labels) and google (5) are fully mapped with nothing falling
to OTHER. Hive's head list is far larger and had real gaps, so
SchemaVersion is now 1.1.0 with four additive categories:
`ALCOHOL_TOBACCO` (legal vice split out of DRUGS, which now means illicit
only), `GAMBLING`, `OFFENSIVE_GESTURE`, and the `ANIMATED_SYNTHETIC`
provenance carrier. The hive class map grew from 31 to 63 heads. Three
groups stay deliberately unmapped and are documented as decisions in the
MODEL_LIMITATIONS.md coverage table: negative/absence heads, ordinary
apparel (the swimwear over-flagging trap), and child-related heads
(vismod defines no special category — invariant 7).

Latest: per-provider-label thresholds ("advanced" mode). A new optional
`provider_thresholds` section with modes off / hybrid / override. The
mode is resolved away by `Thresholds.Merge` at config load, so runtime
keeps ONE resolution path (`ResolveFor(cat, label)`, precedence label >
category > default, field by field) used by both the flag pass and the
block check. Overrides apply as written including when looser, which is
how a noisy negative head gets quieted; `ConfigHash` covers them so a
tuning stays attributable. Boot refuses override mode with no labels.

Latest: desk research on self-hosted open-weight classifiers, written up
in `docs/self-hosted-classifiers.md`. Recommendation is
`google/shieldgemma-2-4b-it` first — the only candidate emitting a real
`*float64` natively, so it needs no new `ScoreOrigin` and no rollup
change. Llama Guard 4 has the best taxonomy fit (14 MLCommons hazards)
but is label-only, and the write-up found two rollup defects that a
label-only score would expose: `Confidence` is a copy of `MaxScore` so it
would read 1.0 whenever any head fired, and `TopCategory` would be
decided by adapter emission order among tied 1.0s. Both must be settled
before a label-only adapter, so no TASKS entry is filed for Llama Guard.
PerspectiveVision is disqualified — no obtainable weights.

The write-up also resolves the override-mode disarm risk with a
boot-time completeness check (adapter declares its label set; boot
rejects a declared label with no *key* under
`provider_thresholds.labels`), which cannot live in `config.Load` because
`Load` runs before the adapter exists. SECURITY.md now separates the two
URL classes: media-source URLs deny private ranges, provider-endpoint
URLs expect them. MODEL_LIMITATIONS.md records the proposed `label_only`
origin and the fact that a self-hosted `model_version` is only an
operator claim.

Latest: the `shieldgemma` adapter — the first SELF-HOSTED adapter,
speaking OpenAI-compatible chat-completions to an operator-run server
serving `google/shieldgemma-2-4b-it`. One request per enabled policy per
frame; the score is `P(Yes)` renormalized over the Yes/No token pair,
carried as the existing `OriginProbability`, so no new `ScoreOrigin`, no
rollup change and no SchemaVersion bump. Policies map
sexually_explicit→SEXUAL, violence_gore→GORE_GRAPHIC, and
dangerous_content→OTHER by documented decision (one score spanning
weapons/drugs/terrorism/suicide cannot be attributed to one of them).
Construction refuses to boot without a valid endpoint, without an explicit
`model_version`, or under any threshold mode but `override`
(`AdapterConfig` gained `ProviderThresholdMode` to make that checkable).
The endpoint validator implements SECURITY.md's provider-endpoint rules
with a test per rule, and the client's `CheckRedirect` errors.

Two conclusions from `docs/self-hosted-classifiers.md` needed correcting
once written, and both are marked inline in that doc:

1. "Deliberately unarmed = an entry with both fields nil" is not
   expressible: viper drops any yaml key with no scalar leaf, so `x: {}`,
   `x:`, `x: null` and `x: {flag_at: null}` all vanish before decoding.
   The shipped surface is a sibling list,
   `provider_thresholds.unarmed_labels`, folded into `Labels` by
   `config.Load` — so key presence, merged-map presence and `ConfigHash`
   coverage all still hold. It does not satisfy override mode's
   at-least-one-armed-label rule.
2. A lone `Yes` token is NOT scored. Falling back to its raw probability
   would put unnormalized `P(token)` under the same `probability` origin —
   two quantities, one origin. A server reporting only one of the pair
   makes the frame unscorable (error verdict, never allow).

Latest: fixed the instrumented-moderator `ModelVersion()` loss.
`observe.InstrumentModerator` wrapped the Moderator without forwarding the
optional `ModelVersion()` interface, so under `serve` (which instruments
BEFORE `buildPipeline`) every envelope carried
`model_version: "unversioned"` and a `config_hash` computed over that
string, while `scan` (no instrumentation) carried the real one — one audit
question, two answers per command. Fixed by forwarding from the wrapper,
not by reordering the call: `instrumentedVersioned` and
`instrumentedVideoVersioned` join `instrumentedVideo`, selected by which
optional interfaces the underlying Moderator satisfies. Cost is one type
per combination, deliberately paid so the wrapper never declares a method
the inner moderator lacks (that would make the caller's `"unversioned"`
fallback unreachable and stamp an empty string instead). `serve` still
asserts `ProviderLabels()` on the UNWRAPPED moderator — the wrapper does
not forward that one, and the boot check should not depend on what the
wrapper happens to forward.

Envelopes written by `serve` before this fix carry
`model_version: "unversioned"` and a `config_hash` over that string. They
are NOT comparable with envelopes written after it, even from an identical
model and threshold set.

The boot-time label completeness check landed as
`cli.validateProviderLabelBoot`, run after `buildModerator` in both `scan`
and `serve` (it cannot live in `config.Load`, which runs before the
adapter exists). The adapter declares its labels via an optional
`ProviderLabels() []string`, type-asserted like `modelVersioner`.

Latest: the Docker image was actually built and run (2026-07-31, engine
29.5.3, linux/amd64), settling the oldest UNVERIFIED entry. Image
`sha256:fc19c39a66f8`, 564 MB. Proven in-container: `version` prints,
UID is 10001 (`vismod`, non-root), ffmpeg and ffprobe 5.1.9 both run and
can write to a mounted volume, `HEALTHCHECK` reports `healthy` under
`serve`, `healthcheck` alone exits 2 with nothing listening (fail
closed), `docker stop` drains cleanly in 0.2s and exits 0, and a `scan`
against an unreachable endpoint yields `verdict:"error"` + exit 2 rather
than an allow.

That run found the README's Docker quick-start wrong: both `docker run`
lines omitted a config file, and there is no usable env-only
configuration — `Load` gives viper no defaults, so `AutomaticEnv` has no
key to overlay and even `VISMOD_ADAPTER_NAME` is ignored; boot fails
with `unknown adapter ""`. The VISMOD_* overlay only overrides keys the
yaml already sets (which is all `TestLoadYAMLAndEnvOverlay` ever
claimed). README now mounts a config, and notes that the default
`intake_addr` of `127.0.0.1:8080` is unreachable from a published port.

Not a Docker defect, but observed: `scene-detect` on a constant-colour
clip extracts zero frames, which is correctly an error verdict, not an
allow.

Latest: Docker Compose support. `compose.yaml` at the repo root runs a
local stack — Redis, two `vismod serve` replicas on the redis queue
driver, Prometheus, Grafana — plus `deploy/compose/compose.prod.example.yaml`,
a minimal single-replica production shape (persisted Redis, resource
limits, no published ports). Verified: both replicas boot healthy on the
redis driver; intake `POST /jobs` was accepted and claimed by exactly one
replica; the UI returns 401 unauthenticated and 200 with basic auth;
`SIGTERM` drain logged `drained cleanly` and exited 0; both replicas'
audit chains verified independently (0 vs 1 records, proving the
per-replica audit volumes are isolated); both Prometheus targets report
`up` and `vismod_queue_depth` returns 2 series; all 8 Grafana dashboard
panel queries returned non-zero series counts; the production stack
parsed, ran, and a Redis marker key plus `dbsize` survived `down` then
`up`. A clean-state re-run of the documented quick start
(`docker compose down -v`, `up -d --build`, `ps`) came up healthy on the
first try — no documented command needed correction.

Every job in this environment ends `verdict:"error"` — there is no
vendor credential here, so no compose run has observed a successful
`allow` end to end. That is correct fail-safe behavior, not a compose
defect; recorded in `docs/agent/UNVERIFIED.md`.

Latest: configurable output sinks. `output.sinks` (`config.example.yaml`)
replaces the fixed stdout-only JSONL writer with any combination of
`stdout`, `file`, and `webhook` entries, selected at boot by
`internal/cli.buildSinks`. `config.OutputConfig.Defaults()` seeds one
`{type: stdout}` entry, so an absent `output` block is byte-identical to
every earlier release. `validateOutput` fails closed: a present-but-empty
`sinks` list refuses to boot (viper preserves `output.sinks: []` as a
real empty slice rather than dropping the key, which is what makes that
guard reachable from yaml — verified by probe, see UNVERIFIED.md), and a
sink missing its required field (`file` without `path`, `webhook` without
a valid `url`) is also a boot error.

`result.MultiSink` fans each envelope out to every configured sink and
attempts ALL of them, returning the FIRST error — not fail-fast, so a
webhook outage never suppresses the local JSONL record. The error reaches
the queue's `Retry` disposition and the job redelivers; sinks that already
succeeded no-op on the second pass because `result.FileSink` and
`result.WebhookSink` are each idempotent per `JobID` (`internal/result`'s
existing `dedupe` helper, now shared by both). `WebhookSink` claims a
JobID before sending and releases it on failure so a failed POST is
retried on redelivery rather than silently skipped forever; it delegates
retry classification to the existing `moderate.DoJSON` (429/5xx/timeout
retryable, other 4xx terminal, `Retry-After` honored) so there's one copy
of that policy. `FileSink` opens `O_APPEND` at construction — an
unwritable path is a boot error, not a runtime surprise — and a later
sink failing to construct closes the file handles already opened.
`observe.SinkWriteFailuresTotal{type}` counts failures per sink type.

Docs updated to match: `config.example.yaml` documents the block,
`README.md` and `deploy/README.md` / `deploy/compose/README.md` note that
a `file` sink needs one path per replica (same hazard as the audit log),
and `AGENTS.md` records the `MultiSink` fan-out/claim-release gotcha.

Latest: test coverage 81.9% -> 93.8%, concentrated on the wiring and
failure paths. Two production changes were needed to make the biggest gap
testable at all, both structural rather than behavioral:

1. `scan`'s logic moved out of its cobra `RunE` closure into
   `cli.runScan(ctx, out, args, scanOptions) (exitCode, error)`. `RunE`
   now only maps the code to `os.Exit`, which also means runScan's own
   deferred closes run before the process exits (they did not before).
2. `runServe` split into `cli.newServer(cfg) (*server, error)` — all boot
   wiring and validation, releasing whatever it had already opened on a
   partial failure — and `(*server).run(ctx)`, the blocking loop.
   `runServe` is the thin wrapper adding `signal.NotifyContext`.

Worth knowing: `go tool cover -func` does NOT list func literals, so
`scan.go` read as fully covered while missing 52 statements inside its
`RunE`. Per-package percentages are the honest number for any file whose
logic lives in a closure.

Per package: cli 39.8 -> 91.5, ui 69.1 -> 97.1, queue 83.3 -> 96.3,
pipeline 87.2 -> 96.6, audit 84.3 -> 93.6, frames 85.9 -> 94.8, hive
82.1 -> 96.4, microsoft 84.3 -> 93.1, google 63.2 -> 89.5.

The google adapter now has a bufconn-backed fake Vision gRPC service, so
the REQUEST it sends is pinned (exactly `SAFE_SEARCH_DETECTION`, bytes
inline, never an image `source` URI for the vendor to fetch) — goldens
could not check that, since they are built from responses this repo
authored. That promoted `google.golang.org/api`, `google.golang.org/grpc`
and `genproto/googleapis/rpc` from indirect to direct in `go.mod`; no new
modules, same versions. `prometheus/client_golang/prometheus/testutil` was
deliberately NOT used — it pulls in `kylelemons/godebug`.

Latest: the `kind:"url"` media source (`feat/url-source-kind`, PR #40).
New `internal/fetch` downloads an allow-listed `https` asset to a
job-scoped temp file, the pipeline scans it exactly as a local file, and
the download is deleted before ack. It is ON by default: there is no
`enabled` flag, and an empty `source.url.allow_hosts` permits any host,
so an evaluator can POST a public URL against a stock config. Operators
narrow with `allow_hosts`; non-public address space needs the separate
`allow_private_hosts`, which relaxes the address policy (and permits
`http`) for exactly the hostnames it names while keeping the
instance-metadata ranges denied. Two independent checks by
design — a parse-time host allow-list and a per-dial address deny-list
(the only DNS-rebinding defense). A presigned URL's query string is
treated as a credential: `Source.Ref` records scheme+host+path and the
new `Source.RefDigest` carries SHA-256 of the full URL, bumping
SchemaVersion to 1.2.0. Documentation landed with it: `SECURITY.md` now
describes three URL trust classes (its old text, claiming media-source
URLs are disabled, was wrong on this branch), plus a new
`docs/rest-api.md`, the `source:` block in `config.example.yaml`, and
three AGENTS.md gotchas (two `Source` values per url job, the typed-nil
`*Fetcher`, allow-list vs deny-list).

Latest: caller pass-through metadata. An optional, opaque JSON object
now rides a job from intake to result: `queue.Job.Metadata
json.RawMessage`, validated and compacted by `queue.ValidateMetadata`
(object-only, ≤ `queue.MaxMetadataBytes` = 4096 bytes post-compaction,
no depth check) at three independent points — `POST /jobs` (`400` on
invalid), `scan --metadata` (a setup error before any billed vendor
call), and again at `pipeline.ProcessJob` execution time, because a job
can reach Redis without ever passing through HTTP intake. Invalid
metadata at execution is `verdict:"error"` plus dead-letter, never an
allow. Valid metadata is stamped onto `ResultEnvelope.Metadata` at both
envelope construction sites, including the gated empty-video-skip
override, and is omitted (never `null`) when absent. By design it never
reaches the audit log, any log line, the operator UI, or a threshold
decision — it is opaque cargo, not a scoring input. The validator lives
in `internal/queue`, not `internal/cli`, because `internal/cli` imports
`internal/pipeline` and a cli-side validator would have created an
import cycle; noted as a deviation from the design spec.

Latest: a full-repo review pass (2026-08-05) landed four fail-safe
fixes, four hot-path optimizations, and per-replica Redis processing
lists.

Fail-safe: the per-job `dedup_threshold` is now capped at
`frames.dedup.hamming_threshold` — it accepted 0..64, and 64 makes every
frame a near-duplicate of frame 0, so a caller could reduce a whole video
to one benign frame and get `allow` (clamped at intake AND in the
pipeline). Workflow guardrails now validate the template PARSE TREE and
re-check the `-i` count and absolute paths against RENDERED args; the
old text matching saw only bare `{{.X}}`, so `{{printf "-i"}}` plus
`{{printf "/etc/%s" "passwd"}}` passed every check and injected a second
input. The audit log gained a head anchor (`<log>.head`) so TAIL
truncation is detectable — deleting the last N records left a chain that
verified perfectly, contradicting SECURITY.md — and `Open` refuses to
append to a truncated log. `audit.VerifyWith` is the new opt-in seam for
checking signatures (`Verify` stays signature-agnostic by design).

Performance, measured: dHash 68ms -> 23ms per 1080p RGBA frame with
allocations 2,073,629 -> 28 (bounded per-cell sampling plus typed
`lumaSampler` fast paths that are bit-identical to the interface path);
workflow render 14.6us -> 2.4us (compiled-template cache plus a literal
fast path); the microsoft adapter builds its body in one right-sized
buffer instead of holding raw + base64 string + marshal copy; ffmpeg
stderr is bounded by a streaming scanner that extracts pts_time as it
arrives instead of buffering one showinfo line per frame.

Leaks: sink claim maps now expire (1h window) and memq keeps a bounded
window of finished job states; both grew for the life of the process.

Redis: the processing list is per-replica with instance heartbeats and a
reaper. It was one shared key that every Start drained, so a scale-up,
rolling deploy, or crashloop re-ran jobs live replicas were processing —
double-billing the vendor. `vismod_processing_depth` is exported so
parked jobs are visible without polluting the autoscaling signal.

## Gate status

`go build ./...`, `go vet ./...`, `go test ./...` all pass locally as of
2026-08-05. Total coverage 93.0%; coverage of the code changed in the
2026-08-05 review pass is 93.2% (492/528 statements).

CI on PR #40 is green on all four jobs (build & test, lint,
vulnerability scan, docker build & smoke) — the only `-race` evidence
that exists, since that job cannot run on this box. The 2026-08-05
changes have NOT had a `-race` run (see UNVERIFIED.md).

## In flight

PR #40 (`feat/url-source-kind`) is open and has had no human review. No
fetch has ever run against a real remote host (`UNVERIFIED.md`).

## Blocked

Proving the hive adapter end-to-end needs `VISMOD_HIVE_API_TOKEN`; the
docs are currently the only evidence that either direction of the wire
format is right. Proving shieldgemma needs a GPU box — the request shape,
response shape and score derivation are all fixture-only assumptions
(`UNVERIFIED.md`).
