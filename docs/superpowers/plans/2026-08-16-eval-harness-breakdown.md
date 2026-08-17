# Eval harness — feature breakdown

**Spec:** `docs/superpowers/specs/2026-08-16-eval-harness-design.md`

**This is not an implementation plan.** The other files in this directory are
per-feature, task-by-task plans for one change. This one sits upstream of them:
it decomposes a design spec into the individual features to be tracked and
worked, each of which will get its own implementation plan when it is picked up.

**Ticket store:** GitHub Issues on `matthupy/vismod`. The repository and its
issues are **public** — so issue bodies are subject to the same disclosure rules
as any other file here, and PRs may reference issue numbers freely.

Sizing rule inherited from `AGENTS.md`: one ticket = one task = one commit,
landed green under the done gate. Every ticket states acceptance criteria
checkable without a judgement call; an entry that cannot is not ready to work.

---

## Coverage check — spec's own 10 acceptance criteria → tickets

| Spec AC | Ticket |
|---|---|
| `vismod-eval run` submits every case, collects from JSONL sink, writes run record with 4 provenance fields | 9, 6 |
| A case with no envelope counts as `missing` and fails the run | 3, 4, 9 |
| Unasserted-by-adapter category reports `unsupported`, excluded from every denominator; test asserts the denominator | 5 |
| `error` cases excluded from P/R, counted in gated abstention; exceeding gate fails run | 4 |
| nil `Score` never becomes `0.0` anywhere in the metric path | 2, 4, 5 (cross-cutting) |
| `--repeat N` reports frame drift + verdict entropy without failing on variation | 10 |
| `--replay` with no `--billed` makes zero vendor calls; identical metrics | 2, 9 |
| Threshold-only config change → different `config_hash`, different metrics, no vendor calls | 9 |
| Repo has no media, no media hashes; `go test ./...` passes with no network/creds | every ticket's done gate |
| `cmd/vismod` gains no subcommand; `internal/cli` gains no replay-moderator reference | 8, 9 |

All ten land. Nothing in the spec's MVP scope is unassigned.

---

## Decisions to settle before/alongside the work

### D-A. `scan` vs `serve` as the MVP driver — **RESOLVED: `serve`**

Spec Open Question 3 says: "MVP should support both and default to `scan`;
**confirm before implementing**." Confirmed 2026-08-16: **default to `serve`**,
and the spec's stated `scan` default is overridden.

The spec's preference for `scan` ("simpler and synchronous") does not survive
contact with what `scan` can actually express. Three limits, all verified in
`internal/cli/scan.go`:

1. **`--metadata` is invocation-scoped.** `scanOptions.Metadata` is validated
   once and the same value is stamped on every job in the loop (`scan.go:190`).
   Per-case `case_id` — the join key the whole design rests on (D6) — cannot
   vary within one invocation. That forces one `scan` process per case, which
   is the boot cost that motivated this decision in the first place.
2. **`--workflow` and `--dedup-threshold` are invocation-scoped** for the same
   reason, while the manifest carries both **per case**.
3. **`scan` is file-only.** It hardcodes `Source{Kind: "file"}` and `os.Stat`s
   the path; there is no `kind:"url"` path in it at all. MVP scope item 1
   requires both kinds, so `scan` categorically cannot run half the corpus.

`POST /jobs` (`serve.go:466`) accepts per-job `metadata`, `workflows`,
`dedup_threshold`, and both `kind` values. It is the only intake surface that
maps 1:1 onto the corpus manifest.

Correction worth recording, because it is the intuitive-but-wrong version of
this argument: `scan` does **not** start and stop the pipeline per file. It
takes `<file>...` and builds the moderator and pipeline once, outside its loop.
The per-case boot cost is real only because of limit (1) above.

**Two consequences that shape ticket 8:**

- **Capture requires an in-process serve stack.** `CapturingModerator` wraps
  the `moderation.Moderator`, so `vismod-eval` must run the queue, workers and
  intake itself rather than shelling out to a separately-launched
  `vismod serve`. Otherwise there is no seam to install the decorator and
  `billed_calls` is uncountable. D4 is preserved by making the seam a generic
  `func(moderation.Moderator) moderation.Moderator` hook on the existing
  `cli.newServer` split — `internal/cli` never names the replay type.
