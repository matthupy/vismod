---
title: Self-hosted classifiers
nav_order: 12
---

# Self-hosted image classifiers as adapter candidates

Research write-up, 2026-07-29. **Status: the ShieldGemma 2 recommendation
below has been implemented** as
`internal/moderate/adapters/shieldgemma`; no adapter exists for any of the
other candidates. Two conclusions needed correcting once code was written —
both are marked inline below (the `unarmed_labels` surface, and requiring
both Yes and No tokens rather than falling back to a lone token). This
decides which open-weight image safety model vismod should
support behind a self-hosted-endpoint adapter, so an operator can scan
against a box they run instead of a cloud provider with usage-based
billing.

## What is already settled

These are design constraints, not conclusions of this document:

- **vismod does not run or load models.** The adapter speaks HTTP to an
  inference server the operator runs separately (vLLM / TGI / Ollama / a
  wrapped ViT). Invariant 7 holds unchanged: the "vendor" is simply
  self-operated. `DoJSON` retry classification and `Limiter` keep their
  existing meaning.
- **The adapter REQUIRES `provider_thresholds.mode: override`.** The
  `default` and per-category thresholds hold severity/6 and
  likelihood-enum magnitudes tuned for the cloud vendors. Override drops
  those rungs of the `ResolveFor` chain, so a self-hosted signal can only
  fire through a `label:<x>` entry the operator wrote deliberately. It
  does **not** remove the need for a `*float64` score — `ResolveFor`
  compares floats, and `rollup.go:19` / `rollup.go:63` compare with `>=`.

## Candidates

### 1. `meta-llama/Llama-Guard-4-12B`

| | |
|---|---|
| Exact model ID | `meta-llama/Llama-Guard-4-12B` |
| Parameters | 12B, dense, natively multimodal (early fusion) |
| License | Llama 4 Community License Agreement |
| Gating | Yes — HF access request; the license additionally requires Meta's written permission above 700M MAU |
| Declared categories | 14, the MLCommons hazard taxonomy: S1 Violent Crimes, S2 Non-Violent Crimes, S3 Sex-Related Crimes, S4 Child Sexual Exploitation, S5 Defamation, S6 Specialized Advice, S7 Privacy, S8 Intellectual Property, S9 Indiscriminate Weapons, S10 Hate, S11 Suicide & Self-Harm, S12 Sexual Content, S13 Elections, S14 Code Interpreter Abuse |
| Output shape | **Label only.** Generates text: `safe`, or `unsafe` followed by a newline-separated list of violated category codes. No per-category numbers. |
| Images per request | Multiple |
| Hardware floor | bf16 weights ≈ 24 GB (2 bytes × 12e9), so a 40–48 GB card with KV cache headroom; a 4-bit quant lands near 7–8 GB and fits a 16 GB card. **Estimated from parameter count — the card states "a single GPU" and bf16 but no VRAM figure.** |

Supersedes `Llama-Guard-3-11B-Vision`. Widest category surface of any
candidate and the closest match to the vendor category sets vismod
already normalizes.

The output is text, so a `*float64` has to be manufactured. Two ways:

- **Manufacture from the label** — 1.0 fired / 0.0 did not fire. Needs
  the new `OriginLabelOnly` origin described below.
- **Read the token logprob** of `safe` vs `unsafe`. vLLM's
  OpenAI-compatible server can return logprobs, so a real probability is
  reachable in principle. It would be a probability over a *decoded
  token*, not a calibrated per-category classifier score, and it is one
  number for the whole verdict — it does not decompose across the 14
  categories that the text names. Recording that as `OriginProbability`
  would claim a comparability it does not have. **Not recommended;** if
  ever taken, it needs its own origin, not `probability`.

### 2. `google/shieldgemma-2-4b-it`

| | |
|---|---|
| Exact model ID | `google/shieldgemma-2-4b-it` |
| Parameters | 4B |
| License | Gemma Terms of Use |
| Gating | Yes — HF access request; must accept Google's usage license |
| Declared categories | 3: sexually explicit, dangerous content, violence/gore |
| Output shape | **Score.** Probability of the `Yes` / `No` token, higher = higher confidence the image violates the supplied policy. **Implementation note (2026-07-29):** the adapter requires BOTH tokens among the reported alternatives and returns `P(Yes)/(P(Yes)+P(No))`. Falling back to a lone token's raw probability would put unnormalized `P(token)` — a different, downward-biased quantity — under the same `probability` origin. A server that reports only one of them makes the frame unscorable: an error verdict, never an allow. |
| Policy supply | **Prompt-defined** — the policy text is inserted into the request template. The card warns the model is highly sensitive to that wording. |
| Reported F1 | 88.6 sexually explicit / 93.7 dangerous content / 85.0 violence & gore (vendor-internal benchmark) |
| Hardware floor | bf16 weights ≈ 8 GB (2 bytes × 4e9), comfortable on a 16 GB card; 4-bit ≈ 3 GB. **Estimated — the card gives bf16 and TPUv5e *training* hardware, no inference VRAM figure.** |

