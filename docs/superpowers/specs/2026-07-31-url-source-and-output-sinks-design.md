# URL source kind and configurable output sinks — design

**Date:** 2026-07-31
**Status:** approved, not yet implemented

## Why

Two capabilities are missing before vismod can be driven by an external
test harness — or by any deployment that does not own the filesystem the
worker runs on:

1. **Jobs can only name local files.** `Source.Kind` is `"file"` in v1.
   Anything that wants to scan remote media must first stage it onto a
   volume both replicas mount.
2. **Results only go to stdout.** `serve` hardwires
   `result.NewJSONLSink(os.Stdout)`. There is no way to push a verdict to
   another system.

This spec covers both. It does **not** cover the test harness itself, a
verdict corpus, or a fixture adapter; those were discussed and
deliberately deferred to a later spec. It also does not cover the
`flag`/`block` → numeric severity refactor, which is its own breaking
change and its own spec.

## Scope

| # | Change | Packages |
|---|---|---|
| 1 | `kind:"url"` source + allow-listed fetcher | `internal/fetch` (new), `pkg/moderation`, `internal/pipeline`, `internal/cli`, `internal/config` |
| 2 | `output.sinks` list — `MultiSink`, `FileSink`, `WebhookSink` | `internal/result`, `internal/cli`, `internal/config` |

The two are independent and land as separate commits.

## Prior art consulted

- **Azure AI Content Safety** — precedent for numeric severity kept
  *alongside* a derived action, relevant to the deferred severity spec,
  not to this one.
- **promptfoo / golden-dataset moderation evaluation** — informed the
  deferred harness design (case manifest, pass/fail matrix, corpus as a
  regression gate on threshold changes).
- **Async queue E2E testing practice** — correlation IDs, poll-to-timeout
  rather than fixed sleeps, run-scoped identity so stale queue entries
  cannot poison a run. Also deferred with the harness.

Nothing from this list changes the design below; it is recorded so the
follow-up spec does not repeat the research.

---

## Part 1 — `kind:"url"` and `internal/fetch`

### Trust posture

`SECURITY.md` currently defines two classes of URL with opposite rules.
This change introduces media-source URLs as a live feature, so the
document must state three:

1. **Media source URLs** (new, this spec) — job-supplied, untrusted, deny
   private ranges.
2. **Provider endpoint URLs** — operator config, private ranges expected
   (`shieldgemma`).
3. **Result webhook URLs** (new, Part 2) — operator config, private
   ranges expected.

Classes 2 and 3 share rules. Class 1 is the adversarial one: the URL
arrives in an intake request body, so an attacker who can post a job
chooses the destination. Every rule below exists to bound that.

### Interface

```go
// Package fetch resolves allow-listed remote media URLs to local files.
//
// The destination is chosen by an untrusted job payload, so every
// control here is fail-closed: the feature is off by default, an empty
// allow-list refuses to boot, and the IP policy is enforced against the
// address actually dialed, not the hostname parsed.
package fetch

type Fetcher struct {
    cfg      URLConfig
    client   *http.Client
    ipPolicy func(netip.Addr) error // seam: tests substitute; nil = deny-list
}

// Fetch downloads rawURL into dir and returns the local path.
//
// cleanup MUST be deferred by the caller immediately, on every exit
// path, before ack — the same contract as FrameSource.Frames. It is
// non-nil even when err != nil.
func (f *Fetcher) Fetch(ctx context.Context, rawURL, dir string) (path string, cleanup func(), err error)
```

### Rules

Each row is a named test with a positive and a negative case.

| Rule | Behavior |
|---|---|
| Off by default | `source.url.enabled: false`. Intake rejects `kind:"url"` with `400`; the pipeline rejects it with `verdict:"error"`. |
| Allow-list required | `enabled: true` with an empty `allow_hosts` refuses to boot. Same fail-closed reasoning as `provider_thresholds.mode=override` with no labels. |
| Scheme | `https` only. No exceptions, no `http`. |
| No userinfo | `https://user:pw@host/…` rejected. Credentials are env-only (invariant 4). |
| Denied ranges | Loopback, RFC 1918, `169.254.0.0/16`, IPv6 link-local, ULA `fc00::/7`, `::1`, unspecified, multicast, CGNAT `100.64.0.0/10`. Unconditional. |
| DNS rebinding | The host allow-list is checked at parse time (it is a hostname list). The **IP policy** is enforced separately in `net.Dialer.Control`, against the resolved address of every connection. So a hostname that is allow-listed and validates, then re-resolves to `169.254.169.254`, is refused at socket level. Parse-time IP checking alone would not catch this. |
| Redirects | `CheckRedirect` returns an error. A redirect is a destination vismod did not choose. Matches the `shieldgemma` client. |
| Size cap | `io.LimitReader` at `source.url.max_bytes` (default 256 MiB). `Content-Length` is a hint and is never trusted. Exceeding the cap deletes the partial file. |
| Timeout | `source.url.timeout` (default 60s), separate from `queue.job_timeout`. |
| Media type | Response `Content-Type` must appear in `source.url.allowed_media_types`. |
| Never to ffmpeg | Extraction receives the local path only. A URL never reaches an ffmpeg argument; invariant 5's protocol deny-list is untouched. |