- **Completion detection is new work `scan` would not have needed.**
  `ProcessJob` returns inline; `POST /jobs` does not. Waiting for N lines in
  the results file never terminates, because a dead-lettered job produces **no
  envelope at all** — exactly the `missing` case the spec requires to be
  counted and to fail the run. Drain must therefore be depth-based:
  `vismod_queue_depth` **and** `vismod_processing_depth` both zero, plus a
  deadline backstop. `AGENTS.md` deliberately keeps `ProcessingDepth` out of
  `QueueDepth`, so both must be read.

`scan` support is **out of MVP**. If it is ever added it is a convenience path
for single-case debugging, not a corpus driver.

### D-B. Commit the design spec — **RESOLVED: committed**

The spec was written but left untracked, while `docs/superpowers/specs/` already
held three committed specs. A public issue citing `§D2` or `§M3` would have been
unreadable to everyone. Committed alongside this breakdown, matching precedent.

Still open, and smaller: whether issue bodies cite spec sections or restate the
reasoning inline. The ticket bodies below **restate**, because this repo's loop
protocol assumes a ticket is read cold. A ticket that says only "implement §M2"
is unworkable without the spec open.

### D-C. Cassette portability (spec Open Question 2)

Spec default assumption: strictly local, never committed. If accepted as-is,
it becomes a stated decision inside ticket 2 plus a `.gitignore` entry, not a
ticket of its own.

---

## Recommended order

**Walking skeleton first**, then the metric model:

```
1 ──┐
2 ──┼──▶ 3 ──▶ 8 ──▶ 9      (corpus runs, captures, replays, collects)
    │                  │
    │                  ├──▶ 4 ──▶ 5 ──▶ 7   (metrics, then the report)
    │                  ├──▶ 6               (run record)
    │                  ├──▶ 10              (video instability)
    └──────────────────┴──▶ 11              (docs)
```

D-A is resolved, so nothing is blocked. 1 and 2 are independent and can run in
parallel. 8 is a small production-code change in `internal/cli` and is the only
ticket that touches the shipped binary's package.

Rationale: the expensive risk in this design is the billed vendor pass and
whether replay reproduces it. Proving 1→2→3→8 end to end retires that risk
first, and it means the metric model (4/5/7 — where the spec's real care
lives) gets built against actually-captured rows rather than imagined
fixtures.

The alternative — metrics first, driver last — keeps every early ticket a
pure function with no I/O, which is easier to test but yields nothing
runnable until the very end, and risks a scorer shaped around fixtures that
don't match what the pipeline really emits.

---

# MVP tickets

---

## 1. Load and validate an evaluation corpus file

**Context.** The harness measures a configured vismod against media the
operator supplies. The corpus manifest is the operator's ground truth: which
media, and what each is expected to be. The repo ships the schema, the
validator, and the scorer — never the media and never hashes of it.

**Scope — in.** `corpus.yaml` v1 parsing; ref resolution relative to the
manifest; the manifest SHA-256 (the corpus digest); the four loader rules.

**Scope — out.** Fetching or reading any media. Scoring. The driver.

**Acceptance criteria**

Positive
- [ ] A valid v1 manifest loads: per case, `id`, `source{kind, ref, media_type}`,
      `expect{verdict, categories}`, `label_provenance`, optional `workflows`,
      optional `dedup_threshold`, optional `notes`.
- [ ] `source.ref` for `kind: file` resolves relative to the manifest's own
      directory, not the process working directory.
- [ ] The loader returns the manifest's SHA-256 as the corpus digest.
- [ ] `expect.categories` is multi-label; an omitted category is *not asserted*
      and is distinguishable from a category asserted as `allow`.
- [ ] A case with `label_provenance: derived` loads, is marked not-scoreable,
      and is counted so a later run record can report how many were excluded.

Negative / edge
- [ ] `expect.verdict: error` is rejected with an error naming the case id.
      (`error` is an outcome of the environment, not a property of an asset.)
