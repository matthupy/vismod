# Evaluation harness — remote targets, on-ramp, and output formats

**Date:** 2026-08-18
**Status:** draft, not yet implemented
**Extends:** `2026-08-16-eval-harness-design.md`. That spec stands
unchanged except where a decision below says otherwise, and its
breakdown (`docs/superpowers/plans/2026-08-16-eval-harness-breakdown.md`,
GitHub issues #56–#70) remains the ticket set.

## Goal

Make the harness usable by a trust & safety team, not only by whoever
runs the pipeline. Three capabilities the base spec does not have:

1. Point the harness at a **remote vismod deployment** and get real
   metrics back — to validate a deployment's configuration, scan a
   one-off set of media, or measure a deployment against a golden
   sample.
2. A **low-ceremony corpus**: a list of refs and an optional
   flagged / not-flagged boolean, without writing the full manifest.
3. **Several output formats**, so the run is consumable by an analyst,
   by CI, and by a ticket — not only by a terminal.

The base spec's governing bias carries over verbatim: the dominant
failure mode of a measuring instrument is not "it fails to run", it is
"it reports a number that is wrong in a flattering direction". Every
decision below is biased toward a number that is harder to fake. Remote
mode makes that harder, not easier, because the harness can observe less
— so remote mode's job is to be **explicit about what it cannot know**
rather than to quietly report the same numbers with less behind them.

## What the base spec could not do, and why

Verified against the tree at `main` (2c8e7d1), not inferred:

| Requirement | Blocker |
|---|---|
| Read a verdict back from a remote instance | There is no read side. `POST /jobs` (`internal/cli/serve.go:466`) returns `202 {job_id}`; verdicts go to the **server's** sinks. `docs/rest-api.md` says so plainly: "The verdict is not in the response." No `GET /jobs/{id}` exists |
| Scan a local file on a remote instance | `kind:"file"` `ref` is resolved with `filepath.Abs` against the **server's** cwd (`serve.go:520-528`), and intake never `stat`s it. There is no upload endpoint — bodies are capped at 1 MiB and carry refs only |
| Capture a cassette from a remote run | `CapturingModerator` (#57) wraps `moderation.Moderator` in-process. There is no such seam across a network, and `billed_calls` is therefore not directly countable |
| Express "flagged / not flagged" | The manifest's `expect.verdict` is 3-valued (`allow`/`flag`/`block`). No boolean form exists |
| Emit anything but a run record and a console report | #62 and #63 are the whole output surface |

## The finding that reshapes remote mode

`ResultEnvelope.Result` carries the full `NormalizedResult`
(`pkg/moderation/types.go:145-160`), which carries
`Frames[].Categories[].Score` — a nullable per-frame, per-category score
**before thresholds are applied**. Thresholds are applied downstream, in
`Rollup`.

So a collected set of envelopes is sufficient to recompute rollup and
re-score at different thresholds offline, with no vendor calls. Remote
mode is therefore not second-class for threshold optimization (#67).

There are two capture tiers, and they must never be conflated:

| Tier | Source | Replays | Modes |
|---|---|---|---|
| `cassette` | the `moderation.Moderator` seam (#57) | frame bytes → adapter → `normalize` → thresholds → rollup | local only |
| `envelope` | the collected result envelopes | thresholds → rollup only | local **and** remote |

Neither re-exercises `normalize` — the cost D1 already accepted and
stated. The envelope tier additionally cannot re-exercise frame
extraction or dedup, so it cannot answer a question about a different
`workflows` or `dedup_threshold` setting. It can answer a question about
a different `thresholds` block, which is the tuning loop the base spec
is built around.

**One asymmetry inside the envelope tier, and it is load-bearing.**
`ApplyThresholds` stamps `CategoryResult.Threshold` with the resolved
**`flag_at` only** (`internal/pipeline/rollup.go:18`). `block_at` is
consumed inside `Rollup` and never serialized. So from envelopes alone:

- the **flag boundary** per category (M1) is fully reconstructable —
  score and `flag_at` are both present;
- the **block boundary** per category is **not**, because the threshold
  that produced it is not on the wire;
- the **asset-level** matrix of M3/M4 is unaffected — `overall.verdict`
  is on the envelope, so over-block, miss, and abstention are exact.

Remote mode therefore takes an optional `--target-config <file>`: the
operator supplies the deployment's config so `block_at` can be resolved
through the one resolution path (`config.Thresholds.ResolveFor`).
Without it, per-category block-boundary metrics are reported
`unavailable` — never `0`, never omitted, and never quietly folded into
the flag boundary's numbers. An unavailable metric that renders as a
number is the exact flattering failure this design exists to prevent.

Adding `block_at` to the envelope would remove the asymmetry, but it is
a schema change to a shipped contract for the benefit of a measuring
tool, so it is not proposed here. Recorded as a candidate if remote
tuning becomes a common workflow.

## Decisions

### D8. `serve` gains a read side: the result readback API

This is the only change to shipped vismod behavior in this document.

```
GET  /jobs/{id}     → 200 job status document | 404 unknown or evicted
POST /jobs/status   → 200 {"jobs":[…]}          batch form
```

```json
{"job_id": "job-1754160000000000000",
 "state": "queued|processing|done|dead_lettered",
 "result": { "…": "ResultEnvelope, only when state=done" },
 "reason": "…only when state=dead_lettered"}
```

**Why a status document rather than the bare envelope.** It separates
*not finished yet* from *dead-lettered* from *never existed*. That is
exactly the distinction the base spec requires between a case counted
`missing` (a run failure, M6) and a case counted in the abstention rate
(M3), and it is the one thing neither a webhook nor a tailed JSONL file
can tell you: both are silent in all three states.

**Why it belongs in vismod rather than in the harness.** The alternative
designs all push the cost onto the operator for a worse result. A local
webhook receiver requires reconfiguring the remote deployment before
every run — which defeats the one-off-scan case outright — needs inbound
reachability, and a webhook sink failure returns `queue.Retry`, which
re-runs the whole job including a **fresh billed vendor call** (this is
D5's reasoning, and it applies with more force here). Reading the
remote's sink out of band is deployment-specific and still cannot
distinguish "not finished" from "dead-lettered". Submit-only remote mode
abandons the measurement the harness exists to make.

The read side also closes a gap that predates the harness: the REST API
is currently write-only, and `docs/rest-api.md` §4 has to instruct
operators to `grep` a JSONL file or stand up a webhook receiver to learn
what vismod decided.

**Constraints, all following existing precedent in this repo:**

- **Off by default.** `intake.result_api.enabled: false`. An operator
  opts into disclosure; it is never on because the harness wants it.
- **Auth mirrors the UI exactly** (`internal/ui/ui.go:90-109`): basic
  auth, `subtle.ConstantTimeCompare`, credentials **env-only**
  (`VISMOD_RESULT_API_USER` / `VISMOD_RESULT_API_PASSWORD`, per
  invariant 4), `auth: "none"` permitted and documented as loopback-only.
  With `auth: basic` and unset credentials the endpoint returns `503`,
  as the UI does — never open.
- **Bounded and ephemeral.** An LRU + TTL store, `max_entries` and `ttl`
  configured. Eviction produces `404`, never a soft "unknown but
  probably fine". **It is not durable and it is not the audit log.**
  The sinks and the hash-chained audit log remain the record of what was
  decided; this is a read-side convenience with a stated horizon, and
  the docs say so where an operator will read it.
- **Multi-replica.** Under `queue.driver: redis` a `GET` can land on a
  replica that never processed the job, so an in-memory store would
  return `404` for a job that succeeded — a `missing` count that is a
  routing artifact. The store is therefore Redis-backed when the redis
  driver is configured (`<prefix>:result:<job_id>`, `SETEX` at the
  configured TTL; `redis.UniversalClient` is already held at
  `internal/queue/redisq.go:53,113`), and in-memory under `memq`.
- **No new disclosure of media or vendor payloads.** Invariant 3 already
  keeps `Raw` off envelopes, so the endpoint cannot leak a provider
  payload. It does disclose verdicts, refs, and caller `metadata`;
  `SECURITY.md` gains that trust boundary, and `docs/rest-api.md`
  repeats the existing "do not put secrets in metadata" rule at the new
  read path.
- **Read-only.** No endpoint added here mutates a job, a queue, or a
  configuration.

**Consequence for the harness, in both modes:** drain detection becomes
per-job terminal state rather than depth-based. The base breakdown's
§D-A settled on polling `vismod_queue_depth` **and**
`vismod_processing_depth` to zero, because waiting for N result lines
never terminates when a dead-lettered job produces no envelope. Terminal
state is strictly better: it works remotely (where the metrics endpoint
may not be reachable), and it names *which* cases went missing instead
of reporting only that some did. The depth check is retained as a
secondary signal in local mode, and the `--drain-timeout` backstop is
unchanged.

### D9. Two target modes, with the capability difference made explicit

```sh
vismod-eval run --corpus <manifest> --config <cfg>      # local
vismod-eval run --corpus <manifest> --target https://…  # remote
```

`--config` and `--target` are mutually exclusive. Local mode is
unchanged from the base spec: an in-process serve stack via the ticket-8
seam, a `CapturingModerator`, a JSONL file sink. Remote mode submits to
`POST /jobs` on the target and collects through D8.

| | local | remote |
|---|---|---|
| `kind: url` cases | yes | yes, subject to the **remote's** fetch allow-list |
| `kind: file` cases | yes | yes, if the remote can resolve the path (D10) |
| cassette capture / replay | yes | no |
| envelope-tier re-scoring (#67) | yes | yes |
| asset-level metrics (M3, M4) | yes | yes |
| per-category **flag** boundary (#61) | yes | yes |
| per-category **block** boundary (#61) | yes | only with `--target-config`; otherwise `unavailable` |
| `billed_calls` | exact | **inferred, marked inexact** |
| `model_id.config_hash` | read from local config | read from the target |

Remote mode cannot count billed calls: there is no seam, and a cassette
hit on the target's side is invisible. It therefore reports
`billed_calls: {"value": N, "exact": false}` where N is the count of
cases submitted — an upper bound on assets, not a call count, since a
video bills per frame. A number the harness cannot verify is never
presented as one it can.

**Honesty is enforced in the record, not only in the prose.** A remote
run record carries `target: {"mode":"remote","url":"…"}`,
`replay_tier: "envelope"`, and `cassette: null`. The report renders a
banner. A run record that claims `replay_tier: "cassette"` with
`mode: "remote"` is refused at write, alongside the existing provenance
refusal in #62.

**The target's identity is read from the target, never assumed.** Remote
mode reads `model_id{adapter, model_version, config_hash}` off the
returned envelopes rather than from any local config, because the point
of a deployment-validation run is that the deployment may not be
configured the way the operator believes. If envelopes in one run
disagree on `config_hash` — a rolling deployment mid-run — the run
fails, naming both hashes. Metrics averaged across two configurations
describe neither.

### D10. Local paths against a remote target: submit as-is, canary first

"Local path" means anything the local filesystem can reach, UNC and
mounted network shares included — which is precisely why the path may
resolve identically on both machines. A corpus on a `\\fileserver\corpus`
share or an NFS mount the workers also mount needs no translation at all.

Whether the remote can resolve a given path is a property of the
deployment, and the harness cannot introspect it. So:

- Paths are submitted **as-is** by default.
- `--remote-media-root <local>=<remote>` rewrites a path prefix when the
  two mount points differ (`./media=/data`).
- Before submitting the corpus, remote mode submits **one** `kind:"file"`
  case as a canary and reads it back to a terminal state. If it comes
  back unreadable, the run aborts naming the path, instead of submitting
  the corpus and reporting 100% abstention.

The canary matters because intake does not `stat` the ref
(`serve.go:520-528`): an unreachable path is accepted with `202` and
fails deep in the pipeline as `verdict:"error"`. Without the canary that
surfaces on the report as an abstention gate failure — indistinguishable
from a provider outage, and easily misread as a model problem. It is
also the shape of a cross-platform mistake: a Windows UNC path handed to
a Linux worker mangles through `filepath.Abs` rather than failing
cleanly.

The canary costs one billed call. It is skipped automatically when the
corpus contains no `kind:file` case — there is nothing to probe — and
explicitly with `--skip-canary`. The run record states which of the
three happened, because "the canary passed" and "no canary ran" are
different amounts of evidence.

No upload endpoint is added. It would be the largest new attack surface
in the repo — body limits, disk quota, temp-file lifecycle, a fresh
trust boundary — to serve a case that a shared mount or a URL already
serves.

### D11. A boolean expectation, and a CSV that desugars into a manifest

**`expect.flagged`** joins the manifest as a per-case shorthand,
mutually exclusive with `expect.verdict` within one case:

```yaml
- id: case-0001
  source: {kind: url, ref: "https://…/a.jpg", media_type: image}
  expect: {flagged: true}
```

`true` means the actual verdict is in `{flag, block}`; `false` means
`allow`. It maps onto `OverallVerdict.Flagged`
(`pkg/moderation/types.go:138`), which already exists and already has
exactly this meaning — the harness introduces no new notion of
flaggedness.

**What the shorthand deliberately cannot express**, stated because a
boolean invites the assumption that it can: `error` remains
unexpressible as an expectation, and `flagged: false` does **not** match
an `error` outcome. Error cases leave the precision/recall denominators
and land in the gated abstention rate exactly as M3 requires. A boolean
corpus still fails against a dead pipeline. A boolean also cannot
distinguish `flag` from `block` — two different operator actions — so a
case asserted with `flagged: true` scores at the **flag boundary only**
(M1), and is reported as not asserting the block boundary. It is a
narrower instrument, not a looser one.

**The flat corpus is a converter, not a second input format.** A CSV is
desugared into a v1 manifest, the generated manifest is **materialized
next to the run output**, and *that* file's SHA-256 is the corpus
digest. One loader, one validator, one digest rule, one code path.

```csv
ref,expected_flagged
https://media.example.com/a.jpg,true
/mnt/corpus/b.png,false
./local/c.mp4,
```

- `ref` is required. `kind` is inferred: an `https://` prefix is `url`,
  anything else is `file`. `media_type` is inferred from the extension
  by the same rule intake uses (`mediaTypeFor`).
- `expected_flagged` is optional per row. Empty means **not asserted** —
  the case is scanned and reported, never scored. This is the
  observe-only mode a one-off scan actually wants.
- Ids are assigned `case-0001…` in file order.
- `label_provenance` is `human` for every row. A CSV cannot express
  `derived`, and defaulting to `human` is safe only because `derived` is
  the *excluded* value: a row someone typed by hand is a human label,
  which is what writing it down meant. An operator with derived labels
  writes a manifest.
- `vismod-eval corpus convert list.csv -o corpus.yaml` emits the
  manifest for editing, so the CSV is an on-ramp with a growth path
  rather than a parallel dead end.

Ref resolution follows the manifest rule (#56): relative paths resolve
against the CSV's own directory, and the generated manifest records
resolved paths so the materialized artifact is unambiguous about what
ran.

### D12. A report format registry

`--format` is repeatable and defaults to `console,json`. The registry
mirrors `internal/result/format.go:81` — a
`map[string]func(Options) (Formatter, error)`, an unknown name is an
error, and each formatter names itself and its content type.

| Format | Content |
|---|---|
| `console` | the front page of #63, unchanged. No single headline F1 |
| `json` | the run record of #62, unchanged, provenance-bound |
| `csv` | one row per case: `case_id, ref, kind, expected, actual_verdict, flagged, max_score, <one column per supported category>, error_reason, missing`. What an analyst sorts by disagreement and hands to a reviewer |
| `junit` | one `testcase` per case; a mismatch is a `failure`; the abstention gate and the `missing` count are run-level failures. Makes the harness a CI gate against a staging deployment with no glue code |
| `md` | the console report as a file, for a ticket or an incident doc |

Rules every formatter obeys, tested per formatter rather than once:

- A null score renders `unknown`, never `0` or an empty cell —
  invariant 2, and the existing rule in `internal/result`.
- `top_category` and `confidence` may be rendered, labelled
  non-scoreable, and appear in no metric (D7).
- No format presents a single aggregate F1 as *the* score (M6).
- A remote run's banner appears in every format that has room for it,
  and in the JSON record as structured fields.
- No format emits media bytes, media hashes, provider `Raw`, or a
  secret (invariants 3 and 4).

## Ticket deltas

Amendments to existing issues:

| Issue | Delta |
|---|---|
| #56 corpus load | `expect.flagged` shorthand (mutually exclusive with `expect.verdict`, flag-boundary only); CSV desugar + `corpus convert`; materialized manifest is the digested artifact |
| #59 collect | Collection through the readback API as well as a JSONL file; terminal state distinguishes `missing` from abstention at the source |
| #62 run record | `target{mode,url}`, `replay_tier`, `billed_calls{value,exact}`, `canary`; refusal when `replay_tier: cassette` meets `mode: remote` |
| #63 report | Remote banner; renders through the D12 registry |
| #64 run driver | `--target` remote path; per-job terminal-state drain replacing depth-based; `--drain-timeout` unchanged |
| #66 docs | Remote mode, the readback API, the CSV on-ramp, format list |
| #67 optimize | Accepts the `envelope` tier as input, and states in its output which tier it used. On envelope-tier input without a `--target-config`, it sweeps and recommends `flag_at` only, and says so — it cannot reproduce the current `block_at` it would be moving away from |

New tickets:

- **N1.** Result readback API: `GET /jobs/{id}`, `POST /jobs/status`, the
  bounded store, auth, off-by-default config, `SECURITY.md` and
  `docs/rest-api.md`.
- **N2.** Redis-backed result store so the readback API is correct under
  `queue.driver: redis` with more than one replica.
- **N3.** `vismod-eval run --target`: remote submission, canary,
  `--remote-media-root`, `--target-config` for block-boundary
  resolution, target-read `model_id`, inexact `billed_calls`.
- **N4.** Report format registry: `csv`, `junit`, `md`.

Unchanged: #57, #58, #60, #61, #65, #68, #69, #70.

## Acceptance criteria

Readback API (N1, N2)

- With `intake.result_api.enabled: false` (the default), `GET /jobs/{id}`
  is `404` and no store is allocated.
- `GET /jobs/{id}` returns `state` for a queued, processing, done and
  dead-lettered job, and `404` for an unknown or evicted one. A test
  asserts all five.
- A dead-lettered job returns `state: "dead_lettered"` with its reason
  and **no** `result` — never an envelope, never a verdict.
- `auth: basic` with unset credentials returns `503`, matching the UI.
  Wrong credentials return `401` with `WWW-Authenticate`. Comparison is
  constant-time.
- An evicted entry is `404` and is distinguishable in the harness from a
  job that was never submitted, because the harness holds the `job_id`
  it received at submission.
- No response body contains media bytes, a media hash beyond what the
  envelope already carries, provider `Raw`, or a secret.
- Under `queue.driver: redis` with two replicas, a job submitted to
  replica A and processed by replica B is readable from replica A.
- `POST /jobs` behavior, `serve` boot order and every existing `serve`
  test are unchanged. Existing tests pass **unmodified**.

Remote mode (N3)

- `--config` and `--target` are mutually exclusive; supplying both is a
  usage error.
- A remote run produces the same metric set as a local run over the same
  corpus and the same target configuration, with `replay_tier: envelope`
  and `cassette: null` on the record.
- `billed_calls.exact` is `false` for every remote run and `true` for
  every local one. A record with `mode: remote` and
  `replay_tier: cassette` is refused at write.
- `model_id` is read from the returned envelopes. Envelopes disagreeing
  on `config_hash` within one run fail the run, naming both.
- Without `--target-config`, every per-category **block**-boundary
  metric renders `unavailable` in every format — never `0`, never
  omitted, never folded into the flag boundary. A test asserts the
  rendered value, not just the internal state.
- With `--target-config`, block boundaries are resolved through
  `config.Thresholds.ResolveFor` — the one resolution path — and the
  flag boundary is still taken from the envelope's own `threshold`
  field, so a disagreement between the two is detectable.
- A `--target-config` whose `ConfigHash` does not match the
  `config_hash` on the collected envelopes fails the run, naming both.
  Scoring a deployment against a config it is not running is the
  flattering-number failure in its purest form.
- A canary case that comes back unreadable aborts the run before the
  corpus is submitted, naming the path and the remote.
- The canary is skipped automatically when the corpus contains no
  `kind:file` case, and explicitly with `--skip-canary`. The run record
  states which of the three happened.
- `--remote-media-root ./media=/data` rewrites the prefix of `kind:file`
  refs and leaves `kind:url` refs untouched.
- A `503` from the target is retried with its `Retry-After` and is not
  counted as `missing`; a `400` is a submission failure naming the case.
- Drain completes when every submitted job reaches a terminal state, and
  a `--drain-timeout` names the outstanding cases rather than scoring a
  partial corpus.

Corpus on-ramp (#56 delta)

- A case with both `expect.flagged` and `expect.verdict` is a load
  error naming the case id.
- `expect.flagged: true` scores at the flag boundary and is reported as
  not asserting the block boundary. A test asserts the block-boundary
  denominator excludes it.
- `expect.flagged: false` does not match an `error` outcome; that case
  lands in the abstention rate.
- A CSV row with an empty `expected_flagged` loads, is scanned, is
  reported, and is not scored.
- A CSV corpus materializes a v1 manifest next to the run output, and
  the corpus digest is that file's SHA-256.
- `vismod-eval corpus convert` emits a manifest that the #56 loader
  accepts without modification.
- A CSV with a duplicate ref, a missing `ref` column, or an
  unparseable `expected_flagged` is a load error naming the line number.

Formats (N4)

- Each of `console`, `json`, `csv`, `junit`, `md` renders a run, and an
  unknown format name is an error.
- A null score renders `unknown` in every format. A test feeds a
  nil-score fixture to each formatter.
- The `junit` output fails on the abstention gate and on a non-zero
  `missing` count, not only on per-case mismatches.
- No format emits a single aggregate F1 as the headline number.
- A remote run's inexact `billed_calls` and `replay_tier` appear in every
  format.

Whole-repo gate

- `cmd/vismod` gains no subcommand. `internal/cli` gains no reference to
  any eval, capturing or replay type.
- The repo contains no media and no media hashes; the example CSV points
  at paths that need not exist.
- `go test ./...` passes with no network and no credentials.

## Open questions

1. **Result store TTL default.** Long enough that a slow corpus drains
   before the first results evict, short enough that the store is not a
   shadow result database. A default keyed to `--drain-timeout` is
   attractive but couples an operator setting to a harness one. Deferred
   to N1, where the store's sizing will be concrete.
2. **Whether remote mode should refuse an unauthenticated target.**
   `auth: "none"` is legitimate on loopback, and a harness run against
   `127.0.0.1` is the local-ish case. Refusing a non-loopback target
   with `auth: none` is probably right and is cheap; deferred to N3
   rather than decided here without a deployment to check against.
