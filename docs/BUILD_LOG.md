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

## NEXT — M1: Azure AI Content Safety adapter (§C)
- Direct REST `POST {endpoint}/contentsafety/image:analyze?api-version=2024-09-01`
  (configurable const). No Go SDK.
- Both auth modes: `Ocp-Apim-Subscription-Key` + Entra ID / Managed Identity.
- Normalize: `severity/6.0`, `ScoreOrigin="severity"`, 4 categories (Hate/SelfHarm/Sexual/Violence
  — NOT Task Adherence). Input: 4 MB cap, format allow-list, dimension cap (default 2048).
- Shared token-bucket rate limiter owned by the adapter (Azure F0 = 5 RPS), `MaxImageBytes` preflight.
- Retry classification (429/5xx→retry, 4xx→terminal), surface `x-ms-error-code`.
- Tests: `httptest` server, golden-file normalization (`-update`).
- SSRF: v1 local-file/inline `content` only; `blobUrl` stays disabled (allow-list when enabled).
