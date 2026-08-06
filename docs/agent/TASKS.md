---
title: Tasks
nav_order: 21
---

# TASKS

Ordered queue. Take the top unblocked entry. One entry = one commit.

Each entry gives: the goal, acceptance criteria that can be checked
without judgement calls, and the files likely touched. An entry that
cannot state its acceptance criteria is not ready to be worked — refine
it first.

Delete an entry when it lands; record the outcome in `STATUS.md`.

---

## 0. Audit the decision before fanning out to sinks

The sink write happens BEFORE `p.Audit.Record` in
`internal/pipeline/pipeline.go`, and a sink error returns `queue.Retry`.
With the webhook sink shipped, a third-party receiver being down now
re-runs the whole job on every retry — frame extraction and a fresh
BILLED vendor call included, up to `queue.max_retries` — and the job
dead-letters with NO audit entry at all, even though stdout and the
`file` sink already hold the envelope. A decision was made and is not in
the audit log: that fails "stay auditable", not just convenience.

The verdict exists before the sink write, and `audit.Open` replays its
file on open, so the audit record is durably idempotent under
redelivery — recording it first is safe.

Acceptance criteria:
- A job whose sink write fails has an audit entry after the failure.
- Redelivery of that job does not append a second audit record
  (existing per-JobID idempotency covers it — assert it in a test).
- `vismod audit verify` passes over a log built through a sink outage.
- The dead-letter path and its `DeadLetterEntry.Reason` are unchanged.
- Existing pipeline and audit tests pass unmodified.

Files likely touched: `internal/pipeline/pipeline.go`,
`internal/pipeline/pipeline_test.go`, `AGENTS.md` (the sink gotcha
paragraph), `config.example.yaml` (the webhook budget warning).

---

## 1. `vismod audit verify` must say when there is no head anchor

`checkAnchor` returns `nil` when `<log>.head` is absent, so a log with no
anchor verifies with the same `audit chain OK: N records verified` line
and the same exit 0 as a fully anchored one. The two states are not
equivalent: without the anchor, tail truncation is undetectable, which is
the exact gap the anchor was added to close.

This matters most where it is least visible — a restore that dropped the
sidecar, or a backup tool that never captured it. The operator has no
signal; `docs/audit-log.md` currently has to tell them to check for the
file by hand, which is documentation standing in for a missing feature.

The tolerant read path is correct and must stay: pre-anchor logs have to
remain verifiable. The gap is only that the CLI does not report which of
the two it did.

Acceptance criteria:
- `vismod audit verify` over a log with no `<log>.head` still exits 0 but
  its output states that no head anchor was present and that tail
  truncation was therefore not checked.
- `vismod audit verify` over an anchored log states that the anchor was
  checked, and names the anchored seq.
- `audit.VerifyWith` reports the same distinction to callers without
  turning a missing anchor into an error.
- An explicitly-passed `VerifyOptions.Anchor` is reported as checked even
  when no sidecar exists on disk.
- Existing audit tests pass unmodified.

Files likely touched: `internal/audit/audit.go`,
`internal/audit/audit_test.go`, `internal/cli/audit.go`,
`docs/audit-log.md` (the three-state table's observation column).
