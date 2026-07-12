# Contributing to vismod

Thanks for helping build open trust & safety infrastructure. vismod is a public
good with **no commercial goals**; correctness, safety, and auditability come
first.

## Ground rules

- **Never test against real or suspected CSAM**, and never commit illegal,
  harmful, or copyrighted media. Use synthetic / clearly-legal fixtures. See
  [RESPONSIBLE_USE.md](RESPONSIBLE_USE.md).
- **No secrets in the repo.** Secrets are environment-only (`VISMOD_` prefix).
  Never put keys/tokens in YAML, tests, or fixtures.
- By contributing you agree your contribution is licensed under
  **Apache-2.0** (see [LICENSE](LICENSE)), and you follow the
  [Code of Conduct](CODE_OF_CONDUCT.md).

## Development

```bash
go build ./...
go test ./...                 # CI also runs -race (needs cgo/gcc)
go vet ./...
gofmt -l .                    # must print nothing
```

Video framing depends on `github.com/matthupy/videosift`, which is
**tracked at latest, never pinned** (it is co-developed). A local
`replace => ../videosift` is active for tandem development — keep a sibling
checkout. videosift execs external `ffmpeg`+`ffprobe`; install both to run the
video path locally.

## Engineering conventions

- **Tests land with features.** Table-driven tests, interface fakes, golden files
  for normalization, `httptest` for provider clients. The pipeline must be
  runnable and testable **without network or credentials** (use the `stub`
  adapter). New behavior is **test-first**.
- **Code to interfaces.** Every external concern (moderator, queue, frame source,
  sink, hash matcher, diverter) sits behind an interface so implementations swap
  via config with no call-site change.
- **Fail safe, never fail silent.** On any provider/frame failure, never emit
  `allow` — emit `error` and route to dead-letter / human review.
- **Don't leak media.** Never log or persist media bytes, PII, or `Raw`
  free-text/OCR/captions. The audit log stores hashes, not content.
- **Small, reviewable PRs** that keep `main` green. Match the surrounding code's
  style and comment density.

## Pull requests

Open PRs against `main`. GitHub pre-fills the
[pull request template](.github/pull_request_template.md) — **fill in every
section** (Links, Description, Technical Solution, Testing). Do not delete
headings; if a section does not apply, say so (e.g. "No change to
functionality"). The Testing section carries a **required safety checkbox** —
confirm no media bytes, PII, or secrets were added to code, tests, or fixtures
before requesting review.

## Adding a moderation adapter

1. Implement `moderation.Moderator` (and optionally `moderation.VideoModerator`)
   in `internal/moderate/adapters/<name>/`.
2. Self-register via `init()` → `moderate.Register("<name>", factory)`; the
   composition root blank-imports it. The registry never imports adapters.
3. Normalize the provider's output into the canonical schema (`pkg/moderation`),
   tagging every score with its `ScoreOrigin`. Map unknown labels to `OTHER`
   (preserve the raw label) — **never drop a result**, never emit `0` for an
   unknown score (use `nil`).
4. Ship golden-file tests (`testdata/<provider>/*.json → normalize → *.golden`)
   and `httptest` coverage incl. retry/backoff/error-mapping.
5. Remember: **thresholds are per-adapter, not portable** — document the score
   scale in [MODEL_AND_HASH_LIMITATIONS.md](MODEL_AND_HASH_LIMITATIONS.md).

## Reporting security issues

Use GitHub Security Advisories, not a public issue. See [SECURITY.md](SECURITY.md).
