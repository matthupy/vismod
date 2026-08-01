# Docker Compose Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a local development compose stack (two `vismod serve` replicas on durable Redis, with Prometheus and a provisioned Grafana dashboard) and a minimal production example compose file.

**Architecture:** No Go code changes. Everything is compose files, a mounted vismod config, Prometheus scrape config, Grafana provisioning, one dashboard JSON, and docs. The local stack proves what a single container cannot: the redis queue driver, two replicas sharing one queue, the `vismod_queue_depth` autoscaling signal, and graceful drain.

**Tech Stack:** Docker Compose v2, `redis:7-alpine`, `prom/prometheus:v2.54.1`, `grafana/grafana:11.2.0`, the repo's own `Dockerfile`.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-31-docker-compose-design.md`. Read it before starting.
- **A mounted config file is mandatory.** `config.Load` (`internal/config/config.go:384`) seeds viper with no defaults, so the `VISMOD_*` env overlay only overrides keys the yaml already sets. With no file, boot fails with `error: moderate: unknown adapter ""`. Never document an env-only run.
- **Container binds must be `0.0.0.0`.** The shipped defaults `intake_addr: 127.0.0.1:8080` and `ui.addr: 127.0.0.1:8081` are unreachable from a published port.
- **Each replica needs its own audit volume.** `audit.Open` (`internal/audit/audit.go:63`) replays the hash chain into per-process state and appends with `O_APPEND`; `audit.Verify` recomputes strictly. Two processes on one file interleave and make the log unverifiable.
- **No secrets in yaml, ever.** Secrets come from `.env` via `env_file`. This is a repo rule (`config.example.yaml` header).
- **No fake or stub moderator ships.** With no vendor key the stack must fail fast at boot.
- Only metrics that exist in `internal/observe/observe.go:50-86` may appear in the dashboard. There is no backpressure/readiness metric.
- Docs updated in the same commit as the behavior they describe (`AGENTS.md` done gate).
- Done gate before the final commit: `go build ./... && go vet ./... && go test ./...` all exit 0.
- Host platform is Windows with PowerShell. Verification commands below are PowerShell-safe; `&&` does not chain in PowerShell 5.1 — run commands one per line.

---

### Task 1: Local compose stack — Redis and two vismod replicas

**Files:**
- Create: `compose.yaml`
- Create: `deploy/compose/config.compose.example.yaml`
- Create: `deploy/compose/env.example`
- Create: `media/.gitkeep`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: the repo `Dockerfile` (build context `.`), and the CLI contract `vismod serve -c <path>`.
- Produces: service names `redis`, `vismod-a`, `vismod-b` on the default compose network; the config path `/etc/vismod/config.yaml` inside both replicas; host ports `8080` (intake) and `8081` (UI) from `vismod-a`; metrics on `vismod-a:9090` and `vismod-b:9090`, unpublished. Tasks 2 and 3 scrape those two names.

- [ ] **Step 1: Write the vismod config the stack mounts**

Create `deploy/compose/config.compose.example.yaml`:

```yaml
# vismod config for the compose stacks.
#
#   cp deploy/compose/config.compose.example.yaml deploy/compose/config.compose.yaml
#
# then set adapter.options.endpoint to your own resource. The copy is
# gitignored. Secrets NEVER go in this file — the API key comes from .env
# as VISMOD_MICROSOFT_API_KEY.
#
# A config file is REQUIRED. The VISMOD_* env overlay can only override
# keys this file already sets, so there is no env-only configuration:
# with no file, boot fails with `unknown adapter ""`.

adapter:
  name: microsoft
  options:
    endpoint: https://REPLACE-ME.cognitiveservices.azure.com/
    api_version: "2024-09-01"
    auth_mode: key
    # Budgeted across the TWO replicas in compose.yaml, per the
    # global_quota / max_replicas rule in deploy/README.md. Azure F0 is
    # 5 RPS, so 2 each leaves headroom. The limiter is PER PROCESS —
    # change this if you change the replica count.
    rate_limit_rps: 2
    max_attempts: 3

thresholds:
  default:
    flag_at: 0.4
    block_at: 0.7

ffmpeg:
  max_frames: 32

frames:
  concurrency: 4
  dedup:
    enabled: true
    hamming_threshold: 14

queue:
  driver: redis            # the point of this stack: durable, multi-replica
  workers: 4
  max_retries: 3
  retry_backoff: 2s
  drain_timeout: 30s
  job_timeout: 5m
  deadletter_max: 1000
  redis:
    addr: redis:6379       # compose service name
    db: 0

audit:
  enabled: true
  path: audit.log          # resolves under /home/vismod — one volume PER replica

log_level: info
metrics_addr: ":9090"      # scraped by prometheus over the compose network

# 0.0.0.0, not the 127.0.0.1 default: a loopback bind inside a container
# is unreachable from a published port.
intake_addr: "0.0.0.0:8080"

ui:
  enabled: true
  addr: "0.0.0.0:8081"
  auth: basic              # credentials env-only: VISMOD_UI_USER / VISMOD_UI_PASSWORD
```

