# Model Limitations

## Scores are NOT comparable across providers

The normalization layer maps every provider into one schema with scores
in [0,1] — but a normalized number's *meaning* depends on where it came
from, recorded in `score_origin`:

| Provider  | Native output                            | Normalization              | `score_origin`    |
|-----------|------------------------------------------|----------------------------|-------------------|
| microsoft | discrete severity 0/2/4/6 (image scale)  | `severity / 6.0`           | `severity`        |
| google    | likelihood enum (VERY_UNLIKELY…VERY_LIKELY) | configurable lookup, UNKNOWN→null | `likelihood_enum` |
| hive      | per-head class probability               | probability as-is          | `probability`     |
| shieldgemma | Yes/No token probability, one policy per request | `P(Yes)` renormalized over the Yes/No pair | `probability`     |

Two `probability` origins are still not freely comparable: hive's is a
trained per-head classifier output, shieldgemma's is a language model's
probability of emitting one token under a policy supplied **in the
prompt**. The model card warns ShieldGemma 2 is highly sensitive to that
wording, which makes the prompt text itself verdict-affecting — and it
lives in adapter source, so `config_hash` does **not** cover it. Editing
`policyPrompts` changes what the scores mean with no visible config change.

A Microsoft `0.667` (severity 4, "Medium") is a policy bucket; a Hive
`0.667` is a model probability; a Google `0.75` is "LIKELY" pushed
through a lookup table. **They are different quantities that happen to
share a range.**

Consequences:

- **Thresholds are per-adapter and NOT portable.** Retune
  `thresholds.*` whenever you switch `adapter.name`. The `config_hash`
  stamped on every envelope exists so you can tell which tuning produced
  which verdict.
- **Per-provider-label thresholds are less portable still.** The optional
  `provider_thresholds` section binds boundaries to a vendor's own head
  names (`illicit_injectables`, `Sexual`). They survive exactly as long as
  the vendor's label list does: a rename does not error, it just stops
  matching, and the category threshold quietly takes over again. Category
  thresholds degrade gracefully across a vendor change; label thresholds
  do not degrade, they evaporate. Prefer category thresholds unless a
  specific head genuinely needs its own boundary.
- Never aggregate, average, or rank scores across providers.
- `MEDICAL`, `SPOOF` and `ANIMATED_SYNTHETIC` are provenance carriers,
  **not harm signals**. They describe what kind of image this is, not
  whether it is harmful.

### `label_only`: a PROPOSED origin, not yet emitted

No shipped adapter emits it and the constant does not exist in
`pkg/moderation` yet. It is recorded here because its non-comparability
is the whole reason it has to be a separate origin.

Some open-weight classifiers (e.g. `meta-llama/Llama-Guard-4-12B`) return
a **label** — "unsafe, categories S9 and S12" — and no magnitude at all.
Normalizing that into the schema means manufacturing a score: `1.0`
fired, `0.0` did not fire, `nil` not evaluated. A `score_origin` of
`label_only` would mark exactly that:

> **fired / did not fire — the magnitude carries no information.**

It is non-comparable with `probability`, `severity`, `likelihood_enum`
and `confidence_pct`, and it is non-comparable in a harsher way than
those are with each other: the others are at least monotone in some
underlying quantity, and `label_only` is not a quantity. Consequences an
operator must understand before enabling such an adapter:

- Any `flag_at` in **(0,1]** behaves identically — an on/off switch, not
  a sensitivity dial. `flag_at: 0` flags *everything*, because the
  comparison is `>=` and unfired heads score `0.0`.
- `block_at` cannot mean "more severe than flag". Only set-vs-unset is
  meaningful.
- Asset-level `max_score` and `confidence` **must not be read as
  confidences.** They are copies of each other and would sit at `1.0`
  whenever any single head fired, and `top_category` would be decided by
  adapter emission order among the tied `1.0`s. `OverallVerdict` carries
  no origin field to warn a consumer of this. See
  [docs/self-hosted-classifiers.md](docs/self-hosted-classifiers.md);
  this gap is why no label-only adapter has been built.

