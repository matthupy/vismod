# Model & Hash Limitations

> Not legal advice. This document states what vismod's outputs *can* and *cannot*
> be trusted to mean, so operators tune thresholds and act on results responsibly.

## Classifiers are imperfect

Visual-moderation classifiers (e.g. Azure AI Content Safety) produce **false
positives** and **false negatives** and can exhibit **bias** across demographics,
art styles, context, and culture. A score is a model's estimate, **not ground
truth**. Consequential actions must keep a **human in the loop**
(see [RESPONSIBLE_USE.md](RESPONSIBLE_USE.md)).

The precision/recall tradeoff is **tunable** per category via two boundaries:

- `flag_at` → `Verdict=flag`
- `block_at` → `Verdict=block`

Lower thresholds catch more (higher recall, more false positives); higher
thresholds are stricter (higher precision, more false negatives). `SEXUAL` is
strictest by default and additionally carries `potential_csam` (§G.8).

## Scores are within-provider comparable ONLY

vismod normalizes every provider into a `[0.0, 1.0]` score tagged with a
`ScoreOrigin`. **These are not the same quantity:**

| Provider (origin) | Normalization | `ScoreOrigin` |
|---|---|---|
| Azure (severity 0/2/4/6) | `severity / 6.0` | `severity` |
| Hive (per-head softmax) | positive-class mass | `probability` |
| AWS Rekognition | `Confidence / 100.0` | `confidence_pct` |
| Google Vision (likelihood enum) | lookup table; `UNKNOWN → nil` | `likelihood_enum` |
| Hash match | `nil` (binary membership) | `list_membership` |

**🔴 A `0.667` threshold means a different thing for each provider.** A severity
bucket, a softmax mass, a confidence percentage, and a likelihood bucket are not
interchangeable. **Thresholds must be re-tuned per adapter and are not
portable.** vismod does **not** claim cross-provider verdict equivalence.

## Hive normalization specifics

Hive's visual model is a bank of independent **heads** (sub-classifiers). The
`/task/sync` API flattens every head's classes into one list of `{class, score}`
with **no head grouping in the wire format**. The adapter reconstructs structure
from a static taxonomy table (`internal/moderate/adapters/hive/taxonomy.go`) and
reduces it:

- **Per head, positive classes that share a canonical category are summed** —
  they are mutually-exclusive sub-states of "the thing is present"
  (e.g. `gun_in_hand + gun_not_in_hand + animated_gun` = P(gun) for WEAPONS).
- **Across heads, a category takes the max** head mass (gun vs knife both →
  WEAPONS; the more confident wins).
- **One head can map to two categories.** `general_nsfw → SEXUAL` and
  `general_suggestive → SUGGESTIVE_RACY` come from the *same* NSFW head;
  `medical_injectables → MEDICAL` and `illicit_injectables → DRUGS` from the
  injectables head.
- **Negative ("safe complement") classes are dropped.** `no_gun`,
  `general_not_nsfw_not_suggestive`, etc. exist only so a head's scores sum to 1.
  They are structural, **not** harm labels.
- **Descriptive heads are deliberately not emitted as harm.** Image type
  (`natural`/`animated`/`hybrid`), `text`, `qr_code`, `child_present`,
  `religious_icon`, `drawing` carry no harm meaning. This is a documented
  provenance decision, parallel to MEDICAL/SPOOF — not a silent drop.
- **Zero-evidence categories are omitted, never emitted as score 0** (absence ==
  not-detected; see "`nil` is not `0`").
- **Unknown future classes fall back to `OTHER`** with the raw label preserved
  and the score carried — never dropped.

Hive coverage is image-only in v1. Hive's API is video-native (it can sample
frames itself), but the adapter operates on a single image and the pipeline
frames video via videosift, identical to Azure. Native video is a documented
future step via the optional `VideoModerator` interface.

## `nil` is not `0`

A missing or unsupported score serializes as JSON **`null`**, never `0.0`. An
all-`null`/unsupported result **never** yields `allow` — it rolls up to `error`
(could-not-evaluate). Consumers must read explicit `null` and must tolerate
**unknown future `Category` values as `OTHER`**.

## Perceptual-hash evadability (CSAM matcher, v1.1)

The CSAM control is a **perceptual-hash match** (PDQ / TMK+PDQF / vPDQ), shipping
in **v1.1** — v1 has only the seam (`CSAM_HASH_MATCH` + `match_*` fields + a
no-op matcher).

- A hash hit is **binary list-membership** (`Score=nil`), never a `1.0` score and
  never a probability.
- Perceptual hashes are **evadable**: research demonstrates **>99.9%** black-box
  evasion of perceptual-hash matchers (cf. arXiv:2106.09820). A non-match is
  **not** proof of safety.
- Lists only catch **known** material. Novel content is invisible to hash
  matching by construction.
- **PhotoDNA is licensed-access only and is never bundled.** Match lists
  (NCMEC/IWF/GIFCT etc.) require lawful access and are operator-supplied.

## Video aggregation

For non-video-native adapters, video is moderated **frame by frame** via
videosift extraction. Coverage is bounded by `frames.max_frames` and the
extraction strategies; a harmful moment between sampled frames can be missed.
The default rollup flags on **any** flagged frame; a configurable
"min flagged frames" / "N consecutive" policy trades sensitivity for noise. An
extraction failure or zero-frames result is **could-not-evaluate → `error`**,
never `allow` (a static/looping harmful video must not pass by producing no
frames).
