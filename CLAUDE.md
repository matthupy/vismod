# CLAUDE.md

Guidance for AI coding agents working in this repo. Keep changes small, tested, and `main` green.

## What this is

`vismod` — open-source **visual content moderation pipeline** (Go). Scans images and video for harmful
content via a pluggable visual-moderation model, normalizes wildly different provider outputs into one
schema (`pkg/moderation`), and runs as a one-shot CLI (`vismod scan`) and a long-running worker
(`vismod serve`). Public good for trust & safety; **no commercial goals**. Correctness, safety, and
auditability come first.

Single binary, subcommands: `scan`, `serve`, `adapters`, `audit verify`, `version`, `healthcheck`
(hidden, operational).

## Safety — read before any change touching media, logs, or moderation logic

- **Never test against real or suspected CSAM.** Never commit illegal/harmful/copyrighted media. Use
  synthetic / clearly-legal fixtures. See `RESPONSIBLE_USE.md`.
- **CSAM detection is NOT implemented (deferred to v1.1).** It is handled by a hash-match pre-stage seam
  (no-op in v1), never the classifier. Don't add classifier-based CSAM logic.
- **Never leak media.** Never log or persist media bytes, PII, or `Raw` free-text/OCR/captions. The audit
  log stores hashes only (`SHA-256(Raw)` + `ModelIdentity` + verdict).
- **Secrets are environment-only** (`VISMOD_` prefix, `.`→`_`). Never put keys/tokens in YAML, tests, or
  fixtures.
- Report security issues via GitHub Security Advisories, not public issues (`SECURITY.md`).

## Build / test / lint

Go `1.26.4` (see `go.mod`). All commands run from repo root.

```bash
go build ./cmd/vismod                                  # build the binary
go build ./...                                         # compile everything
go test ./...                                          # all tests
go test -race -covermode=atomic -coverprofile=coverage.out ./...   # CI test command
go test ./internal/pipeline -run TestName              # a single test
go vet ./...
gofmt -l .                                             # MUST print nothing
golangci-lint run ./...                                # config: .golangci.yml
govulncheck ./...                                      # vuln scan (CI)
```

CI (`.github/workflows/ci.yml`) runs: `go mod tidy` + `git diff --exit-code go.mod go.sum`, build, vet,
`-race` tests w/ coverage, `golangci-lint`, `govulncheck`, and a Docker build. Keep `go.mod`/`go.sum`
tidy or CI fails. There is no standalone gofmt step in CI — formatting is enforced via `golangci-lint`
(the gofmt formatter); run `gofmt -l .` locally to catch it before pushing.

### Critical build gotcha — sibling `videosift` checkout

`go.mod` has `replace github.com/matthupy/videosift => ../videosift`. The sibling repo **must be checked
out next to this one** for build/vet/test to resolve:

```
parent/
  vismod/      # this repo
  videosift/   # git clone https://github.com/matthupy/videosift
```

`videosift` is **tracked at latest, never pinned** (co-developed). It execs external `ffmpeg`+`ffprobe`;
install both to run the video path locally. Docker builds use the **parent dir** as context
(`docker build -f vismod/Dockerfile -t vismod:dev parent/`), not this repo.

## Layout

```
cmd/vismod/            entry point → cli.Execute()
pkg/moderation/        PUBLIC contract: Moderator, VideoModerator, NormalizedResult, Verdict,
                       Category, ScoreOrigin, CategoryResult, HashMatcher. Stable seam; no internal deps.
internal/cli/          cobra command tree + composition root (blank-imports adapters); wire.go = DI helpers
internal/config/       typed Config + viper loader (env overlay, secrets-env-only), ModelFingerprint, ConfigHash
internal/moderate/     registry.go (Register/New/Names); adapters/{stub,azure,hive,google}/
internal/frames/       FrameSource interface; videosift.go (real), fake.go (test)
internal/queue/        Queue interface; memq.go (in-mem FIFO, dev), asynqq.go (Redis/asynq, durable)
internal/pipeline/     Pipeline orchestration; rollup.go (asset verdict aggregation)
internal/result/       Sink interface, ResultEnvelope, jsonl.go writer
internal/audit/        append-only hash-chained decision log
internal/observe/      slog setup + Prometheus metrics
internal/review/       Diverter interface (potential-CSAM frame divert seam)
internal/dedup/        Redis cross-process dedup gate (at-least-once safety)
internal/hashmatch/    HashMatcher impls (no-op default; PDQ/TMK in v1.1)
```

