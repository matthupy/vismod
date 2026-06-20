# vismod Build Log

Running record of work, decisions, and deviations. Append-only; newest milestone
at the bottom of its section. Source of truth for "where are we and why."

- **Spec:** `visual-moderation-pipeline.prompt.md` (v2, self-contained — "this document wins").
- **Module:** `github.com/matthupy/vismod` · **Binary:** `vismod`
- **Plan:** M0 skeleton → M1 Azure → M2 videosift framing → M3 Docker+metrics → M4 docs+audit+CSAM-divert → M5 Redis/scale.

---

## Environment (verified 2026-06-18)

- Go **1.26.4** (satisfies videosift `go 1.26.3`).
- videosift sibling at `../videosift`, HEAD `35ce653`. Public API (`Extract`, `Config`,
  `Frame`, errors) matches spec §B verbatim. License MIT.
- `ffmpeg` + `ffprobe` **8.1.1** on PATH.
- Policy: track videosift **@latest, never pin**; local `replace => ../videosift` active.

---

## M0 — Runnable skeleton ✅ COMPLETE (2026-06-18)

Full pipeline runs end-to-end with a credential-free `stub` adapter. No network/creds needed.

### Packages built
| Package | Responsibility |
|---|---|
| `pkg/moderation` | Public contract: Verdict/Category/ScoreOrigin/NormalizedResult/Moderator/VideoModerator/HashMatcher. No internal deps. |
| `internal/moderate` | Registry (AdapterConfig/Factory/Register/New) + `stub` adapter (self-registers, deterministic, OTHER fallback). |
| `internal/result` | JobID, ModelIdentity, ResultEnvelope, Sink iface, idempotent JSONL sink. |
| `internal/queue` | Disposition/Job/QueueConfig/Queue iface + `memq` (FIFO chan, real DLQ, bounded retry, panic-recover, graceful drain). |
| `internal/frames` | FrameSource iface + pipeline-owned Frame + fake. (videosift impl = M2.) |
| `internal/hashmatch` | No-op HashMatcher (PDQ/TMK = v1.1). |
| `internal/pipeline` | hashmatch pre-stage → frames → per-frame non-fatal fan-out → normalize → §E rollup → sink. |
| `internal/config` | viper loader, per-category thresholds, ConfigHash. |
| `internal/audit` | append-only hash-chained log + `verify`. (Pipeline wiring = M4.) |
| `internal/observe` | slog + `/healthz` `/readyz`. (Prometheus `/metrics` = M3.) |
| `internal/cli` | cobra: scan/serve/adapters/audit/version. |
| `cmd/vismod` | thin main. |

### Key decisions / deviations
- **Package dependency DAG to avoid cycles:** `moderation` (pure, owns `Source` since
  `VideoModerator` references it) ← `result` (owns `JobID`) ← `queue` ← `pipeline`/`cli`.
- **Queue swap is behavior-preserving via explicit `Disposition`** (Ack/Retry/DeadLetter),
  not a bare error — so memq→asynq (M5) is a true drop-in.
- **memq dead-letters build a fail-safe envelope** (Result nil + Error set) — never `allow`.
- **`serve` ingress (M0):** newline-delimited file paths on stdin → enqueue. HTTP intake later.
  `serve` writes results to `os.Stdout`; **`scan` writes to `cmd.OutOrStdout()`** (cobra
  convention, makes it unit-testable).
- **audit canonicalization:** M0 uses Go `json.Marshal` (stable field order) with
  length-prefixed SHA-256 chain. Spec wants RFC 8785 JCS — **deferred to M4** (noted in code).
- **videosift not yet imported** (no `require` line); `replace` directive in place. Import lands M2.
- **potential-CSAM divert (§G.8)** deliberately deferred to M4 (rollup already flags/blocks SEXUAL).

### Verified live
- `vismod scan x.jpg` → valid envelope, `verdict=block` (SELF_HARM 0.867 ≥ block_at 0.8),
  config_hash stamped, `timestamp_sec:null`.
