---
title: Unverified claims
nav_order: 22
---

# UNVERIFIED

Things the repo or its docs assert that have NOT been proven in this
environment. Append here rather than silently claiming success. Each
entry states what would settle it.

Remove an entry only when the proving step has actually been run and its
result recorded in a commit.

## Verified finding: viper preserves `output.sinks: []` as a real empty slice

Not an open item — recorded here because it underlies a fail-safe
guarantee and was easy to get wrong by assumption. `config.validateOutput`
rejects a present-but-empty `output.sinks` list so vismod never boots
silently emitting nowhere. That guard only matters if viper actually
delivers an empty slice to Go when the yaml says `sinks: []`, rather than
dropping the key the way it drops a map key with no scalar leaf (`x: {}`,
`x:`, `x: null` all vanish before `config.Load` ever sees them — the
`provider_thresholds.unarmed_labels` gotcha above exists because of that
exact behavior). Probed directly and reproduced independently
(`TestOutputEmptySinkListRefusesBoot` in `internal/config/config_test.go`,
decoding `output:\n  sinks: []\n`): viper does NOT vanish `[]`; it decodes
to a real zero-length
`[]SinkConfig`, distinct from a nil/absent field. That distinction — nil
slice (key absent, defaults apply) vs. empty slice (key present, empty) —
is exactly what `validateOutput`'s `len(o.Sinks) == 0` check is built on,
and it would silently stop catching the empty-list case if that viper
behavior ever changed.

## ShieldGemma against a real inference server

No call has ever been made against a real ShieldGemma 2 endpoint. Every
test uses `httptest` with fixtures this repo authored, so the request
shape, the response shape, and the score derivation are all **assumed**:

- that a vLLM/TGI OpenAI-compatible server accepts the chat-completions
  body built here (image as a `data:` URI part + policy text part,
  `max_tokens: 1`, `logprobs: true`, `top_logprobs: 8`);
- that it reports BOTH a `Yes` and a `No` alternative for the generated
  token — the adapter errors when it does not, so if a real server ranks
  `No` outside the top-8 the frame becomes unscorable (an error verdict,
  never an allow, but availability-affecting);
- that `P(Yes)` renormalized over the Yes/No pair is what the model card's
  "probability of the Yes token" means;
- that the policy prompt wording carried in `policyPrompts` scores the way
  the published F1 figures (88.6 / 93.7 / 85.0) describe.

**Proves it:** one run against `google/shieldgemma-2-4b-it` on a GPU box
(≈8 GB bf16), scanning a known-benign and a known-violating image, showing
the request accepted, both tokens present in `top_logprobs`, and the
scores ordered as expected. Needs hardware the test suite does not have.

## Hive end-to-end against the live API

The hive adapter's request encoding and response parsing both now match
Hive's published docs (checked 2026-07-29), and both are pinned by tests.
No call has ever been made against the real api.thehive.ai — the docs are
the only evidence, and doc pages lag implementations in both directions.

**Proves it:** one authenticated call against Hive with a real frame,
confirming the multipart `media` upload is accepted and the returned
envelope parses. Needs `VISMOD_HIVE_API_TOKEN`, which the test suite
deliberately does not have.

## Compose stack: no successful allow verdict end to end

The local and production compose stacks were exercised (redis driver,
two-replica claim, `vismod_queue_depth`, graceful drain, independent
audit chains, Prometheus targets, all 8 Grafana panels, Redis data
surviving `down`/`up`), but no compose run has observed a successful
`allow` verdict end to end. There is no vendor credential in this
environment, so every job ends `verdict:"error"` — correct fail-safe
behavior, but it only exercises the queue, metrics, audit, and drain
paths, not a scoring success.

**Proves it:** one `docker compose up` with a real
`VISMOD_MICROSOFT_API_KEY` and endpoint, scanning a benign image, showing
`verdict:"allow"` in the result envelope and
`vismod_jobs_total{verdict="allow"}` incrementing.

## Output sinks: race detector never run

`go test -race` has never been run against any of `internal/result`. This
machine has `CGO_ENABLED=0` and no C toolchain, so `-race` errors with
`-race requires cgo` rather than passing or failing. The `dedupe` claim
helper and `FileSink`'s concurrent-write path were exercised only by a
non-race concurrent test (`TestDedupeConcurrentClaimYieldsExactlyOneWinner`,
`TestFileSinkConcurrentWritesDoNotInterleave`) looped 20x
(`go test -run ... -count=20 ./internal/result/`), which passed but
cannot detect a data race the way `-race` can.

**Proves it:** `go test -race ./internal/result/` on a machine with a C
toolchain.

## Output sinks: webhook and file sink never exercised against real receivers/replicas

`WebhookSink` has only ever POSTed to `httptest` servers this repo wrote,
and `FileSink` has only ever been written to by one process at a time
(single-process concurrent-goroutine tests, not two live OS processes).
Neither has been run the way the brief that shipped them describes:

- no webhook sink has been exercised against a real, independent receiver
  process outside `httptest` — retry/backoff behavior against real network
  conditions (timeouts, connection resets, an actual `Retry-After` header
  from a non-test server) is unverified;
- no `file` sink has been run under two live `vismod serve` replicas
  writing concurrently — the per-replica constraint documented in
  `deploy/README.md` and `deploy/compose/README.md` follows from reading
  `FileSink`'s `O_APPEND` construction and by analogy with the audit log,
  not from having reproduced the interleaving failure with two real
  processes.

**Proves it:** for the webhook sink, one `vismod serve` run configured
with `output.sinks: [{type: webhook, url: ...}]` against a small
standalone receiver process, showing delivery, a deliberate 5xx/timeout
retry, and a deliberate 4xx terminal failure. For the file sink, two
`vismod serve` replicas (or the compose stack, once a `file` sink is
added to it) configured to point at the SAME path, showing the documented
interleaving/corruption, then confirming separate paths avoid it.

## Multi-replica rate limiting

`moderate.Limiter` is per-process. The README and `deploy/README.md`
advise budgeting `global_quota / max_replicas` per replica. No
multi-replica run has been exercised against a real vendor quota.

**Proves it:** a load test with N replicas against a vendor sandbox
showing aggregate request rate stays under quota, or a shared limiter
implementation with its own tests.

## No url fetch has ever run against a real remote host

`internal/fetch` is exercised entirely by `httptest` on loopback, with
the address policy (`DenyPrivate`) replaced by a permissive stub for
every test that actually transfers bytes — loopback is precisely what
the real policy denies. So the happy path has never been proven against
a public `https` host, a real TLS handshake, a real presigned URL, or a
body larger than a few KiB.

The DNS-rebinding defense is likewise verified only against a SIMULATED
policy: `TestFetchDNSRebinding` swaps in an `ipPolicy` that denies on
its second call and asserts the policy ran twice. That proves the hook
runs per-connection, not that a real rebinding resolver is defeated.

`go test -race ./internal/fetch/` has NOT been run locally (this dev box
has no C compiler, so the race detector cannot build). The retry loop
mutates the cleanup closure's `sync.Once` under a mutex; CI's `-race` is
the only gate on that.

**Proves it:** an integration run against a live allow-listed host
(fetching a real image and a real video, including one presigned URL
whose query string must not appear in the envelope, audit record, logs,
or metric labels), plus a rebinding test using a resolver that returns a
public address then the metadata address, plus a green `-race` run in CI.
