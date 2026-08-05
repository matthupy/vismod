---
title: Supported models
nav_order: 4
---

# Supported scanning models

vismod ships **four** adapters. Exactly one is active per process,
selected by `adapter.name` at startup. vismod loads no model weights and
performs no classification itself — it calls out to the configured
provider and normalizes what comes back.

| `adapter.name` | Provider | Hosting | Score origin | Credential |
|---|---|---|---|---|
| `microsoft` | Azure AI Content Safety | Vendor cloud | `severity` (`severity/6`) | `VISMOD_MICROSOFT_API_KEY` or `VISMOD_MICROSOFT_ACCESS_TOKEN` |
| `google` | Google Cloud Vision SafeSearch | Vendor cloud | `likelihood_enum` | ADC / `GOOGLE_APPLICATION_CREDENTIALS` |
| `hive` | Hive (thehive.ai) visual moderation | Vendor cloud | `probability` | `VISMOD_HIVE_API_TOKEN` |
| `shieldgemma` | `google/shieldgemma-2-4b-it` | **Self-hosted by you** | `probability` | none (endpoint only) |

All four are image-scoring (`SupportsVideo: false`), so video is
frame-extracted by vismod and each frame scored as an image.

`vismod adapters` prints the live registry and each adapter's
capabilities. An unknown `adapter.name` fails at boot and lists the valid
ones — it never falls back.

> **Scores are not portable between rows of this table.** `severity/6`, a
> likelihood bucket, and a head probability are different quantities that
> happen to share the `[0,1]` range. Thresholds are per-adapter and must
> be retuned when you switch. See
> [MODEL_LIMITATIONS.md](https://github.com/matthupy/vismod/blob/main/MODEL_LIMITATIONS.md).

---

## `microsoft` — Azure AI Content Safety

Vendor-hosted REST. Set `adapter.options.endpoint` to your Content Safety
resource.

- **Categories:** `HATE`, `SEXUAL`, `VIOLENCE`, `SELF_HARM` — from the
  provider labels `Hate`, `Sexual`, `Violence`, `SelfHarm`.
- **Scoring:** Azure returns a discrete severity; vismod normalizes to
  `severity / 6.0` and tags `score_origin: "severity"`.
- **Auth:** `auth_mode: key` reads `VISMOD_MICROSOFT_API_KEY`;
  `auth_mode: entra` reads `VISMOD_MICROSOFT_ACCESS_TOKEN`, refreshed by
  an external process (vismod does not perform the token dance).
- **Limits:** max image 4 MB, no batch API, F0 tier is ~5 RPS — budget
  `adapter.rate_limit_rps` accordingly.

## `google` — Cloud Vision SafeSearch

Vendor-hosted, via the official Cloud Vision SDK.

- **Categories:** `adult → SEXUAL`, `racy → SUGGESTIVE_RACY`,
  `violence → VIOLENCE`, `medical → MEDICAL`, `spoof → SPOOF`.
- **Scoring:** SafeSearch returns a five-step likelihood enum
  (`VERY_UNLIKELY … VERY_LIKELY`), mapped to fixed points in `[0,1]` and
  tagged `score_origin: "likelihood_enum"`. This is the coarsest of the
  four — there are only five distinct values a threshold can sit between.
- **Auth:** Application Default Credentials / service account, env-only
  (`GOOGLE_APPLICATION_CREDENTIALS`). Client construction validates
  credentials at boot and fails fast when they're absent.
- **Limits:** max image 20 MB.

## `hive` — thehive.ai visual moderation

Vendor-hosted REST, multipart upload.

- **Categories:** the widest taxonomy of the four — `SEXUAL`,
  `SUGGESTIVE_RACY`, `VIOLENCE`, `GORE_GRAPHIC`, `WEAPONS`, `SELF_HARM`,
  `HATE`, `OFFENSIVE_GESTURE`, `DRUGS`, `ALCOHOL_TOBACCO`, `GAMBLING`,
  `MEDICAL`, `ANIMATED_SYNTHETIC`, `OTHER`.
- **Scoring:** per-head probabilities, `score_origin: "probability"`.
  Hive returns many heads (`general_nsfw`, `yes_sexual_activity`,
  `yes_undressed`, …); the built-in class map folds them onto canonical
  categories, and `adapter.options.class_map` overrides or extends it.
  Any head with no mapping goes to `OTHER` with its label preserved.
- **Auth:** `VISMOD_HIVE_API_TOKEN`.
- **Limits:** max image 20 MB.

## `shieldgemma` — self-hosted, no per-call billing

The one adapter with **no vendor**: you run an inference server (vLLM,
TGI, or anything exposing an OpenAI-compatible chat-completions endpoint)
serving `google/shieldgemma-2-4b-it`, and vismod speaks HTTP to it. No
media leaves your network and there is no per-call cost. Model selection
rationale: [self-hosted classifiers](self-hosted-classifiers.md).

- **Categories:** three trained policies —
  `sexually_explicit → SEXUAL`, `violence_gore → GORE_GRAPHIC`,
  `dangerous_content → OTHER`.
- **Scoring:** each request scores **one** policy and returns the
  probability of the `Yes` token, renormalized over the `Yes`/`No` pair.
  `score_origin: "probability"`. Three policies therefore mean **three
  requests per frame** — size your GPU for 3× the frame rate.
- **Auth:** none. The endpoint is validated against an SSRF guard and may
  not carry credentials in the URL.
- **Required config:** this adapter refuses to construct unless
  `provider_thresholds.mode: override` and an explicit `model_version`
  are set. Probabilities must not inherit thresholds tuned for
  `severity/6` and likelihood buckets — different quantities.
- **The prompt text is verdict-affecting.** ShieldGemma is documented as
  highly sensitive to policy wording; the prompts in the adapter are not
  cosmetic and must not be edited casually.

### Sizing and the vLLM retry trap

In-flight concurrency is bounded by `queue.workers × frames.concurrency`,
**not** by `rate_limit_rps` — that product is what has to fit the GPU.
The limiter is a per-request burst damper only.

The failure mode to avoid: TGI returns `429` under load, which vismod
treats as retryable and backs off correctly. vLLM instead **queues** —
the request doesn't fail, it just gets slower until the client timeout
fires, and a timeout is also retryable, so vismod would retry work the
server is still doing and *add* load. Mitigate by setting
`timeout_seconds` well above the server's observed p99 (a VLM is seconds
per image, not milliseconds) and keeping `max_attempts` small.

---

## Verification status

Honest accounting — the full test suite runs with no network and no
credentials, which means adapter correctness rests on fixtures and vendor
docs, not on live calls:

| Adapter | Evidence |
|---|---|
| `microsoft` | Golden tests over fixture JSON; exercised end to end in compose only to the point of a fail-safe `error` verdict |
| `google` | Golden tests over fixture JSON |
| `hive` | Request/response shapes match Hive's published docs (checked 2026-07-29) and are pinned by tests; **never called against the real API** |
| `shieldgemma` | `httptest` with fixtures this repo authored; **never run against a real inference server** — request shape, response shape, and score derivation are all assumed |

Current open items and what would settle each:
[docs/agent/UNVERIFIED.md](agent/UNVERIFIED.md).

## Adding a model

One adapter package plus golden tests, zero pipeline changes. The
extension point is documented in
[CONTRIBUTING.md](https://github.com/matthupy/vismod/blob/main/CONTRIBUTING.md).