- [ ] **Step 2: Write the env template**

Create `deploy/compose/env.example`:

```sh
# cp deploy/compose/env.example .env
#
# Secrets live here and ONLY here — never in a yaml file. .env is
# gitignored. Without VISMOD_MICROSOFT_API_KEY the stack fails fast at
# boot; that is deliberate, there is no stub moderator.

VISMOD_MICROSOFT_API_KEY=

# The UI is published on the host, so it runs with basic auth. Local
# development credentials — change them for anything reachable by anyone
# but you.
VISMOD_UI_USER=vismod
VISMOD_UI_PASSWORD=vismod

# Only if your Redis requires AUTH (the compose Redis does not).
# VISMOD_QUEUE_REDIS_PASSWORD=
```

- [ ] **Step 3: Ignore the generated secret and config copies**

Append to `.gitignore`:

```gitignore

# docker compose local stack: secrets and the per-operator config copy
.env
deploy/compose/config.compose.yaml
media/*
!media/.gitkeep
```

Create an empty `media/.gitkeep` so the mount source exists on a fresh clone.

- [ ] **Step 4: Write the local compose file**

Create `compose.yaml` at the repo root:

```yaml
# vismod local development stack.
#
#   cp deploy/compose/env.example .env
#   cp deploy/compose/config.compose.example.yaml deploy/compose/config.compose.yaml
#   # edit config.compose.yaml: set adapter.options.endpoint
#   # edit .env: set VISMOD_MICROSOFT_API_KEY
#   docker compose up --build
#
# Two replicas on ONE durable Redis queue, so this stack exercises what a
# single container cannot: the redis driver, multi-replica claim, the
# vismod_queue_depth signal, and graceful drain.
#
# NOT a production deployment. See deploy/compose/compose.prod.example.yaml
# for a minimal production shape, and deploy/README.md for Kubernetes.

name: vismod

x-vismod: &vismod
  build:
    context: .
    args:
      VERSION: compose-dev
  env_file: .env
  command: ["serve", "-c", "/etc/vismod/config.yaml"]
  volumes:
    # The config copy is REQUIRED; there is no env-only configuration.
    - ./deploy/compose/config.compose.yaml:/etc/vismod/config.yaml:ro
    # Job payloads carry a FILE PATH. Either replica may claim any job,
    # so the media mount must resolve identically in both.
    - ./media:/data:ro
  depends_on:
    redis:
      condition: service_healthy
  restart: unless-stopped
  stop_grace_period: 40s   # > queue.drain_timeout (30s) so drain finishes

services:
  redis:
    image: redis:7-alpine
    # No persistence: this stack is disposable. The production example
    # enables appendonly.
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 10

  vismod-a:
    <<: *vismod
    ports:
      - "8080:8080"   # dev intake: POST /jobs
      - "8081:8081"   # operator UI (basic auth, creds from .env)
    volumes:
      - ./deploy/compose/config.compose.yaml:/etc/vismod/config.yaml:ro
      - ./media:/data:ro
      # Own audit volume. The audit log is a hash chain replayed into
      # per-process state; two replicas sharing one file would interleave
      # and make `vismod audit verify` fail.
      - audit-a:/home/vismod

  vismod-b:
    <<: *vismod
    # No published ports: it would collide with vismod-a. Prometheus
    # reaches it over the compose network.
    volumes:
      - ./deploy/compose/config.compose.yaml:/etc/vismod/config.yaml:ro
      - ./media:/data:ro
      - audit-b:/home/vismod

volumes:
  audit-a:
  audit-b:
```

