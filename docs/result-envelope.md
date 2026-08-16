---
title: Result envelope
nav_order: 7
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

- **`result.raw` is never emitted.** The `NormalizedResult` type carries
  an optional `raw` field for the provider's sanitized response, but it is
  the adapter's handoff to the pipeline and stops there — no sink envelope
  has ever contained it, and none will. What the raw response is used for
  is the audit log's `raw_sha256`, which binds a verdict to the response
  that produced it by hash; see [the audit log](audit-log.md). The digest
  is not in the envelope either: the audit log is where evidence binding
  lives, and adding it here would put a field on every sink and webhook
  receiver that nothing downstream asked for.

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

### Format: what the bytes look like

`type` is the transport; `format` is the rendering. They are orthogonal,
so a chat integration is a format rather than a new sink type.

| Format | Body |
|---|---|
| `json` (default) | The result envelope documented above, unchanged |
| `discord` | A Discord webhook embed for a human-watched channel |

Omitting `format` keeps the exact bytes every existing receiver is written
against.

**A non-JSON renderer is a narrowing boundary.** It emits a named list of
fields, so caller `metadata` — which may carry PII — and a url source's
`ref_digest` never cross into a third-party chat service. A null
`max_score` renders as `unknown`, never as a number, for the same reason
it serializes as `null`: `0.00` reads as "confidently safe".

Do not point a `json` sink at a chat webhook to get around that narrowing.
The full envelope is the contract for a receiver you control, not for a
third-party service.

#### Passing metadata through to a notification

Your `metadata` is meant to survive the whole pipeline, and a correlation
id that reaches the JSON envelope but not the alert a human reads has not
survived it. `metadata_fields` names the keys a non-JSON sink may publish:

```yaml
- type: webhook
  format: discord
  metadata_fields: [request_id, tenant]   # or ["*"] for every key
```

Named keys render in the order you list them, ahead of the source ref and
error string so a long ref can never truncate your id away. `["*"]`
renders every key, sorted, so two renders of one envelope are identical.
Omitting the key publishes nothing, which is what every sink did before.

- **Absent keys render nothing** — naming a key the caller didn't send is
  not an error, so one sink config works across callers that attach
  different fields.
- **Scalars render bare** (`abc-123`, `42`, `true`); objects and arrays
  render as compact JSON.
- **Embeds cap at 25 fields.** A `["*"]` over a large metadata object is
  truncated rather than rejected — the verdict fields always survive.
- **`metadata_fields` on `format: json` is a boot error**, not a no-op:
  that envelope already carries all metadata.

This is an allow-list on purpose. The person who *writes* metadata is
usually not the person who *configures the sink*, so naming a key is a
disclosure decision on behalf of every future caller. vismod validates key
names at boot but never values — values arrive per job. The guarantee is
"only the fields you named," never "only safe content."

### Predicate: which results a sink receives

A sink may carry a `predicate` that narrows what reaches it. It is a
closed set of conditions, never an expression language, so a rule that can
never fire is a boot refusal rather than a silent drop.

| Group | Meaning |
|---|---|
| `verdicts` | Any of `allow`, `flag`, `block`, `error` |
| `categories` | Canonical category → minimum score; matches when **any** frame carries a non-null score at or above it |
| `source_kinds` | Any of `file`, `url` |

OR within a group, AND across groups; an omitted group is unconstrained.
A **null score never satisfies a minimum**, including a minimum of `0` —
could-not-evaluate is not a value. An envelope with no result at all
(an errored job) routes as verdict `error`.

A predicate miss is a routing decision, not a delivery failure: it does
not trigger redelivery, so a sink declining a result never re-runs a
billed vendor call.

### The catch-all rule

A predicate can only ever narrow delivery, so if **every** sink carries
one, an envelope matching none of them is emitted nowhere. No check on an
individual predicate detects that — a rule that is syntactically perfect
and simply never matches passes every static check — so the guarantee
comes from the shape of the config: **at least one sink must have no
predicate**, or vismod refuses to boot.