- [ ] A manifest asserting on `top_category` or `confidence` is rejected.
      Both are known-defective (`docs/agent/TASKS.md` entry 2).
- [ ] An unknown `version`, an unknown `source.kind`, an unknown
      `media_type`, an unknown `label_provenance`, or a duplicate case `id`
      is a load error, not a silent skip.
- [ ] A `dedup_threshold` above `frames.dedup.hamming_threshold` is rejected
      at load — a loosened threshold collapses a video to one frame, which
      is a fail-open.

Done gate
- [ ] `gofmt -l .` clean; `go build ./... && go vet ./... && go test ./...` pass.
- [ ] Patch coverage ≥90%; uncovered lines named in the commit message.
- [ ] No media and no media hashes added to the repo. Tests use manifests
      pointing at paths that need not exist.

**Files likely touched.** `internal/eval/corpus/{manifest.go,manifest_test.go}`,
`internal/eval/corpus/testdata/*.yaml`.

**Depends on.** Nothing.

---

## 2. Capture and replay vendor results so a corpus is billed once

**Context.** One corpus pass against a real vendor should be reusable forever.
Capture happens at the `moderation.Moderator` seam — an `http.RoundTripper`
cassette cannot cover google (gRPC, not REST) and multiplies keys on
shieldgemma (one request per policy per frame). The Moderator seam is uniform
across all four adapters and captures exactly what the optimizer needs:
per-frame, per-category scores *before* thresholds are applied.

Stated cost, not a defect: **replay does not re-exercise `normalize`.** A
vendor class-map regression is invisible to a replayed run; that path stays
covered by the per-adapter golden fixtures in the unit suite.

**Scope — in.** `CapturingModerator` decorator, `ReplayModerator`, the on-disk
cassette format, key derivation, hit/miss accounting.

**Scope — out.** Any wiring into `internal/cli`. Threshold sweeping.

**Acceptance criteria**

Positive
- [ ] `CapturingModerator` wraps any `moderation.Moderator` and records the
      returned `NormalizedResult` including `Raw`.
- [ ] The cassette key is SHA-256 of the **frame bytes handed to the adapter** —
      not the source file, not `ref_digest`. (ffmpeg frame counts vary between
      runs of the same video and dHash dedup then drops a different set, so a
      file-keyed cassette will not line up on replay.)
- [ ] `ReplayModerator` returns, for a byte-identical frame, a
      `NormalizedResult` deep-equal to the captured one.
- [ ] The cassette reports hit and miss counts for a run record to carry.
- [ ] `ModelVersion()` and any other optional interface the wrapped moderator
      satisfies are forwarded. (Precedent: `observe.InstrumentModerator`
      silently dropped `ModelVersion()` and stamped `"unversioned"` on every
      `serve` envelope — see `docs/agent/STATUS.md`.)

Negative / edge
- [ ] A cassette **miss is a hard error**. It is never a silent live call.
- [ ] A `*float64` score that was nil when captured is nil when replayed —
      never `0.0`. A round-trip test asserts pointer-nil distinctly.
- [ ] A cassette written by a different adapter than the one replaying is
      refused, naming both.
- [ ] A truncated or malformed cassette file is a load error, not a partial
      replay.

Done gate
- [ ] Done gate as ticket 1.
- [ ] Cassette paths are gitignored; no cassette is committed. Record the
      "strictly local, never committed" default (spec Open Q2 / D-C) in the
      package doc comment.

**Files likely touched.** `internal/eval/cassette/*`, `.gitignore`.

**Depends on.** Nothing (parallel with 1).

---

## 3. Collect run results and join them back to their corpus cases

**Context.** Results are collected from a `file` (JSONL) sink, never a webhook:
a sink error returns `queue.Retry`, which re-runs the whole job **including a
fresh billed vendor call**. A flaky network collector bills repeatedly and
biases the corpus toward easy cases.

The join key is caller `metadata`, which already exists and is already
validated (`queue.ValidateMetadata`, ≤4096 B compacted, echoed verbatim on the
envelope, never audited, never scoring-relevant).

**Scope — in.** The `vismod_eval` metadata contract and its builder; reading a
results JSONL file; joining envelopes to cases by `case_id`; detecting cases
that produced no envelope.