- [ ] **Step 5: Verify the file parses and the interpolation resolves**

```powershell
cp deploy/compose/env.example .env
cp deploy/compose/config.compose.example.yaml deploy/compose/config.compose.yaml
docker compose config --quiet
```

Expected: exits 0, no output. Any `services.vismod-a.volumes` merge error means the per-service `volumes:` override is malformed — the anchor's `volumes` key is fully replaced, not merged, which is why each service repeats the config and media mounts.

- [ ] **Step 6: Verify the stack boots and both replicas join the same Redis**

```powershell
docker compose up -d --build
docker compose ps
docker compose logs vismod-a vismod-b | Select-String "vismod serve started"
```

Expected: `redis`, `vismod-a`, `vismod-b` all `running`; `vismod-a` and `vismod-b` healthy after ~15s (the Dockerfile `HEALTHCHECK` start period); two `vismod serve started` lines, both with `"queue_driver":"redis"`.

If a replica exits with `unknown adapter ""`, the config mount is wrong. If it exits on a Redis dial error, `queue.redis.addr` is not `redis:6379`.

- [ ] **Step 7: Verify the intake accepts a job and both replicas can claim work**

Put any image at `media/sample.png` first (`docker run --rm -v "${PWD}/media:/out" --entrypoint sh vismod-vismod-a -c "ffmpeg -loglevel error -y -f lavfi -i color=c=blue:s=320x240 -frames:v 1 /out/sample.png"` works if you have no image handy).

```powershell
$body = '{"kind":"file","ref":"/data/sample.png","media_type":"image"}'
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8080/jobs -ContentType application/json -Body $body
docker compose logs vismod-a vismod-b | Select-String "job started"
```

Expected: the POST returns a job id (HTTP 202). A `job started` line appears in exactly one replica's logs.

With no real vendor endpoint the job ends `verdict:"error"` — that is correct fail-safe behavior, not a stack failure. The queue, worker, audit and metrics paths are still exercised.

- [ ] **Step 8: Verify the UI is reachable and authenticated**

```powershell
(Invoke-WebRequest -Uri http://127.0.0.1:8081/ -SkipHttpErrorCheck).StatusCode
$c = Get-Credential -UserName vismod -Message "password: vismod"
(Invoke-WebRequest -Uri http://127.0.0.1:8081/ -Credential $c -AllowUnencryptedAuthentication).StatusCode
```

Expected: `401` unauthenticated, `200` with credentials. A `200` on the first call means the UI is serving unauthenticated — stop and fix `ui.auth` before continuing.

- [ ] **Step 9: Verify graceful drain**

```powershell
docker compose stop vismod-b
docker compose logs vismod-b | Select-String "drained cleanly"
docker inspect vismod-vismod-b-1 --format "exit={{.State.ExitCode}}"
```

Expected: a `shutdown signal received; draining` line followed by `drained cleanly`, and `exit=0`.

- [ ] **Step 10: Verify each replica has its own verifiable audit chain**

```powershell
docker compose start vismod-b
docker compose exec vismod-a /vismod audit verify --path /home/vismod/audit.log
docker compose exec vismod-b /vismod audit verify --path /home/vismod/audit.log
```

Expected: both report a valid chain. Check the flag name first with `docker compose exec vismod-a /vismod audit verify --help` and use whatever it actually is — do not guess.

- [ ] **Step 11: Commit**

