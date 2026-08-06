---
title: Scaling and observability
nav_order: 8
---

# Scaling out

Each replica runs a fixed pool of `queue.workers` goroutines. There is no
autoscaling inside a process — horizontal scale is replica count, driven
by the **`vismod_queue_depth`** gauge.

KEDA `ScaledObject` and HPA examples, the drain-on-deploy contract, and
rate-limit budgeting live in
[deploy/README.md](https://github.com/matthupy/vismod/blob/main/deploy/README.md).

## Queue driver

**The memory queue driver is not for production intake.** `memq` is
single-process, non-durable, and at-most-once: a crash loses queued and
in-flight jobs. `serve` warns about this at boot and in `/readyz`.

Production and any multi-replica deployment require
`queue.driver: redis` — durable, at-least-once, crash-recovering.

## In-flight work is claimed per replica

A dequeued job is not gone from Redis — it moves to that replica's own
**processing list**, `<prefix>:processing:<instance>`, and is deleted only
on ack. The instance id is random per process, so no two replicas, and no
restarted pod, ever share a list.

Liveness is a heartbeat into `<prefix>:instances`. A replica refreshes it
about every 10s; a **reaper** running on every replica sweeps roughly
every 15s and moves work back to pending from any instance whose heartbeat
has been stale for about 60s.

Three consequences worth holding:

- **A replica recovers only its own list at startup.** Everything else
  comes back through another replica's reaper. If the reaper is broken,
  a crashed replica's jobs are stranded *silently* —
  `vismod_processing_depth` is the only thing that will tell you.
- **The reclaim window is a redelivery window.** Work reclaimed from a
  replica that was slow rather than dead gets handled twice. That is
  at-least-once behaving as designed, not a fault.
- **A single replica cannot reap itself.** With one replica, jobs left by
  its own previous life are recovered at startup; with none running,
  nothing reclaims anything.

Upgrading from a release older than 2026-08? See
[Upgrading](upgrading.md).

This model has only been exercised against miniredis, never two real
processes against a real Redis, and the 60s reclaim window is an unmeasured
guess ([UNVERIFIED.md](agent/UNVERIFIED.md)).

## At-least-once means redelivery happens

Plan for it. Sink idempotency is per-process and does not survive a
restart; see [result envelope](result-envelope.md#idempotency-and-where-it-stops).

## FIFO ≠ completion order

Dequeue order equals enqueue order, but with `queue.workers > 1`
completion order is not guaranteed. Strict end-to-end ordering needs
`workers: 1`, or per-key serialization upstream.

## Rate limits multiply with replicas

`moderate.Limiter` is a token bucket **shared across all workers and
frame fan-out within one process** — and only within one process. N
replicas make N buckets, so your effective request rate is N ×
`rate_limit_rps`.

Budget `global_quota / max_replicas` per replica, or put a shared limiter
in front. This has never been validated against a real vendor quota
([UNVERIFIED.md](agent/UNVERIFIED.md)).

For `shieldgemma` the binding constraint is different — it's
`queue.workers × frames.concurrency` against your GPU, not the limiter.
See [supported models](models.md#sizing-and-the-vllm-retry-trap).

# Observability

Prometheus `/metrics` on the metrics port (`:9090` by default):

**Consumer** is who the metric is *for*. Exactly one metric is an
autoscaler input; wiring anything else to a scaler is a misread, not a
tuning choice.

| Metric | Type | Consumer | What it's for |
|---|---|---|---|
| `vismod_queue_depth` | gauge | KEDA/HPA | Pending + delayed retries. The only autoscaling input |
| `vismod_processing_depth` | gauge | Alert | Claimed but not yet acked. **Cluster-wide, reported identically by every replica** — read it with `max`, never `sum` |
| `vismod_deadletter_depth` | gauge | Alert | DLQ growth — alert on any sustained rise |
| `vismod_jobs_total{verdict}` | counter | Alert | Verdict mix; a jump in `error` is a provider problem |
| `vismod_workers_active` | gauge | Alert | Pool saturation; also the qualifier that separates a busy pipeline from a stalled one |
| `vismod_adapter_request_seconds` | histogram | Dashboard | Provider latency |
| `vismod_adapter_errors_total` | counter | Dashboard | Provider failures feeding backpressure |

### Reading the two depth gauges together

`processing_depth` is deliberately excluded from `queue_depth` — scaling on
in-flight work makes the autoscaler chase itself. The cost of that split is
that neither gauge means much alone:

| `queue_depth` | `processing_depth` | Reading |
|---|---|---|
| high | high | Healthy saturation. Add replicas |
| high | ~0 | Nothing is claiming. Suspect Redis auth, a wedged pool, or replicas that never started |
| ~0 | high, not falling | **Jobs are parked.** A failed dead-letter write, or a dead replica whose work no reaper returned. The autoscaler sees an empty queue and scales down on top of them |
| ~0 | ~0 | Idle |

Row 3 is the one that pages. The alert expression for it lives with the
scaler examples in
[deploy/README.md](https://github.com/matthupy/vismod/blob/main/deploy/README.md).

Endpoints:

- **`/healthz`** — liveness.
- **`/readyz`** — readiness. Also flips **not-ready under sustained
  provider failure** (backpressure with hysteresis) so ingress backs off
  instead of black-holing jobs into `error` verdicts. The hysteresis
  matters: a flapping provider must not flap readiness.

The compose stack ships Prometheus and a Grafana dashboard wired to these
metrics —
[deploy/compose/README.md](https://github.com/matthupy/vismod/blob/main/deploy/compose/README.md).