**Scope — out.** Computing any metric. Submitting jobs.

**Acceptance criteria**

Positive
- [ ] The eval metadata shape is
      `{"vismod_eval":{"case_id":…,"manifest_sha256":…,"run_id":…}}` and
      passes `queue.ValidateMetadata` unchanged.
- [ ] A results JSONL file is read into per-case envelopes joined on `case_id`.
- [ ] Cases in the manifest with no matching envelope are reported as
      `missing`, as their own count — distinct from any verdict outcome.
- [ ] Counts are reported as submitted / collected / missing and reconcile.

Negative / edge
- [ ] Metadata carries **case identity only, never expected labels.** A
      builder that would emit a label field does not exist, and a test asserts
      the emitted key set exactly. (Labels in metadata would make every result
      row self-certifying and would tempt a future contributor across the
      "metadata never influences a verdict" invariant.)
- [ ] An envelope whose `manifest_sha256` does not match the loaded manifest
      is refused — a results file from a different corpus never scores.
- [ ] Two envelopes for the same `case_id` are an error, not a last-wins
      overwrite.
- [ ] An envelope with no `vismod_eval` metadata at all is reported as
      unjoinable, not silently dropped.
- [ ] A malformed JSONL line fails the collection with its line number.

Done gate
- [ ] Done gate as ticket 1.

**Files likely touched.** `internal/eval/collect/*`.

**Depends on.** 1.

---

## 4. Score asset-level outcomes, with `error` as its own class

**Context.** The dominant failure mode of a measuring instrument is not "it
fails to run" — it is "it reports a number that is wrong in a flattering
direction". Verdicts are 4-valued with strict precedence
`block > error > flag > allow`, and `error` is a **designed** outcome. Verdict-only
exact match scores an outage as a clean pass.

**Scope — in.** The expected {allow, flag, block} × actual
{allow, flag, block, **error**} matrix; abstention rate and its gate;
over-block and miss rates; `missing` as a run failure.

**Scope — out.** Per-category metrics. Report formatting.

**Acceptance criteria**

Positive
- [ ] The asset-level report is a full matrix of expected {allow, flag, block}
      × actual {allow, flag, block, error}.
- [ ] Over-block rate (expected `allow` → got `block`) and miss rate
      (expected `block` → got `allow`) are each reported with their case counts
      and case id lists.
- [ ] Abstention rate = share of collected cases whose actual verdict is
      `error`, reported with the count.
- [ ] Cases excluded for `label_provenance: derived` are excluded from every
      rate and reported as their own count.

Negative / edge
- [ ] Cases whose actual verdict is `error` **leave the precision/recall
      denominators** — there is no decision to grade — and land only in the
      abstention rate.
- [ ] A run whose abstention rate exceeds `--max-abstention` **fails**,
      regardless of how good the remaining numbers look. A test builds a run
      that is near-totally dead and asserts it fails rather than reporting
      perfect precision on the two cases that worked.
- [ ] `missing` is its own number and is a **failure**. A dead-lettered job
      produces no envelope at all, so a vanished case never reads as an
      absence or a pass. A test asserts the failure, not just the count.
- [ ] A collected set of zero scoreable cases does not report a rate of 0 or
      100 — it fails as unscoreable.

Done gate
- [ ] Done gate as ticket 1.

**Files likely touched.** `internal/eval/score/{asset.go,asset_test.go}`.

**Depends on.** 3.

---

## 5. Score each category at both of its decision boundaries

**Context.** This is the part most designs get wrong. Every category has **two**
decision boundaries — `flag_at` and `block_at` — so the harness reports two
binary classifiers per category, not one. A report giving one precision per
category is hiding one of the two decisions the operator configured.

Vendor coverage also bounds the denominator: microsoft emits 4 of the 15
canonical categories, google 5. Counting a vendor's silence on VIOLENCE as a
true negative is free recall it never earned.

**Scope — in.** Per-boundary precision/recall/F1; the three coverage states;
provenance-carrier exclusion; macro F1 over supported categories.

**Scope — out.** Threshold sweeping (fast-follow). Report formatting.