The only candidate that natively yields a continuous score the threshold
engine can consume without inventing a new quantity. One request scores
one policy, so three categories means three calls per frame (or one call
per policy the operator enabled) — that multiplies the per-frame request
count and matters for the `Limiter` discussion below.

### 3. `Falconsai/nsfw_image_detection`

| | |
|---|---|
| Exact model ID | `Falconsai/nsfw_image_detection` |
| Parameters | 85.8M — ViT-base, fine-tuned from `google/vit-base-patch16-224-in21k` |
| License | **Apache-2.0** |
| Gating | None |
| Declared categories | 2 labels, one axis: `normal`, `nsfw` |
| Output shape | **Score.** Two-class softmax; `P(nsfw)` is a real probability. |
| Input | 224×224 |
| Reported accuracy | 98.04% on the author's held-out set (proprietary ~80k-image training set) |
| Hardware floor | fp32 weights ≈ 343 MB. CPU-viable, no GPU required. |

The cheap, unencumbered floor. Differentiates on cost and license
permissiveness, not coverage: one axis can only ever populate `SEXUAL`,
so it can never fill a multi-category `NormalizedResult`. `normal` is a
negative/absence head and must land in `OTHER` under the existing
MODEL_LIMITATIONS rule — scoring it as harm would count the same signal
twice, inverted.

### 4. PerspectiveVision — **disqualified on availability**

| | |
|---|---|
| Model ID | None. No HuggingFace model repo could be located. |
| Base | LoRA fine-tune of LLaVA; the paper does not state the parameter size (7B vs 13B) |
| License | The UnsafeBench **code** is MIT. No license is stated for the **weights**, because no weights are published. The **dataset** is gated on HF (`yiting/UnsafeBench`) and research-use only. |
| Declared categories | 11, from OpenAI's April 2022 content policy: Hate, Harassment, Violence, Self-Harm, Sexual, Shocking, Illegal Activity, Deception, Political, Public and Personal Health, Spam |
| Output shape | Not documented in the paper, site, or repo |
| Reported F1 | 0.810 overall across six datasets; 0.844 on the UnsafeBench test set |

The paper commits to sharing PerspectiveVision "as an open-source tool",
but as of this write-up the artifact behind that sentence is the
benchmark and the dataset, not downloadable classifier weights. An
adapter cannot be written against a model an operator cannot obtain.
Recheck if a weights repo appears.