### Deliberate omission

There is no `allow_private_hosts` flag. An operator with an internal
RFC 1918 media store cannot use this feature. That is a real use case,
refused under the fail-safe bias: a boolean that disarms the entire
deny-list is the wrong shape for the risk. If it is needed later it
should arrive as an explicit allow-list of private CIDRs — narrower, and
reviewable per entry.

Testability does not depend on that flag. The `ipPolicy` seam lets unit
tests substitute a permissive policy to exercise HTTP behavior against
`httptest` on loopback, while the deny-list itself is table-tested
directly.

### Pipeline integration

One new step at the head of the per-job flow:

```
resolve source -> [url? fetch to job temp dir] -> frames -> dedup -> fan-out -> ...
```

After a successful fetch the pipeline rewrites the in-flight `Source` to
`{kind:"file", ref:<local path>}` before anything downstream observes it.

This matters specifically for `VideoModerator.AnalyzeVideo(ctx, video
Source)`: handing a video-native vendor the original URL would delegate
the fetch this whole section exists to constrain, to a third party
operating under different rules.

The temp file lives in the job's working directory and is removed by the
same `defer cleanup()` discipline the frame WorkDir already uses — on
every exit path, before ack.

### Presigned URLs are credentials

An S3/GCS presigned URL carries its authorization in the query string.
`Source.Ref` currently lands verbatim in the result envelope, the audit
record, and structured logs. For a `url` source that would write a live
credential into all three, breaking invariant 4 (secrets never in
envelopes, logs, or audit) and invariant 3 (audit stores hashes, never
raw values).

Resolution, for `kind:"url"` only:

- `Source.Ref` stores **scheme + host + path**. Query and fragment are
  dropped.
- `moderation.Source` gains
  `RefDigest string \`json:"ref_digest,omitempty"\`` carrying
  `SHA-256(full URL)`, so a verdict can still be correlated back to the
  exact request without storing the credential.
- The full URL exists in memory for the duration of the fetch and is
  never logged, audited, or serialized.

Versioning note: `Source` is a public contract type in `pkg/moderation`,
but it is serialized into `result.ResultEnvelope`, not into
`NormalizedResult` — and the envelope has no version field of its own.
`SchemaVersion` is therefore bumped to `1.2.0` as the additive signal for
this change, on the grounds that it is the only version marker consumers
have. That the envelope lacks independent versioning is a real gap; it is
noted here and left alone rather than fixed opportunistically in this
spec.

### Failure matrix

Retries live inside the fetcher, mirroring `moderate.DoJSON`. The queue's
`Retry` disposition stays reserved for job-level infrastructure failure
(sink writes), unchanged.

| Failure | Class | Outcome |
|---|---|---|
| Bad scheme, userinfo, host not allow-listed | Terminal | Intake `400`; if reached at execution, `verdict:"error"` + DLQ |
| Resolved IP in a denied range | Terminal | `verdict:"error"` + DLQ. Policy, not transient — no retry |
| DNS temp failure, timeout, 429, 5xx | Retryable | Backoff inside the fetcher; exhausted → `verdict:"error"` + DLQ |
| Other 4xx | Terminal | `verdict:"error"` + DLQ |
| Oversize, wrong content-type, disk write failure | Terminal | Partial file deleted, `verdict:"error"` + DLQ |

No path produces `allow`. Invariant 1 holds by construction.

### Validation happens twice

At intake (`POST /jobs`) and again at execution, following the existing
rule for per-job overrides. Intake validation gives the caller a `400`
with a reason; execution validation is what actually protects the
fetcher, because a job can also arrive directly on the Redis queue from
another producer.

### Config

```yaml
source:
  url:
    enabled: false            # off by default
    allow_hosts: []           # required non-empty when enabled
    max_bytes: 268435456      # 256 MiB
    timeout: 60s
    allowed_media_types:
      - video/mp4
      - video/webm
      - image/jpeg
      - image/png
```

---

## Part 2 — configurable output sinks

### Config

```yaml
output:
  sinks:
    - type: stdout
    - type: file
      path: /var/lib/vismod/results.jsonl
    - type: webhook
      url: https://collector.internal/results
      timeout: 5s
      max_attempts: 3
```

An absent `output` block means stdout only — current behavior, unchanged,
so no existing deployment is affected. An `output` block with an empty
`sinks` list refuses to boot: silently emitting nothing is precisely the
failure mode this project exists to prevent.

### MultiSink

Fans one envelope to N sinks. Failure policy: **every sink is attempted,
the first error is returned.** Not fail-fast — a webhook outage must not
suppress the local JSONL record.

The returned error propagates to the existing queue `Retry` disposition,
so the job redelivers. Idempotency-per-JobID means the sinks that already
succeeded are no-ops on the second pass, and the one that failed gets
another attempt.

### WebhookSink

