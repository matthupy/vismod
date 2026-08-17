# Evaluation harness — design

**Date:** 2026-08-16
**Status:** draft, not yet implemented
**Supersedes:** the deferred "Test harness" follow-up sketched in
`2026-07-31-url-source-and-output-sinks-design.md` §"Follow-up specs, not
this one" (1).

## Goal

Measure how well a *configured* vismod actually moderates, on media the
operator supplies, and then tune the configuration against that
measurement instead of against intuition.

Concretely: run a known corpus through the pipeline once against a real
vendor, capture what the adapter returned, compare against expected
results, report precision / recall / F1, then adjust thresholds and
re-score **against the captured results** — no second billed call — and
repeat until F1 stops improving.

The harness is a measuring instrument, not a test suite. Its dominant
failure mode is not "it fails to run"; it is "it reports a number that
is wrong in a flattering direction". Every decision below is biased
toward a number that is harder to fake, at the cost of convenience —
the same trade `CLAUDE.md` makes for the pipeline itself.

## What changed since the 2026-07-31 sketch

That sketch assumed a harness that "enqueues a corpus of URLs, collects
results via the webhook sink, compares actual verdict to expected
verdict (exact match, verdict only)". Four things about the repo have
moved since, and each one invalidates part of it:

| Then | Now |
|---|---|
| Webhook sink as the collector | A sink error returns `queue.Retry`, which re-runs the **whole job including a fresh billed vendor call**. A flaky network collector bills you repeatedly and biases the corpus toward easy cases. Collect from a `file` (JSONL) sink |
| "Exact match, verdict only" | Verdicts are 4-valued with strict precedence `block > error > flag > allow`, and `error` is a *designed* outcome, not a failure. Verdict-only exact match scores an outage as a clean pass |
| No correlation story | `metadata` shipped (`queue.ValidateMetadata`, ≤4096 B compacted, echoed verbatim on the envelope, never audited, never scoring-relevant). It is the correlation key, and it already exists |
| Nothing to replay | `model_id{adapter, model_version, config_hash}` is stamped on every envelope, so a run is self-identifying and N-run comparison is a group-by |

Also new since then, and load-bearing here: `provider_thresholds`
(off / hybrid / override) means a category is no longer the only tuning
surface; `source.ref_digest`; the head-anchored audit log; and two known
rollup defects (`top_category` decided by adapter emission order on
ties, `confidence` a copy of `max_score`) that the harness must refuse
to treat as ground truth.

## Scope

**MVP**

1. A predefined corpus runs through the pipeline via the supported
   input types (`kind:"file"` and `kind:"url"`, image and video).
2. Results are collected and compared to expected results.
3. Precision / recall / F1 and the companion rates below are reported
   per run.

**Fast-follow, after MVP ships**

4. A threshold optimization loop that reuses ONE captured set of vendor
   results to optimize class threshold configuration.
5. A recommendation engine suggesting configuration changes from run
   results.
6. Run comparison across 2-to-many runs.

Capture/replay (below) lands in **MVP**, not with (4). It is what makes
(4) buildable at all, and without it even MVP re-billing is wasteful:
one corpus pass against a vendor should be reusable forever.

**Non-goals**

- Shipping media. The repo carries **no corpus, no media bytes, and no
  hashes of media**. See "Corpus of record".
- Cross-vendor quality comparison at score level. Microsoft severity/6,
  Google likelihood buckets and Hive head probabilities are not the same
  quantity (`MODEL_LIMITATIONS.md`). Comparing runs across adapters is
  permitted at *verdict* level only, and the report says so.
- Auto-applying tuning. Recommendations are patches a human applies.
  Auto-correcting a safety system's thresholds contradicts
  `RESPONSIBLE_USE.md`.
- Replacing the unit suite. `go test ./...` still runs with no network
  and no credentials; this is a separate binary and a separate loop.

## Shape

```
                     ┌─────────── billed, once per corpus ──────────┐
corpus manifest ──▶ vismod-eval run ──▶ scan | POST /jobs ──▶ pipeline
   (operator)            │                                        │
                         │                          frames ──▶ CapturingModerator
                         │                                        │      │
                         │                                   adapter ──▶ VENDOR
                         │                                        │      │
                         │                          thresholds ◀───┘   cassette
                         │                          rollup             (key: sha256
                         │                          file sink           of frame bytes)
                         ▼                              │
                    run record  ◀───── scorer ◀─── results.jsonl
                         │
        ┌────────────────┴───────────────┐
        ▼                                ▼
  vismod-eval optimize            vismod-eval compare
  (replays cassette, no          (group-by model_id +
   vendor calls, sweeps           corpus digest)
   thresholds, maximizes F1)
```

