---
title: Audit log
nav_order: 9
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

### What `raw_sha256` covers

For an **image**, the digest is over the provider's sanitized response to
that one image.

For a **video**, it is over a JSON array holding one entry per *scanned*
frame, in timestamp order — the same order as `frames` in the result
envelope, so position *i* in the array is the response behind frame *i*.
A frame the provider did not score (a failed call) appears as `null`
rather than being skipped, so a failure never shifts the frames after it.
Scanned means after dedup and after the `max_frames` cap: frames that
never reached the provider are not in `frames` and are not in the digest.

The field is the **empty string** when no provider response stands behind
the record at all — an extraction failure, or a video where every frame
errored. An empty digest means "no evidence", not "evidence of nothing";
these records still carry a verdict (always `error` — vismod never allows
on failure) and still chain normally.

Records written before this binding existed also carry `raw_sha256: ""`.
They verify, and they must not be rewritten to add it: the log is
append-only, and a chain is only evidence for as long as nobody edits its
history to improve it.

Reproducing a digest requires the provider's response, which vismod
deliberately does not keep. The field is for the case where you *have* a
response — from the vendor's own logs, or a captured payload — and need to
prove it is the one that produced a given verdict.

## Verifying

```sh
vismod audit verify
```

Recomputes the chain and reports the **first broken link** — the earliest
record whose stored hash doesn't match a recomputation over its
predecessor. Everything after that point is suspect.

## What verification can tell you

There are three outcomes, not two, and the command distinguishes only two
of them. The third is the one to watch for.

| State | How you observe it | What it proves | What it does **not** prove |
|---|---|---|---|
| **Verified** | Exit 0, `audit chain OK: N records verified`, **and `<log>.head` exists** | No in-place edit, and no records removed from the tail past the anchored seq | That a write-capable insider didn't rewrite the log *and* the anchor together |
| **Failed** | Non-zero exit, with the error naming truncation, a head-anchor mismatch, or a broken link | Records were lost or altered. Treat as an incident | Which of accident or attack caused it |
| **Unanchored** | Exit 0, the **same** `audit chain OK` line, but **no `<log>.head` on disk** | Only that the chain is self-consistent | Nothing at all about tail truncation — dropping the last N records leaves a chain that still verifies clean |

`vismod audit verify` does not warn you about the unanchored case: a
missing anchor is not an error, because logs written before anchoring
existed must stay verifiable. **Check for the file yourself.** A restore
that silently dropped `<log>.head` looks exactly like a healthy log.

Two failure texts mean different things. *"log is truncated: head anchor
names seq N but the log ends at seq M"* means records went missing from
the end. *"does not match the head anchor (rewritten history)"* means the
anchored record itself changed. A malformed anchor is a third case —
corrupt storage, not evidence of tampering.

One non-failure worth recognizing: an anchor **behind** the chain is
normal. The anchor is written after the record lands, so a crash in
between leaves it one seq short, and verification tolerates that. The
guarantee it buys is bounded accordingly — everything up to the anchored
seq is covered, anything after it is chain-only.

## Restoring a log

Order matters here, and there is a point at which you must stop.

1. Restore `<log>` **and** `<log>.head` from the same backup generation.
   If your backup tool captures them separately, capture the log first and
   the anchor second — an anchor newer than its log is precisely the
   truncation signature.
2. Run `vismod audit verify <path>` **before** starting the service.
3. Confirm `<log>.head` is actually present. A clean result without it is
   the unanchored state above, not a pass.
4. If verification reports truncation or a rewritten history, **halt and
   escalate.** Do not start the service against that log.

`audit.Open` will refuse to append to a broken or truncated log anyway, so
the failure surfaces at boot if it is not caught here. Do not resolve that
by deleting `<log>.head` to make the process start: that converts a
detected, evidenced loss into a permanently unanchored log.

This is the detail behind the head-anchor line in the
[production checklist](production-checklist.md).

## Restart behavior

`audit.Open` replays the existing file and rebuilds the seen-`job_id` set
before appending. This is why the audit log's per-`job_id` idempotency
survives a restart when the result sinks' does not — a job redelivered
after a crash gets a second line in the `file` sink but not a second
audit record.

## Scope honesty

The log is **tamper-evident, not tamper-proof.**

Anyone with write access to the file can rewrite it from some point
forward and recompute every subsequent hash — the chain will verify
clean. What the chain actually catches is *modification in place* without
rehashing, which is the accidental and casual case, not the determined
one.

Truncating the tail is caught separately, by the head anchor written
alongside the log (`<log>.head`): it records the last `(seq,
entry_hash)`, and verification requires that record to still be present.
Because the default anchor lives next to the log, an insider can update
both — but truncation is then two coordinated writes rather than one, and
accidental loss is always caught. Hold the anchor elsewhere and pass it
to `audit.VerifyWith` to close that gap.

If you need tamper-*proof*, the chain head has to be published somewhere
you don't control (a timestamping service, a second system's log, an
append-only object store with retention lock). vismod does not do this
for you. See
[SECURITY.md](https://github.com/matthupy/vismod/blob/main/SECURITY.md).

## Per-replica chains

Each replica maintains its own independent chain. Two replicas must not
share an audit log path — the interleaving breaks the chain. Aggregate
after the fact if you need one view.