**Acceptance criteria**

Positive
- [ ] Per category, two independent classifiers are computed:
      *flag boundary* — predicted positive when `score >= flag_at`, against
      expected label ∈ {flag, block}; *block boundary* — predicted positive
      when `score >= block_at`, against expected label = block.
- [ ] Precision, recall and F1 are reported at each boundary independently.
- [ ] Each category is reported in exactly one of three states:
      `supported` (adapter can emit it, corpus asserts on it — in the
      denominator), `unsupported` (adapter cannot emit it — reported
      separately, in no denominator), `untested` (adapter can emit it, no case
      asserts on it — reported as a coverage gap, in no denominator).
- [ ] Macro F1 is reported per boundary, with the count of supported
      categories it averages over.
- [ ] Thresholds are resolved through `ResolveFor(cat, label)` — the one
      resolution path — so `provider_thresholds` (off/hybrid/override) is
      honored without the scorer branching on the mode.

Negative / edge
- [ ] An `unsupported` category is excluded from **every** denominator. A test
      asserts the denominator, not just the label.
- [ ] A nil `Score` is unknown, never `0`. It never satisfies a boundary,
      including a boundary of `0`, and a JSON decode never coerces it to
      `0.0`. A test feeds a nil-score fixture and asserts the category reports
      unknown, not benign.
- [ ] MEDICAL, SPOOF and ANIMATED_SYNTHETIC are excluded from every precision,
      recall, F1 and macro average, and reported in their own section. They are
      documented as not harm signals.
- [ ] A category asserted in the manifest that the active adapter cannot emit
      is reported `uncovered`/`unsupported` rather than failing the load.
- [ ] `block_at < flag_at` in the config under test is surfaced as a
      configuration warning on the report, not silently scored.

Done gate
- [ ] Done gate as ticket 1.
- [ ] No cross-vendor score comparison is computed anywhere. Scores from
      different adapters are not the same quantity (`MODEL_LIMITATIONS.md`).

**Files likely touched.** `internal/eval/score/{category.go,category_test.go}`.

**Depends on.** 3, 4.

---

## 6. Write a run record that binds every metric to its provenance

**Context.** A metric with no provenance is unattributable, and an unbound
report can be edited after a bad run. Binding it to the corpus digest, the
`config_hash` and the adapter identity is what makes a tuning defensible
later — the same reasoning as the audit chain, at a coarser grain.

**Scope — in.** The run record schema and writer, and its refusal to emit an
unprovenanced record.

**Scope — out.** Comparing records (fast-follow 13).

**Acceptance criteria**

Positive
- [ ] One JSON document per run, written next to the results, carrying
      `run_id`, `corpus_sha256`, `model_id{adapter, model_version,
      config_hash}`, `started_at`, `finished_at`, `billed_calls`,
      `cassette{path, hits, misses}`, `counts{submitted, collected, missing,
      excluded_derived}`, and `metrics`.
- [ ] `billed_calls` is the real count of live vendor calls the run made, and
      is printed on every run.

Negative / edge
- [ ] The record is **void unless all four provenance fields are present**
      (`run_id`, `corpus_sha256`, `model_id`, and the timestamps). A missing
      one is a write refusal, not an empty string.
- [ ] `model_version` is never written as `"unversioned"` when the moderator
      can report a real one.
- [ ] The record contains no media bytes, no media hashes, no provider `Raw`,
      no secret, and no caller metadata beyond the eval `case_id`.

Done gate
- [ ] Done gate as ticket 1.

**Files likely touched.** `internal/eval/record/*`.

**Depends on.** 4, 5.

---

## 7. Render a run report with no single headline number

**Context.** A single headline F1 invites the flattering read. The front page
shows the numbers an operator has to act on, each with its own denominator.

**Scope — in.** The front-page renderer and the full report body.

**Acceptance criteria**

Positive
- [ ] The front page renders cases submitted/collected/missing, abstention
      rate with its gate and PASS/FAIL, over-block, miss, the worst category
      with both its F1s, macro F1 per boundary with the supported-category
      count, and the coverage line (supported / unsupported / untested).
