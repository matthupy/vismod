# Security

## Reporting a vulnerability

Open a private GitHub security advisory on this repository (Security →
Report a vulnerability). Do not file public issues for exploitable bugs.

## Threat model and hard boundaries

### FFmpeg workflows are an operator-trust boundary

Custom extraction workflows are configuration written by the operator —
they are **not** untrusted user input, but vismod still enforces
guardrails because a compromised or careless config must not become code
execution or exfiltration:

- Workflows are **argument-list templates**, rendered per-element and
  passed to `exec.CommandContext`. There is **no shell** anywhere in the
  extraction path, and `-nostdin` is always enforced.
- Allowed placeholders only: `{{.Input}}`, `{{.WorkDir}}`,
  `{{.MaxFrames}}`, `{{.MaxWidth}}`. Anything else fails validation.
- `{{.Input}}` is bound to the current job's local file: exactly one
  `-i {{.Input}}` pair; a second `-i` fails validation.
- **Protocol deny-list**: `http:`, `https:`, `rtmp*:`, `concat:`,
  `pipe:`, `subfile:` (including the `subfile,,opts:` comma form),
  `data:`, `tcp:`, `udp:`, `file:`, protocol chaining (`cache:http:`),
  and any `://` are rejected at validation time in both templates and
  rendered args. Only plain local paths reach ffmpeg. This blocks
  arbitrary-file reads and network exfiltration via a crafted workflow.
- **Output confinement**: the output pattern must live under the
  pipeline-owned per-job `WorkDir`; other absolute paths and `..`
  traversal are rejected. `max_frames` and `timeout` are hard caps.

`vismod workflows validate` runs the full gate and must pass at boot.

### SSRF / egress posture

**Three** kinds of URL exist in this system, in two different trust
classes, and they take **opposite** rules. Do not apply one class's rule
to the other.

| Class | URLs | Chosen by | Private ranges |
|---|---|---|---|
| 1 | media source (`kind:"url"`) | the **job** — untrusted | **denied at dial** |
| 2 | `shieldgemma` inference endpoint | operator yaml | expected, allowed |
| 3 | `webhook` result sink receiver | operator yaml | expected, allowed |

**1. Media source URLs — untrusted, deny private ranges.**

A job may name a remote asset (`{"kind":"url","ref":"https://…"}`), which
is the only place in vismod where an untrusted payload chooses an
outbound destination. It is **on by default**: with
`source.url.allow_hosts` empty, a job may name any host that survives the
address policy below. There is no separate enable flag — the destination
rules are the control surface, and a second switch could only ever
disagree with them. **Set `allow_hosts` in production**; it narrows to
exactly the hostnames you list. Media is still sent to providers as
**inline content only** — Azure `blobUrl` and any provider-side
remote-fetch parameter remain unused, because that would move the fetch
outside these controls.

Every control below is fail-closed (`internal/fetch`, one test per rule):

- **Exact hostname allow-list** (`source.url.allow_hosts`). Empty means
  any host. When populated: no wildcards, no suffix matching —
  `example.com` does not permit `evil.example.com`.
- **`https` only.** The single exception is a host named in
  `source.url.allow_private_hosts`, below.
- **No userinfo** in the URL; credentials stay env-only.
- **Address deny-list at dial time** (`fetch.DenyPrivate`, run from
  `net.Dialer.Control`): loopback, RFC 1918, `169.254.0.0/16` (cloud
  metadata), IPv6 link-local and ULA (`fc00::/7`), the unspecified
  address, multicast, and CGNAT `100.64.0.0/10`. Addresses are unmapped
  first, so a v4-mapped form (`::ffff:127.0.0.1`) cannot smuggle a
  private v4 past the v4 predicates.
- **Non-public address space requires `source.url.allow_private_hosts`.**
  A hostname listed there — and only that hostname — is dialed under the
  weaker `fetch.DenyMetadata` policy, which permits loopback, RFC 1918
  and ULA so an operator can scan media they serve themselves, and may be
  reached over plain `http`. It still denies the instance-metadata
  endpoints (`169.254.0.0/16` and AWS's IPv6 `fd00:ec2::/32`, which sits
  inside the otherwise-permitted ULA range), the unspecified address,
  multicast, and CGNAT. The policy is selected from the **hostname being
  dialed**, before resolution, so the relaxation cannot leak: a public
  host that re-resolves into RFC 1918 still meets `DenyPrivate`.
