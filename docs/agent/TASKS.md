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

## 0. Document the url source kind (code has landed, docs have not)

`kind:"url"` intake, `internal/fetch`, `source.url` config, fetch
metrics and `Source.RefDigest` all shipped on `feat/url-source-kind`.
No prose was written for any of it, and one shipped doc is now WRONG:
`SECURITY.md` still states that media-source URLs are disabled as an
SSRF vector. Fix that first.

Acceptance criteria:
- `SECURITY.md` describes the THREE URL trust classes separately and
  does not apply the media-source deny-list rule to provider endpoints
  or webhook sinks: (1) media source urls — job-supplied, untrusted,
  https-only, exact host allow-list, private/link-local/CGNAT denied at
  dial, no redirects, size cap on bytes read, transient download,
  presigned query string never recorded; (2) provider endpoint urls —
  operator config, private ranges expected; (3) webhook sink urls —
  operator config, private ranges expected.
- `config.example.yaml` carries the `source:` block (off by default,
  `allow_hosts: []`, `max_bytes`, `timeout`, `max_attempts`,
  `allowed_media_types`) with the security note.
- `README.md`'s `POST /jobs` section documents `kind:"url"`, that it is
  off by default, and that `ref` must be an allow-listed `https` URL.
- Scanning-from-URL instructions exist for the REST API with worked
  `curl` (macOS/Linux) and PowerShell (`Invoke-RestMethod`, Windows)
  commands, covering an image job, a video job with `workflows`, and
  reading back the result envelope. Note that one process runs ONE
  vendor (`adapter.name`): scanning the same URL through several
  vendors means several instances on separate `intake_addr` ports.
- `CLAUDE.md` "Shape of the code" and `AGENTS.md` "Architecture map"
  both list `internal/fetch/`.
- `AGENTS.md` gains three gotchas: the two `Source` values per url job
  (`resolved{local, env}`); `fetch.New` returning a typed-nil `*Fetcher`
  that `cli.newFetcher` must convert to an untyped nil; and the host
  allow-list (parse-time) versus the address deny-list (per-dial, the
  only DNS-rebinding defense) being separate checks.
- `docs/result-envelope.md` documents `ref_digest` and SchemaVersion
  1.2.0.

Files: `SECURITY.md`, `config.example.yaml`, `README.md`, `CLAUDE.md`,
`AGENTS.md`, `docs/result-envelope.md`, `docs/index.md`.

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
