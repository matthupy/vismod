# Caller pass-through metadata — design

**Date:** 2026-08-03
**Status:** approved, not yet implemented

## Goal

Let a caller attach their own opaque JSON to a job and have it come back
untouched on the result envelope — so a webhook receiver can correlate a
verdict with the caller's own record (ticket ID, tenant, upload row)
without maintaining a side table keyed on `job_id`.

vismod never interprets, indexes, validates the *contents of*, or acts on
this data. It cannot influence a verdict.

Non-goal: a queryable or filterable field. Metadata is carried, not used.

## The invariant this touches

`AGENTS.md`'s done gate reads:

> No secret, media byte, provider `Raw`, or free text added to any
> envelope, log, audit record, queue payload, or UI surface.

Caller metadata *is* free text in the queue payload and the envelope.
This design is an explicit, bounded carve-out, not a silent widening:

| Surface | Metadata allowed? |
|---|---|
| `queue.Job` payload (incl. Redis, DLQ entry) | **yes** |
| `result.ResultEnvelope` → all sinks (stdout, file, webhook) | **yes** |
| Audit log (`internal/audit`) | **no** |
| Logs (`internal/observe`, slog) | **no** |
| Operator UI (`internal/ui`) | **no** |

The audit chain stays free of caller text: an audit record for a job with
metadata must hash identically to one without it.

`AGENTS.md` must be amended in the same commit to state this carve-out.
Without that, the next agent reads the gate as written and reverts the
feature.

## Shape

Opaque JSON object, not a typed map. The operator runs vismod on their
own infrastructure and supplies their own metadata; a typed
`map[string]string` would only invite vismod to grow opinions about
content it has no business reading.

`queue.Job` gains:

```go
// Metadata is opaque caller-supplied JSON, passed through untouched to
// the result envelope. vismod never interprets, indexes, or acts on it.
// Never logged, never rendered in the UI, never recorded in the audit
// log — the audit chain stays free of caller free text.
Metadata json.RawMessage `json:"metadata,omitempty"`
```

`result.ResultEnvelope` gains the identical field and comment.

`omitempty`, not always-emitted-null. The repo's null-discipline
invariant exists because `score: null` means *could-not-evaluate* and
omission would hide a real signal. Metadata has no such semantics —
absent means the caller sent none. Envelopes without metadata must
serialize byte-identical to today.

## Validation

One shared validator, `queue.ValidateMetadata(json.RawMessage)
(json.RawMessage, error)`, in `internal/queue` beside the `Job` type it
guards. It cannot live in `internal/cli` as first sketched: `internal/cli`
imports `internal/pipeline`, so the pipeline could not import it back for
execution-time validation. It returns the **compacted** bytes so no call
site can forget to compact.

Rules:

- Must be a JSON **object** (`{…}`). Array, scalar, and `null` rejected —
  keeps the envelope field shape stable for consumers and keeps
  `.metadata.foo` addressable.
- Compacted with `json.Compact` on accept; the cap is measured after
  compaction, so the limit bounds content rather than indentation.
- **≤ 4 KiB** compacted. This is the real bound: metadata rides every
  Redis payload and every webhook POST. The intake's 1 MiB body limit is
  far too loose for a field with that reach.
- No depth limit of our own. `encoding/json`'s scanner already errors
  past `maxNestingDepth = 10000`.

Called from three places, per the "validate at intake AND at execution"
rule — a job can reach Redis without passing through `POST /jobs`:

1. **Intake** (`POST /jobs`) → `400 bad request: <msg>`
2. **`vismod scan --metadata '<json>'`** → setup error, non-zero exit
   before any scanning happens
3. **Execution** (`pipeline.ProcessJob`) → invalid metadata →
   `verdict:"error"` + dead-letter. Fail safe: never allow.

## Flow

```
POST /jobs (intakeRequest.Metadata)  ─┐
vismod scan --metadata '<json>'      ─┤→ validate → queue.Job.Metadata
redis enqueue (bypasses intake)      ─┘
                                        ↓
                          pipeline.ProcessJob (re-validate)
                                        ↓
                        ResultEnvelope.Metadata = j.Metadata
                                        ↓
                   MultiSink → stdout / file JSONL / webhook
                                        ↓
                   audit.Record — ignores Metadata entirely
```

Two envelope construction sites in `ProcessJob` must both stamp it:

- `internal/pipeline/pipeline.go:172` — the normal path
- `internal/pipeline/pipeline.go:153` — the empty-video-skip gated
  override, which returns an envelope with no verdict. Missing this one
  silently drops the caller's correlation ID on exactly the jobs a
  caller most needs to reconcile.

`queue.DeadLetterEntry` embeds the whole `Job`, so a dead-lettered job
keeps its metadata with no extra work. That is the desired behavior.

No pipeline logic, rollup, threshold, or `ConfigHash` input changes.
Metadata cannot reach a verdict.

## Decided edge cases

- **Idempotency** stays keyed on `JobID` alone. Metadata is not part of
  any dedupe key.
- **No redaction, no secret scanning.** It is the operator's own text.
  Documented as: vismod does not inspect this — do not put secrets in it.
- **No config surface.** Metadata is per-job. Nothing in
  `config.example.yaml` changes.
- **UI unchanged.** No new column; `internal/ui` never receives it.

## Tests

- `internal/cli` — `validateMetadata` table: valid object; array,
  scalar, `null`, malformed JSON all rejected; exactly 4 KiB accepted;
  4 KiB + 1 rejected; a whitespace-padded object that is oversize raw but
  in-bounds compacted is **accepted** (proves the cap measures content).
- `internal/cli/serve_test.go` — `POST /jobs` with metadata returns
  `202` and the enqueued job carries the compacted bytes; oversize
  metadata returns `400`.
- `internal/pipeline` — metadata reaches the envelope on the normal path
  **and** on the empty-video-skip override path; invalid metadata at
  execution yields `verdict:"error"` + dead-letter.
- `internal/audit` — an envelope with metadata produces an audit record
  that does not contain it, with a chain hash identical to the same
  envelope without it. This is the invariant guard.
- Log assertion — metadata never appears in any log line for a job that
  carries it.
- `internal/result` — an envelope without metadata serializes
  byte-identical to today (`omitempty` regression guard).

## Docs to update in the same commit

- `docs/rest-api.md` — `metadata` row in the request-body table; new
  `400` cause.
- `docs/result-envelope.md` — the field; explicit note that it is absent
  when not supplied and never present in the audit log.
- `AGENTS.md` — amend the done-gate free-text line with the carve-out
  table above.
- `README.md`, `config.example.yaml` — no change; no config surface.