```powershell
git add compose.yaml deploy/compose/config.compose.example.yaml deploy/compose/env.example .gitignore media/.gitkeep
git commit -m "Local compose stack: two serve replicas on durable Redis" -m "Two vismod serve replicas share one Redis queue, so this stack exercises the redis driver, multi-replica claim and graceful drain -- none of which a single container can prove.

Three constraints the file is built around: a mounted config is mandatory (config.Load seeds viper with no defaults, so the VISMOD_* overlay only overrides keys the yaml already sets; with no file boot fails with 'unknown adapter'); intake_addr and ui.addr bind 0.0.0.0 because the 127.0.0.1 default is unreachable from a published port; and each replica gets its OWN audit volume because audit.Open replays the hash chain into per-process state and two writers would make the log unverifiable.

rate_limit_rps is 2 per replica -- the F0 quota of 5 budgeted across two processes, per deploy/README.md." -m "Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: Prometheus

**Files:**
- Create: `deploy/compose/prometheus.yml`
- Modify: `compose.yaml` (add the `prometheus` service)

**Interfaces:**
- Consumes: `vismod-a:9090` and `vismod-b:9090` from Task 1.
- Produces: a Prometheus service reachable at `prometheus:9090` on the compose network and `http://127.0.0.1:9090` on the host. Task 3's Grafana datasource points at `http://prometheus:9090`.

- [ ] **Step 1: Write the scrape config**

Create `deploy/compose/prometheus.yml`:

```yaml
# Scrapes both vismod replicas over the compose network. vismod exposes
# /metrics on metrics_addr (":9090") — all interfaces, so no per-replica
# port publishing is needed.
global:
  scrape_interval: 5s
  evaluation_interval: 5s

scrape_configs:
  - job_name: vismod
    static_configs:
      - targets: ["vismod-a:9090", "vismod-b:9090"]
        labels:
          stack: compose-local
```

- [ ] **Step 2: Add the service to `compose.yaml`**

Add under `services:`:

```yaml
  prometheus:
    image: prom/prometheus:v2.54.1
    command:
      - --config.file=/etc/prometheus/prometheus.yml
      - --storage.tsdb.retention.time=6h
    volumes:
      - ./deploy/compose/prometheus.yml:/etc/prometheus/prometheus.yml:ro
    ports:
      - "9090:9090"   # host 9090 is Prometheus; the replicas' own :9090 stays internal
    depends_on:
      - vismod-a
      - vismod-b
    restart: unless-stopped
```

- [ ] **Step 3: Verify both replicas are UP as targets**

```powershell
docker compose up -d
Start-Sleep -Seconds 10
(Invoke-RestMethod http://127.0.0.1:9090/api/v1/targets).data.activeTargets |
  Select-Object -ExpandProperty health
```

Expected: two entries, both `up`. A `down` target with `connection refused` means `metrics_addr` is not `":9090"` in the mounted config.

- [ ] **Step 4: Verify the autoscaling signal is actually queryable**

```powershell
(Invoke-RestMethod "http://127.0.0.1:9090/api/v1/query?query=vismod_queue_depth").data.result.Count
(Invoke-RestMethod "http://127.0.0.1:9090/api/v1/query?query=vismod_jobs_total").data.result
```

Expected: the first returns `2` (one series per replica). The second returns at least one series with a `verdict` label if you posted a job in Task 1.

- [ ] **Step 5: Commit**

```powershell
git add deploy/compose/prometheus.yml compose.yaml
git commit -m "Scrape both compose replicas with Prometheus" -m "5s scrape of vismod-a:9090 and vismod-b:9090 over the compose network. Host port 9090 belongs to Prometheus; the replicas' own metrics endpoints stay internal, so vismod_queue_depth -- the KEDA/HPA signal described in deploy/README.md -- is visible per replica without publishing anything extra." -m "Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: Grafana with a provisioned dashboard

**Files:**
- Create: `deploy/compose/grafana/provisioning/datasources/prometheus.yaml`
- Create: `deploy/compose/grafana/provisioning/dashboards/dashboards.yaml`
- Create: `deploy/compose/grafana/dashboards/vismod.json`
- Modify: `compose.yaml` (add the `grafana` service)

**Interfaces:**
- Consumes: `http://prometheus:9090` from Task 2.
- Produces: Grafana on host `3000`, anonymous viewer access, one dashboard with uid `vismod-overview`.