- [ ] Every rate is rendered with its absolute count beside it.
- [ ] Null scores render as `unknown`, never `0.00` — matching the existing
      formatter rule in `internal/result`.

Negative / edge
- [ ] No single aggregate F1 is presented as *the* score.
- [ ] A report whose run compared different adapters carries an explicit
      banner that scores are not comparable across vendors, and shows
      verdict-level output only.
- [ ] `top_category` and `confidence` may be rendered for observation but are
      labelled non-scoreable and appear in no metric.

Done gate
- [ ] Done gate as ticket 1.

**Files likely touched.** `internal/eval/report/*`.

**Depends on.** 4, 5, 6.

---

## 8. Let a caller decorate the moderator when building a server

**Context.** D-A settled the driver as `serve`, and `CapturingModerator` wraps
the `moderation.Moderator`. So the eval binary has to run the serve stack
**in-process** — there is no way to install a decorator into a separately
launched `vismod serve`, and `billed_calls` would be uncountable from outside.

That needs one seam in production code. `runServe` is already split into
`cli.newServer(cfg) (*server, error)` (boot wiring) and `(*server).run(ctx)`
(the blocking loop), so the seam is an optional hook on `newServer`.

D4 is preserved by keeping the hook **generic**: a
`func(moderation.Moderator) moderation.Moderator`. `internal/cli` never names,
imports, or references the capturing or replay moderator — it only knows that
a caller may wrap what `buildModerator` returned.

**Scope — in.** The optional decorator hook, applied at the one place the
moderator is constructed. Nothing else.

**Scope — out.** Any eval code. Any change to `vismod serve`'s behavior.

**Acceptance criteria**

Positive
- [ ] `cli.newServer` accepts an optional
      `func(moderation.Moderator) moderation.Moderator`; when supplied it is
      applied to the moderator `buildModerator` returned, before instrumentation
      and before `buildPipeline`.
- [ ] When the hook is absent, `serve`'s wiring, boot order, and behavior are
      byte-for-byte what they were. Existing `serve` tests pass **unmodified**.
- [ ] A decorator that forwards `ModelVersion()` still produces a real
      `model_version` on every envelope, not `"unversioned"`.

Negative / edge
- [ ] `internal/cli` gains no import of, and no reference to, any eval,
      capturing, or replay moderator type. A test or an import-graph assertion
      pins it.
- [ ] `cmd/vismod` gains no subcommand and no new flag.
- [ ] `validateProviderLabelBoot` continues to run against the **unwrapped**
      moderator. (Precedent: the boot check must not depend on what a wrapper
      happens to forward — see `docs/agent/STATUS.md`.)
- [ ] A hook returning nil is a boot error, not a nil-moderator panic.

Done gate
- [ ] Done gate as ticket 1.
- [ ] `AGENTS.md` records the seam and why it is generic.

**Files likely touched.** `internal/cli/serve.go`, `internal/cli/serve_test.go`,
`AGENTS.md`.

**Depends on.** Nothing. (Can land before or alongside 1–3.)

---

## 9. Run a whole corpus through an in-process serve stack

**Context.** `POST /jobs` is the only intake surface carrying per-job
`metadata`, `workflows`, `dedup_threshold` and both `kind` values, so it is the
only one that maps onto the corpus manifest (see D-A). The eval binary runs the
serve stack in-process — queue, workers, intake — so the capture decorator can
be installed and billed calls counted.

The shipped `vismod` command surface does not grow, the replay moderator is not
reachable from `internal/cli`'s composition root, and the eval tooling stays out
of the production image.

**Scope — in.** `cmd/vismod-eval`; the `run` subcommand; booting the in-process
serve stack via the ticket-8 seam; submitting every case; stamping per-case eval
metadata; configuring the JSONL file sink; drain detection; the `--billed` gate.

**Scope — out.** `optimize` and `compare` subcommands (fast-follows). Metric
computation (4, 5) — this ticket produces the results file and hands off.

**Acceptance criteria**

Positive
- [ ] `vismod-eval run --corpus <manifest> --config <cfg>` submits every case
      through `POST /jobs`, collects from a JSONL file sink, and writes a run
      record with all four provenance fields populated.
