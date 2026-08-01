---
title: Scaling and observability
nav_order: 5
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

| Metric | Type | What it's for |
|---|---|---|
| `vismod_queue_depth` | gauge | **The autoscaling signal.** KEDA/HPA target |
| `vismod_deadletter_depth` | gauge | DLQ growth — alert on any sustained rise |
| `vismod_jobs_total{verdict}` | counter | Verdict mix; a jump in `error` is a provider problem |
| `vismod_adapter_request_seconds` | histogram | Provider latency |
| `vismod_adapter_errors_total` | counter | Provider failures feeding backpressure |
| `vismod_workers_active` | gauge | Pool saturation |

Endpoints:

- **`/healthz`** — liveness.
- **`/readyz`** — readiness. Also flips **not-ready under sustained
  provider failure** (backpressure with hysteresis) so ingress backs off
  instead of black-holing jobs into `error` verdicts. The hysteresis
  matters: a flapping provider must not flap readiness.

The compose stack ships Prometheus and a Grafana dashboard wired to these
metrics —
[deploy/compose/README.md](https://github.com/matthupy/vismod/blob/main/deploy/compose/README.md).
