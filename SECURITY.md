# Security Policy & Threat Model

> Not legal advice. This document describes vismod's security posture and the
> threats it does and does **not** defend against, so operators can deploy it
> with eyes open.

## Reporting a vulnerability

Report suspected vulnerabilities privately via GitHub Security Advisories
("Report a vulnerability" on the repository's Security tab) rather than a public
issue. Please include a reproduction and the affected version/commit. Do **not**
include real illegal content or live secrets in a report.

---

## Secrets handling

- **Secrets are environment-only.** API keys and bearer tokens are read solely
  from `VISMOD_`-prefixed environment variables (e.g. `VISMOD_AZURE_KEY`). They
  are **never** read from, or written to, the YAML config. Boot fails fast if a
  selected adapter's required secret is missing.
- Secrets are excluded from the `ConfigHash` provenance stamp and from all logs.

## SSRF / egress (§C)

The Azure adapter accepts an image as inline base64 `content` **or** a remote
`blobUrl`. A remote-fetch URL is an **SSRF / egress vector**.

- **v1 default: local-file / inline `content` only.** Remote `blobUrl` (and any
  future `Source.Kind=url`/`s3`) is disabled.
- If `blobUrl` is ever enabled, it **must** enforce a host/scheme **allow-list**
  and **forbid private / link-local / metadata ranges** — RFC 1918,
  `169.254.0.0/16` (incl. cloud metadata `169.254.169.254`), `::1`, and other
  loopback/internal ranges.

## Logging / data-exposure (§F.6, §G.2)

- Structured logs carry job id, adapter, latency, and verdict — **never** media
  bytes, PII, provider `Raw` free-text, OCR, or captions.
- The **`Raw`** field is optional and **sanitized**: a descriptive-output
  adapter (e.g. a future local vision-LLM) must strip natural-language image
  descriptions before they reach the Sink, logs, or audit.

## Audit log: tamper-evidence scope (§G.5)

The audit log (`internal/audit`) is an **append-only, hash-chained** decision
log. Each record is
`{seq, timestamp, prev_hash, payload, entry_hash}` where
`entry_hash = SHA-256(seq ‖ timestamp ‖ prev_hash ‖ canonical(payload))`,
the genesis `prev_hash` is all-zeros, fields are **length-prefixed**, and
`canonical(payload)` is **RFC 8785 JCS** (sorted-key, compact, UTF-8) so
`vismod audit verify` recomputes byte-identical hashes across processes.
Appends are **idempotent per `job_id`** (no duplicate `seq`, no gap). The
payload binds a decision to its inputs **by hash** — `SHA-256(Raw)` +
`ModelIdentity` + verdict — and **never stores `Raw` itself**.

**🔴 Honest scope.** A bare hash chain detects **truncation** and **in-place
edits**. It does **NOT** detect a **full-chain rewrite by a write-capable
insider**: anyone who can rewrite the whole file can recompute every
`entry_hash` and present an internally-consistent forged chain.

**Tamper-*resistant* upgrade (documented seam, not in v1):** sign each
`entry_hash` (HMAC or Ed25519) with a key held **outside** the writer's trust
boundary, **or** periodically anchor the chain head-hash to WORM / external
storage. Either makes a silent full rewrite detectable. The chain layout already
isolates the hash computation so this can be added without a schema break.

## Anti-abuse / evasion-oracle residual risk (§G.7)

vismod emits the per-category `Score` and the exact `Threshold`. In theory this
is an **evasion oracle**: an adversary can read distance-to-threshold and tune
content to slip under it.

- **v1 accepts this risk and ships no control.** The underlying scanning models
  are already publicly available for testing, so suppressing scores would add
  friction without removing the adversary's capability. There is **no
  `antiabuse.*` config key or output mode** in v1.
- Operators exposing scores to untrusted parties should weigh this; the residual
  risk is stated here plainly so the decision is explicit.

## Fail-safe posture (§F.5)

vismod is security-critical and **fails safe, never silent**:

- A provider/frame/extraction failure yields `Verdict=error` (**never `allow`**)
  and dead-letters. A partially-errored video never rolls up to `allow`.
- A worker **panic** dead-letters that job and the pool keeps running.
- A **sustained** provider outage flips readiness to **not-ready** (backpressure)
  rather than flooding the human-review queue.
- The dead-letter queue is **bounded**; at capacity new enqueues are rejected and
  alerted — never dropped, never auto-allowed.

## Container posture (§I)

The Docker image runs **non-root**, ships only `ffmpeg`+`ffprobe` plus the static
binary, uses a writable ephemeral `frames.workdir` (so a read-only rootfs still
works), and drains on `SIGTERM`. Durable queue payloads and operator UIs (M5)
must carry opaque refs only and be access-controlled.