- [ ] **Step 1: Provision the datasource**

Create `deploy/compose/grafana/provisioning/datasources/prometheus.yaml`:

```yaml
apiVersion: 1
datasources:
  - name: Prometheus
    uid: vismod-prom
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
```

- [ ] **Step 2: Provision the dashboard loader**

Create `deploy/compose/grafana/provisioning/dashboards/dashboards.yaml`:

```yaml
apiVersion: 1
providers:
  - name: vismod
    type: file
    allowUiUpdates: false
    options:
      path: /var/lib/grafana/dashboards
      foldersFromFilesStructure: false
```

- [ ] **Step 3: Write the dashboard**

Create `deploy/compose/grafana/dashboards/vismod.json`. Every panel uses a metric that exists in `internal/observe/observe.go`; nothing here is invented.

```json
{
  "uid": "vismod-overview",
  "title": "vismod overview",
  "schemaVersion": 39,
  "version": 1,
  "editable": true,
  "refresh": "10s",
  "time": { "from": "now-30m", "to": "now" },
  "panels": [
    {
      "type": "timeseries",
      "title": "Queue depth (the autoscaling signal)",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "vismod-prom" },
      "targets": [
        { "expr": "vismod_queue_depth", "legendFormat": "{{instance}}", "refId": "A" }
      ]
    },
    {
      "type": "timeseries",
      "title": "Dead-letter depth",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "vismod-prom" },
      "targets": [
        { "expr": "vismod_deadletter_depth", "legendFormat": "{{instance}}", "refId": "A" }
      ]
    },
    {
      "type": "timeseries",
      "title": "Verdicts / sec",
      "description": "block > error > flag > allow. A rising error rate is the fail-safe path working, not silence.",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 8 },
      "datasource": { "type": "prometheus", "uid": "vismod-prom" },
      "targets": [
        { "expr": "sum by (verdict) (rate(vismod_jobs_total[1m]))", "legendFormat": "{{verdict}}", "refId": "A" }
      ]
    },
    {
      "type": "timeseries",
      "title": "Adapter errors / sec (readiness proxy)",
      "description": "There is NO backpressure or readiness metric — that state is exposed only as /readyz text. This panel is a proxy: sustained errors here are what trips backpressure.",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 8 },
      "datasource": { "type": "prometheus", "uid": "vismod-prom" },
      "targets": [
        { "expr": "sum by (code) (rate(vismod_adapter_errors_total[1m]))", "legendFormat": "{{code}}", "refId": "A" }
      ]
    },
    {
      "type": "timeseries",
      "title": "Adapter request latency",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "vismod-prom" },
      "targets": [
        { "expr": "histogram_quantile(0.5, sum by (le, adapter) (rate(vismod_adapter_request_seconds_bucket[5m])))", "legendFormat": "p50 {{adapter}}", "refId": "A" },
        { "expr": "histogram_quantile(0.95, sum by (le, adapter) (rate(vismod_adapter_request_seconds_bucket[5m])))", "legendFormat": "p95 {{adapter}}", "refId": "B" }
      ]
    },
    {
      "type": "timeseries",
      "title": "Workers active",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "vismod-prom" },
      "targets": [
        { "expr": "vismod_workers_active", "legendFormat": "{{instance}}", "refId": "A" }
      ]
    },
    {
      "type": "timeseries",
      "title": "Frames scanned / sec",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 24 },
      "datasource": { "type": "prometheus", "uid": "vismod-prom" },
      "targets": [
        { "expr": "sum(rate(vismod_frames_scanned_total[1m]))", "legendFormat": "frames/s", "refId": "A" }
      ]
    },
    {
      "type": "timeseries",
      "title": "Frames per job (p95, by media type)",
      "description": "FFmpeg workflow tuning signal.",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 24 },
      "datasource": { "type": "prometheus", "uid": "vismod-prom" },
      "targets": [
        { "expr": "histogram_quantile(0.95, sum by (le, media_type) (rate(vismod_job_frames_bucket[5m])))", "legendFormat": "{{media_type}}", "refId": "A" }
      ]
    }
  ]
}
```

