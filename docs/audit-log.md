---
title: Audit log
nav_order: 6
---

# Audit log

Every decision appends one record to a hash-chained, append-only log.
Each record binds the verdict to its inputs **by hash**:

- `SHA-256(Raw)` — a digest of the provider's sanitized raw response;
- the model identity (`adapter`, `model_version`);
- the `config_hash` (adapter + model version + resolved thresholds).

**Never the media, and never the provider payloads.** The log answers
"what decided this, under which thresholds, against which input digest" —
it is not a copy of the content and cannot be used to reconstruct it.
This is deliberate: an audit trail for moderation decisions must not
itself become a store of the material being moderated.

## Verifying

```sh
vismod audit verify
```

Recomputes the chain and reports the **first broken link** — the earliest
record whose stored hash doesn't match a recomputation over its
predecessor. Everything after that point is suspect.

## Restart behavior

`audit.Open` replays the existing file and rebuilds the seen-`job_id` set
before appending. This is why the audit log's per-`job_id` idempotency
survives a restart when the result sinks' does not — a job redelivered
after a crash gets a second line in the `file` sink but not a second
audit record.

## Scope honesty

The log is **tamper-evident, not tamper-proof.**

Anyone with write access to the file can truncate it, or rewrite it from
some point forward and recompute every subsequent hash — the chain will
verify clean. What the chain actually catches is *modification in place*
without rehashing, which is the accidental and casual case, not the
determined one.

If you need tamper-*proof*, the chain head has to be published somewhere
you don't control (a timestamping service, a second system's log, an
append-only object store with retention lock). vismod does not do this
for you. See
[SECURITY.md](https://github.com/matthupy/vismod/blob/main/SECURITY.md).

## Per-replica chains

Each replica maintains its own independent chain. Two replicas must not
share an audit log path — the interleaving breaks the chain. Aggregate
after the fact if you need one view.