Also seen during this research and **not evaluated**: SafeVision
([arXiv 2510.23960](https://arxiv.org/pdf/2510.23960)), an image
guardrail claiming policy adherence and explainability. Noted so it is
not lost; no claims made about it here.

## Score vs label, and the new `OriginLabelOnly`

| Candidate | Real per-category `*float64`? | `ScoreOrigin` |
|---|---|---|
| ShieldGemma 2 | Yes — Yes/No token probability | existing `OriginProbability` |
| Falconsai | Yes — two-class softmax | existing `OriginProbability` |
| Llama Guard 4 | **No** — text label only | needs new `OriginLabelOnly` |
| PerspectiveVision | Unknown (no weights) | n/a |

### Proposed `OriginLabelOnly`

```go
// OriginLabelOnly means the provider emitted a LABEL, not a magnitude.
// Score is 1.0 (fired), 0.0 (did not fire), or nil (not evaluated).
// The magnitude carries NO information: a 1.0 is not "more confident"
// than another origin's 1.0, and MaxScore/Confidence computed over
// these values are not confidences.
OriginLabelOnly ScoreOrigin = "label_only"
```

`0.0` here means "the model did not name this category", which is
exactly the existing meaning of 0 ("confidently safe") — so invariant 2
is not weakened. `nil` stays reserved for could-not-evaluate.

**Two rollup defects follow, and both must be settled before any
label-only adapter ships:**

1. `rollup.go:82-86` sets `Confidence` to a copy of `MaxScore`. Under
   `OriginLabelOnly` an envelope would read `confidence: 1.0` whenever
   any single head fired. `OverallVerdict` carries no `score_origin`
   field, so a consumer has nothing to disambiguate it with.
2. `rollup.go:56` breaks ties with strict `>`, so with every fired head
   at exactly 1.0, `TopCategory` becomes "whichever category the adapter
   happened to emit first" — arbitrary, and stable enough to look
   meaningful.

Neither is fixed by documentation. This is why the recommendation below
is not Llama Guard.

### `flag_at` and `block_at` under `OriginLabelOnly`

Scores are drawn from {0.0, 1.0} and both comparisons are `>=`:

- Any `flag_at` in **(0, 1]** is behaviourally identical — fires on 1.0,
  silent on 0.0. The `label:` entry is an **on/off switch**, not a
  sensitivity dial.
- `flag_at: 0` is a **trap**: `0.0 >= 0` is true, so every non-fired head
  flags. A label-only adapter must reject it at boot rather than let the
  operator discover it in production.
- `block_at` **cannot** mean "more severe than flag" when there is no
  magnitude. The only meaningful distinction is set vs unset: `block_at`
  in (0,1] blocks on fire; `block_at` absent means flag-only. Two
  different non-zero numbers for `flag_at` and `block_at` are
  indistinguishable at runtime and should be rejected as a
  misunderstanding, not silently accepted.

## The override disarm risk, and the fix

`internal/config/config.go:69` states it plainly: in override mode a
label with no configured entry has no boundaries at all, so it can never
flag and never block. `validateProviderThresholds` only refuses an
*entirely empty* `labels` map. Override has no `default` net, so a model
taxonomy bump or a single typo — `s13` where `s3` was meant — silently
disarms that hazard. Nothing errors, nothing logs, and the affected
category simply stops being able to block.

**Proposed fix: a boot-time completeness check.**

The adapter declares the label set it can emit at construction, and boot
rejects any declared label that has no key in
`provider_thresholds.labels`.

- The check is on **key presence, not value**. An entry with both fields
  nil means "deliberately unarmed", which is a decision the operator
  wrote down. It also lands in the merged map, so `ConfigHash` changes
  and the decision stays attributable.
  - **Correction from implementation (2026-07-29):** that entry cannot be
    written under `labels` at all. Viper drops a yaml key whose value has
    no scalar leaf, so `some_label: {}`, `some_label:`, `some_label: null`
    and `some_label: {flag_at: null}` all lose the key before decoding
    (all four checked). The shipped surface is therefore a sibling list,
    `provider_thresholds.unarmed_labels: [name, …]`, which `config.Load`
    folds into `Labels` as boundary-less entries. Everything above still
    holds — key presence, merged-map presence, `ConfigHash` coverage — and
    an unarmed label does **not** satisfy override mode's "at least one
    armed label" rule.
- **It cannot live in `config.Load`.** `Load` runs before the adapter
  exists — `wire.go:26` builds the Moderator *from* the loaded config.
  The check belongs in a separate boot step alongside
  `validateFrameBoot`, run after `buildModerator`. `cfg.ProviderThresholds`
  survives `Merge` intact, so the mode and the operator's original keys
  are both still available there.
- The label set should be exposed by an **optional interface**
  type-asserted in `internal/cli`, exactly like `modelVersioner`
  (`wire.go:106-115`) — not by widening `moderation.Moderator`, and not
  by reusing `Caps.Categories`, which holds canonical categories rather
  than provider labels.
- **Negative heads:** the declaration is every label the adapter can
  emit, including `normal`. Requiring an explicit key forces the operator
  to state that a safety head is intentionally unarmed instead of
  inheriting silence.

**Feasibility per candidate:**

- Llama Guard 4 — fixed S1–S14, trivially declarable.
- Falconsai — fixed `normal` / `nsfw`, trivially declarable.
- ShieldGemma 2 — **feasible, and stronger here than elsewhere.** The
  categories are prompt-defined, which means *the adapter authors them*:
  it sends the policy strings, so it necessarily knows its own label set
  and can declare exactly the policies it was configured to send. If the
  policy list ever becomes operator-configurable, the declaration is
  derived from that same list and the property holds.

## Rate limiting a box you own

There is no billing quota to stay under. The ceiling exists to protect
one machine's VRAM and queue depth, which is a **concurrency** problem,
and `moderate.Limiter` is not a concurrency limiter — it is
`NewLimiter(rps)`, FIFO pacing on an interval
(`internal/moderate/ratelimit.go:25-30`).

Recommendation:

- **Bound concurrency with the knobs that already do it.** In-flight
  requests are capped by `queue.workers` × `frames.concurrency`; that
  product, not RPS, is what has to fit the GPU. Size it to the inference
  server's own batch/slot count.
- **Keep the `Limiter` but treat it as a burst damper, not the ceiling.**
  A rate below the box's measured throughput prevents a queue-depth
  spiral; `rps <= 0` disables it entirely, which is defensible for a
  loopback endpoint where the concurrency cap is doing the real work.
- **ShieldGemma multiplies the count:** one request per policy per frame.
  A 3-policy config triples both the RPS and the in-flight count for the
  same frame budget. Whatever number is chosen must be per *request*, not
  per frame.

**On backpressure**, the two common servers behave differently and
`DoJSON` treats them differently:

- **TGI** returns `429` with an overload message. That is retryable
  (`httpx.go:107`), honours `Retry-After`, and otherwise backs off from
  the 2s `rate429Floor`. Correct behaviour, no change needed.
- **vLLM** generally **queues** rather than rejecting. The request does
  not fail, it just gets slower, until the client timeout fires — a
  timeout is also retryable, so vismod would retry a request the server
  is still working on and *add* load. Mitigate with an HTTP client
  timeout set well above observed p99 latency (a 12B VLM is seconds per
  image, not milliseconds) and a small `maxAttempts`. This is a genuine
  amplification risk and belongs in the adapter's package comment.

## The operator-supplied endpoint URL

This would be the first adapter whose host is not vendor-fixed, so the
SSRF posture needs stating precisely — and it is **not** the rule
SECURITY.md already has.

The existing rule ("a URL source MUST sit behind an allow-list that
forbids RFC 1918, 169.254.0.0/16, and loopback") governs **media source
URLs**: attacker-influenceable job input that vismod would fetch. A
self-hosted inference endpoint is the opposite case — it is *expected* to
be loopback or RFC 1918, and forbidding those would forbid the feature.
Conflating the two would either break the adapter or quietly license
SSRF. They must be documented as two separate rules.

Proposed posture for a provider endpoint:

- **Config-only.** The endpoint comes from `adapter.options` in yaml.
  It is never read from a job, a queue payload, or an HTTP intake body.
  This is the load-bearing control: with no request-time URL there is no
  attacker-supplied destination, and the endpoint sits in the same
  operator-trust class as an ffmpeg workflow.
- **Scheme allow-list:** `http` and `https` only.
- **Plaintext only inward.** `http://` permitted for loopback and RFC
  1918 hosts; a public host must be `https`. Credentials (if any) are
  env-only per invariant 4, and must not cross a public network in clear.
- **`169.254.0.0/16` rejected unconditionally** — no legitimate inference
  server lives on the cloud metadata range, and it is the one range where
  a misconfiguration converts into credential theft.
- **No userinfo in the URL** (`http://user:pass@host`) — that is a secret
  in yaml, which invariant 4 forbids outright.
- **Redirects not followed.** Set `CheckRedirect` to error. A redirect is
  a destination vismod did not choose.
- Resolution is at request time, so DNS rebinding is not fully closed by
  a boot-time check; the config-only rule is what makes that acceptable.

Minor, pre-existing, worth noting because a self-hosted server makes it
more likely: `HTTPError.Error()` embeds up to 200 bytes of response body
(`httpx.go:25`), and that string reaches `FrameResult.Error` in the
envelope. For cloud vendors those bodies are terse JSON error objects.
A VLM server's 4xx body can echo part of the request, which for
ShieldGemma means the policy prompt. Never media bytes, so invariant 3
holds, but the adapter should not add anything to that body.

**Does this need its own TASKS entry?** No. It is a package comment, a
URL validator with tests, and one SECURITY.md subsection — small enough
to land inside the adapter commit, and dangerous to land separately
from it. SECURITY.md is being updated now with the *distinction* between
the two URL classes, since that distinction is true today.

## Fail-safe path

Every failure mode must yield a frame error and therefore
`verdict: "error"`, never `allow`. Traced against the current code:

| Failure | Path | Result |
|---|---|---|
| Connection refused (server down) | `client.Do` returns an error → retried → `moderation.Retryable` after `maxAttempts` (`httpx.go:99`) | `analyze:` error → `FrameError` (`pipeline.go:284-290`) → rollup `error` → DLQ |
| CUDA OOM mid-inference | 500 → retryable (`httpx.go:107`) → same as above | same |
| Model not loaded / wrong model name | 404 → **terminal** 4xx, no retry (`httpx.go:72`) | still returns an error from `AnalyzeImage`, so still `FrameError` → `error` |
| 200 with an unparseable body | adapter returns a parse error, or emits nil scores | error, or all-nil → `maxScore == nil` → `error` (`rollup.go:74`) |

**A connection-refused localhost is classified identically to a vendor
5xx** — both are transport/5xx-class retryables that exhaust into
`moderation.Retryable` and land on the same fail-safe path. Confirmed by
reading `DoJSON`; the distinction between "the box is off" and "the
vendor is down" does not exist at this layer, and does not need to.

The one hole no code path can close: a server that returns a
well-formed `safe` for an image it never actually looked at — a
misconfigured policy prompt, a silently swapped model, a stub. That is
indistinguishable from a true negative. It is the strongest argument for
the `modelVersion` handling below.

## `ConfigHash`'s `modelVersion` when the server is self-hosted

`config.ConfigHash(adapterName, modelVersion, thresholds)`
(`config.go:459`) exists so a verdict is attributable to a tuning. Today
`modelVersion` comes from the adapter's own pinned constant —
`"vision-v1-safesearch"`, `"visual-moderation-v2"`, or Azure's
`api-version`. A self-hosted server has no such thing, and the weights
behind it can be swapped with vismod none the wiser.

Proposed, in order of strength:

1. **Require an explicit `model_version` in `adapter.options`.** No
   default. Construction is boot validation (AGENTS.md, adapter
   extension point step 1), so an absent value refuses to boot. This
   makes the operator state what they are running.
2. **Probe the server at boot and refuse a mismatch.** vLLM exposes
   `GET /v1/models`; TGI exposes `GET /info` with `model_id` and
   `model_sha`. Where a sha is available it is the strongest signal
   obtainable and should be preferred as the recorded version.
3. **Nothing else is available.** vismod never sees the weights, so it
   cannot hash them.

The honest framing, which belongs in MODEL_LIMITATIONS.md: for a
self-hosted adapter, `model_version` is an **operator claim**, not a
vendor-pinned identifier. vLLM's `/v1/models` returns
`--served-model-name`, which the operator chose. A swapped checkpoint
under an unchanged name produces an unchanged `config_hash` over
changed behaviour. The audit chain remains internally consistent and
still says nothing true about which weights decided. Operators who need
that guarantee must pin the served model by digest in their own
deployment.

## Recommendation

**Ship `google/shieldgemma-2-4b-it` first.**

- It is the only candidate that yields a real per-category `*float64`
  natively, so it maps onto the existing `OriginProbability` and needs
  **no new `ScoreOrigin`, no rollup change, and no envelope change**. The
  two rollup defects above stay unexercised.
- 4B fits a single 16 GB card at bf16, which is a realistic floor for an
  operator choosing self-hosting to avoid per-call billing.
- Its prompt-defined policies make the boot-time completeness check
  *stronger*, not weaker: the adapter authors its own label set.
- Its three categories map cleanly onto existing taxonomy —
  sexually explicit → `SEXUAL`, violence/gore → `GORE_GRAPHIC`,
  dangerous content → `OTHER` pending a mapping decision at
  implementation time. No new `Category` values, no SchemaVersion bump.

Costs accepted: three categories is far narrower than hive, the Gemma
license is gated and carries use restrictions, and the card's own
warning about policy-wording sensitivity means the exact prompt text
becomes verdict-affecting config that `ConfigHash` should arguably
cover.

**Not chosen, and why:**

- **Llama Guard 4** has the best taxonomy fit and is the right *second*
  adapter, but it is label-only. It needs `OriginLabelOnly`, both rollup
  defects closed, and a 40 GB-class card or a quant. No TASKS entry is
  filed for it: the rollup-origin decision comes first and is not this
  entry's to make.
- **Falconsai** is the easiest thing here to ship — Apache-2.0, no gate,
  CPU-only, real softmax — but one binary axis cannot populate a
  multi-category result. Worth revisiting explicitly as a "no GPU at all"
  option; it is a smaller job than ShieldGemma, not a bigger one.
- **PerspectiveVision** is disqualified: no obtainable weights.
