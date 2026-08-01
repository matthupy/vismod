# vismod

**Open-source visual content moderation pipeline.** Scans images and
video for harmful content using a pluggable visual-moderation model
(Azure AI Content Safety, Google Cloud Vision SafeSearch, or thehive.ai),
normalizes their wildly different outputs into **one common scoring
schema**, and runs as a one-shot CLI or a long-running containerized
worker that scales horizontally by queue depth.

vismod is a public good with no commercial goals, built for trust &
safety organizations and smaller platforms without in-house moderation
infrastructure. It is designed **fail-safe first**: a provider outage, a
broken video, or an unscorable frame yields `verdict: "error"` and human
review — never a silent `allow`.

> Read [RESPONSIBLE_USE.md](RESPONSIBLE_USE.md) before deploying. All
> content detection — including any special-category detection and
> protections — is performed by the configured scanning vendor under
> that vendor's terms. This project's docs are not legal advice.

---

## Quick start

```sh
go build -o vismod ./cmd/vismod

# configure a provider (secrets are env-only, never yaml)
export VISMOD_MICROSOFT_API_KEY=<key>
cp config.example.yaml config.yaml   # set adapter.options.endpoint

# one-shot scan (exit code: 0 allow, 1 flag/block, 2 error)
./vismod -c config.yaml scan photo.jpg clip.mp4

# long-running worker (metrics on :9090, dev intake on 127.0.0.1:8080)
./vismod -c config.yaml serve
```

Docker (one image, both modes — bundles ffmpeg/ffprobe, non-root):

```sh
docker build -t vismod .

# A config file is REQUIRED — mount it. There is no usable env-only
# configuration: the VISMOD_* overlay only overrides keys the yaml
# already sets, so with no file the adapter name is empty and boot
# fails with `unknown adapter ""`.
docker run -e VISMOD_MICROSOFT_API_KEY -v "$PWD:/data" \
  vismod scan -c /data/config.yaml /data/clip.mp4

docker run -e VISMOD_MICROSOFT_API_KEY -p 9090:9090 -v "$PWD:/data" \
  vismod serve -c /data/config.yaml   # :9090 = /metrics /healthz /readyz
```

`intake_addr` defaults to `127.0.0.1:8080`, which inside a container is
reachable only from within it; publishing that port does nothing until
you set the address to `0.0.0.0:8080`. The dev intake has no auth — read
[SECURITY.md](SECURITY.md) before exposing it.

Compose (two replicas, durable Redis queue, Prometheus + Grafana):

```sh
cp deploy/compose/env.example .env                                        # set your API key
cp deploy/compose/config.compose.example.yaml deploy/compose/config.compose.yaml   # set your endpoint
docker compose up --build     # intake :8080, UI :8081, Prometheus :9090, Grafana :3000
```

See [deploy/compose/README.md](deploy/compose/README.md), and
[deploy/README.md](deploy/README.md) for Kubernetes.

Other commands: `adapters` (registry + capabilities), `workflows
list|validate`, `audit verify`, `version`, `healthcheck`.

## How it works

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

- **One model active per process**, selected by `adapter.name` at
  startup; restart to change. Unknown names fail fast listing the
  registered adapters. Adding a vendor = one adapter package + golden
  tests, zero pipeline changes ([CONTRIBUTING.md](CONTRIBUTING.md)).
- **Registered adapters:** `microsoft` (Azure AI Content Safety),
  `google` (Cloud Vision SafeSearch), `hive` (thehive.ai), and
  `shieldgemma` — **self-hosted**: you run an inference server serving
  `google/shieldgemma-2-4b-it` and vismod speaks HTTP to it, with no
  per-call billing and no media leaving your network. It requires
  `provider_thresholds.mode: override` and an explicit `model_version`,
  and it has never been run against a real server
  ([docs/agent/UNVERIFIED.md](docs/agent/UNVERIFIED.md)).
- **Video** is frame-extracted via direct `ffmpeg`/`ffprobe`
  (config-driven, guardrailed workflows —
  [docs/custom-ffmpeg-workflows.md](docs/custom-ffmpeg-workflows.md)),
  then each frame is moderated as an image. Jobs may select one or more
  workflows (`scan --workflow …` / `"workflows":[…]` on the intake);
  frames are the union, capped by `max_frames`. An optional
  `frames.dedup` stage drops near-duplicate frames (dHash Hamming
  distance) before they spend moderation calls. Video-native adapters
  can implement `VideoModerator` and skip extraction.
- **Normalization**: every provider signal becomes a `CategoryResult`
  with a canonical category, the raw `provider_label`, a normalized
  score in [0,1] (or **null** for could-not-evaluate — never 0), and its
  `score_origin`. Unmapped labels land in `OTHER`; nothing is dropped.
- **Verdicts** roll up per asset with strict precedence
  `block > error > flag > allow`; any errored frame, zero frames, or an
  all-null score set is `error` — never `allow`.

### Example envelope (one JSON line per job)

