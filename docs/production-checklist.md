---
title: Production checklist
nav_order: 10
---

# Before you run this in production

Short list. Each item links to the detail.

- [ ] **`queue.driver: redis`.** The memory driver is single-process,
      non-durable, at-most-once — a crash loses jobs. Required for any
      multi-replica deployment.
      → [Scaling](scaling.md#queue-driver)

- [ ] **Downstream consumers dedupe on `job_id`.** Delivery is
      at-least-once and sink idempotency does not survive a restart.
      → [Result envelope](result-envelope.md#idempotency-and-where-it-stops)

- [ ] **Thresholds retuned for the adapter you actually run.** Scores are
      not portable between providers; a threshold carried over from
      another vendor is meaningless.
      → [Supported models](models.md) ·
      [MODEL_LIMITATIONS.md](https://github.com/matthupy/vismod/blob/main/MODEL_LIMITATIONS.md)

- [ ] **Rate limit budgeted as `global_quota / max_replicas`.** The
      limiter is per-process; replicas multiply it.
      → [Scaling](scaling.md#rate-limits-multiply-with-replicas)

- [ ] **One `file` sink path and one audit log path per replica.** Shared
      paths interleave and break the audit chain.
      → [Audit log](audit-log.md#per-replica-chains)

- [ ] **`<log>.head` is backed up and restored with the audit log.** The
      head anchor is what makes tail truncation detectable. Restore the
      log without it and `vismod audit verify` still reports success —
      it has silently degraded to chain-only.
      → [Audit log](audit-log.md#restoring-a-log)

- [ ] **The dev intake is not exposed.** `intake_addr` defaults to
      `127.0.0.1:8080` and has **no authentication**. Put it behind
      something, or don't publish the port.
      → [SECURITY.md](https://github.com/matthupy/vismod/blob/main/SECURITY.md)

- [ ] **Custom ffmpeg workflows reviewed like code.** They are an
      operator-trust boundary. Validation is hard (no shell, single bound
      input, protocol deny-list, output confinement) but a workflow
      change is still a change to what gets executed.
      → [Custom ffmpeg workflows](custom-ffmpeg-workflows.md)

- [ ] **Alerting on `vismod_deadletter_depth` and
      `vismod_jobs_total{verdict="error"}`.** Fail-safe means failures
      become `error` verdicts and pile up for humans — that only works if
      someone is told.
      → [Observability](scaling.md#observability)

- [ ] **Alerting on `vismod_processing_depth` staying non-zero while
      `vismod_queue_depth` is 0.** A crashed replica's in-flight jobs come
      back only through another replica's reaper. If that path fails they
      are stranded silently, and this gauge is the only signal. Read it
      with `max`, not `sum` — every replica reports the same cluster-wide
      number.
      → [Reading the two depth gauges together](scaling.md#reading-the-two-depth-gauges-together)

- [ ] **A human actually reviews the `error` and `flag` queues.** vismod
      is designed to hand work to people. If nothing consumes that
      output, the fail-safe design has bought you nothing.
      → [RESPONSIBLE_USE.md](https://github.com/matthupy/vismod/blob/main/RESPONSIBLE_USE.md)

- [ ] **You've read what is still unverified.** Several adapters have
      never been called against their real API.
      → [UNVERIFIED.md](agent/UNVERIFIED.md)