- **The host allow-list and the address deny-list are two separate
  checks, deliberately.** The allow-list runs at parse time, on text; the
  address policy runs per-connection, against the address actually
  dialed. Only the second one catches **DNS rebinding** — an allow-listed
  name that re-resolves to `169.254.169.254` between validation and
  connect. Collapsing them into one parse-time check would silently
  remove that defense.
- **Redirects are refused** — a redirect is a destination vismod did not
  choose. **The size cap is enforced on bytes read**
  (`source.url.max_bytes`), never on `Content-Length`. The response
  `Content-Type` must be in `source.url.allowed_media_types`.
- **The download is transient.** It lands in a job-scoped temp file,
  under a per-job temp directory, deleted on every exit path before the
  job is acked — the same contract as frame extraction's `WorkDir`.
- **No URL ever reaches ffmpeg.** The pipeline hands analysis the local
  temp path as a `kind:"file"` source; the protocol deny-list below is
  unchanged and still rejects any input path carrying a scheme.
- **A presigned URL's query string is a credential.** Only
  scheme+host+path is recorded in `source.ref`; `source.ref_digest`
  carries SHA-256 of the full URL so a verdict stays traceable to the
  exact request. The full URL never reaches a result envelope, audit
  record, log line, error string, HTTP error response, operator-UI job
  feed, or metric label. Metric failure reasons come from a fixed label
  set, never from an error string.
- **Fail-safe is preserved.** Any fetch failure — rejected URL, denied
  address, oversize body, timeout, exhausted attempts — yields
  `verdict:"error"` and a dead-letter. Never `allow`.

Rejections are terminal and are not retried: retrying cannot change a
destination that was refused. Only transient transport failures and
retryable HTTP statuses consume `source.url.max_attempts`.

Validation runs **twice by design** — at intake (so a caller gets a `400`
with a reason) and again in the fetcher (because with
`queue.driver: redis` a job can arrive straight onto the queue without
passing through intake). The same rule as the per-job workflow and dedup
overrides.

**Who can enqueue can choose destinations.** The `serve` intake
(`intake_addr`) has no authentication — it is a dev/demo surface. Because
url sources are on by default, anyone who can reach that port can make
this process issue outbound requests. Do not publish it on an untrusted
network: the compose stack binds it to the host's loopback
(`127.0.0.1:8080:8080`), and a real deployment should put it behind an
authenticated ingress or feed the queue directly. Setting `allow_hosts`
bounds the damage to hosts you chose.

- Frame extraction rejects any input path containing a protocol scheme.

**2. Operator-supplied endpoint URLs — operator config, private ranges
expected.**

The cloud adapters' hosts are vendor-fixed. Two URLs in the codebase are
operator-supplied instead: the `shieldgemma` adapter's inference endpoint
(outbound, egress to a classifier) and the `webhook` result sink's
receiver URL (outbound, egress of result envelopes). Both are *expected*
to be loopback or RFC 1918 — that is the feature, and rule 1's deny-list
must not be applied to them. The rules that govern them instead (enforced
by `validateEndpoint` in
`internal/moderate/adapters/shieldgemma/endpoint.go`, one test per rule):

- **Config-only.** The endpoint comes from `adapter.options` in yaml and
  is never read from a job, queue payload, or intake body. With no
  request-time URL there is no attacker-chosen destination; the endpoint
  is in the same operator-trust class as an ffmpeg workflow.
- `http` and `https` schemes only. `http` is permitted for loopback and
  RFC 1918 hosts; a public host must be `https`.
- `169.254.0.0/16` (and IPv6 link-local) rejected unconditionally, under
  both schemes — no legitimate inference server lives on the metadata
  range, and it is the one range where a misconfiguration turns into
  cloud-credential theft.
- No userinfo in the URL; credentials are env-only per the secrets rule.
- Redirects are not followed (`CheckRedirect` errors) — a redirect is a
  destination vismod did not choose.

A hostname that is not an IP literal (other than `localhost`) is treated
as public and therefore requires `https`: vismod cannot know at boot what
a name will resolve to at request time. Resolution happens per request, so
a boot-time check does not close DNS rebinding — the **config-only** rule
above is what makes that acceptable.