### Self-hosted `model_version` is an operator claim

For the cloud adapters, `model_version` is vendor-pinned (Azure's
`api-version`, `vision-v1-safesearch`, `visual-moderation-v2`) and
`config_hash` therefore attributes a verdict to a real model identity.

For a self-hosted endpoint (`shieldgemma`) it cannot. The adapter requires
an explicit `model_version` in `adapter.options` and refuses to boot
without one, which forces the operator to state what they are running —
but that is all it is: a claim. vismod never sees the weights, and
an inference server reports whatever name it was started with. A swapped
checkpoint under an unchanged name yields an **unchanged `config_hash`
over changed behaviour** — the audit chain stays internally consistent
while saying nothing true about which weights decided. Operators who need
that guarantee must pin the served model by digest in their own
deployment; vismod can only record the claim.

## Category coverage per vendor

Audited 2026-07-29 against each vendor's current published label list.
The taxonomy is deliberately wider than any single vendor: a category
with no vendor emitting it today is not a defect, it is room for the next
adapter.

Vendors cover very different amounts of it:

| Category | microsoft | google | hive | shieldgemma |
|---|---|---|---|---|
| `SEXUAL` | `Sexual` | `adult` | `general_nsfw`, `yes_sexual_activity`, `yes_realistic_nsfw`, `yes_sexual_intent`, `yes_undressed`, `yes_female_nudity`, `yes_male_nudity`, `yes_genitals`, `yes_breast`, `yes_sex_toy`, `animal_genitalia_and_human` | `sexually_explicit` |
| `SUGGESTIVE_RACY` | — | `racy` | `general_suggestive`, `yes_female_underwear`, `yes_male_underwear`, `yes_bra`, `yes_panties`, `yes_negligee`, `yes_cleavage`, `yes_bulge`, `yes_butt`, `kissing`, `licking` | — |
| `VIOLENCE` | `Violence` | `violence` | `yes_fight`, `yes_animal_abuse` | — |
| `GORE_GRAPHIC` | — | — | `very_bloody`, `a_little_bloody`, `other_blood`, `human_corpse`, `animated_corpse` | `violence_gore` |
| `WEAPONS` | — | — | `gun_in_hand`, `gun_not_in_hand`, `animated_gun`, `knife_in_hand`, `knife_not_in_hand` | — |
| `SELF_HARM` | `SelfHarm` | — | `hanging`, `noose`, `yes_self_harm`, `yes_emaciated_body` | — |
| `HATE` | `Hate` | — | `yes_nazi`, `yes_kkk`, `yes_confederate`, `yes_terrorist` | — |
| `OFFENSIVE_GESTURE` | — | — | `yes_middle_finger` | — |
| `DRUGS` (illicit only) | — | — | `illicit_injectables`, `yes_pills`, `yes_marijuana` | — |
| `ALCOHOL_TOBACCO` | — | — | `yes_alcohol`, `yes_drinking_alcohol`, `animated_alcohol`, `yes_smoking` | — |
| `GAMBLING` | — | — | `yes_gambling` | — |
| `MEDICAL` (provenance) | — | `medical` | `medical_injectables` | — |
| `SPOOF` (provenance) | — | `spoof` | — | — |
| `ANIMATED_SYNTHETIC` (provenance) | — | — | `animated`, `hybrid`, `natural`, `yes_drawing`, `animated_animal_genitalia` | — |
| `OTHER` (fallback) | any future category | — | see below | `dangerous_content` |

microsoft emits exactly four labels and google exactly five; both are
fully mapped, with no label falling to `OTHER`. Hive's head list is an
order of magnitude larger, so the unmapped remainder is where the
judgement lives.

shieldgemma is the narrowest of the four: three prompt-defined policies,
and the operator may enable fewer. `dangerous_content` is mapped to
`OTHER` **by decision, not by fallback**: that single policy covers
weapons manufacture, terrorism promotion, illicit drugs and suicide
instruction in ONE probability, so routing it to `WEAPONS`, `DRUGS` or
`SELF_HARM` would claim a decomposition the model never made. It keeps its
label and score, and — since this adapter requires override mode anyway —
its boundary is set by `label: dangerous_content` regardless of the
category it lands in. If ShieldGemma ever exposes those sub-policies
separately, that is when the mapping changes.