## Decisions

**D1. Capture at the `moderation.Moderator` seam.** A
`CapturingModerator` decorator records the adapter's `NormalizedResult`
(plus `Raw`) on the way out; a `ReplayModerator` serves them back.

Rationale: an `http.RoundTripper` cassette cannot cover google (gRPC,
not REST) and multiplies keys on shieldgemma (one request *per policy*
per frame). The Moderator seam is uniform across all four adapters, and
it captures exactly the quantity the optimizer needs — per-frame,
per-category scores *before* thresholds are applied.

Accepted cost, stated plainly: **replay does not re-exercise
`normalize`.** A vendor class-map regression is invisible to a replayed
run. That path stays covered by the per-adapter golden fixtures in the
unit suite. The harness measures configuration quality, not wire-format
correctness.

**D2. Key the cassette on SHA-256 of the frame bytes handed to the
adapter**, not on the source file or `ref_digest`. ffmpeg frame counts
vary between runs of the same video and dHash dedup then removes a
different set each time, so a file-keyed cassette will not line up on
replay. A byte-identical frame is a cassette hit no matter which run
extracted it.

**D3. A cassette miss is a hard error.** Never a silent live call. Live
vendor traffic requires an explicit `--billed` flag, and every run
record prints the number of billed calls it made.

**D4. Ship as `cmd/vismod-eval`, a separate binary in the same module.**
The shipped `vismod` command surface does not grow, the replay moderator
is not reachable from `internal/cli`'s composition root, and the eval
tooling stays out of the production image.

**D5. Collect from a `file` JSONL sink. Never a webhook.** See the table
above — webhook failure means `queue.Retry` means a re-billed re-run.

**D6. `metadata` carries case identity only, never labels.**
`{"vismod_eval":{"case_id":…,"manifest_sha256":…,"run_id":…}}`. Putting
expected labels in metadata would make every result row self-certifying
and would tempt a future contributor across the "metadata never
influences a verdict" invariant. Labels stay in the corpus of record;
`case_id` is the join key.

**D7. `top_category` and `confidence` are non-scoreable.** The harness
refuses a manifest that asserts on them, because both are known-defective
(`docs/agent/TASKS.md` 2). It may *report* them for observation.

## Corpus of record

The repo ships **no media and no media hashes**. It ships the manifest
schema, a validator, and the scorer. The operator brings the corpus and
points at it with `--corpus <path>`.

This is not squeamishness. This is CSAM-adjacent trust & safety tooling;
invariant 7 says vismod defines no child-related category, and a
committed corpus of known-harmful media — or a committed index of one —
is a liability that would outlive any benefit. It also keeps the
manifest honest: the operator's corpus is the one their thresholds get
tuned against, not a synthetic proxy that flatters them.

Contributor runnability does not depend on it: the eval package's own
unit tests generate tiny images in-process and drive a fake moderator,
exactly as the rest of the suite already does.

```yaml
# corpus.yaml — operator-supplied, lives outside the repo
version: 1
cases:
  - id: case-0001
    source:
      kind: file           # file | url
      ref: ./media/a.jpg   # resolved relative to the manifest
      media_type: image    # image | video
    expect:
      verdict: block       # allow | flag | block   (never "error")
      categories:          # multi-label; omitted = not asserted
        SEXUAL: block      # allow | flag | block, per category
        VIOLENCE: allow
    label_provenance: human   # human | vendor | derived
    notes: "..."

  - id: case-0002
    source: {kind: url, ref: "https://…/clip.mp4", media_type: video}
    workflows: [scene-detect]      # optional, per-case
    dedup_threshold: 8             # optional, per-case
    expect:
      verdict: allow
```

Rules the loader enforces:

- `label_provenance: derived` (a label produced by a previous vismod
  run) is **loaded but not scored**, and the run record says how many
  cases were excluded for it. Grading a pipeline against its own past
  output reports 100% F1 forever.
- `expect.verdict: error` is rejected. `error` is an outcome of the
  environment, not a property of an asset.
- A category the active adapter cannot emit may appear in `expect`; it
  is reported as *uncovered*, not scored. See M2 below.