- `vismod serve` → stdin path → enqueue → process → envelope to stdout; `/healthz`=ok,
  `/readyz`=ready; durability warning logged.
- `vismod adapters` lists stub + caps.

### Known limitations
- `-race` not runnable locally (no gcc/cgo). CI runs it on Linux at M5. Concurrency uses
  atomics + mutex + WaitGroup.
- SIGINT graceful-drain not verified on Windows (MSYS→native signal mapping). Drain logic is
  unit-tested via `queue.Close`; real SIGTERM works in container (M3 `STOPSIGNAL SIGTERM`).

---

## Test coverage pass ✅ (2026-06-18)

Module total **62.4% → 78.3%** (`go test -coverpkg=./...`).

| Package | Coverage |
|---|---|
| moderation, frames, hashmatch, moderate | 100% |
| config | 93.6% · pipeline / observe 85% · stub 83% · audit 80.5% · queue 78.9% · result 75% |
| cli | 46.8% (serve daemon untested by design) · cmd/vismod (main) 0% |

- Added: config (defaults/file/env/validation/ConfigHash), **moderation JSON-null
  serialization contract** (acceptance #6), stub determinism/OTHER/caps, queue
  depth/DLQ-full/required-DLQ, observe readiness toggle, **end-to-end `scan` CLI test**,
  `jobHandler` Ack-vs-Retry.
- **Deliberate gap:** the `serve` daemon main loop (signal/health/queue wiring) + `main()`.
  Signal-injection unit tests are brittle/OS-specific; covered by the live smoke run instead.

---

## Repo + CI setup ✅ (2026-06-19)

Git init + GitHub Actions PR validation. Repo `matthupy/vismod` (**private**).

- **`go mod tidy` run:** original go.mod misclassified cobra/viper/sync as `// indirect`
  and carried an unused `require videosift v0.0.0`. Tidy fixed both (replace directive
  kept; videosift still unimported until M2). Build/vet/race-tests all green post-tidy.
- **`.gitignore`:** added editor/OS junk + `/.claude/` + `coverage.txt`.
- **CI (`.github/workflows/ci.yml`)** — 3 jobs, trigger on PR→main + push→main:
  - `build-test`: tidy-check (`go mod tidy` + `git diff --exit-code`), `go build`,
    `go vet`, `go test -race -covermode=atomic`.
  - `lint`: golangci-lint (v2 cfg, standard set + misspell; std-error-handling preset
    exempts unchecked `.Close()`).
  - `govulncheck`: official `golang.org/x/vuln` scanner.
  - **videosift sibling:** every job checks out `matthupy/videosift` (public) to
    `../videosift` so the local `replace` resolves on the runner.
  - Least-privilege `contents: read`; concurrency cancels superseded runs.
- **`.github/dependabot.yml`:** weekly gomod + github-actions updates.
- Tooling: built-in `go vet`/`gofmt`/`go test`; golangci-lint + govulncheck are the
  de-facto-standard Go lint/vuln tools (widely used, not niche).
- **golangci-lint install via `go install ...@latest`, NOT the prebuilt action:**
  prebuilt releases lag new Go versions — the action's binary (built w/ go1.24)
  refused our go.mod `go 1.26.4` (exit 3). Building from source w/ the runner's
  toolchain always matches. Trade-off: ~1–2 min slower, no lint-result cache.
- **Verified green:** push→main CI run all 3 jobs ✅ (build-test, lint, govulncheck).
- Open nit: actions emit Node20-deprecation warnings (non-fatal); bump to newer
  action majors later.

---

## M1 — Azure AI Content Safety adapter ✅ COMPLETE (2026-06-19)

Second in-tree adapter at `internal/moderate/adapters/azure/`, self-registers via `init()`
alongside `stub`. Switching `adapter.name: azure` selects it at startup, zero call-site changes.
Branch `feat/m1-azure-adapter` (PR, not merged to main directly).

### Files
| File | Responsibility |
|---|---|
| `azure.go` | Factory `New` (fail-fast on missing endpoint/secret), `Moderator` impl, `Caps`, `validateInput` (4 MB cap + MIME allow-list). |
| `client.go` | Direct-REST data-plane client: request/response structs, single-attempt `do`, retry loop with bounded backoff, `classifyHTTPError`. |
| `normalize.go` | Azure response → canonical `[]CategoryResult` (`severity/6.0`, `ScoreOrigin="severity"`, unknown native label → OTHER). |
| `auth.go` | `authProvider` seam: `apiKeyAuth` (default) + `bearerAuth` (zero-dep). |
| `options.go` | Decodes the provider-opaque `adapter.options` map into a typed struct. |
| `*_test.go` + `testdata/` | httptest client tests, golden normalization (`-update`), factory/validate/auth tests. |

### Key decisions / deviations
- **Auth — zero new heavy deps.** Spec §C says "support both" auth modes. Shipped
  `apikey` (default, `Ocp-Apim-Subscription-Key`) + `bearer` (`Authorization: Bearer`).
  Bearer covers Microsoft Entra ID via a token acquired out-of-band (scope
  `…/.default`). **Full `DefaultAzureCredential` / Managed Identity is deliberately
  NOT wired** — it pulls the large `azidentity` SDK tree, violating "add a dep only
  when it earns its place". The `authProvider` interface is the seam to add it later.
- **Secrets (§F.1/§F.2):** env-only via the `AdapterConfig.Secret` accessor.
  `VISMOD_AZURE_KEY` (apikey), `VISMOD_AZURE_TOKEN` (bearer). Endpoint is non-secret —
  read from `options.endpoint` first, else `VISMOD_AZURE_ENDPOINT`. **Boot fails fast**
  if endpoint or the active scheme's secret is missing (verified live via `scan`).
- **Rate limiter (§F.3):** added `golang.org/x/time/rate` (stdlib-adjacent, the standard).
  Token bucket constructed in the Factory, owned by the single adapter, burst=1 so
  aggregate rate == `rps` regardless of `workers × frames.concurrency`. Default 5 RPS (F0).
- **Retry (§F.4):** adapter-internal bounded retry — 429/5xx/408/transport-error →
  retryable (backoff doubles per attempt, honors `Retry-After` delta-seconds on 429);
  other 4xx + unparseable-200 → terminal. `x-ms-error-code` surfaced (header, else body).
  This is the inner safety net; the queue `Disposition` retry is the outer one.
- **Fail-safe (§F.5):** `AnalyzeImage` returns an `error` on any provider/validation
  failure → pipeline records frame `Status=error` → `Verdict=error`, **never allow**.
  Verified live against an unreachable host (DNS fail → exhaust retries → `verdict:error`).
- **api-version** const `2024-09-01`, overridable via `options.api_version`; set as
  `NormalizedResult.ModelVersion` (the §E single source the envelope copies).
- **SSRF (§C):** v1 sends inline base64 `content` ONLY; `blobUrl` not implemented
  (no remote-fetch vector). Documented in `config.example.yaml`; `SECURITY.md` is M4.
- **Dimension cap (2048) NOT enforced in M1.** Azure rejects oversize dimensions itself
  (terminal 4xx, surfaced) and decoding every frame just to measure W/H is wasteful;
  the 4 MB byte cap is the hard pre-flight bound. Pixel-dimension pre-flight deferred —
  low value vs. decode cost. (Re-evaluate if false 4xx noise shows up in practice.)

### Verified live
- `vismod adapters` lists `azure` + `stub` (azure shows a clear init-error when creds absent).
- `vismod scan --config az.yaml x.png` with no creds → fail-fast boot error.
- Same with bogus endpoint → `verdict:error`, `config_hash` stamped, never `allow`.

### Tests
- `go test ./...` all green. Module coverage **79.8%** (`-coverpkg=./...`); azure pkg
  well-covered (normalize/validate/caps 100%, client 79–91%).
- Golden fixtures: `mixed_severity`, `all_safe`, `unknown_category` (proves OTHER fallback +
  `TaskAdherence` is treated as unmapped, never special-cased). `-update` regenerates.
- httptest covers: success+auth-header+api-version, 429→retry→success, 4xx terminal
  (no retry, code surfaced), 5xx exhausts retries, Retry-After parse.

### Deferred to later milestones (unchanged)
- videosift import + video framing = M2. Audit RFC 8785 JCS, docs (SECURITY/RESPONSIBLE_USE),
  potential-CSAM divert = M4. asynq/Redis + `-race` in CI = M5.

---

## NEXT — M2: Video via videosift (§B)
- `FrameSource` impl backed by `videosift.Extract`: absolute caller-owned `cfg.WorkDir`,
  `defer cleanup` on every exit path, map `videosift.Frame`→`frames.Frame`, set non-zero
  `MaxFrames` (config default 64). Boot probe for `ffmpeg`+`ffprobe` (`ErrNoBinaries`).
- Lazy-decode fan-out already in `pipeline.analyzeVideoByFrames` — swap `FakeFrameSource`
  for the real one in `wire.go`.
- Fail-safe: `ErrNoFrames` / `*FFmpegError` → `Verdict=error` (never allow on zero frames).
- Add the `require github.com/matthupy/videosift` line (replace directive already present);
  track @latest, never pin (§B).

---

## M4 — Responsible-use & docs (§G) — DONE

Branch `feat/m4-responsible-use` off `main` (post #6 merge).

### Audit log (§G.5)
- Canonicalization upgraded to **RFC 8785 JCS** (`internal/audit`: `jcs`/`writeCanonical`)
  — object keys sorted lexicographically, compact, `json.Number`-preserving — so
  `audit verify` recomputes byte-identical hashes regardless of Go struct order.
  Length-prefixed `SHA-256(seq‖ts‖prev‖canonical(payload))`, idempotent per job_id
  (all pre-existing). Added `audit.ReadRecords` for inspection.
- **Pipeline wired**: `Pipeline.Audit *audit.Log`; appends after a successful
  `Sink.Write`, binding verdict + `SHA-256(Raw)` + `ModelIdentity` — never `Raw`.
  Append failure → infra error → retry (Sink+audit both idempotent). New
  `audit.path` config key (empty = disabled; OK for one-shot scan). Wired in `wire.go`.

### Potential-CSAM divert (§G.8)
- New `internal/review`: `Diverter` interface + `Item` (carries `SHA-256(frame)` +
  metadata, **never bytes/Raw**) + default `LogDiverter` (WARN event).
- Pipeline trigger in `moderateFrame`: a `SEXUAL` score ≥ `thresholds.SEXUAL.potential_csam`
  diverts the frame BEFORE `Sink.Write`. `jobMeta` threads job identity into the
  fan-out. Default `LogDiverter` wired in `wire.go`. Frames path already never
  persists bytes/Raw, so §G.2 transient-handling holds by construction.

### Docs shipped (§G.9)
- `LICENSE` (Apache-2.0 full text) + `NOTICE` (third-party + CSAM/PhotoDNA notes).
- `RESPONSIBLE_USE.md` (not-legal-advice, no-CSAM-in-v1, potential-CSAM policy,
  NCMEC reporting, "do not test against real CSAM").
- `SECURITY.md` (SSRF/egress, audit tamper-evidence honest scope + signing/anchoring
  seam, anti-abuse residual risk, fail-safe posture).
- `MODEL_AND_HASH_LIMITATIONS.md` (classifier bias/FP, perceptual-hash evadability,
  cross-provider score non-portability, nil≠0).
- `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1 + T&S clause).
- `README.md` status→M4 + responsible-use section; `config.example.yaml` audit block.

### Tests / verification
- TDD: JCS sorted-keys, audit-wiring round-trip + redelivery idempotency, divert
  trigger + below-threshold no-divert, LogDiverter logs hash-not-bytes.
- `go build`/`go vet`/`gofmt -l`/`go test ./...` all green. `-race` deferred to CI
  (no local gcc). E2E smoke: `scan` appends an audit record, `audit verify` intact.

### Deferred (unchanged)
- Real PDQ/TMK CSAM matcher = **v1.1**. asynq/Redis + scale + `-race` in CI = **M5**.