- [ ] **Step 4: Add the service to `compose.yaml`**

Add under `services:`:

```yaml
  grafana:
    image: grafana/grafana:11.2.0
    environment:
      # Local stack only: no login wall, view-only. This is why the
      # production example ships no Grafana.
      GF_AUTH_ANONYMOUS_ENABLED: "true"
      GF_AUTH_ANONYMOUS_ORG_ROLE: Viewer
      GF_AUTH_BASIC_ENABLED: "false"
      GF_USERS_DEFAULT_THEME: light
    volumes:
      - ./deploy/compose/grafana/provisioning:/etc/grafana/provisioning:ro
      - ./deploy/compose/grafana/dashboards:/var/lib/grafana/dashboards:ro
    ports:
      - "3000:3000"
    depends_on:
      - prometheus
    restart: unless-stopped
```

- [ ] **Step 5: Verify the dashboard provisions**

```powershell
docker compose up -d
Start-Sleep -Seconds 15
(Invoke-RestMethod http://127.0.0.1:3000/api/search?query=vismod).title
docker compose logs grafana | Select-String -Pattern "error|failed" -CaseSensitive:$false
```

Expected: `vismod overview` returned; no provisioning errors in the logs. A "Dashboard title cannot be empty" or datasource-not-found error means the JSON or the provisioning yaml is malformed.

- [ ] **Step 6: Verify every panel query returns data, not just the dashboard loading**

Post a job (Task 1 Step 7) so counters are non-zero, then check each expression against Prometheus directly:

```powershell
$q = @(
  "vismod_queue_depth",
  "vismod_deadletter_depth",
  "sum by (verdict) (rate(vismod_jobs_total[1m]))",
  "sum by (code) (rate(vismod_adapter_errors_total[1m]))",
  "histogram_quantile(0.95, sum by (le, adapter) (rate(vismod_adapter_request_seconds_bucket[5m])))",
  "vismod_workers_active",
  "sum(rate(vismod_frames_scanned_total[1m]))",
  "histogram_quantile(0.95, sum by (le, media_type) (rate(vismod_job_frames_bucket[5m])))"
)
foreach ($e in $q) {
  $r = Invoke-RestMethod ("http://127.0.0.1:9090/api/v1/query?query=" + [uri]::EscapeDataString($e))
  "{0,-6} {1}" -f $r.data.result.Count, $e
}
```

Expected: every line reports a non-zero series count. A `0` means either the metric name is wrong or that code path has not run yet — check `internal/observe/observe.go` before assuming the query is fine. Histogram queries need the `_bucket` suffix; a `0` there usually means the suffix was dropped.

- [ ] **Step 7: Commit**

```powershell
git add deploy/compose/grafana compose.yaml
git commit -m "Provision a Grafana dashboard over the compose stack" -m "Eight panels, every one backed by a series that exists in internal/observe: queue depth, dead-letter depth, verdicts/sec by verdict, adapter errors by code, adapter latency p50/p95, workers active, frames scanned, frames-per-job p95 by media type.

There is NO backpressure or readiness metric -- that state is exposed only as /readyz text -- so the error-rate panel is titled and described as a proxy rather than implying a readiness series exists. Grafana runs anonymous-viewer in the local stack only; the production example ships no Grafana." -m "Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: Minimal production example

**Files:**
- Create: `deploy/compose/compose.prod.example.yaml`

**Interfaces:**
- Consumes: the same `Dockerfile` and the same mounted-config contract as Task 1.
- Produces: nothing other tasks depend on. Standalone file.

- [ ] **Step 1: Write the file**

Create `deploy/compose/compose.prod.example.yaml`:

```yaml
# vismod — MINIMAL production example. Copy it, do not run it as-is.
#
#   docker compose -f deploy/compose/compose.prod.example.yaml up -d
#
# What this gives you: one vismod worker on a durable, persisted Redis
# queue, restarted on failure, with a graceful-drain window and a
# persisted audit log.
#
# What this does NOT give you, and you must add yourself:
#   - TLS anywhere.
#   - Authentication on the intake. It is off here for that reason.
#   - Backups of the audit log or the Redis append-only file.
#   - Multi-host, or more than one replica. A second replica on this file
#     would share the audit volume and corrupt the hash chain; give each
#     replica its OWN audit volume (see compose.yaml at the repo root).
#   - Shared cross-replica rate limiting. The limiter is per process; see
#     deploy/README.md.
# Read SECURITY.md before exposing any port, and deploy/README.md for the
# Kubernetes path, which is the reference deployment.

