# Local compose stack

This is the `compose.yaml` at the repo root: Redis, two `vismod serve`
replicas, Prometheus, and Grafana, all on one Docker network. It is a
development and demo stack, not a production deployment.

## Quick start

```sh
cp deploy/compose/env.example .env
cp deploy/compose/config.compose.example.yaml deploy/compose/config.compose.yaml
# edit config.compose.yaml: set adapter.options.endpoint
# edit .env: set VISMOD_MICROSOFT_API_KEY
docker compose up --build
```

Both `.env` and `deploy/compose/config.compose.yaml` are gitignored — they
are your per-operator copies, not repo content.

## What this stack proves

- **The redis queue driver**, not the single-process memory driver. Both
  replicas dequeue from the same durable Redis LIST.
- **Two replicas claiming from one queue.** A job posted to either
  replica's intake is claimed by exactly one of them.
- **`vismod_queue_depth`**, the Prometheus gauge that autoscaling is
  driven by (`deploy/README.md`), moving as jobs are enqueued and drained.
- **Graceful drain.** `docker compose stop` (or `down`) sends `SIGTERM`;
  each replica stops intake, finishes in-flight jobs within
  `queue.drain_timeout`, logs `drained cleanly`, and exits 0.

What it does not prove: a real vendor call. With no vendor credential
configured, every job ends `verdict:"error"` — see
[docs/agent/UNVERIFIED.md](../../docs/agent/UNVERIFIED.md).

## URLs

| Service | URL | Notes |
|---|---|---|
| Intake | `http://localhost:8080` | `POST /jobs`, dev intake, no auth. Only `vismod-a` publishes it — `vismod-b` is reachable over the compose network only. |
| Operator UI | `http://localhost:8081` | Basic auth, credentials from `.env` (`VISMOD_UI_USER` / `VISMOD_UI_PASSWORD`). Also `vismod-a` only. |
| Prometheus | `http://localhost:9090` | Scrapes `vismod-a:9090` and `vismod-b:9090` every 5s. |
| Grafana | `http://localhost:3000` | Anonymous viewer access, no login. Dashboard: "vismod overview" (uid `vismod-overview`), provisioned from `deploy/compose/grafana/`. |

## Result envelopes

`serve` writes one JSON result envelope per line (JSONL) to stdout —
there is no other sink in this stack. (Envelope destinations are
configurable via `output.sinks` — see `config.example.yaml` — but this
stack's `config.compose.example.yaml` doesn't set one, so the built-in
stdout default applies.) Read them with:

```sh
docker compose logs -f vismod-a vismod-b
```

## Constraints this stack is built around

- **A mounted config file is mandatory.** `config.Load` seeds viper with
  no defaults, so the `VISMOD_*` env overlay only overrides keys the yaml
  already sets — with no file, boot fails with `unknown adapter ""`.
  There is no env-only way to run vismod; both replicas mount
  `config.compose.yaml` read-only.
- **Container binds must be `0.0.0.0`.** The shipped defaults
  (`intake_addr: 127.0.0.1:8080`, `ui.addr: 127.0.0.1:8081`) are
  unreachable from a published port because the loopback interface is
  scoped to the container's own network namespace. `config.compose.example.yaml`
  binds both to `0.0.0.0`.
- **Each replica needs its own audit volume.** The audit log is an
  append-only hash chain replayed into per-process state; two processes
  appending to one file interleave and break `vismod audit verify`.
  `vismod-a` and `vismod-b` each get a private named volume (`audit-a`,
  `audit-b`).
- **A `file` output sink has the same constraint.** `output.sinks` with
  `type: file` opens its path `O_APPEND`; two replicas writing the same
  path interleave JSONL lines exactly like the audit log. Give each
  replica its own path or its own volume — this stack doesn't configure
  a `file` sink (see "Result envelopes" above), but if you add one,
  don't share it across `vismod-a` and `vismod-b`.
- **Do not `docker compose up --scale vismod-b=N`.** `vismod-b` publishes
  no ports, so scaling it succeeds silently — but every replica would
  share the one `audit-b` volume and corrupt the hash chain. To run a
  third replica, add a `vismod-c` service with its own `audit-c` volume
  instead.

## What this is not

Not a production deployment: no TLS, a dev intake with no auth, a
disposable non-persistent Redis, and a Grafana with no login wall. For a
minimal production shape (persisted Redis, resource limits, no published
ports), see
[compose.prod.example.yaml](compose.prod.example.yaml). For the
reference deployment — Kubernetes, KEDA/HPA autoscaling by queue depth,
and the rate-limit budgeting rule — see
[deploy/README.md](../README.md).
