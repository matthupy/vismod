# Docker Compose support — design

**Date:** 2026-07-31
**Status:** approved, not yet implemented

## Goal

Ship two compose stacks:

1. **Local** (`compose.yaml`, repo root) — a working multi-component
   development stack: two `vismod serve` replicas against durable Redis,
   with Prometheus and Grafana. Proves what the single container cannot:
   the redis queue driver, multi-replica claim/orphan recovery, the
   `vismod_queue_depth` autoscaling signal, and graceful drain.
2. **Production example** (`deploy/compose/compose.prod.example.yaml`) —
   deliberately minimal. One `vismod`, one Redis, durable and restartable,
   with a header stating what it does not provide.

Non-goal: replacing the Kubernetes path. `deploy/README.md` remains the
reference deployment; compose is a second, smaller target.

## Layout

```
compose.yaml                              # local stack; zero-arg `docker compose up`
.env                                      # gitignored; from deploy/compose/env.example
deploy/compose/
  compose.prod.example.yaml
  config.compose.example.yaml             # cp -> config.compose.yaml (gitignored)
  env.example
  prometheus.yml
  grafana/provisioning/datasources/prometheus.yaml
  grafana/provisioning/dashboards/dashboards.yaml
  grafana/dashboards/vismod.json
  README.md
```

The local file sits at the repo root because that is where `docker
compose` looks with no arguments. Everything it mounts, and the
production example, live under `deploy/` beside the existing k8s scalers.

## Local stack

Services: `redis`, `vismod-a`, `vismod-b`, `prometheus`, `grafana`.

**redis** — `redis:7-alpine`, healthcheck `redis-cli ping`. No
persistence configured; this stack is disposable by design.

**vismod-a / vismod-b** — both `build: .`, both running
`serve -c /etc/vismod/config.yaml`, both `depends_on: redis:
service_healthy`, both `env_file: .env`.

- Config is mounted read-only from `deploy/compose/config.compose.yaml`.
  A config file is mandatory: `config.Load` seeds viper with no defaults,
  so the `VISMOD_*` env overlay can only override keys the yaml already
  sets. With no file, boot fails with `unknown adapter ""`.
- The config sets `queue.driver: redis`, `queue.redis.addr: redis:6379`,
  and binds `intake_addr` and `ui.addr` to `0.0.0.0`. The shipped default
  of `127.0.0.1` is unreachable from a published port inside a container.
- `./media:/data:ro` is mounted on **both** replicas. An intake job
  carries a file path, and either replica may claim it, so the path must
  resolve identically in both.
- Only `vismod-a` publishes intake (`8080`) and UI (`8081`); `vismod-b`
  stays internal to avoid a host-port collision. Neither publishes
  `:9090` — Prometheus scrapes both over the compose network, and host
  `9090` belongs to Prometheus.
- **Each replica gets its own audit volume.** `audit.Open` replays the
  hash chain into per-process state (`seq`, `prevHash`) and appends with
  `O_APPEND`; `audit.Verify` recomputes the chain strictly. Two processes
  appending to one file interleave and make the log unverifiable. One
  named volume per replica, mounted at `/home/vismod` (the working dir,
  which is where the default `audit.path: audit.log` resolves, and which
  the Dockerfile already declares a volume).
- `adapter.options.rate_limit_rps: 2`, i.e. the Azure F0 quota of 5 RPS
  budgeted across two replicas with headroom, following
  `deploy/README.md`'s `global_quota / max_replicas` rule. The comment
  states the rule and the per-process limiter caveat, so changing the
  replica count carries an obvious instruction to change this number.

**prometheus** — scrapes `vismod-a:9090` and `vismod-b:9090` at 5s.
Published on host `9090`.

**grafana** — published on `3000`, anonymous viewer role enabled so the
stack is usable with no login. Datasource and dashboard are provisioned
from committed files.

**Credentials.** `.env` supplies `VISMOD_MICROSOFT_API_KEY`. With no key
the stack fails fast at boot with the adapter's own error. No stub or
fake moderator ships in this repo.

## Production example

One `vismod`, one `redis`. Minimal on purpose:

- `restart: unless-stopped` on both.
- Redis with `appendonly yes` on a named volume.
- A named volume for the audit log.
- `deploy.resources.limits` for cpu and memory on both.
- `env_file` for secrets; no secret in yaml, matching the repo rule.
- `stop_grace_period` set above `queue.drain_timeout` so SIGTERM drain
  completes rather than being killed mid-job.

A header comment states what the file does **not** give you — TLS, auth
on the intake or UI surfaces, backups, multi-host, or shared
cross-replica rate limiting — and points at `SECURITY.md` and
`deploy/README.md`.

## Dashboard

One provisioned dashboard, every panel backed by a metric that exists in
`internal/observe`:

| Panel | Series |
|---|---|
| Queue depth | `vismod_queue_depth` |
| Dead-letter depth | `vismod_deadletter_depth` |
| Verdicts/sec | `rate(vismod_jobs_total[1m])` by `verdict` |
| Adapter errors | `rate(vismod_adapter_errors_total[1m])` by `code` |
| Adapter latency | p50/p95 of `vismod_adapter_request_seconds` |
| Workers active | `vismod_workers_active` |
| Frames scanned | `rate(vismod_frames_scanned_total[1m])` |
| Frames per job | `vismod_job_frames` by `media_type` |

There is **no metric for backpressure or readiness state** — it is
exposed only as `/readyz` text. The dashboard therefore shows adapter
error rate as a proxy, and the panel is labelled as a proxy rather than
implying a readiness series exists.

## Verification

Both stacks get brought up and checked, not merely written:

- both replicas connect to Redis and appear as Prometheus targets;
- `POST /jobs` against the published intake is accepted;
- `vismod_queue_depth` and `vismod_jobs_total` move in response;
- every dashboard panel returns data;
- `docker compose stop` drains cleanly (log line + exit 0);
- the production stack survives `down` then `up` with Redis data intact.

**Limit, to be recorded in `UNVERIFIED.md`:** with no vendor credential
every job ends `verdict:"error"`. That exercises the queue, metrics,
audit, drain and dedup paths, but no successful `allow` verdict is
observed end to end through compose.

## Docs and protocol

- README's Docker section links the compose quick start.
- `deploy/README.md` gains a compose section stating the same scaling
  contract caveats (per-process rate limiter, memory driver ban for
  multi-replica).
- `.gitignore` covers `.env` and `deploy/compose/config.compose.yaml`.
- `docs/agent/STATUS.md`, `TASKS.md` and `UNVERIFIED.md` updated in the
  landing commit, per `AGENTS.md`.
- Done gate (`go build ./... && go vet ./... && go test ./...`) still
  applies even though no Go code changes.