### What lands in `OTHER`, and why

`OTHER` is never a dropped signal: the provider label and score are
preserved, the result flags against the `OTHER` threshold, and per-label
thresholds can target it by name. Every Hive head not in the table above
falls into one of these four groups, each a decision rather than an
oversight:

1. **Negative / absence heads** — `no_*`, `general_not_nsfw_not_suggestive`,
   `no_tongue`, `no_blood`, `no_corpse`, `no_hanging_no_noose`, and the
   rest. These are the complement of a mapped head. Scoring them as harm
   would count the same signal twice, inverted: a high `no_gun` means the
   image is *safer*, and a taxonomy that cannot say that must not pretend
   otherwise.
2. **Ordinary apparel** — `yes_female_swimwear`, `yes_sports_bra`,
   `yes_sportswear_bottoms`, `yes_bodysuit`, `yes_miniskirt`,
   `yes_male_shirtless`. Mapping clothing to `SUGGESTIVE_RACY` is exactly
   the swimwear over-flagging described below. Anatomy and intent heads
   are mapped instead; an operator who genuinely wants beachwear triaged
   can set a per-label threshold rather than making every beach photo
   inherit a harm category.
3. **Child-related heads** — `yes_child_safety`, `no_child_safety`,
   `yes_child_present`. vismod defines no special category for these and
   adds no detection logic of its own. Detection scope belongs to the
   vendor under the vendor's terms; the operator's legal obligations are
   theirs and are not discharged by a category name in this schema. See
   RESPONSIBLE_USE.md.
4. **Context signals with no harm meaning** — `text`, `no_text`,
   `yes_overlay_text`, `yes_qr_code`, `yes_religious_icon`,
   `animal_genitalia_only`, `culinary_knife_in_hand`,
   `culinary_knife_not_in_hand`. Useful for triage, not evidence of harm.
   Note that the text heads are booleans about text *presence* — vismod
   never carries the text itself (see SECURITY.md).

### Adding a category

New `Category` values are additive: consumers are required to treat
unknown values as `OTHER`, so adding one bumps `SchemaVersion` by a minor
version and breaks nothing. Removing or redefining one is a major bump.
A new category needs a vendor label that actually produces it — the
taxonomy describes signals vendors emit, it does not invent detection
vismod then has to perform.

## Null discipline

`score: null` means *could not evaluate* — an unknown enum value or an
unsupported category. It is never collapsed to `0` (which would mean
"confidently safe"). Asset-level `max_score` and `confidence` are null
when no non-null score exists, and an asset with no scorable signal
rolls up to `verdict: "error"`, never `"allow"`.

## Classifier limitations

- **Detection scope is the vendor's.** vismod performs no content
  detection of its own; each adapter surfaces exactly what its vendor's
  API returns, under that vendor's terms and category definitions.
  Special-category detection and protections are handled by each
  scanning vendor.
- **False positives / bias**: visual classifiers systematically
  over-flag some content (medical imagery, breastfeeding, art,
  swimwear; with measured demographic skews) and under-flag others.
  Treat scores as triage signals, tune thresholds per category, staff an
  appeals path.
- **Context blindness**: a frame classifier cannot see narrative
  context (news reporting vs. glorification), consent, or subject age
  with reliability.
- **Adversarial fragility**: crops, filters, borders, and perturbations
  measurably shift scores.

## Video-specific caveats

- Frame extraction is sampling: content between sampled frames is not
  evaluated. `scene-detect` misses slow morphs; `interval` can miss
  single-frame insertions; `keyframe` sees only I-frames.
- The rollup default is any-frame (one blocked frame blocks the asset);
  a partially errored video is `error`, never `allow`.
- FIFO applies to dequeue/start order only; with >1 worker, completion
  order is not guaranteed.
