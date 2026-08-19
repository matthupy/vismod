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

## `raw_sha256` against a live vendor response

The evidence binding is proven only against the fake adapter. Every test
in `internal/pipeline/raw_evidence_test.go` computes its expected digest
from `fakeRawBody`, so what is actually verified is that the pipeline
hashes whatever `NormalizedResult.Raw` an adapter returns, in the right
order, without desynchronizing it from the frames.

What that does NOT prove is the property an auditor would rely on: that a
digest recomputed from a response captured out of band — from the
vendor's own logs, or a proxy — matches the one in the record. Each
adapter builds `Raw` by re-marshaling its parsed response
(`microsoft.go:232`, and the same shape in google/hive/shieldgemma), so
the digest covers vismod's *re-serialization*, not the bytes the vendor
put on the wire. Key order, whitespace, and any field the adapter's
structs do not model all differ. That is a deliberate consequence of
`Raw` being sanitized, but it means "hash the vendor's response and
compare" does not work, and nobody has confirmed what a holder of the
original response can actually check.

**Proves it:** one live scan (the compose stack against Azure Content
Safety reaches this) that captures the adapter's `Raw` at the boundary,
recomputes SHA-256 over it, and shows the value equal to `raw_sha256` in
the resulting audit record — for an image and for a multi-frame video.
Then a line in `docs/audit-log.md` stating precisely which bytes an
auditor must hold to reproduce the digest.

The pre-fix state is settled and needs no further proof: the empty
`raw_sha256` was observed on live Azure records dated 2026-08-03 and
2026-08-07 in the `audit-b` volume.

## Mostly verified: the Discord payload reaches a real Discord webhook

**Settled 2026-08-08/09.** An operator ran the compose stack against a
live Discord webhook with `format: discord` and validated **all three
verdict renderings — `block`, `allow`, and `error`** — with the matching
`predicate.verdicts` config for each. Messages arrived in the channel.
`result.discordFormatter`'s Execute Webhook body (`username`, `embeds[]`,
each embed carrying `title`, `color`, `timestamp`, `fields[]` of
`{name,value,inline}`) is therefore accepted by the real API, not only by
the `httptest` server this repo wrote. That covers every embed color, the
`Top category`/`Score` fields on a scored verdict, and the degraded
rendering an `error` produces.

The SHAPE was separately verified against the Discord API reference on
2026-08-07 (`docs.discord.com/developers/resources/webhook` for Execute
Webhook, `.../resources/message` for the Embed object): embeds an array
of at most 10, a successful call answering `204 No Content`, and the
limits enforced in `fitEmbedBudget` — title 256, field name 256, field
value 1024, and a 6000-character combined budget across title plus all
field names and values.

Named caller metadata was validated live on 2026-08-09: a job carrying
`{"test":"123"}` with `metadata_fields: [test]` rendered the key as its
own embed field in a `block` notification. That covers the whole
`metadataFields` path for named keys — key lookup, scalar rendering, and
placement ahead of the free-text fields.

`format: json` needs no live run of its own. `jsonFormatter` is
`json.Marshal(env)` — the pre-existing envelope contract, unchanged and
pinned byte-for-byte by `TestJSONFormatterIsByteIdenticalToTheEnvelope`.
It has no limits to clamp and no fields to drop, so there is nothing a
live receiver could reject that the existing wire format did not already
face.

**Still open — the three size/shape limits, none of which a normal job
reaches.** Each exists specifically to avoid a `400`, and a `400` is
terminal in `moderate.DoJSON`: notification lost, no retry.

1. The **6000-character combined embed budget** (`fitEmbedBudget`). No
   live envelope was large enough to reach the squeeze path.
2. The **25-field embed cap**. Only a `metadata_fields: ["*"]` over a
   metadata object with many keys reaches it; the core fields never do.
3. The **wildcard itself**. `["*"]` is covered by unit tests but has
   never rendered against a live endpoint, including its sorted key
   order.

**Proves them:** three jobs against a live webhook — one whose
`source.ref` is a few thousand characters, one with `metadata_fields:
["*"]` and a metadata object of ~30 keys, and one with `["*"]` over a
small object to confirm ordinary wildcard rendering. Record the returned
status for each.

## The result readback store has never run under `-race`, or on Redis

`intake.result_api` (issue #72) adds `resultStore` in `internal/cli`: a
bounded LRU + TTL map written from the worker handler and from the intake
handler, and read from every HTTP request. In a running `serve` that is
`queue.workers` goroutines plus the intake goroutine plus one goroutine
per in-flight request, all on one `sync.Mutex` that covers the map and
the `container/list` together. `go test -race` has never been run against
it: this box has `CGO_ENABLED=0` and no C toolchain, so `-race` errors
with `-race requires cgo`. The concurrent exercise it did get is
`TestResultStoreUnderConcurrentTransitionsAndReads` (8 goroutines x 60
jobs, interleaving all three transitions with reads against a store
deliberately smaller than the job count), looped `go test -run
TestResultStoreUnderConcurrentTransitionsAndReads -count=20
./internal/cli/`, which passed but cannot detect a data race the way
`-race` can.

**Proves it:** `go test -race ./internal/cli/` on a machine with a C
toolchain. CI is the gate.

Two behavioral claims written into `SECURITY.md` and `docs/rest-api.md`
§4 are reasoned rather than observed, because neither is reachable from
this suite:

1. **The multi-replica `404`.** Both docs state that under
   `queue.driver: redis` with more than one replica a `GET` can land on a
   replica that never processed the job and answer `404` for a job that
   succeeded. That follows from the store being per-process, but no
   two-replica run has been made. **Proves it:** two `serve` replicas
   against one Redis, jobs submitted to replica A, then `GET /jobs/{id}`
   against replica B. It is the whole reason for issue #73, the
   Redis-backed store.
2. **A url job never reaches `done` in this suite**, so the redaction of a
   SUCCESSFUL url job's `source.ref` on this endpoint rests on the
   envelope's own contract rather than on an observation.
   `TestResultAPINeverDisclosesThePresignedURL` drives a real url job, but
   with no credentials and no reachable host it dead-letters — so what
   that test proves is that the free-text `reason` is redacted. The `done`
   path returns `env.Source` unmodified, and `pipeline.resolveSource` is
   what makes that the redacted form. **Proves it:** a url job fetched
   from a reachable https origin with a query string on the ref, read back
   through `GET /jobs/{id}`.