## Architecture conventions (enforced — follow them)

- **Code to interfaces.** Every external concern (Moderator, Queue, FrameSource, Sink, HashMatcher,
  Diverter) sits behind an interface in its package and swaps via config with zero call-site changes.
- **Adapter self-registration.** An adapter calls `moderate.Register("<name>", factory)` in its `init()`.
  The composition root (`internal/cli/root.go`) blank-imports each adapter to trigger registration. The
  registry **never imports adapter packages**. This makes "exactly one model per process" structural.
- **One model per process**, chosen at startup; restart-to-change. No hot-swap.
- **Fail safe, never fail silent.** Any provider/frame/extraction failure yields `Verdict=error` (never
  `allow`) and dead-letters. Verdict precedence: `block > error > flag > allow`. A worker panic
  dead-letters that job; the pool keeps running.
- **Nullable scalars serialize as JSON `null`, never omitted.** No `omitempty` on `Score`, `Threshold`,
  `MaxScore`, `Confidence`. A `nil` score means could-not-evaluate; never emit `0` for unknown.
- **Scores are within-provider comparable only.** Thresholds are **per-adapter and not portable** (a
  `0.667` means different things for Azure severity/6 vs Hive probability vs Google likelihood bucket).
- **Tests land with features**, test-first. Table-driven tests, interface fakes, golden files for
  normalization (`testdata/<adapter>/*.json → *.golden`), `httptest` for provider clients (incl.
  retry/backoff/error-mapping). The pipeline must run and test **without network or credentials** via the
  `stub` adapter.

## Adapters

Registered: `stub` (credential-free, deterministic), `azure` (AI Content Safety), `hive` (thehive.ai),
`google` (Cloud Vision SafeSearch). All in `internal/moderate/adapters/<name>/`.

**To add one:**
1. Implement `moderation.Moderator` (optionally `VideoModerator`) in `internal/moderate/adapters/<name>/`.
2. Self-register via `init()` → `moderate.Register("<name>", factory)`; add the blank import to
   `internal/cli/root.go`.
3. Normalize provider output into `pkg/moderation`, tagging every score with `ScoreOrigin`. Map unknown
   labels to `OTHER` (preserve raw label). **Never drop a result; never emit `0` for an unknown score
   (use `nil`).**
4. Ship golden-file tests + `httptest` coverage (auth, retry, backoff, error-mapping).
5. Document the score scale in `MODEL_AND_HASH_LIMITATIONS.md` — thresholds are per-adapter.

## Config

Configure via `config.example.yaml`. File (YAML, non-secret) + env overlay (`VISMOD_` prefix). Boot
validation fails fast (adapter name, queue driver, ffmpeg/ffprobe presence). Key sections: `adapter`,
`thresholds` (per-category `flag_at`/`block_at`; `block_at > 1.0` ⇒ never auto-block, route flagged to
review), `queue` (`driver: memory|redis`), `frames` (videosift tuning; `max_frames` REQUIRED > 0), `log`,
`metrics`, `audit`.

`memq` is non-durable single-process (dev/CLI). Production intake uses `driver=redis` (durable,
at-least-once, deduped per `job_id`).

## Reference docs

- `visual-moderation-pipeline.prompt.md` — the locked build spec (§A–§C = research-verified fact;
  §D–§L = build instructions, milestones, scope). **On any conflict, this document wins.**
- `RESPONSIBLE_USE.md`, `SECURITY.md`, `MODEL_AND_HASH_LIMITATIONS.md`, `CONTRIBUTING.md` — acceptance
  criteria, not optional reading.
- `README.md` — quick start, Docker, observability details.
