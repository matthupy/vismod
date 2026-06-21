# Design — Cross-replica job dedup (issue #9, M5 §L)

## Problem

`Pipeline.Process` (`internal/pipeline/pipeline.go`) writes the result Sink then
appends the audit chain. Each write is guarded only by an in-memory `seen` map
(`internal/result/jsonl.go`, `internal/audit/audit.go` — both documented
SINGLE-WRITER ONLY).

Under the durable, at-least-once redis/asynq driver (PR #8), at-least-once
delivery REQUIRES idempotent writes. The current guarantee is only partial:

- `audit.Open` replays the chain file and rebuilds `seen`, so a **same-process
  restart over the same file is already safe for audit**.
- The result `Sink` does **not** replay — a fresh process starts with an empty
  `seen`.
- A redelivered job (worker died after the Sink write but before the asynq ack,
  redelivered to a fresh process or a second replica) can therefore **double
  write**: a duplicate result line AND a duplicate audit-chain `seq`, breaking
  the tamper-evident "each job recorded once" property (§K.8).

Cross-process once-only is not yet guaranteed. This closes that gap.

## Fix — `Deduper` pipeline seam

A new optional pipeline seam provides durable cross-process once-only recording.

```go
// Deduper provides durable cross-process once-only job recording. Optional:
// a nil Deduper falls back to the in-memory Sink/audit guards (single-process
// scan/memq path). The redis-backed impl makes dedup survive restart/replica.
type Deduper interface {
    // Done reports whether jobID's result is already durably committed.
    Done(ctx context.Context, jobID string) (bool, error)
    // Commit durably marks jobID recorded, AFTER Sink+audit succeed.
    Commit(ctx context.Context, jobID string) error
}
```

### Gate placement — single gate in `Process` (Option A, approved)

`Pipeline` gains an optional `Dedup Deduper` field. `Process` flow:

1. `if p.Dedup != nil { if done, err := p.Dedup.Done(ctx, jobID); ... ; if done { return nil } }`
2. analyze + `Sink.Write` + `Audit.Append` (unchanged)
3. `if p.Dedup != nil { p.Dedup.Commit(ctx, jobID) }`

One key `vismod:done:<jobid>` gates the Sink+audit pair as a unit. Skipping at
the top of `Process` also skips re-`analyze` on redelivery — no duplicate
classifier call/cost. A nil `Dedup` is exactly today's behavior (scan/memq).

Rejected Option B (redis check inside Sink and audit separately): two keys, no
shared gate, four round-trips, still pays the classifier cost on redelivery.

### Ordering — check → write → commit (write-then-commit)

Fail-safe is the project's #1 principle: never lose a verdict, never auto-allow.

- A crash **before** Commit → redelivery redoes the whole job → never a silent
  loss.
- Residual duplicate window = a crash strictly **between** the Sink+audit writes
  and Commit. Strictly narrower than the status quo, where **every**
  fresh-process redelivery double-writes.

Rejected: claim-lease-then-commit (SETNX lease + Retry-on-contention). asynq
delivers a task to one worker at a time, so concurrent-replica double-PROCESSING
is not the live hazard; sequential redelivery after a death is, and the
write-then-commit gate covers it. The lease only narrows the already-narrow
crash window at the cost of a Retry path and lease-expiry tuning — not worth it
for v1. Documented as a future hardening seam.

### Fail-safe on redis error

`Done`/`Commit` returning an error is an infrastructure failure, not a verdict:
`Process` returns the error → the job is retried / dead-lettered (never
auto-allow, never silently skipped). A redis outage already flips `/readyz` via
the queue `Pinger` (§F.2), so intake stops rather than black-holing.

## Redis impl — `internal/dedup.RedisDeduper`

go-redis/v9 (already in the tree, indirect via asynq — promoted to direct).

- `Commit` = `SET vismod:done:<jobid> 1 NX EX <ttl>` — idempotent; a second
  Commit of the same id is a harmless no-op.
- `Done` = `EXISTS vismod:done:<jobid>`.
- Key prefix `vismod:done:` (namespaced; opaque JobID only — no media, §G.2).
- TTL configurable via `queue.dedup_ttl` (default **168h / 7d**). Bounds redis
  growth; MUST exceed the maximum redelivery window (asynq retention + retry
  backoff budget). Documented in `config.example.yaml`.

miniredis is the test double (already a dev dep).

## Wiring — `internal/cli/wire.go`

When `queue.driver=redis`, build a `RedisDeduper` against `queue.redis.addr` and
set `Pipeline.Dedup`. The memq and one-shot scan paths leave `Dedup` nil
(single-process; in-memory guards suffice).

## Testing (TDD)

- **Red first (the issue's required test):** miniredis-backed `RedisDeduper`;
  drive a JobID through `Process` twice (redelivery after the first Sink write);
  assert exactly **one** result line in the Sink and **one** audit `seq`.
- `RedisDeduper` unit: `Done` false→Commit→`Done` true; Commit idempotent; TTL
  set; redis-down → error surfaced.
- `Process` with a nil `Dedup` keeps today's behavior (regression).
- `Process` with a fake `Deduper` returning `Done=true` short-circuits before
  analyze (assert the Moderator is not called).

## Acceptance

- Re-enqueuing a completed JobID under `driver=redis` does NOT double-write the
  Sink or the audit chain across a fresh process (§K.7).
- Fail-safe preserved: a redis dedup error retries/dead-letters, never allows.
- nil `Dedup` path (scan/memq) unchanged.
- `go test ./...` + golangci-lint v2 green.
