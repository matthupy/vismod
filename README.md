# vismod — open-source visual content moderation pipeline

Scans **images and video** for harmful content using a pluggable visual-moderation
model, normalizes wildly different provider outputs into one schema, and runs as a
one-shot CLI (`vismod scan`) and a long-running worker (`vismod serve`).

A public good for trust & safety. Positioned as the **Classification** stage in the
Hash Matching → Classification → Review → Investigation taxonomy (cf. ROOST), feeding
a review console / rules engine downstream.

> **Status: M2 (video framing).** The full pipeline runs end-to-end with the
> credential-free `stub` adapter or Azure (M1), and video inputs are framed by the
> real `videosift` extractor (M2). Docker + metrics (M3), responsible-use docs +
> audit wiring (M4), and Redis/scale (M5) follow. Responsible-use, security and
> licensing docs land in M4 — **do not deploy against real-world content yet.**

## Quick start

```bash
go build -o vismod ./cmd/vismod

./vismod adapters                 # list registered models + capabilities
./vismod scan path/to/image.jpg   # one-shot, prints a NormalizedResult envelope (JSONL)
printf 'a.jpg\nb.mp4\n' | ./vismod serve --config config.example.yaml   # worker; drains on SIGTERM
./vismod audit verify audit.log   # recompute the tamper-evident decision chain
```

Configure via `config.example.yaml`. **Secrets are env-only** (`VISMOD_` prefix), never yaml.

## Design notes (carried into later milestones)

- **One model per process**, chosen at startup; restart-to-change. The registry never
  imports adapter packages — adapters self-register and are blank-imported at the
  composition root.
- **Fail-safe, never fail-silent.** Any provider/frame/extraction failure yields
  `Verdict=error` (never `allow`) and dead-letters. A worker panic dead-letters that
  job and the pool keeps running.
- **FIFO is a queue property:** dequeue order == enqueue order. With >1 worker,
  **completion order is not guaranteed** — strict ordering needs `workers=1`.
- **memq is non-durable, single-process (dev/CLI only).** A crash loses jobs.
  Production intake will require `driver=redis` (M5).
- **Scores are within-provider comparable only.** A `0.667` threshold means different
  things per provider; thresholds are per-adapter and **not portable**.
- **CSAM** is handled by a hash-match pre-stage seam (no-op in v1; PDQ/TMK in v1.1),
  never the classifier. The schema ships the `CSAM_HASH_MATCH` category + `match_*`
  fields now.

## Internal dependency

Video framing uses `github.com/matthupy/videosift` (MIT) — **tracked at latest, never
pinned** (co-developed). A local `replace => ../videosift` is active for tandem dev.
videosift execs external `ffmpeg`+`ffprobe`; the runtime must provide both.