- [ ] Each case is submitted with its **own** `case_id` in
      `metadata.vismod_eval`, plus its per-case `workflows` and
      `dedup_threshold` when the manifest sets them.
- [ ] Both `kind: file` and `kind: url` cases submit successfully.
- [ ] The run is complete when `vismod_queue_depth` **and**
      `vismod_processing_depth` are both zero, with a `--drain-timeout`
      backstop. (Waiting for N result lines never terminates: a dead-lettered
      job produces no envelope at all — that is the `missing` case.)
- [ ] A second run with `--replay <cassette>` and no `--billed` makes **zero**
      vendor calls, and its metrics are identical to the captured run under an
      unchanged `config_hash`.
- [ ] Changing only `thresholds` in the config and replaying produces a
      different `config_hash`, different metrics, and no vendor calls.
- [ ] `billed_calls` equals the cassette miss count under `--billed`, and is
      zero otherwise.

Negative / edge
- [ ] Live vendor traffic requires an explicit `--billed` flag. Without it a
      cassette miss fails the run rather than calling the vendor.
- [ ] Hitting `--drain-timeout` **fails the run** and reports how many cases
      were still outstanding. It never scores a partial corpus silently.
- [ ] A `503` from intake (backpressure, or operator pause) is retried with the
      returned `Retry-After` and is **not** counted as a `missing` case. A `400`
      is a submission failure naming the case id.
- [ ] The collector is a `file` sink. A `webhook` sink is refused for
      collection, naming the reason: a sink error returns `queue.Retry`, which
      re-runs the whole job including a fresh billed vendor call.
- [ ] `cmd/vismod` gains no subcommand, and `internal/cli` gains no reference
      to the replay moderator. A test or import-graph assertion pins it.
- [ ] The production Docker image does not contain `vismod-eval`.
- [ ] The queue driver is in-process (`memq`); the run does not require Redis.

Done gate
- [ ] Done gate as ticket 1.
- [ ] `go test ./...` still passes with no network and no credentials. The eval
      package's own tests generate tiny images in-process and drive a fake
      moderator, exactly as the rest of the suite already does.

**Files likely touched.** `cmd/vismod-eval/*`, `internal/eval/run/*`,
`Dockerfile`, `AGENTS.md` (architecture map), `CLAUDE.md` (shape of the code).

**Depends on.** 1, 2, 3, 8.

---

## 10. Report video instability instead of mistaking it for model drift

**Context.** Frame counts vary per extraction and dHash dedup then removes a
different set each time, so a video's verdict is **not a stable function of its
file**. Asserting a single stable expected verdict on video without measuring
this manufactures noise that will be misread as model drift.

**Acceptance criteria**

Positive
- [ ] `--repeat N` runs each video case N times.
- [ ] Verdict entropy per video case is reported.
- [ ] Post-dedup frame-count drift (min/max/spread) per video case is reported.

Negative / edge
- [ ] Variation alone does **not** fail the run. A video case whose verdict
      differs across repeats is reported as unstable, not as a miss.
- [ ] `--repeat N` on an image case is a no-op or a refusal — never N billed
      calls for a deterministic input.
- [ ] Repeats reuse the cassette where frames are byte-identical, so N repeats
      do not mean N× the bill for identical extractions.

Done gate
- [ ] Done gate as ticket 1.

**Files likely touched.** `internal/eval/run/*`, `internal/eval/report/*`.

**Depends on.** 9.

---

## 11. Document the eval harness for operators

**Acceptance criteria**

Positive
- [ ] `docs/eval-harness.md` covers: the corpus manifest schema, what the
      operator must supply, capture/replay and the billing model, how to read
      each number on the report, and the abstention gate.
- [ ] It states plainly that replay does not re-exercise `normalize`, so a
      vendor class-map regression is invisible to a replayed run.
- [ ] It states that scores are not comparable across adapters, linking
      `MODEL_LIMITATIONS.md`.
- [ ] It states that recommendations are patches a human applies — nothing is
      auto-applied to a safety system's thresholds (`RESPONSIBLE_USE.md`).
