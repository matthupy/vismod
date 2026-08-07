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

---

## 2. `top_category` is decided by adapter emission order when scores tie

`Rollup` (`internal/pipeline/rollup.go:55-61`) keeps the first strictly
greater score, so among equal scores the winner is whichever category the
adapter happened to emit first. The tie that matters most is the common
one: a fully benign image where every category scores `0`.

Confirmed against a **live Azure Content Safety run** on 2026-08-07. A
benign image produced `max_score: 0` and `top_category: "HATE"` — an
operator-visible field naming a category the content had nothing to do
with, on the most frequent verdict vismod emits. It reads as a weak
signal about the content when it is in fact an artifact of map order.

`STATUS.md` already records this as one of two rollup defects blocking a
label-only adapter (Llama Guard 4, whose 1.0 labels would tie constantly).
This entry covers the tie-break alone; `Confidence` being a copy of
`MaxScore` is the other and is not in scope here.

The fix is a decision, not just code: `null` when the top score is tied
(nothing outranks anything), `null` when `max_score` is `0` (nothing was
detected), or a deterministic canonical ordering. Prefer whichever a
reader of the envelope can be told in one sentence. Note that
`top_category` is nullable already, so `null` costs no schema change.

Acceptance criteria:
- A result whose highest score is shared by two or more categories
  produces the same `top_category` regardless of the order the adapter
  emitted them in. A test that shuffles emission order asserts it.
- An all-zero-score benign result does not name an arbitrary category.
- Verdict, `flagged`, `max_score`, and `confidence` are unchanged by this
  — only `top_category` moves.
- Existing rollup tests pass UNMODIFIED (per the done gate: weakening
  them to pass is a failed gate).
- `docs/result-envelope.md` states the tie-break rule.

Files likely touched: `internal/pipeline/rollup.go`,
`internal/pipeline/rollup_test.go`, `docs/result-envelope.md`,
`docs/agent/STATUS.md` (the label-only-adapter blocker note).
