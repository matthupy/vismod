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

## Per-replica Redis processing lists have never run on real Redis

**Claimed:** a booting replica no longer re-queues jobs that other live
replicas are processing, work from a replica that stops heartbeating is
reclaimed within ~60s, and payloads left in the pre-upgrade shared
`<prefix>:processing` key are reclaimed after a rolling upgrade.

**Actually tested:** miniredis only, in-process, with time simulated by
writing stale scores directly into the `<prefix>:instances` ZSET.
`TestRedisqStartDoesNotStealLiveReplicasWork` was confirmed to FAIL
against the old shared-key behavior (the job was handled twice) and to
pass after the change, so the test does discriminate. But miniredis is
not Redis: `SCAN` semantics under concurrent writes, `LMOVE` behavior
against a real server, and the heartbeat's behavior across an actual
network partition are all unexercised. No test runs two real processes.

The reaper is now load-bearing in a way orphan recovery never was:
instance ids are random, so a crashed replica's jobs are reclaimed ONLY
by another replica's reaper. If the reaper is broken in production,
those jobs are stranded silently rather than redelivered — the failure
mode moved, it did not disappear. `vismod_processing_depth` exists to
make that visible; nothing alerts on it automatically.

`instanceReclaimAfter` (60s) is a guess. It must exceed the worst
heartbeat gap under GC pause plus Redis latency, and it has not been
measured under load.

**Proves it:** two real `vismod serve` processes against a real Redis —
one holding a long job while the other boots (no duplicate handling),
then SIGKILL one and confirm its in-flight jobs are redelivered within
the reclaim window and its instance is deregistered; plus a rolling
upgrade from the previous version with payloads in the shared key.

## The 2026-08-05 review changes have not been run under -race

**Claimed:** the new concurrent state — `frames.argTemplates`
(`sync.Map`), the time-based sweep in `result.dedupe`, the bounded
`finished` slice in `memq.setState`, and redisq's heartbeat/reaper
goroutines — is race-free.

**Actually tested:** `go test ./...` without `-race` (this dev box has no
C compiler). `TestDedupeConcurrentClaimYieldsExactlyOneWinner` exercises
`dedupe` under concurrency but proves mutual exclusion, not the absence
of a data race.

**Proves it:** a green `go test -race ./...` in CI.

## dHash values changed for images larger than 8px per grid cell

**Claimed:** bounded per-cell sampling does not meaningfully change which
frames dedup collapses.

**Actually tested:** the typed `lumaSampler` fast paths are proven
bit-identical to the interface path
(`TestLumaSamplerFastPathsMatchTheGenericPath`), and `step` is proven to
stay 1 for small cells, so every existing dedup test hashes exactly as
before. The SAMPLING change is different: for a frame larger than
72x64px the cell average is now computed from at most 8x8 samples per
cell rather than every pixel, so a hash bit CAN flip where a cell average
sat on a boundary. No before/after comparison was run over real video
frames.

The blast radius is bounded — a flipped bit changes Hamming distance by
1, so it can only matter for frame pairs sitting exactly on the
threshold, and dedup never empties a non-empty set — but "the same frames
are dropped" is not proven.

**Proves it:** hashing a corpus of real extracted frames with the old and
new samplers and reporting the distribution of Hamming-distance deltas,
plus the count of pairs whose keep/drop decision changes at the shipped
threshold.
