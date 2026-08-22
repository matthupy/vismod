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
cmd/vismod-eval/  eval harness CLI; never on the serving path
pkg/moderation/   public contract types (Moderator, NormalizedResult, Verdict)
internal/cli/     cobra composition root; the only place adapters are wired.
                  boot.go exports cli.Serve(ctx, cfg, opts...) for a caller
                  running the serve stack in-process (the eval harness)
internal/config/  viper loader, thresholds, workflows, ConfigHash
internal/moderate/  adapter registry, rate limiter, retrying HTTP; adapters/*
internal/frames/  ffmpeg frame extraction, workflow guardrails, dHash dedup
internal/fetch/   allow-listed https media download for kind:"url" sources
internal/queue/   memq (dev) and redisq (durable, at-least-once)
internal/pipeline/  frames -> dedup -> fan-out -> thresholds -> rollup -> sink
internal/result/  result envelope + Sinks (JSONL, file, webhook, multi),
                  routing (Predicate/RoutedSink), formats (json, discord)
internal/audit/   append-only hash-chained decision log
internal/observe/ slog, Prometheus metrics, backpressure
internal/ui/      embedded read-mostly operator dashboard (off by default)
internal/eval/    evaluation harness: corpus loader (manifest + CSV on-ramp)
```

## Commands

Build and test with `go build ./...`, `go vet ./...`, `go test ./...`.
The full suite runs with no network and no credentials.

```sh
go test ./internal/pipeline/...              # one package
go test ./internal/pipeline/ -run TestRollupVerdictPrecedence   # one test
go test -update ./internal/moderate/...      # regenerate goldens
gofmt -l .                                   # must print NOTHING
golangci-lint run ./... && govulncheck ./... # CI also runs both
go mod tidy && git diff --exit-code go.mod go.sum
```

`go test -race` cannot run on the primary dev box (`CGO_ENABLED=0`, no C
toolchain) — CI is the only data-race gate. Everything else above runs
locally. Full done gate: [AGENTS.md](AGENTS.md).

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
| What each envelope field means, sink routing and formats | [docs/result-envelope.md](docs/result-envelope.md) |
| Per-model limits, class maps, auth, verification status | [docs/models.md](docs/models.md) |
| Which open-weight model to self-host, and why | [docs/self-hosted-classifiers.md](docs/self-hosted-classifiers.md) |
| What the audit chain records, and verifying it | [docs/audit-log.md](docs/audit-log.md) |
| Human contributor rules | [CONTRIBUTING.md](CONTRIBUTING.md) |
| Custom ffmpeg workflows | [docs/custom-ffmpeg-workflows.md](docs/custom-ffmpeg-workflows.md) |
| Submitting jobs over HTTP, scanning from a URL | [docs/rest-api.md](docs/rest-api.md) |
| Scaling, KEDA/HPA, rate-limit budgeting | [deploy/README.md](deploy/README.md) |
| Try it locally with Docker Compose | [deploy/compose/README.md](deploy/compose/README.md) |
| What to check before going to production | [docs/production-checklist.md](docs/production-checklist.md) |
| Config surface | [config.example.yaml](config.example.yaml) |