name: vismod-prod

services:
  redis:
    image: redis:7-alpine
    command: ["redis-server", "--appendonly", "yes"]
    volumes:
      - redis-data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 3s
      retries: 5
    restart: unless-stopped
    deploy:
      resources:
        limits:
          cpus: "1"
          memory: 512M

  vismod:
    image: vismod:latest        # build and tag it yourself; no floating build here
    command: ["serve", "-c", "/etc/vismod/config.yaml"]
    env_file: .env              # secrets ONLY here, never in yaml
    volumes:
      - ./config.yaml:/etc/vismod/config.yaml:ro
      - ./media:/data:ro
      - audit:/home/vismod      # audit log is an append-only hash chain: persist it
    depends_on:
      redis:
        condition: service_healthy
    restart: unless-stopped
    # Must exceed queue.drain_timeout, or SIGTERM drain gets killed
    # mid-job and work is left for orphan recovery unnecessarily.
    stop_grace_period: 60s
    deploy:
      resources:
        limits:
          cpus: "2"
          memory: 2G
    # No ports published. The intake and UI have no TLS and the intake
    # has no auth; publish them only behind something that does.

volumes:
  redis-data:
  audit:
```

- [ ] **Step 2: Verify it parses**

```powershell
docker compose -f deploy/compose/compose.prod.example.yaml config --quiet
```

Expected: exits 0.

- [ ] **Step 3: Verify it runs and survives a restart with data intact**

```powershell
docker build -t vismod:latest .
cd deploy/compose
cp ../../deploy/compose/config.compose.yaml ./config.yaml
cp ../../.env ./.env
New-Item -ItemType Directory -Force ./media | Out-Null
docker compose -f compose.prod.example.yaml up -d
docker compose -f compose.prod.example.yaml ps
docker compose -f compose.prod.example.yaml down
docker compose -f compose.prod.example.yaml up -d
docker compose -f compose.prod.example.yaml exec redis redis-cli dbsize
```

Expected: services `running` both times; `dbsize` responds (proving the volume survived `down`). Then clean up the copies:

```powershell
docker compose -f compose.prod.example.yaml down -v
Remove-Item ./config.yaml, ./.env
Remove-Item ./media -Recurse
cd ../..
```

Confirm `git status --short` shows no stray `deploy/compose/config.yaml` or `.env`.

- [ ] **Step 4: Commit**

```powershell
git add deploy/compose/compose.prod.example.yaml
git commit -m "Minimal production compose example" -m "One vismod worker, one persisted Redis (appendonly on a named volume), restart policies, resource limits, a persisted audit volume, and stop_grace_period above queue.drain_timeout so SIGTERM drain completes.

Deliberately not provided, and stated in the file header: TLS, intake/UI auth (no ports are published), backups, multi-host, more than one replica (a second replica sharing the audit volume would corrupt the hash chain), and shared cross-replica rate limiting. Kubernetes in deploy/README.md remains the reference deployment." -m "Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 5: Documentation and the done gate

**Files:**
- Create: `deploy/compose/README.md`
- Modify: `README.md` (Docker section, around line 39)
- Modify: `deploy/README.md` (new compose section)
- Modify: `docs/agent/STATUS.md`, `docs/agent/TASKS.md`, `docs/agent/UNVERIFIED.md`

**Interfaces:**
- Consumes: everything from Tasks 1-4.
- Produces: nothing code-facing.

