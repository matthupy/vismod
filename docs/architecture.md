---
title: Architecture
nav_order: 2
---

# How vismod works

```
            video   ┌───────────────┐
 job ──────────────▶│ FFmpegSource  │─ frames ─┐
      │             │ (workflows)   │          │
      │             └───────────────┘          ▼
      │ image                        ┌───────────────────┐
      └─────────────────────────────▶│ Moderator adapter │
                                     │ (exactly 1 active)│
                                     └─────────┬─────────┘
                                               ▼
                          normalize → thresholds → rollup
                                               ▼
                                  Sink → audit → ack/DLQ
```

A job carries one asset. Images go straight to the adapter; video is
frame-extracted first and every frame is moderated as an image. Whatever
comes back is normalized into one schema, compared against per-category
thresholds, rolled up into a single asset verdict, written to the
configured sinks, recorded in the audit log, and only then acked.

## Package map

| Package | Responsibility |
|---|---|
| `cmd/vismod/` | Thin `main`, no logic |
| `pkg/moderation/` | Public contract types (`Moderator`, `NormalizedResult`, `Verdict`) |
| `internal/cli/` | Cobra composition root — the only place adapters are wired |
| `internal/config/` | Viper loader, thresholds, workflows, `ConfigHash` |
| `internal/moderate/` | Adapter registry, rate limiter, retrying HTTP; `adapters/*` |
| `internal/frames/` | ffmpeg extraction, workflow guardrails, dHash dedup |
| `internal/queue/` | `memq` (dev) and `redisq` (durable, at-least-once, per-replica claims) |
| `internal/pipeline/` | frames → dedup → fan-out → thresholds → rollup → sink |
| `internal/result/` | Result envelope + `Sink` implementations |
| `internal/audit/` | Append-only hash-chained decision log |
| `internal/observe/` | slog, Prometheus metrics, backpressure |
| `internal/ui/` | Embedded read-mostly operator dashboard (off by default) |

## One model per process

The active model is selected by `adapter.name` at startup and cannot
change without a restart. An unknown name fails fast and lists the
registered adapters rather than falling back to anything.

This is deliberate. A process that could silently switch models would
make its own audit log ambiguous — the same `config_hash` has to mean the
same decision function for the entire life of the process. Running two
models means running two deployments.

Adding a vendor is one adapter package plus golden tests, with zero
pipeline changes; the registry never imports adapters, so the single
wiring point is a blank import in `internal/cli/root.go`. See
[CONTRIBUTING.md](https://github.com/matthupy/vismod/blob/main/CONTRIBUTING.md).

## Video → frames

Video is frame-extracted by invoking `ffmpeg`/`ffprobe` directly, driven
by named, guardrailed workflows in config
([custom ffmpeg workflows](custom-ffmpeg-workflows.md)). A job may select
one or more workflows (`scan --workflow …`, or `"workflows":[…]` on the
intake API); the extracted frame set is their **union**, capped by
`max_frames`.

An optional `frames.dedup` stage drops near-duplicate frames by dHash
Hamming distance before they spend moderation calls — worth a lot on
static-camera or slideshow content, worth little on fast cuts. The hash
is computed from a bounded sample per grid cell rather than every pixel,
so cost does not grow with frame size; for frames above roughly 72x64 a
hash bit can differ from a full scan, and no before/after comparison over
real footage has been run
([UNVERIFIED.md](agent/UNVERIFIED.md)).

An adapter whose vendor scores video natively can implement
`VideoModerator` and skip extraction entirely.

## Normalization

Every provider signal becomes a `CategoryResult` carrying:

- a **canonical category** from vismod's taxonomy;
- the raw **`provider_label`**, preserved verbatim;
- a **normalized score** in `[0,1]` — or **`null`** for
  could-not-evaluate, never `0`;
- a **`score_origin`** naming what the number actually was upstream
  (`severity`, `likelihood_enum`, `probability`).

Two rules do the heavy lifting. Unmapped provider labels land in `OTHER`
with their label intact, so nothing a provider said is ever silently
dropped. And "no score" is `null`, never `0` — because `0` means
"confidently safe" and would otherwise be indistinguishable from "the
provider didn't answer."

`score_origin` exists because the scores are not comparable. See
[MODEL_LIMITATIONS.md](https://github.com/matthupy/vismod/blob/main/MODEL_LIMITATIONS.md).

## Thresholds and rollup

Per-category `flag_at` / `block_at` thresholds turn scores into
per-category flags, then the asset verdict rolls up with strict
precedence:

```
block > error > flag > allow
```

`error` outranks `flag` and `allow` on purpose. Any of the following
yields `error` and human review:

- any frame that errored;
- zero frames extracted;
- an all-`null` score set.

There is no configuration that makes an unscorable asset `allow`.

## Related

- [Supported models](models.md)
- [Result envelope](result-envelope.md)
- [Audit log](audit-log.md)
- [Scaling and observability](scaling.md)