- The manifest's SHA-256 is the corpus digest, stamped on every run
  record and on every job's metadata.

## The metric model

This is the part most designs get wrong, so it is specified in full.

**M1. There is no single decision boundary.** Every category has two —
`flag_at` and `block_at` — so the harness reports **two binary
classifiers per category**, not one:

- *flag boundary*: predicted positive when `score >= flag_at`, against
  expected label ∈ {flag, block}.
- *block boundary*: predicted positive when `score >= block_at`, against
  expected label = block.

Precision, recall and F1 are computed at each boundary independently.
A report that gives one precision per category is hiding one of the two
decisions an operator configured.

**M2. Vendor coverage bounds the denominator.** Microsoft emits 4 of the
15 canonical categories; Google 5. Counting its silence on VIOLENCE as a
true negative is free recall it never earned. Each category is reported
in one of three states:

| State | Meaning | In the denominator? |
|---|---|---|
| `supported` | adapter can emit it, corpus asserts on it | yes |
| `unsupported` | adapter cannot emit it | no — reported separately |
| `untested` | adapter can emit it, no case asserts on it | no — reported as a coverage gap |

**M3. `error` is a fourth outcome class, never folded and never
dropped.** The asset-level report is a matrix of expected
{allow, flag, block} × actual {allow, flag, block, **error**}. Cases
whose actual verdict is `error` leave the precision/recall denominators
— there is no decision to grade — and land in a gated **abstention
rate**. A run whose abstention rate exceeds `--max-abstention` fails,
regardless of how good the remaining numbers look. Without this gate a
totally dead pipeline scores perfect precision on the two cases that
happened to work.

**M4. A nil score is unknown, never 0.** `CategoryResult.Score` is
`*float64` and the codebase is explicit that nil means could-not-
evaluate. The scorer asserts pointer-nil distinctly and never lets a
JSON decode coerce it to `0.0`, which would grade an unscorable frame as
confidently benign.

**M5. Provenance carriers are excluded from harm metrics.** MEDICAL,
SPOOF and ANIMATED_SYNTHETIC are documented as not harm signals. They
are reported in their own section and never enter a precision, recall,
F1 or macro average.

**M6. No single headline F1.** The front page of a run report is:

```
cases              412 submitted · 409 collected · 3 MISSING
abstention        4.2% (17 error)          gate: 10.0%  PASS
over-block        1.9% (expected allow -> got block)      8 cases
miss              0.7% (expected block -> got allow)      3 cases
worst category    SELF_HARM   flag F1 0.61 · block F1 0.44
macro F1          flag 0.83 · block 0.79   (over 9 supported categories)
coverage          9 supported · 6 unsupported · 2 untested
```

`missing` is its own number and is a failure: a dead-lettered job
produces no envelope at all, so a case that vanished must never read as
an absence.

**M7. Video instability is a declared metric, not a flake.** Frame
counts vary per extraction, so a video's verdict is not a stable
function of its file. `--repeat N` runs each video case N times and
reports verdict entropy and post-dedup frame-count drift per case.
Asserting a single stable expected verdict on video without this
manufactures noise that will be misread as model drift.

## Run record

One JSON document per run, written next to the results. It is void
unless all four provenance fields are present:

```json
{
  "run_id": "run-2026-08-16-1",
  "corpus_sha256": "…",
  "model_id": {"adapter": "microsoft", "model_version": "…",
               "config_hash": "…"},
  "started_at": "…", "finished_at": "…",
  "billed_calls": 412,
  "cassette": {"path": "…", "hits": 0, "misses": 412},
  "counts": {"submitted": 412, "collected": 409, "missing": 3,
             "excluded_derived": 6},
  "metrics": { "…": "as above" }
}
```

A metric with no provenance is unattributable, and an unbound report can
be edited after a bad run. Binding it to the corpus digest, the
`config_hash` and the adapter identity is what makes a tuning
defensible later — the same reasoning as the audit chain, at a coarser
grain.

## Fast-follows

**(4) Threshold optimization.** Pure arithmetic over the cassette: for
each category, sweep `flag_at` and `block_at` over the observed score
distribution, recompute rollup, recompute M1 metrics, keep the pair that
maximizes that category's F1 subject to `block_at >= flag_at`. Zero
vendor calls per generation. Emits a `thresholds:` patch plus the exact
list of cases each change flips and in which direction — a
recommendation without a blast radius is how an operator ships a
regression confidently.