`output.allow_unrouted: true` is the gated override, and it accepts that
some results reach no destination at all.

### When delivery fails

A `webhook` sink retries transient failures — `429`, any `5xx`, timeouts,
and network errors — with exponential backoff, honoring `Retry-After` up
to 120s. `max_attempts` and `timeout` are per sink. Every other `4xx` is
**terminal and never retried**: a rejected payload does not become
acceptable by being sent again.

Each backoff emits one `WARN` record naming the sink, the attempt, and how
long it is about to sleep, so a stalled worker always says why. A give-up
emits exactly one `ERROR` record with a bounded `reason`.

#### Reading the log fields

```json
{"level":"WARN","msg":"retrying after transient failure","target":"webhook[1]",
 "attempt":1,"max_attempts":3,"delay_ms":500,"status":503,"reason":"status"}
{"level":"ERROR","msg":"result sink delivered nothing","sink":"webhook[1]",
 "job_id":"scan-…","reason":"exhausted","status":0,"max_attempts":3}
```

| Field | On | Meaning |
|---|---|---|
| **`target`** | `WARN` | **Which configured thing was being called — a label, never an address.** |
| **`sink`** | `ERROR` | The same label, under the name this layer uses. |
| `attempt` / `max_attempts` | both | Position in the retry budget |
| `delay_ms` | `WARN` | How long this backoff will sleep |
| `status` | both | HTTP status; `0` when there was no response at all |
| `reason` | both | Bounded — see the table below |
| `job_id` | `ERROR` | The job whose envelope was lost |

**`target` is the field most likely to be misread.** The name suggests a
destination URL. It is not one, and it never will be: a Discord webhook
URL carries its token in the path, so logging it would publish a
credential on every `429` — exactly when a throttled destination produces
the most log volume. `url.Redacted()` does not help either; it strips
userinfo and leaves the path intact.

What `target` actually holds is **the sink's position in your
`output.sinks` list** — `webhook[0]` is the first sink, `webhook[1]` the
second. That is the handle you map back to your config. Adapter calls use
the same field with a label like `adapter:hive`.

`target` and `sink` are two names for one value. They differ because two
layers emit them: the retry layer is shared with adapter calls and speaks
of a generic `target`, while the delivery layer speaks of a `sink`, matching
`output.sinks` and the `vismod_sink_*` metrics. **Join a stall to its
outcome by matching `target` on the warnings against `sink` on the error.**

Reasons:

| `reason` | Meaning | What to do |
|---|---|---|
| `exhausted` | The retry budget ran out | The destination is down or throttling |
| `rejected` | The receiver refused the payload | Retrying will not help; fix the payload or the receiver |
| `format` | The envelope could not be rendered | A vismod bug — file it |
| `write` | A local write failed (`file` sink) | Disk, permissions, or a full volume |

Two Prometheus series cover this: `vismod_sink_retries_total` counts
backoffs, `vismod_sink_write_failures_total` counts give-ups. Rising
retries with flat failures is a destination that is throttling but still
delivering — the signal to lower your send rate before it becomes an
outage.

A failed delivery still returns an error to the pipeline, which is what
triggers redelivery of the whole job. Budget `timeout × max_attempts`
accordingly: attempts are serial, so one unreachable receiver can hold a
worker for the product of the two.

**A webhook outage is not local to that sink.** The whole job re-runs on
each retry — frame extraction and a fresh, *billed* vendor call included —
so with `queue.max_retries: 3` a down receiver costs up to 4× vendor quota
per job. And because the audit record is written *after* the sinks, a job
that dead-letters this way leaves **no audit entry at all**, even though
`stdout` and the `file` sink already hold the envelope. An unreliable
receiver is better fed by tailing the `file` sink than by putting it in
`output.sinks`.

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
