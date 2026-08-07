# Deploying vismod with queue-depth autoscaling

## The scaling contract

- Each `vismod serve` replica runs a **fixed pool** of `queue.workers`
  goroutines. There is no in-process elasticity by design.
- Horizontal scale comes from **adding replicas**, driven by the
  **`vismod_queue_depth`** Prometheus gauge (pending + delayed retries).
  `QueueDepth` is uniform across drivers, so the signal is identical in
  dev (`memory`) and production (`redis`).
- **`vismod_processing_depth`** counts jobs claimed by a replica but not
  yet acked. It is deliberately NOT part of the scaling signal — scaling
  on it would chase in-flight work — but it must be alerted on: a failed
  dead-letter write parks a payload in processing, and without this gauge
  those jobs are in a queue nothing counts, while `vismod_queue_depth`
  reads 0 and the autoscaler scales to zero on top of them. Alert on
  `vismod_processing_depth > 0` sustained while `vismod_queue_depth == 0`
  and `vismod_workers_active == 0`.
- **Multi-replica REQUIRES `queue.driver=redis`.** The memory driver is
  single-process, non-durable, at-most-once: replicas would each own a
  private queue and a crash loses jobs. `serve` warns at boot and in
  `/readyz` output when `driver=memory`.
- Readiness (`/readyz`) also encodes **fail-safe backpressure**: sustained
  provider failure (default: 20 consecutive errors, or ≥50% error rate
  over 60s) flips the replica not-ready until 5 consecutive successes.
  Keep the readiness probe wired so ingress stops routing during a
  provider outage instead of black-holing jobs.
- **A `file` output sink needs a volume per replica, same as the audit
  log.** `output.sinks` (`config.example.yaml`) can route result
  envelopes to an append-only JSONL file instead of, or in addition to,
  stdout. That file is opened `O_APPEND`; two replicas appending to the
  same path interleave writes and produce corrupt JSONL, exactly the
  audit-log hazard described in
  [deploy/compose/README.md](compose/README.md). Give each replica its
  own path or its own persistent volume — never share one across
  replicas.

## Trying this locally with Docker Compose

Before standing up Kubernetes, `compose.yaml` at the repo root runs the
same shape — two replicas on one durable Redis queue, plus Prometheus and
Grafana — on a single machine. See
[deploy/compose/README.md](compose/README.md) for the quick start and
what it proves. Two caveats from this page apply there too, because a
compose operator may never read the sections above:

- **Multi-replica requires `queue.driver=redis`.** The compose stack
  exists specifically to exercise this; its Redis has no persistence, so
  it is not a production substitute.
- **The rate limiter is per process.** `rate_limit_rps` must be budgeted
  `global_quota / replicas`. The local stack ships `rate_limit_rps: 2`
  per replica for its two replicas, against an Azure F0 quota of 5 RPS.

Two example scalers ship here:

- `keda-scaledobject.yaml` — KEDA prometheus trigger (recommended).
- `hpa-custom-metrics.yaml` — plain HPA via a Prometheus adapter, if you
  already run one.

Both target **queue depth per replica**: scale up when
`vismod_queue_depth / replicas` exceeds ~10, floor of 1 replica.

## Rate limits when scaling out (read this)

The token-bucket rate limiter is **per process**: N replicas × a
per-process limit can overshoot the vendor quota by N×.

Options, in order of simplicity:

1. **Budget it**: set each replica's `adapter.options.rate_limit_rps` to
   `global_vendor_quota / maxReplicaCount`. Simple, slightly wastes quota
   when scaled below max. (Example: Azure S0 = 100 RPS, maxReplicas 10 →
   `rate_limit_rps: 10`.)
2. **Shared limiter**: put a Redis-backed limiter in front of the vendor
   call (e.g. a Lua token bucket on the same Redis instance as the
   queue). Not shipped in v1; the `moderate.Limiter` seam is where it
   plugs in.

If you overshoot, adapters back off on 429 and jobs retry with bounded
backoff — safe, but wasteful; prefer budgeting.

## Graceful drain on deploy

The container handles `SIGTERM` (Kubernetes default stop signal) by
stopping intake, finishing in-flight jobs within `queue.drain_timeout`,
and leaving anything unfinished in its own Redis processing list
(`<prefix>:processing:<instance>`). Set
`terminationGracePeriodSeconds` > `queue.drain_timeout`.

That list is **per replica**, and it is a claim rather than a queue. A
replica reclaims only its own list at startup; work left by a replica
that has stopped heartbeating (`<prefix>:instances`) is returned by the
reaper on another replica, within about a minute.

This matters for autoscaling: when the processing list was a single
shared key, every replica start moved the whole thing back to pending, so
a scale-up, a rolling deploy, or a crashlooping pod re-queued jobs other
replicas were actively running — paying for the vendor call and the
webhook POST twice, and looking exactly like ordinary at-least-once
redelivery. Upgrading from a version that used the shared key: leftovers
there are registered at startup and reclaimed once they go stale, but the
adoption has timing edges and has never run against a real Redis — read
[docs/upgrading.md](../docs/upgrading.md) before a rolling upgrade.