Optimization is **per category**, which matches how thresholds are
actually configured. A note on the request to "optimize for F1": a
single aggregate F1 target will happily trade SELF_HARM recall for
ALCOHOL_TOBACCO precision and report an improvement. Per-category
optimization avoids that by construction, and `--recall-floor
SELF_HARM=0.9` is available for categories where a miss is not tradeable
at all. Macro F1 is still *reported*; it is just not the objective.

**(5) Recommendation engine.** Reads a run record and proposes: threshold
moves (from 4), `provider_thresholds` entries where one vendor head
drives most of a category's false positives (Hive's negative `no_*`
heads are the known case), `unarmed_labels` candidates, corpus gaps
(`untested` categories), and workflow/`dedup_threshold` changes where
video instability (M7) exceeds a bound. Every recommendation carries its
evidence case list. Nothing is applied.

**(6) Run comparison.** Group concatenated run records by
`model_id{adapter, model_version, config_hash}` + `corpus_sha256`. Same
adapter, different `config_hash` → full metric diff including per-case
verdict flips. Different adapter → **verdict-level diff only**, with an
explicit banner that scores are not comparable across vendors. Cases
that regressed after previously passing are reported as a distinct class
from new failures: "we knew this and broke it again" and "this is new"
are different bugs.

## Open questions

1. **Unlabeled corpora.** The design assumes ground truth exists. For a
   corpus nobody has labeled, the more useful first output may be
   *disagreement surfacing* — rank cases by proximity to a threshold
   boundary and hand a human the 50 that a ±0.05 move would flip,
   harvesting labels as a byproduct of tuning. Deliberately out of MVP
   scope; recorded because it may be the better second thing to build.
2. **Cassette portability.** Whether a captured `NormalizedResult` set
   is shareable between operators (it contains vendor scores for the
   operator's own media) or is strictly local. Default assumption:
   strictly local, never committed.
3. **`serve` vs `scan` as the MVP driver.** `scan` is simpler and
   synchronous; `serve` + `POST /jobs` exercises the queue, backpressure
   and the real production path. MVP should support both and default to
   `scan`; confirm before implementing.

   > **RESOLVED 2026-08-16 — the driver is `serve`; `scan` is out of MVP.**
   > This paragraph's preference for `scan` was written without checking
   > what `scan` can express. Verified against `internal/cli/scan.go`:
   > `--metadata`, `--workflow` and `--dedup-threshold` are all
   > **invocation**-scoped, while the corpus manifest carries all three
   > **per case** — so a distinct `case_id` per case is impossible within
   > one invocation. `scan` also hardcodes `Source{Kind: "file"}`, so it
   > cannot run the `kind:"url"` half of scope item 1 at all. `POST /jobs`
   > is the only intake surface that maps onto the manifest. Two
   > consequences — capture requires an in-process serve stack, and drain
   > detection must be depth-based rather than counting result lines —
   > are recorded in
   > `docs/superpowers/plans/2026-08-16-eval-harness-breakdown.md` §D-A.

## Acceptance criteria (MVP)

- `vismod-eval run --corpus <manifest> --config <cfg>` submits every
  case through the real intake surface, collects from a JSONL file sink,
  and writes a run record with all four provenance fields populated.
- A case that never produces an envelope is counted as `missing` and
  fails the run; it never reads as a pass or an absence.
- A run against a config whose adapter cannot emit an asserted category
  reports that category as `unsupported` and excludes it from every
  denominator. A test asserts the denominator, not just the label.
- Cases returning `verdict:"error"` are excluded from precision/recall
  and counted in a gated abstention rate; exceeding the gate fails the
  run.
- A nil `Score` never becomes `0.0` anywhere in the metric path. A test
  feeds a nil-score fixture and asserts the category is reported unknown,
  not benign.
- `--repeat N` on a video case reports frame-count drift and verdict
  entropy without failing on variation alone.
- A second run with `--replay <cassette>` and no `--billed` flag makes
  zero vendor calls, and its metrics are identical to the captured run
  under an unchanged `config_hash`.
- Changing only `thresholds` in the config and replaying produces a
  different `config_hash`, different metrics, and no vendor calls.
- The repo contains no media and no media hashes. `go test ./...` still
  passes with no network and no credentials.
- `cmd/vismod` gains no subcommand; `internal/cli` gains no reference to
  the replay moderator.