- [ ] `CLAUDE.md`'s doc table and `AGENTS.md`'s architecture map both gain the
      harness.

Negative / edge
- [ ] The doc ships no media, no media hashes, and no example corpus that
      points at real-world harmful content.

Done gate
- [ ] Done gate as ticket 1.

**Depends on.** 9. (Note: `AGENTS.md` requires docs describing changed behavior
to be updated in the **same commit** as the change — so tickets 1–10 each carry
their own doc edits, and this ticket is the standalone operator guide only.)

Also record here, because operators will hit it: the harness drives `serve`,
not `scan`. `scan` cannot express per-case metadata, per-case workflow or
dedup overrides, or `kind: url` sources at all.

---

# Fast-follow tickets (after MVP ships)

Deliberately less refined — the spec defers them, and their acceptance criteria
depend on what MVP's captured data actually looks like.

## 12. `vismod-eval optimize` — sweep thresholds against the cassette

Pure arithmetic over the cassette: per category, sweep `flag_at` and `block_at`
over the observed score distribution, recompute rollup, recompute the
ticket-5 metrics, keep the pair maximizing that category's F1 subject to
`block_at >= flag_at`. **Zero vendor calls per generation.**

Key constraint: optimization is **per category**. A single aggregate F1 target
will happily trade SELF_HARM recall for ALCOHOL_TOBACCO precision and report an
improvement. `--recall-floor SELF_HARM=0.9` is available where a miss is not
tradeable at all. Macro F1 is still reported; it is not the objective.

Emits a `thresholds:` patch plus the exact list of cases each change flips and
in which direction — a recommendation without a blast radius is how an operator
ships a regression confidently.

## 13. Recommendation engine

Reads a run record and proposes: threshold moves (from 12), `provider_thresholds`
entries where one vendor head drives most of a category's false positives
(Hive's negative `no_*` heads are the known case), `unarmed_labels` candidates,
corpus gaps (`untested` categories), and workflow/`dedup_threshold` changes
where video instability exceeds a bound. Every recommendation carries its
evidence case list. **Nothing is applied.**

## 14. `vismod-eval compare` — diff runs

Group concatenated run records by `model_id{adapter, model_version,
config_hash}` + `corpus_sha256`. Same adapter, different `config_hash` → full
metric diff including per-case verdict flips. Different adapter →
**verdict-level diff only**, with an explicit banner. Cases that regressed after
previously passing are a distinct class from new failures: "we knew this and
broke it again" and "this is new" are different bugs.

---

# Icebox

## 15. Disagreement surfacing for unlabeled corpora

Spec Open Question 1. For a corpus nobody has labeled, the more useful first
output may be ranking cases by proximity to a threshold boundary and handing a
human the 50 that a ±0.05 move would flip — harvesting labels as a byproduct of
tuning. Deliberately out of MVP scope; filed so it is not lost, because it may
be the better second thing to build.

---

# Notes on what these ticket bodies deliberately exclude

Recorded now because drafting forced the question; the full policy is still open.

- **No media, no media hashes, no example harmful URLs** — in any ticket, on a
  public tracker, ever. Matches the spec's own non-goal.
- **No vendor credentials, endpoints, or account identifiers.**
- **No verbatim copy of the spec.** Each ticket carries the *reasoning* it needs
  to be worked without the spec open, but the spec stays the single source for
  the full design. This only works if D-B commits it.
- **No `WI-N` or "work item" numbering** — deprecated designation.
- Each ticket restates the *why* in one short paragraph rather than linking
  only. A ticket that just says "implement §M2" is unworkable cold, and this
  repo's loop protocol assumes tickets are read cold.

Still open, for the next pass:
- Whether tickets carry a size/effort estimate at all.
- Whether `docs/agent/TASKS.md` and GitHub Issues both exist, or one replaces
  the other — the repo's loop protocol currently reads `TASKS.md` as the queue.
- Whether PRs reference issue numbers (now trivially yes — public repo, public
  issues) and whether they auto-close via `Fixes #N`.
- Labels: the repo has only GitHub defaults today. An `eval-harness` label plus
  a milestone would make the epic navigable without a parent-issue type.
