# Contributing to vismod

Thanks for helping build a safety-critical public good. A few rules keep
it safe to iterate on.

Working with a coding agent? [AGENTS.md](AGENTS.md) holds the machine-
facing version of these rules — invariants, the done gate, and the
architecture map. [CLAUDE.md](CLAUDE.md) is the orientation page.

## Ground rules

- **Never test against real illegal material.** Use the fakes
  (`fakeModerator`, `fakeFrameSource`), synthesized media
  (`ffmpeg -f lavfi testsrc`), and fixture JSON. See RESPONSIBLE_USE.md.
- **Fail safe is non-negotiable.** No change may create a path where a
  provider/extraction failure, an all-null score set, or an empty frame
  set produces `allow`. The rollup tests encode this; don't weaken them.
- **The full test suite runs with no network and no credentials.**
  Provider clients are tested with `httptest`; Redis with miniredis;
  ffmpeg integration tests skip when the binary is absent.

## Development

```sh
go build ./...
go vet ./...
go test ./...            # add -update to regenerate golden files
```

Keep `main` green: land features with their tests as one coherent unit.

## Adding a vendor adapter (the designed extension point)

1. Create `internal/moderate/adapters/<name>/` with a factory
   `func New(cfg moderate.AdapterConfig) (moderation.Moderator, error)`
   and `moderate.Register("<name>", New)` in `init()`.
2. Secrets come ONLY from `cfg.Secret("<name>.<key>")` (env-backed);
   options from `cfg.Options`. Fail fast on missing credentials.
3. Normalize into the `pkg/moderation` schema:
   - tag every score with the correct `ScoreOrigin`;
   - unknown/unsupported values are `nil`, never `0`;
   - unmapped provider labels go to `OTHER` with `ProviderLabel`
     preserved — never drop a signal;
   - `Raw` must be sanitized: no free text, OCR, captions, media bytes.
4. Add a blank import in `internal/cli/root.go` (the only wiring point;
   the registry never imports adapters).
5. Ship golden tests: raw fixture JSON → `NormalizedResult` →
   `testdata/*.golden`, plus retry/terminal classification tests via
   `httptest` if you use REST.
6. Document the score semantics in MODEL_LIMITATIONS.md
   (scores are per-provider; thresholds are not portable).

Zero pipeline changes should be needed — if you find yourself editing
`internal/pipeline`, the interface is missing something; open an issue
first.

## Dependencies

The dependency set is deliberately small (cobra, viper, errgroup,
prometheus, the Google Vision SDK, go-redis; miniredis test-only). Every
new import is a decision, not a reflex — justify it in the PR
description.

## Commit / PR expectations

- Table-driven tests where they fit; golden files for normalization.
- No secrets in code, config examples, tests, or fixtures.
- Update the docs that ship with behavior you change (README, CLAUDE,
  AGENTS, SECURITY, RESPONSIBLE_USE, MODEL_LIMITATIONS, this file).

## Licensing of contributions

vismod is **GPL-3.0-or-later** ([LICENSE](LICENSE)). There is no CLA and
no copyright assignment: contributions are inbound=outbound, meaning by
opening a pull request you license your contribution to the project under
GPL-3.0-or-later on the same terms, and you keep your copyright.

Two consequences worth knowing before you write code:

- **Anything that links into vismod must be GPL-3.0-compatible.** New
  dependencies under Apache-2.0, BSD, MIT, ISC, or MPL-2.0 are fine.
  GPL-2.0-**only** and proprietary/source-available licenses (BUSL, SSPL,
  Elastic, "free for non-commercial") are not — they cannot be combined
  with this work. Check before you add an import.
- **Forks and modified deployments you distribute must ship source.**
  That is the point of the license: derivative moderation tooling stays
  auditable. Note that GPL-3.0 obligations are triggered by
  *distribution*, not by running a modified vismod as a hosted service.

Per-file license headers are not required; the repository-level
[LICENSE](LICENSE) and [NOTICE](NOTICE) govern every file.

## Code of conduct

See CODE_OF_CONDUCT.md. Moderation tooling attracts hard discussions;
keep them kind and evidence-based.