POSTs the `ResultEnvelope` as JSON. Retry classification reuses what
`moderate.DoJSON` already implements — 429/5xx/timeout retryable, other
4xx terminal — rather than growing a second copy of that logic. Capped at
`max_attempts`.

Idempotency is per-JobID in-process, matching `JSONLSink`. The receiver
also gets `JobID` in the body so it can dedupe redeliveries itself, which
is the only defense that survives a worker restart.

The webhook URL is operator configuration, never job input. It is class 3
in the trust posture above: private ranges are expected and permitted,
and the fetcher's deny-list must not be applied to it. `SECURITY.md` has
to say this explicitly, or a future reader will apply the wrong rule.

### FileSink

Appends JSONL to a path. `O_APPEND`, one `Write` per envelope,
mutex-guarded so concurrent workers cannot interleave partial lines.

Two replicas must not share one file — the same hazard the audit log has,
which is why compose already gives each replica its own volume. This goes
in the deploy docs.

### Boot validation, all fail-closed

- unknown sink `type` → refuse
- `webhook` with no `url`, or a URL failing the operator-endpoint rules
  (no userinfo, `169.254.0.0/16` denied) → refuse
- `file` sink whose path is not writable → refuse at boot, not at first
  write
- empty `sinks` list → refuse

### ConfigHash

Neither `output` nor `source.url` affects a verdict, so neither may enter
`ConfigHash`. If they did, changing a webhook URL would make every
subsequent envelope incomparable with every previous one. A test asserts
the hash is unchanged across both new config blocks.

---

## Observability

New metrics in `internal/observe`:

- `vismod_fetch_duration_seconds` (histogram)
- `vismod_fetch_bytes_total` (counter)
- `vismod_fetch_failures_total{reason}` (counter)
- `vismod_sink_write_failures_total{type}` (counter)

`reason` is a bounded label set drawn from the failure matrix — never a
free-text error string, which would be both a cardinality bomb and a
potential leak of a URL into metrics.

## Testing

Everything runs with no network and no credentials, so `go test ./...`
stays clean.

### internal/fetch

- `ipPolicy` table test over ~20 addresses: loopback, RFC 1918,
  `169.254.0.0/16`, IPv6 link-local, ULA, `::1`, unspecified, multicast,
  CGNAT, public v4 and v6 — positive and negative in one table
- One named test per validation rule: scheme, userinfo, allow-list miss,
  allow-list hit
- **DNS rebinding**: a custom resolver returns a public address on the
  first lookup and `169.254.169.254` at dial time; the connection is
  refused in `Dialer.Control`. This is the test that proves the design
  rather than the parser
- Redirect returned by `httptest` → error, not followed
- Body one byte over `max_bytes` → error, temp file removed
- Content-type outside the allow-list → error
- Timeout and context cancellation mid-body → error, temp file removed
- Retry classification: 429 and 500 retried, 403 and 404 terminal,
  attempts capped
- Cleanup runs on every exit path: success, each failure mode, and
  cancellation
- The resulting `Source` is `kind:"file"`; no ffmpeg argument and no
  `AnalyzeVideo` call ever receives a URL
- Envelope `Source.Ref` carries no query string and `ref_digest` is set

### internal/result

- `MultiSink`: all sinks attempted when one fails; first error returned;
  success path writes to every sink
- Idempotency per JobID for each sink type, including after a partial
  failure and redelivery
- `WebhookSink` against `httptest`: method, content-type, and a body that
  round-trips to an identical envelope; retry on 429/5xx; terminal on
  4xx; `max_attempts` respected; `JobID` present for receiver-side dedupe
- `FileSink`: append semantics; concurrent writes from many goroutines
  produce N well-formed lines with no interleaving
- Negative boot tests: unknown type, webhook without URL, unwritable file
  path, empty sink list — each refuses with a specific message

### internal/config

- Positive and negative parse tests for `source.url` and `output`
- `ConfigHash` unchanged across both new blocks

### internal/pipeline

- A `url` job whose fetch fails yields `verdict:"error"`, never `allow`,
  for each terminal and each exhausted-retry failure class

## Docs to update in the same commits

`SECURITY.md` (the three URL classes and the fetcher rules),
`config.example.yaml`, `README.md`, `CLAUDE.md` and `AGENTS.md`
(architecture map gains `internal/fetch`; a gotcha entry for the
ref-redaction rule), `deploy/README.md` (FileSink is per-replica), and
`docs/agent/{STATUS,TASKS,UNVERIFIED}.md`.

## Follow-up specs, not this one

1. **Test harness** — enqueues a corpus of URLs, collects results via the
   webhook sink, compares actual verdict to expected verdict (exact
   match, verdict only), reports url / expected / actual / reason / score
   / threshold. Single binary, compose overlay on the existing stack.
   This spec's two parts are its prerequisites.
2. **Numeric severity** — replacing `flag`/`block` with a numeric
   severity scale. Breaking: touches rollup, thresholds, config, UI,
   audit, and `SchemaVersion` major. The harness from (1) is the
   regression gate that makes it safe to attempt.
