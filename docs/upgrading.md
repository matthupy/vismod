---
title: Upgrading
nav_order: 13
---

# Upgrading past the per-replica processing change

This page covers exactly one upgrade: crossing the release that replaced
the single shared Redis in-flight key with per-replica processing lists
(2026-08). It has a retirement condition — see the end — and is meant to be
deleted, not kept.

## Does this apply to you?

```sh
redis-cli EXISTS <prefix>:processing
```

`0` and you are done; close this page. Anything else means a pre-upgrade
vismod left in-flight payloads in the old shared key, and the rest of this
matters.

## What each binary does during the rollout

The dangerous interval is the deploy itself, when both versions are live
against the same Redis.

| | Claims into | On `Start` |
|---|---|---|
| **Old** | `<prefix>:processing` (shared) | Drains that whole key back to pending — including work other live replicas are running |
| **New** | `<prefix>:processing:<instance>` | Recovers only its own key; if the shared key is non-empty, registers it in `<prefix>:instances` as a pseudo-instance named `legacy`, dated now |

That `legacy` entry is the adoption mechanism: it lets the ordinary reaper
age the old key out and return its payloads to pending, instead of leaving
them somewhere nothing looks. It is deliberately dated *now* rather than
reclaimed on the spot, because during a rolling upgrade an old replica may
still be actively working those jobs.

## Three timing facts

**Adoption happens at `Start`, and only at `Start`.** A new replica that
boots while the shared key is empty registers nothing. If an old replica
then claims work into the shared key and dies, no `legacy` entry exists to
age out, and those payloads wait until some new replica restarts and
notices them. Ordering your rollout so the old replicas stop first avoids
the whole question.

**The `legacy` entry is never refreshed.** Roughly 60 seconds after it is
registered it goes stale and becomes reapable. If old replicas are still
executing those jobs at that point, the reaper returns them to pending and
they are handled a second time. That is at-least-once working as designed —
audit records dedupe per `job_id`, but result sinks do not across a process
boundary
([result envelope](result-envelope.md#idempotency-and-where-it-stops)).

**Rolling back mid-flight leaves work an old binary cannot see.** Payloads
claimed by new replicas live in `<prefix>:processing:<instance>` keys the
old code has no concept of. Nothing reclaims them until you roll forward
again.

## The path that depends on none of the above

If you can take the downtime, drain instead of overlapping:

1. Stop intake.
2. Scale the old deployment to zero.
3. Wait past `queue.drain_timeout`.
4. Confirm `LLEN <prefix>:processing` is `0`.
5. Start the new replicas.

This is the only sequence where no old and new replica ever touch Redis at
the same time, and it needs no adoption path at all.

## Watching it clear

`vismod_processing_depth` counts the legacy key as well as the per-replica
ones, so it is the single number to watch during the transition. It should
fall to zero and stay there. See
[Reading the two depth gauges together](scaling.md#reading-the-two-depth-gauges-together).

## Confidence

The adoption path above describes **what the code attempts.** It has been
exercised against miniredis only — never two real processes against a real
Redis, never a real rolling upgrade — and the 60-second reclaim window is
an unmeasured guess. The drain procedure is the part that does not depend
on any of that. See
[UNVERIFIED.md](agent/UNVERIFIED.md).

## Retirement

Delete this page, its row in [the docs index](index.md), and the pointers
to it in [scaling.md](scaling.md) and `deploy/README.md` once no supported
release predates the change — the same moment the `legacyInstance` constant
leaves `internal/queue/redisq.go`. The doc and the constant retire
together.
