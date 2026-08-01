# vismod

[![ci](https://github.com/matthupy/vismod/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/matthupy/vismod/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/matthupy/vismod/branch/main/graph/badge.svg)](https://codecov.io/gh/matthupy/vismod)
[![Go Reference](https://pkg.go.dev/badge/github.com/vismod/vismod.svg)](https://pkg.go.dev/github.com/vismod/vismod)
[![Go 1.26](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: GPL v3](https://img.shields.io/badge/license-GPL--3.0--or--later-blue.svg)](LICENSE)

**Open-source visual content moderation pipeline.** Scans images and
video for harmful content using a pluggable visual-moderation model,
normalizes wildly different vendor outputs into **one common scoring
schema**, and runs as a one-shot CLI or a long-running containerized
worker that scales horizontally by queue depth.

vismod is a public good with no commercial goals, built for trust &
safety organizations and smaller platforms without in-house moderation
infrastructure. It is designed **fail-safe first**: a provider outage, a
broken video, or an unscorable frame yields `verdict: "error"` and human
review — never a silent `allow`.

> Read [RESPONSIBLE_USE.md](RESPONSIBLE_USE.md) before deploying. All
> content detection — including any special-category detection and
> protections — is performed by the configured scanning model under that
> provider's terms. This project's docs are not legal advice.

📖 **[Full documentation](https://matthupy.github.io/vismod/)**

---

## Quick start

```sh
go build -o vismod ./cmd/vismod

# configure a model (secrets are env-only, never yaml)
export VISMOD_MICROSOFT_API_KEY=<key>
cp config.example.yaml config.yaml   # set adapter.options.endpoint

# one-shot scan (exit code: 0 allow, 1 flag/block, 2 error)
./vismod -c config.yaml scan photo.jpg clip.mp4

# long-running worker (metrics on :9090, dev intake on 127.0.0.1:8080)
./vismod -c config.yaml serve
```

Other commands: `adapters` (registry + capabilities), `workflows
list|validate`, `audit verify`, `version`, `healthcheck`.

## Supported scanning models

Exactly **one** model is active per process, selected by `adapter.name`
at startup; restart to change it. An unknown name fails fast and lists
the registered adapters — it never falls back.

| `adapter.name` | Model | Hosting | Categories | Score origin | Credential |
|---|---|---|---|---|---|
| `microsoft` | Azure AI Content Safety | Vendor cloud | `HATE` `SEXUAL` `VIOLENCE` `SELF_HARM` | `severity` (`severity/6`) | `VISMOD_MICROSOFT_API_KEY` or `VISMOD_MICROSOFT_ACCESS_TOKEN` |
| `google` | Cloud Vision SafeSearch | Vendor cloud | `SEXUAL` `SUGGESTIVE_RACY` `VIOLENCE` `MEDICAL` `SPOOF` | `likelihood_enum` | ADC / `GOOGLE_APPLICATION_CREDENTIALS` |
| `hive` | Hive (thehive.ai) visual moderation | Vendor cloud | 14 categories incl. `GORE_GRAPHIC` `WEAPONS` `DRUGS` `GAMBLING` | `probability` | `VISMOD_HIVE_API_TOKEN` |
| `shieldgemma` | `google/shieldgemma-2-4b-it` | **Self-hosted by you** | `SEXUAL` `GORE_GRAPHIC` `OTHER` | `probability` | none (endpoint only) |

**`shieldgemma` is the no-vendor option**: you run an inference server
(vLLM, TGI, or anything with an OpenAI-compatible chat-completions
endpoint), and vismod speaks HTTP to it. No per-call billing, no media
leaving your network. It requires `provider_thresholds.mode: override`
and an explicit `model_version`, and it has never been run against a real
server.

All four are image-scoring, so video is frame-extracted by vismod and
each frame scored as an image.

> ⚠️ **Scores are not portable between these models.** `severity/6`, a
> likelihood bucket, and a head probability are different quantities that
> happen to share the `[0,1]` range. Thresholds are per-adapter and must
> be retuned when you switch.

Per-model detail — limits, class maps, auth modes, and an honest
verification status for each — is in
**[docs/models.md](docs/models.md)**. Adding a model is one adapter
package plus golden tests, with zero pipeline changes
([CONTRIBUTING.md](CONTRIBUTING.md)).

## Docker

One image, both modes. Bundles `ffmpeg`/`ffprobe`, runs non-root.

```sh
docker build -t vismod .
```

A config file is **required** — mount it. There is no usable env-only
configuration: the `VISMOD_*` overlay only overrides keys the yaml
already sets, so with no file the adapter name is empty and boot fails
with `unknown adapter ""`.

```sh
# one-shot scan
docker run -e VISMOD_MICROSOFT_API_KEY -v "$PWD:/data" \
  vismod scan -c /data/config.yaml /data/clip.mp4

# worker — :9090 serves /metrics /healthz /readyz
docker run -e VISMOD_MICROSOFT_API_KEY -p 9090:9090 -v "$PWD:/data" \
  vismod serve -c /data/config.yaml
```

`intake_addr` defaults to `127.0.0.1:8080`, which inside a container is
reachable only from within it; publishing that port does nothing until
you set the address to `0.0.0.0:8080`. The dev intake has **no auth** —
read [SECURITY.md](SECURITY.md) before exposing it.

## Docker Compose

The compose stack is the fastest way to see vismod behave like a real
deployment: two worker replicas against a durable Redis queue, with
Prometheus and Grafana already wired to the metrics.

```sh
# 1. credentials (env-only, never yaml)
cp deploy/compose/env.example .env
$EDITOR .env                      # set VISMOD_MICROSOFT_API_KEY

# 2. config (mounted into both replicas)
cp deploy/compose/config.compose.example.yaml deploy/compose/config.compose.yaml
$EDITOR deploy/compose/config.compose.yaml    # set adapter.options.endpoint

# 3. up
docker compose up --build
```

| Service | Published port | What it is |
|---|---|---|
| `vismod-a` | `:8080` intake, `:8081` operator UI | Worker + the only replica publishing ports |
| `vismod-b` | none | Second worker on the same queue; Prometheus reaches it over the compose network |
| `prometheus` | `:9090` | Scrapes both replicas. Host `:9090` is Prometheus — the replicas' own metrics port stays internal |
| `grafana` | `:3000` | Preprovisioned vismod dashboard, anonymous view-only |
| `redis` | none | Durable at-least-once queue; data survives `down`/`up` |

Drop a file in `./media/` (mounted read-only at `/data`) and submit it:

```sh
curl -X POST localhost:8080/jobs \
  -H 'content-type: application/json' \
  -d '{"kind":"file","ref":"/data/clip.mp4"}'
```

`media_type` is inferred from the extension; `workflows` is optional.
Then watch `vismod_queue_depth` in Grafana drain as the two replicas
claim work — the queue is the coordination point, so replicas need no
knowledge of each other.

⚠️ **Do not `docker compose up --scale vismod-b=N`.** It succeeds
silently, but every scaled replica shares the one `audit-b` volume and
corrupts the audit hash chain. Each replica needs its own audit volume —
add a `vismod-c` service with an `audit-c` volume instead.

What this stack exercises, and what it deliberately does not, is written
up in **[deploy/compose/README.md](deploy/compose/README.md)** — including
`compose.prod.example.yaml` for a production-shaped variant. For
Kubernetes, KEDA, and HPA: **[deploy/README.md](deploy/README.md)**.

Without a real vendor credential every job correctly ends
`verdict:"error"` — that is the fail-safe design working, not a broken
stack.

## Documentation

Technical detail lives in [docs/](docs/), published at
**[matthupy.github.io/vismod](https://matthupy.github.io/vismod/)**.

| Page | What's in it |
|---|---|
| [Architecture](docs/architecture.md) | Pipeline shape, package map, normalization, verdict rollup |
| [Supported models](docs/models.md) | Per-adapter detail and verification status |
| [Result envelope](docs/result-envelope.md) | Output schema, sinks, idempotency boundaries |
| [Scaling and observability](docs/scaling.md) | Queue drivers, replica scaling, metrics, backpressure |
| [Audit log](docs/audit-log.md) | Hash chain, verification, tamper-evident scope |
| [Production checklist](docs/production-checklist.md) | The list to walk before going live |
| [Custom ffmpeg workflows](docs/custom-ffmpeg-workflows.md) | Writing and validating extraction workflows |
| [Self-hosted classifiers](docs/self-hosted-classifiers.md) | Which open-weight model to run, and why |

Policy and posture: [SECURITY.md](SECURITY.md) ·
[RESPONSIBLE_USE.md](RESPONSIBLE_USE.md) ·
[MODEL_LIMITATIONS.md](MODEL_LIMITATIONS.md) ·
[CONTRIBUTING.md](CONTRIBUTING.md) ·
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) ·
[config.example.yaml](config.example.yaml)

Working with a coding agent? [AGENTS.md](AGENTS.md).

## Development

```sh
go build ./...
go vet ./...
go test ./...     # no network, no credentials: fakes, httptest, miniredis
```

Golden files regenerate with `go test -update ./internal/moderate/...`.
FFmpeg integration tests skip automatically when ffmpeg is absent.

## License

**GPL-3.0-or-later** — see [LICENSE](LICENSE) and [NOTICE](NOTICE).

vismod is copyleft on purpose. Fork it, modify it, deploy it; if you
distribute your changes, they stay open source too. Moderation tooling
that decides what people can post should be auditable by the people it
decides about.

Contributions are inbound=outbound with no CLA
([CONTRIBUTING.md](CONTRIBUTING.md#licensing-of-contributions)).
