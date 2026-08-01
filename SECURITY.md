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

Two kinds of URL exist in this system and they take **opposite** rules.
Do not apply one rule to the other.

**1. Media source URLs — untrusted, deny private ranges.**

- v1 sends media to providers as **inline content only**. Azure
  `blobUrl` (and any future `url:`/`s3:` source kind) is a remote-fetch
  vector and is disabled. If a URL source is ever added it MUST sit
  behind a scheme+host allow-list that forbids RFC 1918 ranges,
  169.254.0.0/16 (cloud metadata), and loopback.
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
ref, and an error summary — never
media bytes, provider `Raw`, or secrets — but that is still moderation
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
  refs/IDs, never media bytes**. Secure the Redis instance (auth +
  network policy) — it is part of the trust boundary.
- Per-job frame extraction happens in a transient `WorkDir` deleted on
  every exit path before the job is acked. Nothing in the ordinary
  result/audit path persists media.
- Secrets are env-only (`VISMOD_*`); they never appear in yaml, logs, or
  envelopes. Logging never includes media bytes, PII, `Raw` payloads,
  OCR text, or captions.
