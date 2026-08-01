---
title: vismod docs
nav_order: 1
---

# vismod documentation

Open-source visual content moderation pipeline. Scans images and video
frames through one configured vendor classifier, normalizes the wildly
different vendor outputs into a common schema, and runs as a one-shot CLI
or a horizontally-scaled worker.

The governing bias in every design decision: **fail safe and stay
auditable, even at the cost of convenience.** A provider outage, a broken
video, or an unscorable frame yields `verdict: "error"` and human review
— never a silent `allow`.

Start at the [README](https://github.com/matthupy/vismod) for install and
quick start. These pages are the technical detail.

## Reference

| Page | What's in it |
|---|---|
| [Architecture](architecture.md) | Pipeline shape, package map, normalization rules, verdict rollup |
| [Supported models](models.md) | All four adapters: categories, score origins, credentials, limits, verification status |
| [Result envelope](result-envelope.md) | Output schema, sinks, idempotency boundaries, exit codes |
| [Scaling and observability](scaling.md) | Queue drivers, replica scaling, metrics, readiness/backpressure |
| [Audit log](audit-log.md) | Hash chain, verification, tamper-evident scope |
| [Production checklist](production-checklist.md) | The list to walk before going live |
| [Custom ffmpeg workflows](custom-ffmpeg-workflows.md) | Writing and validating extraction workflows |
| [Self-hosted classifiers](self-hosted-classifiers.md) | Which open-weight model to run, and why ShieldGemma |

## Policy and posture

These live at the repository root:

- [SECURITY.md](https://github.com/matthupy/vismod/blob/main/SECURITY.md)
  — trust boundaries, SSRF posture, audit scope
- [RESPONSIBLE_USE.md](https://github.com/matthupy/vismod/blob/main/RESPONSIBLE_USE.md)
  — deployment ethics, human-in-the-loop
- [MODEL_LIMITATIONS.md](https://github.com/matthupy/vismod/blob/main/MODEL_LIMITATIONS.md)
  — why scores are not portable across vendors
- [CONTRIBUTING.md](https://github.com/matthupy/vismod/blob/main/CONTRIBUTING.md)
  — contributor rules and the adapter extension point

## Project state

- [STATUS.md](agent/STATUS.md) — what is in flight
- [TASKS.md](agent/TASKS.md) — what to pick up next
- [UNVERIFIED.md](agent/UNVERIFIED.md) — what is claimed but unproven

---

vismod is free software under
[GPL-3.0-or-later](https://github.com/matthupy/vismod/blob/main/LICENSE).
