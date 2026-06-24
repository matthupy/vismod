# Design — M5 §L one-model-cluster-wide invariant (dead-letter on model-fingerprint mismatch)

**Date:** 2026-06-21
**Milestone:** M5 §L (multi-replica hardening)
**Status:** approved-for-planning
**Related:** §D.5 (`ModelIdentity`), §F.6 (metrics), §L(b)/(c) (one-model-cluster-wide invariant + deploy strategy), prior dedup gate (PR #11/#12, `docs/superpowers/specs/2026-06-21-cross-replica-dedup-design.md`)

## Problem

`serve` runs single-logical-queue `"vismod"` (`internal/cli/wire.go:redisQueueName`). Under the M5 redis
driver a multi-replica deploy can run two model/config versions against the *same* queue during a rolling
deploy: replica A (model X) enqueues a job; replica B (model Y) dequeues it and **silently moderates with
the wrong model**. §L(b) requires workers to **dead-letter (not silently process)** any job whose required
model identity ≠ the worker's loaded model. No such guard exists today.

## Goal / non-goals

- **Goal:** a worker processes a job ONLY if the job's stamped model identity matches the worker's loaded
  model. Mismatch ⇒ dead-letter, never silently process (fail-safe).
- **Goal:** ship the hard safety net (dead-letter) + **document** per-model-version queue namespacing as the
  recommended deploy strategy.
- **Non-goal:** implement queue namespacing (documented only — operator deploy choice).
- **Non-goal:** authentication. The fingerprint is a *misconfiguration / rollout-skew* guard, not an
  anti-adversary control — a malicious enqueuer can stamp any value (honest-scoping, mirrors §G.5 audit-log
  threat scope).
- **Non-goal:** redis-backed shared rate limiter (separate §L workstream).

## Components

### 1. `Config.ModelFingerprint() string` (`internal/config`)

Boot-knowable identity of the loaded model. SHA-256 over the **canonicalized** verdict/deploy-affecting
config:

- adapter name (`adapter.name`)
- **canonical(`adapter.options`)** — the deploy surface; `api_version` / model-id / endpoint live here, so a
  model change ⇒ different fingerprint
- resolved per-category threshold map + defaults + `sexual_potential_csam` (same fields as `ConfigHash`)

**🔴 Canonicalization is the #1 correctness landmine.** `adapter.options` is `map[string]any` from viper;
naive `fmt` formatting over a Go map yields **random key order** ⇒ a non-deterministic hash ⇒ replicas with
identical config compute *different* fingerprints ⇒ everything dead-letters.

**Resolution:** `encoding/json.Marshal` sorts map keys lexicographically **recursively at every level**
(Go stdlib guarantee), so `json.Marshal(adapter.options)` is already a deterministic canonical encoding —
nested `map`/`[]any` included. No custom encoder, no dependency on audit's (unexported, `Payload`-typed)
`jcs`. Caveat: viper yields consistent Go scalar types (int/float64/string/bool) from the same config source
across replicas, so marshalling is stable. A **nested-options fixture + map-insertion-order-permutation
test** is required to lock this guarantee against future refactors.

**Distinct from `ConfigHash(modelVersion)`** — do NOT merge:
- `ConfigHash`: per-job audit provenance; folds in the adapter's *runtime-reported* `ModelVersion`; excludes
  `adapter.options`.
- `ModelFingerprint`: boot-time deploy guard; folds in `adapter.options`; no runtime model version.

### 2. Stamp on enqueue — single helper

Add `ModelFingerprint string` to `queue.Job` and `queue.jobPayload` (asynqq). It is an opaque hash ⇒
payload-hygiene safe (§D.3/§G.2: no media/PII), fine in Redis + asynqmon.

**All enqueues go through one stamping helper** (e.g. `enqueueJob(ctx, q, src, fp)` in `internal/cli`) — no
per-call-site stamping. This guarantees: in steady state every job is stamped, so an **empty** fingerprint
can only mean a pre-feature (older-binary) job. Current call sites: `enqueueFromStdin` (serve). `scan` is
one-shot single-process and does not go through the queue — out of scope.

memq path: enqueuer == worker == same process == same config ⇒ fingerprints always match ⇒ guard is a no-op,
zero behavior change for `driver=memory`.

### 3. Dead-letter on mismatch (`jobHandler`, `internal/cli/serve.go`)

Worker fingerprint computed **once** in `runServe` (`workerFP := cfg.ModelFingerprint()`), captured in the
handler closure. Per job:

| `job.ModelFingerprint` | action |
|---|---|
| == `workerFP` | normal: `pipeline.Process` |
| non-empty, **≠** `workerFP` | `queue.DeadLetter`, never call `Process`; WARN log; `RecordModelMismatch("mismatch")`. DLQ envelope `Error` = `"model fingerprint mismatch: job=<jobfp-prefix> worker=<workerfp-prefix>"` ⇒ auditable via existing DLQ sink |
| empty | process normally (skip guard); WARN log; `RecordModelMismatch("unstamped")` — visible, never silent |

DeadLetter (not Retry): mismatch is deterministic; retrying loops on the same wrong replica. Matches the
existing poison-message → DeadLetter pattern.

### 4. Metric (`internal/observe`)

`vismod_jobs_model_mismatch_total{reason}` counter, `reason ∈ {mismatch, unstamped}`. New method
`Metrics.RecordModelMismatch(reason string)`. `mismatch` = wrong model deployed; `unstamped` = old binary
in flight. Distinguishing the two is the ops signal.

### 4b. Job lifecycle metrics — `vismod_jobs_*` family

Independent observability add (separate commit; bundled in this PR). Today `vismod_queue_depth` reports only
the **backlog** (buffered, not-yet-started), and `vismod_jobs_total{verdict}` reports only **moderation
outcome** (allow/flag/block/error) for jobs that produced a verdict. Three signals are missing:

- **in-flight jobs** — pulled by a worker, not yet acked/dead-lettered. A stuck/slow worker (or, under
  at-least-once redis, a crashed worker holding an unacked job) is invisible: backlog can read 0 while jobs
  are wedged in processing.
- **queue-level processing outcome** — a dead-lettered job (infra failure: retry-exhausted, terminal,
  panic, model mismatch) produces **no verdict envelope at all**, so failures are invisible in
  `jobs_total`. And there is no driver-uniform success counter.

New `vismod_jobs_*` family (note: *active == in-flight == unacked* are one state — one gauge, not three):

| metric | type | meaning |
|---|---|---|
| `vismod_jobs_active` | gauge | jobs pulled by a worker, not yet acked/dead-lettered (asynq `Active`; memq processing) |
| `vismod_jobs_completed_total` | counter | jobs acked (successfully processed). ≈ `sum(vismod_jobs_total)` but at the queue layer, driver-uniform |
| `vismod_jobs_failed_total` | counter | jobs dead-lettered (retry-exhausted / terminal / panic / mismatch) — never carries a verdict |

**Why our own counters, not asynq's stats:** asynq's `info.Processed`/`info.Failed` are daily-resetting and
absent on memq. Emitting our own at the driver's terminal disposition gives proper monotonic Prometheus
counters with identical semantics on both drivers.

**Active gauge (scrape-time, live read):**
- `DepthReporter` gains `ActiveDepth() int`.
- **memq:** an `inflight atomic.Int64`, incremented at the top of `process`, decremented (defer) on terminal
  disposition. `ActiveDepth()` returns its load. (memq is at-most-once so active == actively-processing,
  including retry-backoff sleeps — correct, the job is still held.)
- **asynq:** `ActiveDepth()` returns Inspector `GetQueueInfo(qname).Active`. Missing queue ⇒ 0.
- **metrics:** `RegisterQueueDepth` gains a third arg `active func() float64`; registers gauge
  `vismod_jobs_active`. Local accessor (no error path).
- **serve:** wire `func() float64 { return float64(q.ActiveDepth()) }`.

**Completed / failed counters (emitted by the driver at terminal disposition):**
- New nil-safe interface in `queue`: `type Recorder interface { RecordJobCompleted(); RecordJobFailed() }`.
  `observe.Metrics` implements it (registers the two counters). `QueueConfig` gains a `Metrics Recorder`
  field (optional; nil ⇒ no-op, so tests and CLI without metrics are unaffected). This keeps the `queue`
  package decoupled from prometheus.
- **memq.process:** `Ack` ⇒ `RecordJobCompleted()`; every `deadLetter(...)` path ⇒ `RecordJobFailed()`.
- **asynq:** processor returns nil (Ack) ⇒ `RecordJobCompleted()`; `onError` on the **final** failure (the
  same branch that writes the DLQ envelope) ⇒ `RecordJobFailed()`. Intermediate retry attempts are NOT
  counted (they are attempts, not terminal failures).
- **serve:** set `qc.Metrics = metrics` when building the queue.

Tests: memq `ActiveDepth` rises during a blocked handler, returns to 0 after ack/dead-letter; completed
counter increments on ack, failed on dead-letter (retry-exhaust + panic + mismatch paths); nil Recorder is a
safe no-op; gauges registered + scrape.

### 5. Docs

- README + `MODEL_AND_HASH_LIMITATIONS.md` (or a §L ops note): the invariant + the deploy guidance.
- **🔴 Correct deploy = drain-first or namespacing — NOT "manually requeue onto the shared queue".** A naive
  requeue of archived mismatches onto the shared `"vismod"` queue re-hits the wrong replica and
  **re-archives in a loop.** Document the two safe paths:
  1. **Drain-first rolling deploy:** stop intake, let the old queue empty (existing §D.3 graceful drain),
     then cut over replicas. Simplest for single-queue.
  2. **Per-model-version queue namespacing** (`vismod:<fp-prefix>`): each replica consumes only its model's
     queue ⇒ mismatch structurally impossible; old jobs drain on old-model replicas. Recommended for
     zero-downtime. Architected-for via `redisQueueName`; not implemented this PR.
- Honest-scope line: fingerprint guards misconfiguration / rollout skew, not adversarial enqueue.

## Data flow

```
enqueue:  src + workerFP --> enqueueJob() --> Job{ModelFingerprint: fp} --> queue (memq | asynq payload)
dequeue:  Job --> jobHandler:
            empty      -> WARN + count(unstamped) -> Process
            == workerFP-> Process
            != workerFP-> WARN + count(mismatch)  -> DeadLetter (DLQ envelope w/ reason); NO Process
```

## Error handling / fail-safe

- Mismatch never reaches `pipeline.Process` ⇒ no wrong-model verdict ever emitted (§F.5 fail-safe
  direction).
- Dead-lettered mismatch lands in the DLQ sink + asynq archive with a descriptive `Error` — could-not-process,
  never `allow`.
- Empty (pre-feature) job is processed but counted + logged ⇒ the only "process despite unknown identity"
  path is bounded to the introducing rollout and is observable.

## Testing

- `ModelFingerprint`: stability (same config ⇒ same hash across repeated calls / map-insertion-order
  permutations), sensitivity (adapter name change, option change incl. `api_version`, threshold change ⇒
  different hash), **nested-options fixture** (map within options) proving deterministic recursion.
- `jobHandler`: match ⇒ Process called; mismatch ⇒ DeadLetter + Process NOT called; empty ⇒ Process called.
  Use a fake/stub pipeline-process seam + assert via the disposition + a call flag.
- metric increments per branch with correct `reason`.
- enqueue helper stamps the worker fingerprint onto the Job.
- (memq) end-to-end: same-config enqueue+process never dead-letters.

## Acceptance (maps to §K)

- Multi-replica wrong-model job is dead-lettered, never silently processed; `vismod_jobs_model_mismatch_total`
  increments.
- Same-config (incl. memq) path is unaffected — no spurious dead-letters; fingerprint deterministic across
  replicas regardless of option map order.
- Docs state the drain-first / namespacing deploy strategy and the honest scope.
