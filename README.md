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

## Why this exists

Trust & safety teams all hit the same wall: more images and video coming
in than anyone can look at by hand. The classifiers to triage that are
the easy part — a handful of vendors sell one, and they work well enough.
Everything around them is the work, and it's the same work every time:
auth for one more vendor API, retries and rate limits, a way to turn a
video into something an image classifier will accept, a schema your
reviewers and your incident log can both live with. vismod is that glue,
written once and in the open, so getting to a decision on a piece of
media doesn't start with building a pipeline.

Video is what makes that expensive. A ten-minute clip sampled at one
frame per second is six hundred API calls, and most of them are the same
shot from slightly different moments. vismod extracts frames on a
workflow you control — scene changes, fixed intervals, whatever suits the
content — then drops perceptual near-duplicates before anything is sent
to a model. You pay for the frames that actually differ, which on real
footage is a fraction of what you'd otherwise send.

Normalization started as a convenience and turned into the more useful
half. Because every vendor's output lands in the same shape, changing
which model you run is a config edit rather than a rewrite — you can put
the same media through two of them and compare what comes back. Scores
still aren't portable across vendors and thresholds have to be retuned
when you switch, but the plumbing stops being the reason you never
checked.

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
endpoint) and vismod speaks HTTP to it — no per-call billing, no media
leaving your network. It has never been run against a real server.

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

## Submitting jobs over HTTP

`serve` exposes one endpoint. It takes a job per request, returns `202` +
`{"job_id":…}`, and puts the verdict in your configured sinks — never in
the HTTP response. It binds `127.0.0.1` by default and has **no
authentication** — read [SECURITY.md](SECURITY.md) before moving it off
localhost.

```sh
# a local file
curl -X POST localhost:8080/jobs -H 'content-type: application/json' \
  -d '{"kind":"file","ref":"/data/clip.mp4"}'

# a remote asset (works out of the box)
curl -X POST localhost:8080/jobs -H 'content-type: application/json' \
  -d '{"kind":"url","ref":"https://media.example.com/clip.mp4"}'
```

A `kind:"url"` job is fetched to a job-scoped temp file, scanned, and
deleted before ack. It needs no configuration to work, but
`source.url.allow_hosts` is what you set in production to narrow the
destinations a job can name — private, loopback and metadata addresses
are denied at connect time either way. The request body, the SSRF rules,
and worked curl/PowerShell examples are in
**[docs/rest-api.md](docs/rest-api.md)**.

## Running it in a container

One image, both modes, `ffmpeg` bundled, non-root:

```sh
docker build -t vismod .
docker run -e VISMOD_MICROSOFT_API_KEY -v "$PWD:/data" \
  vismod scan -c /data/config.yaml /data/clip.mp4
```

A mounted config file is **required** — there is no usable env-only
configuration, and a container without one fails fast with `unknown
adapter ""`. That and the `intake_addr` publishing gotcha are in
**[docs/docker.md](docs/docker.md)**.

To see it behave like a real deployment, `docker compose up --build`
brings up two workers on a durable Redis queue with Prometheus and
Grafana already wired to the metrics — setup, what the stack proves, and
what it deliberately doesn't, in
**[deploy/compose/README.md](deploy/compose/README.md)**. For Kubernetes,
KEDA and HPA: **[deploy/README.md](deploy/README.md)**.

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
| [REST intake](docs/rest-api.md) | `POST /jobs`, scanning from a URL, curl/PowerShell examples |
| [Running in Docker](docs/docker.md) | Building the image, the required config mount, publishing the intake |
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

Golden files, the adapter extension point, and the rest of the
contributor rules: [CONTRIBUTING.md](CONTRIBUTING.md).

## License

**GPL-3.0-or-later** — see [LICENSE](LICENSE) and [NOTICE](NOTICE).

vismod is copyleft on purpose. Fork it, modify it, deploy it; if you
distribute your changes, they stay open source too. Moderation tooling
that decides what people can post should be auditable by the people it
decides about.

Contributions are inbound=outbound with no CLA
([CONTRIBUTING.md](CONTRIBUTING.md#licensing-of-contributions)).