- [ ] **Step 1: Write `deploy/compose/README.md`**

It must cover, in this order: the four-command quick start from `compose.yaml`'s header; what the local stack proves (redis driver, two replicas on one queue, `vismod_queue_depth`, graceful drain); the URLs (intake `:8080`, UI `:8081` basic auth, Prometheus `:9090`, Grafana `:3000` anonymous viewer); the three constraints from Global Constraints above, each stated as a rule with its reason; and a "what this is not" section pointing at the production example and `deploy/README.md`.

- [ ] **Step 2: Link it from the root README**

In `README.md`, after the existing Docker block, add:

```markdown
Compose (two replicas, durable Redis queue, Prometheus + Grafana):

```sh
cp deploy/compose/env.example .env                                        # set your API key
cp deploy/compose/config.compose.example.yaml deploy/compose/config.compose.yaml   # set your endpoint
docker compose up --build     # intake :8080, UI :8081, Prometheus :9090, Grafana :3000
```

See [deploy/compose/README.md](deploy/compose/README.md), and
[deploy/README.md](deploy/README.md) for Kubernetes.
```

- [ ] **Step 3: Add a compose section to `deploy/README.md`**

Place it after the scaling contract. It must repeat two caveats already stated there, because a compose operator may never read the k8s sections: multi-replica requires `queue.driver=redis`, and the rate limiter is per process so `rate_limit_rps` must be budgeted `global_quota / replicas` (the local stack ships `2` for two replicas against an F0 quota of 5).

- [ ] **Step 4: Update the agent docs**

- `TASKS.md`: no new entry; the queue stays empty unless something is left undone.
- `UNVERIFIED.md`: add an entry stating that no compose run has observed a successful `allow` verdict end to end, because the stack has no vendor credential in this environment — every job ends `verdict:"error"`. What would prove it: one `docker compose up` with a real `VISMOD_MICROSOFT_API_KEY` and endpoint, scanning a benign image, showing `verdict:"allow"` in the result envelope and `vismod_jobs_total{verdict="allow"}` incrementing.
- `STATUS.md`: rewrite the "Where things stand" tail with what landed, and record every verification actually run — target health, panel queries, drain, audit chains, prod restart persistence.

- [ ] **Step 5: Run the done gate**

```powershell
go build ./...
go vet ./...
go test ./...
```

Expected: all three exit 0. No Go code changed, so a failure here means something unrelated is broken — investigate before committing.

- [ ] **Step 6: Verify the documented quick start works from a clean state**

```powershell
docker compose down -v
docker compose up -d --build
docker compose ps
```

Expected: every service running, replicas healthy. Follow the README's own commands literally — if a documented command fails, fix the doc, not your memory of it.

- [ ] **Step 7: Commit**

Replace the bracketed values below with what the verification steps actually produced — do not commit the placeholders:

```powershell
git add deploy/compose/README.md README.md deploy/README.md docs/agent
git commit -m "Document the compose stacks" -m "Quick start in the root README, a compose section in deploy/README.md repeating the redis-driver requirement and the per-process rate-limiter budgeting rule, and deploy/compose/README.md covering what the local stack proves.

Verified by running it: [N] Prometheus targets up, all 8 dashboard panel queries returned series, intake accepted a job, drain logged 'drained cleanly' with exit 0, both replicas' audit chains verified independently, and the production stack survived down/up with Redis data intact.

UNVERIFIED: no compose run has observed a successful allow verdict end to end -- with no vendor credential every job ends verdict=error, which exercises the queue, metrics, audit and drain paths but not a scoring success." -m "Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Notes for the implementer

- The uncommitted working-tree changes to `README.md` and `docs/agent/*` at the start of this work are from the Docker image verification done on 2026-07-31 (image `sha256:fc19c39a66f8`). They are unrelated to compose; leave them alone or commit them separately first — do not fold them into a compose commit.
- If a verification step fails, fix the cause. Do not weaken the check, and do not record a claim in `STATUS.md` that a step proved something it did not. `UNVERIFIED.md` exists precisely so gaps can be stated rather than glossed.
