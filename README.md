# vismod — open-source visual content moderation pipeline

Scans **images and video** for harmful content using a pluggable visual-moderation
model, normalizes wildly different provider outputs into one schema, and runs as a
one-shot CLI (`vismod scan`) and a long-running worker (`vismod serve`).

A public good for trust & safety. Positioned as the **Classification** stage in the
Hash Matching → Classification → Review → Investigation taxonomy (cf. ROOST), feeding
a review console / rules engine downstream.

> **Status: M3 (Docker + observability).** The full pipeline runs end-to-end with
> the credential-free `stub` adapter or Azure (M1); video inputs are framed by the
> real `videosift` extractor (M2); and the worker ships a Docker image, boot
> validation, and Prometheus metrics + `/healthz`/`/readyz` (M3). Responsible-use
> docs + audit wiring (M4) and Redis/scale (M5) follow. Responsible-use, security
> and licensing docs land in M4 — **do not deploy against real-world content yet.**

## Quick start

```bash
go build -o vismod ./cmd/vismod

./vismod adapters                 # list registered models + capabilities
./vismod scan path/to/image.jpg   # one-shot, prints a NormalizedResult envelope (JSONL)
printf 'a.jpg\nb.mp4\n' | ./vismod serve --config config.example.yaml   # worker; drains on SIGTERM
./vismod audit verify audit.log   # recompute the tamper-evident decision chain
```

Configure via `config.example.yaml`. **Secrets are env-only** (`VISMOD_` prefix), never yaml.

## Docker (M3)

One image, both modes. The runtime stage bundles `ffmpeg`+`ffprobe` (videosift execs
them — a static binary alone is insufficient), runs **non-root**, and drains on
`SIGTERM`.

> 🔴 **Build context is the PARENT directory**, not this repo. `go.mod` has
> `replace … => ../videosift`, so the sibling checkout must be in the context (the
> same layout CI uses). Lay the repos out as siblings:

```bash
parent/
  vismod/      # this repo
  videosift/   # git clone https://github.com/matthupy/videosift

docker build -f vismod/Dockerfile -t vismod:dev parent/

# serve (default): metrics/health on :9090, frames workdir is an ephemeral volume
docker run --rm -p 9090:9090 vismod:dev

# one-shot scan
docker run --rm -v "$PWD/data:/data" vismod:dev scan /data/clip.mp4
```

> **Config in-container:** the image bakes no config file. Override via the
> `VISMOD_` env overlay, or mount a file and point at it with `VISMOD_CONFIG`
> (e.g. `-v "$PWD/config.yaml:/etc/vismod.yaml" -e VISMOD_CONFIG=/etc/vismod.yaml`).
> The `HEALTHCHECK` runs `vismod healthcheck` with no `--config` flag, so a config
> that moves `metrics.addr` must be reachable via `VISMOD_CONFIG` (or `VISMOD_METRICS_ADDR`)
> for the probe to target the right port.

## Observability (M3)

`serve` exposes one HTTP server on `metrics.addr` (default `:9090`):

| Endpoint | Purpose |
|---|---|
| `/healthz` | Liveness — always 200 while the process is up. |
| `/readyz`  | Readiness — JSON `{ready, adapter_name, checks, warnings}`; 503 until boot validation passes. `checks` are health verdicts only (e.g. `"adapter":"ok"`); the active adapter's identity is the separate `adapter_name`. Carries the memq non-durability warning. |
| `/metrics` | Prometheus text exposition. |

Metrics: `vismod_jobs_total{verdict}`, `vismod_adapter_request_seconds{adapter}`,
`vismod_adapter_errors_total{adapter,code}`, `vismod_queue_depth`,
`vismod_deadletter_depth`. Adapter latency/errors are recorded by an instrumenting
decorator that wraps the active `Moderator`, so the pipeline stays adapter-agnostic.
The container `HEALTHCHECK` uses the self-contained `vismod healthcheck` subcommand
(no curl/wget in the image).

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
