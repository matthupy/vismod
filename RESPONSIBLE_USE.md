# Responsible Use

> **This document is not legal advice.** Content-moderation deployments
> intersect with criminal law, privacy law, and platform-liability
> regimes that vary by jurisdiction. Consult counsel before deploying
> vismod in production, and review the terms of service and acceptable-
> use policies of whichever scanning vendor you configure — vendor
> policies constrain what content categories their APIs may be used to
> detect, and compliance with those policies is the operator's
> responsibility.

## What vismod is

An open-source pipeline that runs images and video frames through a
commercial visual-moderation classifier and normalizes the output. It is
a **public good with no commercial goals**, intended for trust & safety
teams and smaller platforms without in-house moderation infrastructure.

vismod itself performs no content detection: all classification is done
by the configured scanning vendor (Azure AI Content Safety, Google Cloud
Vision SafeSearch, or thehive.ai). Detection scope, special-category
handling, and any vendor-side protections are the vendor's, governed by
the vendor's terms; vismod transports, normalizes, and routes their
signals.

## Human-in-the-loop is mandatory

No fully-automated consequential action (account termination, law
enforcement referral, permanent content deletion) should be taken on a
positive classifier signal alone. Route positives and borderline results
to trained human reviewers with wellness support — moderation exposure
is harmful to reviewers, too.

## Media-handling rules the pipeline enforces (and you must not defeat)

- Never persist or transmit flagged media through result/queue/audit
  surfaces: everything downstream of analysis carries hashes, refs, and
  metadata only.
- Per-job working copies (extracted frames) are transient and deleted
  before the job is acknowledged.
- If you build storage for review material: encrypt at rest, minimize
  retention, restrict and log access.
- **Do not test vismod against illegal material.** Use synthetic media
  (`ffmpeg -f lavfi testsrc`) and the credential-free test fakes.

## Fail-safe defaults

A provider outage, an unreadable video, or an unscorable frame yields
`verdict: "error"` and dead-letters the job for human attention — never
a silent `allow`. Keep it that way: the gated
`failsafe.allow_empty_video_skip` override exists for specific
operational situations, is off by default, and writes a prominent audit
event when used.

## Thresholds, false positives, and bias

Classifier scores are probabilistic, provider-specific, and exhibit
demographic and contextual biases (e.g. over-flagging of some skin
tones, medical or breastfeeding imagery, art). Thresholds are fully
configurable per category (`flag_at`/`block_at`) precisely so operators
can tune the precision/recall tradeoff for their community and appeal
volume. Publish an appeals path; measure your false-positive rate.