```json
{"job_id":"scan-...","source":{"kind":"file","ref":"/data/clip.mp4","media_type":"video"},
 "model_id":{"adapter":"microsoft","model_version":"2024-09-01","config_hash":"9b6f…"},
 "result":{"schema_version":"1.1.0","provider":"microsoft","media_type":"video",
   "asset_id":"/data/clip.mp4",
   "frames":[{"timestamp_sec":2.0,"status":"ok","categories":[
     {"category":"SEXUAL","provider_label":"Sexual","score":0.333,
      "score_origin":"severity","threshold":0.4,"flagged":false}]}],
   "overall":{"verdict":"allow","flagged":false,"top_category":"SEXUAL",
     "max_score":0.333,"confidence":0.333}},
 "started_at":"…","finished_at":"…"}
```

## Things you must know before production

- **Scores are not portable across providers.** `severity/6`, a
  likelihood bucket, and a head probability are different quantities;
  thresholds are per-adapter and must be retuned on switch. Details:
  [MODEL_LIMITATIONS.md](MODEL_LIMITATIONS.md).
- **FIFO ≠ completion order.** Dequeue order equals enqueue order, but
  with `queue.workers > 1` completion order is not guaranteed. Strict
  end-to-end ordering needs `workers: 1` (or per-key serialization
  upstream).
- **The memory queue driver is not for production intake.** It is
  single-process, non-durable, at-most-once: a crash loses queued and
  in-flight jobs. `serve` warns at boot and in `/readyz`. Production and
  any multi-replica deployment require `queue.driver: redis` (durable,
  at-least-once, crash-recovering).
- **At-least-once means redelivery happens, and sink idempotency is
  per-process.** Every result sink is idempotent per `job_id` within a
  process lifetime, so a redelivery to a running worker never
  double-writes. That guarantee does NOT survive a restart: the dedupe
  set is in memory, so a job redelivered after a crash or a rolling
  restart gets a second line in the `file` sink (and a second POST to a
  `webhook` receiver). The audit log is the exception — `audit.Open`
  replays its file and rebuilds the seen-set before appending, so its
  per-`job_id` guarantee holds across restarts. Downstream consumers of
  the `file` and `webhook` sinks must dedupe on `job_id` themselves.
- **Result envelope destinations are configurable.** `output.sinks`
  (`config.example.yaml`) fans each envelope out to any combination of
  stdout, an append-only JSONL file, and an HTTP webhook. Every sink is
  attempted and the first failure is what triggers redelivery — a
  webhook outage never suppresses the local record. Omit the `output`
  block for the stdout-only default every earlier release used. A `file`
  sink needs one path per replica ([deploy/README.md](deploy/README.md)).
- **Custom FFmpeg workflows are an operator-trust boundary.** They are
  validated hard (no shell, single bound input, protocol deny-list,
  output confinement — [SECURITY.md](SECURITY.md)), but review workflow
  changes like code.
- **Rate limits**: the token bucket is shared across all workers and
  frame fan-out in one process, but replicas multiply it — budget
  `global_quota / max_replicas` per replica or add a shared limiter
  ([deploy/README.md](deploy/README.md)).

## Scaling out

Each replica runs a fixed pool of `queue.workers` goroutines; horizontal
scale is replica-count driven by the **`vismod_queue_depth`** gauge.
KEDA `ScaledObject` and HPA examples, the drain-on-deploy contract, and
the rate-limit budgeting note live in [deploy/](deploy/README.md).

Observability: Prometheus `/metrics` (`vismod_jobs_total{verdict}`,
`vismod_adapter_request_seconds`, `vismod_adapter_errors_total`,
`vismod_queue_depth`, `vismod_deadletter_depth`,
`vismod_workers_active`), `/healthz` liveness, `/readyz` readiness —
which also flips not-ready under sustained provider failure
(backpressure with hysteresis) so ingress backs off instead of
black-holing jobs.

## Audit log

Every decision appends to a hash-chained, append-only audit log binding
the verdict to its inputs **by hash** (`SHA-256(Raw)` + model identity +
config hash — never media or provider payloads). `vismod audit verify`
recomputes the chain and reports the first broken link. Scope honesty:
tamper-evident, not tamper-proof — see [SECURITY.md](SECURITY.md).

## Development

```sh
go test ./...     # no network, no credentials: fakes, httptest, miniredis
go vet ./...
```

Golden files regenerate with `go test -update ./internal/moderate/...`.
FFmpeg integration tests skip automatically when ffmpeg is absent.

## Docs

[SECURITY.md](SECURITY.md) ·
[RESPONSIBLE_USE.md](RESPONSIBLE_USE.md) ·
[AGENTS.md](AGENTS.md) ·
[MODEL_LIMITATIONS.md](MODEL_LIMITATIONS.md) ·
[CONTRIBUTING.md](CONTRIBUTING.md) ·
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) ·
[config.example.yaml](config.example.yaml) ·
[docs/custom-ffmpeg-workflows.md](docs/custom-ffmpeg-workflows.md) ·
[deploy/README.md](deploy/README.md)

## License

Apache-2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
