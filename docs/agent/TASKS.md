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

## 1. Audit the decision before fanning out to sinks

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
