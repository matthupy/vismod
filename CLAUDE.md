# CLAUDE.md — start here

vismod is an open-source visual content moderation pipeline in Go. It
scans images and video frames through ONE configured vendor classifier
(microsoft | google | hive | shieldgemma, the last of which is a
self-hosted endpoint you run), normalizes their different outputs into a
common schema, and runs either as a one-shot CLI (`scan`) or a
long-running worker (`serve`).

It is public-good trust & safety tooling. The governing bias in every
design decision: **fail safe and stay auditable, even at the cost of
convenience.** A provider outage, a broken video, or an unscorable frame
yields `verdict: "error"` and human review — never a silent `allow`.

## Shape of the code

```
cmd/vismod/       thin main
pkg/moderation/   public contract types (Moderator, NormalizedResult, Verdict)
internal/cli/     cobra composition root; the only place adapters are wired
internal/config/  viper loader, thresholds, workflows, ConfigHash
internal/moderate/  adapter registry, rate limiter, retrying HTTP; adapters/*
internal/frames/  ffmpeg frame extraction, workflow guardrails, dHash dedup
internal/fetch/   allow-listed https media download for kind:"url" sources
internal/queue/   memq (dev) and redisq (durable, at-least-once)
internal/pipeline/  frames -> dedup -> fan-out -> thresholds -> rollup -> sink
internal/result/  result envelope + Sink implementations (JSONL, file, webhook, multi)
internal/audit/   append-only hash-chained decision log
internal/observe/ slog, Prometheus metrics, backpressure
internal/ui/      embedded read-mostly operator dashboard (off by default)
```

Build and test with `go build ./...`, `go vet ./...`, `go test ./...`.
The full suite runs with no network and no credentials.

## Where to go next

**Changing code? Read [AGENTS.md](AGENTS.md) first** — invariants that
must not be weakened, the loop protocol, the done gate, the adapter
extension point, and the gotchas that have already bitten someone.

| Question | Doc |
|---|---|
| How do I work in this repo safely? | [AGENTS.md](AGENTS.md) |
| What is in flight right now? | [docs/agent/STATUS.md](docs/agent/STATUS.md) |
| What should I pick up next? | [docs/agent/TASKS.md](docs/agent/TASKS.md) |
| What is claimed but unproven? | [docs/agent/UNVERIFIED.md](docs/agent/UNVERIFIED.md) |
| What does this project do, for users? | [README.md](README.md) |
| Trust boundaries, SSRF posture, audit scope | [SECURITY.md](SECURITY.md) |
| Deployment ethics, human-in-the-loop | [RESPONSIBLE_USE.md](RESPONSIBLE_USE.md) |
| Why scores are not portable across vendors | [MODEL_LIMITATIONS.md](MODEL_LIMITATIONS.md) |
| Which open-weight model to self-host, and why | [docs/self-hosted-classifiers.md](docs/self-hosted-classifiers.md) |
| Human contributor rules | [CONTRIBUTING.md](CONTRIBUTING.md) |
| Custom ffmpeg workflows | [docs/custom-ffmpeg-workflows.md](docs/custom-ffmpeg-workflows.md) |
| Submitting jobs over HTTP, scanning from a URL | [docs/rest-api.md](docs/rest-api.md) |
| Scaling, KEDA/HPA, rate-limit budgeting | [deploy/README.md](deploy/README.md) |
| Try it locally with Docker Compose | [deploy/compose/README.md](deploy/compose/README.md) |
| Config surface | [config.example.yaml](config.example.yaml) |