See [docs/self-hosted-classifiers.md](docs/self-hosted-classifiers.md)
for the reasoning.

#### The `webhook` result sink enforces the same rule set

`output.sinks[].url` (see `config.example.yaml`) is the second URL in
this class. It is enforced by `validateWebhookURL` in
`internal/config/config.go`, deliberately written as the same rule set in
the same order as `validateEndpoint`, plus `CheckRedirect` on the client
built by `result.NewWebhookSink`
(`internal/result/webhook.go`). Rule by rule:

- **Config-only.** The URL comes from the `output.sinks` block in yaml
  and is never read from a job, queue payload, or intake body.
- `http` and `https` schemes only. `http` is permitted for loopback and
  RFC 1918 hosts; a public host must be `https`. A hostname that is not
  an IP literal (other than `localhost` / `*.localhost`) is treated as
  public.
- `169.254.0.0/16` (and IPv6 link-local) rejected unconditionally, under
  both schemes.
- No userinfo in the URL; credentials are env-only.
- Redirects are not followed (`CheckRedirect` errors).

Two things this rule set does **not** cover, in either place. DNS
rebinding is not closed — resolution happens per request, and
config-only provenance is what makes that acceptable. And there is no
outbound authentication on the webhook POST: vismod does not sign or
authenticate the request, so the receiver cannot verify the sender, and
anything reachable at the configured URL will be handed result
envelopes. Envelopes carry verdicts, scores, model identity, the source
ref, an error summary, and the caller-supplied `metadata` object (if
any) — never media bytes, provider `Raw`, or secrets. `metadata` is
caller-controlled free text: vismod validates only that it is a JSON
object under 4096 bytes, and never redacts, scans, or inspects its
contents. It rides the same unauthenticated POST as the rest of the
envelope, so a caller who puts a secret in `metadata` hands it to
whatever the operator pointed the webhook at. That is still moderation
metadata leaving the process, and choosing the receiver is the
operator's whole control.

### Audit log: tamper-EVIDENT, not tamper-PROOF (honest scope)

The audit log is an append-only hash chain:
`entry_hash = SHA-256(seq ‖ timestamp ‖ prev_hash ‖ JCS(payload))`.

What it detects: truncation, deletion, reordering, and in-place edits —
`vismod audit verify` reports the first broken link.

What it does NOT detect: a **full-chain rewrite** by an insider with
write access to the log file, who can recompute every hash. Upgrades to
tamper-RESISTANT are the `audit.Signer` seam: HMAC or Ed25519 signing of
each entry hash with a key the writer does not hold, or periodic
anchoring of the head hash to an external system (ticketing, a
transparency log, another machine).

The log stores `SHA-256(Raw)`, the `ModelIdentity`, and the verdict —
never provider payloads, media bytes, OCR text, or captions.

### Score-output / evasion-oracle residual risk

v1 emits full normalized scores and thresholds in envelopes. An
adversary who can submit media and read envelopes can probe decision
boundaries. This is accepted for v1 because the underlying vendor models
are already publicly testable; there is no `antiabuse.*` output mode.
Operators exposing results to semi-trusted parties should strip scores at
their own API layer. Revisit if a coarse-output mode is added.

### Media handling

- Job payloads (queue, Redis, any ops surface) carry **opaque file
  refs/IDs, never media bytes**. They may also carry the caller's
  `metadata` object, unredacted — one more reason to secure the Redis
  instance (auth + network policy); it is part of the trust boundary.
- **A `kind:"url"` job payload carries the URL as submitted**, query
  string included: the fetcher needs the whole thing, and redaction
  happens where results are recorded, not on the queue. If you submit
  presigned URLs, the queue holds live credentials for as long as the job
  is pending — one more reason the Redis instance is inside the trust
  boundary, and a reason to keep presigned lifetimes short.
- Per-job frame extraction happens in a transient `WorkDir` deleted on
  every exit path before the job is acked. Nothing in the ordinary
  result/audit path persists media.
- Secrets are env-only (`VISMOD_*`); they never appear in yaml, logs, or
  envelopes. Logging never includes media bytes, PII, `Raw` payloads,
  OCR text, or captions.
