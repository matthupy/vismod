---
title: Result envelope
nav_order: 4
---

# The result envelope

One JSON object per job, emitted to every configured sink.

```json
{"job_id":"scan-...","source":{"kind":"file","ref":"/data/clip.mp4","media_type":"video"},
 "model_id":{"adapter":"microsoft","model_version":"2024-09-01","config_hash":"9b6f…"},
 "result":{"schema_version":"1.2.0","provider":"microsoft","media_type":"video",
   "asset_id":"/data/clip.mp4",
   "frames":[{"timestamp_sec":2.0,"status":"ok","categories":[
     {"category":"SEXUAL","provider_label":"Sexual","score":0.333,
      "score_origin":"severity","threshold":0.4,"flagged":false}]}],
   "overall":{"verdict":"allow","flagged":false,"top_category":"SEXUAL",
     "max_score":0.333,"confidence":0.333}},
 "started_at":"…","finished_at":"…"}
```

A job submitted as `kind:"url"` ([REST intake](rest-api.md)) carries a
`source` of this shape instead — note the truncated `ref`:

```json
"source":{"kind":"url","ref":"https://media.example.com/clip.mp4",
          "ref_digest":"7b1f…","media_type":"video"}
```

## Fields that matter downstream

- **`model_id`** is the decision's provenance: which adapter, which model
  version, and `config_hash` — a SHA-256 over the verdict-affecting
  config (adapter name + model version + resolved per-category
  thresholds). Secrets, log level, and addresses are excluded. Two
  envelopes with the same `config_hash` were produced by the same
  decision function; a changed hash means thresholds moved and results
  are not comparable across the boundary.
- **`score`** is `null`, never `0`, when a category could not be
  evaluated. Consumers that coerce `null → 0` will read
  "could-not-evaluate" as "confidently safe."
- **`score_origin`** tells you what the number was upstream. Do not
  aggregate or average scores across different origins.
- **`provider_label`** is the vendor's own string, preserved even when
  the canonical `category` is `OTHER`.
- **`overall.verdict`** is one of `allow`, `flag`, `block`, `error`,
  rolled up with precedence `block > error > flag > allow`.
- **`source.ref_digest`** appears on `kind:"url"` sources only (omitted
  for files) and is SHA-256 of the **full** submitted URL. `source.ref`
  for a url is deliberately truncated to scheme+host+path, because a
  presigned URL's query string is a credential and `ref` reaches
  envelopes, audit records, and logs. Correlate on `ref_digest` when two
  jobs differ only in their query string — `ref` alone cannot tell them
  apart. Verifying a digest requires the original URL; vismod does not
  store it.
- **`metadata`** is whatever you attached to the job (`POST /jobs` or
  `scan --metadata`), echoed back verbatim and compacted. It is **absent**
  when you supplied none — not `null` — so envelopes from callers who do
  not use it are byte-identical to before. vismod never interprets it, so
  it can never affect a verdict, and it is deliberately **not** written to
  the audit log: the hash chain stays free of caller free text. It is also
  never logged and never shown in the operator UI. Do not put secrets in
  it — it reaches every configured sink, including your webhook receiver.

`result.schema_version` is **`1.2.0`** as of the `ref_digest` addition
(`1.1.0` added four categories). Both bumps are additive: no field was
removed, renamed, or given a new meaning, so a `1.1.0` consumer keeps
working. Note that `source` is serialized by the envelope rather than by
`NormalizedResult`, and the envelope carries no version of its own —
`result.schema_version` is the only version signal for a `source` change.
`metadata` rides the **envelope**, not `NormalizedResult`, so it does not
move `result.schema_version`; like `source`, it has no version signal of
its own, and it is additive — a consumer that ignores it keeps working.

## Sinks

`output.sinks` ([config.example.yaml](https://github.com/matthupy/vismod/blob/main/config.example.yaml))
fans each envelope out to any combination of:

| Type | Destination |
|---|---|
| `stdout` | One JSONL line to standard output (the default when `output` is omitted) |
| `file` | Append-only JSONL file |
| `webhook` | HTTP POST to a receiver |

Every sink is attempted, and the **first failure** is what triggers
redelivery — a webhook outage never suppresses the local record. Omit the
`output` block entirely for the stdout-only behavior every earlier
release used.

A present-but-empty `output.sinks: []` is rejected at boot rather than
silently emitting nowhere.

## Idempotency, and where it stops

Every sink is idempotent per `job_id` **within a process lifetime**, so a
redelivery to a running worker never double-writes.

That guarantee does **not** survive a restart. The dedupe set is in
memory, so a job redelivered after a crash or a rolling restart gets a
second line in the `file` sink and a second POST to a `webhook` receiver.

The audit log is the exception: `audit.Open` replays its file and
rebuilds the seen-set before appending, so its per-`job_id` guarantee
holds across restarts.

**Downstream consumers of the `file` and `webhook` sinks must dedupe on
`job_id` themselves.**

A `file` sink needs one path per replica — see
[deploy/README.md](https://github.com/matthupy/vismod/blob/main/deploy/README.md).

## Exit codes (`scan`)

| Code | Meaning |
|---|---|
| `0` | `allow` |
| `1` | `flag` or `block` |
| `2` | `error` |
