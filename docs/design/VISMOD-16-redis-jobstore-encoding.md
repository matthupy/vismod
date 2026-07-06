# VISMOD-16 — Redis JobStore: hash encoding + Lua merge + EXPIRE semantics

**Status:** Design spike complete. **No production code ships from this ticket.**
Blocks VISMOD-19 (driver implementation).

**Proof-of-concept:** `internal/jobstore/spike_redis_test.go` — throwaway, retained
so VISMOD-19 can lift the encoding, the Lua script, and the miniredis harness
verbatim. Delete it once the real driver lands. All proofs below are demonstrated
there against `miniredis` v2.38.0 (`go test ./internal/jobstore/ -run TestSpike`).

Ground truth mirrored: `internal/jobstore/store.go:72-92` (fields),
`mem.go` (reference semantics), `sink.go:24` (`recordFromEnvelope` mandate),
`pkg/moderation/types.go:139-142` (`Source`).

---

## 1. Hash encoding contract

**One Redis hash per JobID**, key `vismod:job:<id>`. Every field is a string
(Redis hashes store strings only). `moderation.Source` is a **flat 3-field**
sub-hash (`source_kind` / `source_ref` / `source_media_type`), **not** a nested
JSON blob — keeps every value a plain string so Lua can read/copy/compare fields
without `cjson`.

### 1.1 Field surface — 10 fields, 3 null-classes

| Field(s) | Class | Encoding rule |
|---|---|---|
| `verdict`, `flagged`, `top_category`, `max_score`, `confidence` (5 verdict scalars) | **nullable pointer** | nil ⇒ **field omitted**; present ⇒ real value |
| `started_at`, `finished_at` (2 times) | **nullable pointer** | nil ⇒ **field omitted**; present ⇒ RFC3339Nano UTC |
| `worker_id`, `error` (2 `omitempty` strings) | **omitempty string** | `""` ⇒ **field omitted**; present ⇒ real value |
| `submitted_at` (1 non-null time) | **always written** | Go supplies the race-ahead fallback value before encode |
| `status`, `source_kind`, `source_ref`, `source_media_type` | non-null scalars | always written (Source empties are meaningful struct zeros) |

`flagged` encodes as `"true"`/`"false"`; floats via `strconv.FormatFloat(_, 'g', -1, 64)`.

### 1.2 Null-vs-zero disambiguation (the crux, CLAUDE.md P0)

`HGETALL` returns an absent field as if it never existed, so the scheme must
disambiguate **HGET-miss vs written-nil vs written-empty-string**. The contract
collapses them safely rather than inventing a sentinel:

- **written-nil ≡ HGET-miss.** A nil `*T` is encoded as *field-omitted*. There is
  **no null sentinel string**; absence *is* nil. So the two states are identical
  by construction — nothing to disambiguate.
- **written-empty-string cannot occur for a nullable field.** The only fields that
  could carry `""` are the two `omitempty` strings, and they also omit on empty.
  Therefore **a present field always denotes a real, non-nil value.** A `""`-vs-nil
  ambiguity is structurally impossible.

**Round-trip (proven, `TestSpike_EncodingRoundTrip_NilIsNull`):**
nil pointer → field omitted on `HSET` → `HEXISTS`=0 → `HGETALL` miss → `nil` in Go
→ `json.Marshal` emits `"max_score":null` etc. — **never `0`, never `""`, never a
dropped key.** A dead-letter envelope (Result==nil) leaves all five verdict
scalars and both time pointers nil end-to-end. Success scalars round-trip exactly
(`TestSpike_EncodingRoundTrip_SuccessScalars`).

> **Decision:** `Source` is stored **flat 3-field**, not as a nested decision blob.

---

## 2. Go / Lua boundary

Per `sink.go:24`, RFC3339 parsing and verdict extraction stay **Go-side** and
**MUST reuse `recordFromEnvelope`** — not re-implement. The PoC's `Complete`
writer calls it directly; parse-fail ⇒ nil (never panic).

Lua does **`HGET` → rank-branch → `HSET` / `HEXISTS` / `EXPIRE` only**. It never
parses time, never ranks a verdict, and **needs no `cjson` on the write path**
(every value crosses the boundary as an already-formatted string in `ARGV`).

### 2.1 One parameterised CAS-merge script

Rather than three near-identical scripts, the PoC uses **one** `luaCASMerge`
script driven by an `ARGV` protocol; each writer supplies different args. This is
the recommendation for the driver — one script to load/`EVALSHA`, three call sites.

```
ARGV[1] = target rank (1|2|3)
ARGV[2] = ttl seconds (applied ONLY on create; 0 = no ttl)
ARGV[3] = N  ; then 2N "overwrite" args   -> always HSET
next    = M  ; then 2M "preserve" args     -> HSET only if field absent (HEXISTS==0)
returns 1 on apply, 0 on rank-drop no-op
```

- **overwrite** = terminal/advancing fields that always win.
- **preserve** = merge fields where an existing record's value wins
  (`HEXISTS`-gated). This is how `Complete` keeps the prior `submitted_at`,
  `worker_id`, `started_at` (`mem.go:162-170`).

Rank guard: `HGET status` → `{pending=1, processing=2, done=3, dead_letter=3}`.
A missing key (`redis.call` returns Lua `false`) ⇒ create. `curRank >= target`
⇒ `return 0` (drop). This gives monotonicity **and** idempotency in one branch.

### 2.2 Per-writer mapping (mirrors mem.go)

| Writer | target | overwrite | preserve (set-if-absent) |
|---|---|---|---|
| `SetPending` | 1 | status, source_*, submitted_at | — (only ever applies on create) |
| `SetProcessing` | 2 | status, worker_id, started_at | submitted_at = startedAt *(race-ahead fallback, `mem.go:140`)* |
| `Complete` | 3 | status, source_*, verdict scalars (nil-omitted), finished_at, error | submitted_at *(fallback FinishedAt→StartedAt, `mem.go:178-184`)*, started_at, worker_id |

`Complete` never sends `worker_id` in *overwrite* (its envelope record has none),
so an existing worker is preserved simply by not overwriting.

### 2.3 Proven paths (apply **and** drop for every writer)

- `SetPending`: apply on create; **drop** on equal rank (second pending) and on
  lower rank (pending after processing) — no status regression. `TestSpike_SetPending_ApplyAndDrop`
- `SetProcessing`: apply; **drop** on equal rank (second worker) with `worker_id`
  unchanged; race-ahead `submitted_at := started_at`; preserves an existing
  pending's `submitted_at` + `source`. `TestSpike_SetProcessing_*`
- `Complete`: apply with merge-preserve (`submitted_at` always, `worker_id` if
  non-empty, `started_at` existing-wins even when the envelope carries a
  different value); **idempotent rank-3-vs-3 drop leaves the record byte-identical**;
  race-ahead `submitted_at` fallback to `FinishedAt`. `TestSpike_Complete_*`

### 2.4 Concurrent-writer behavior (defined + proven)

Two simultaneous writers — `SetProcessing` (rank 2) and `Complete` (rank 3) —
resolve deterministically: **the terminal status always wins, never lost.**
`EVAL` executes atomically (Redis runs scripts single-threaded; miniredis
serializes likewise), so the monotonic guard is evaluated under mutual exclusion —
unlike the dedup gate, which gives ordering but not exclusion
(`internal/dedup/redis.go:33-38`). Whichever script runs second sees the other's
committed status and branches correctly:
- Complete-then-Processing: Processing sees rank 3 ⇒ drops.
- Processing-then-Complete: Complete sees rank 2 ⇒ applies, merges (keeps
  `worker_id`/`started_at`), advances to done.

Proven by `TestSpike_ConcurrentProcessingVsComplete` (two goroutines racing the
same key; asserts `done` + verdict survive).

---

## 3. EXPIRE semantics — fixed-from-create, no slide

`mem` measures TTL from an **immutable `createdAt`** (`mem.go:16`). A naïve
`EXPIRE` on every write would **slide** the window and diverge the TTL-conformance
test.

> **Decision + mechanism:** the Lua script issues `EXPIRE` **only on create**
> (`isCreate = not HGET(status)`). Advance-writes skip `EXPIRE` entirely, so the
> key keeps counting down from its original window — equivalent to `KEEPTTL` but
> achieved by *not touching* the TTL at all.

**Proven (`TestSpike_ExpireNoSlideOnAdvance`):** create with TTL 100s →
`miniredis.FastForward(40s)` → advance-write (processing) → remaining TTL is
**~60s, not reset to ~100s**. A slide would have failed the assertion.

Recommendation for the driver: keep the create-only-`EXPIRE` mechanism. If a
future requirement wants TTL refreshed on activity, that is an explicit product
change, not a driver default.

---

## 4. Client / addr reuse

> **Decision:** reuse `queue.redis_addr` + the shipped boot-ping pattern from
> `internal/cli/wire.go:140-154` (deduper: `redis.NewClient({Addr: cfg.Queue.RedisAddr})`
> + bounded boot-`Ping` + fail-closed + `Close`). **Do not** add a dedicated
> `jobstore.redis_addr`.

Rationale: the jobstore and the dedup gate are co-located production concerns on
the same Redis intake; a second addr is config surface with no operational
benefit. Lift the exact wiring (bounded ping, fail-closed on unreachable Redis,
caller owns `Close`). The driver loads the script once at construction
(`redis.NewScript` → `EVALSHA` with `EVAL` fallback, as the PoC uses).

---

## 5. miniredis Lua fidelity finding + fallback

**What the PoC empirically confirms under miniredis v2.38.0:** `EVAL` runs; the
write-path primitives all behave correctly — `HGET` returns Lua `false` on miss,
`HSET`, `HEXISTS`, `EXPIRE`, Lua table literals, `tonumber`, string coercion,
and **atomic script execution** all match what the contract needs. The write path
uses **no `cjson`**, sidestepping miniredis's historically partial JSON support.

**Fidelity gaps that remain unverified against real Redis** (miniredis runs a
`gopher-lua` interpreter, not Redis's embedded Lua 5.1):
- subtle `nil`/`false` ↔ Redis-reply coercion beyond the `HGET`-miss case we rely on;
- `redis.call` error-object propagation / `pcall` semantics;
- number formatting edge cases returned from Lua to Go;
- any future use of `cjson`, `redis.sha1hex`, `redis.status_reply`, or
  data-structure commands the PoC does not exercise.

**Named fallback:** a **testcontainers real-Redis integration test** — the same
three script paths behind a `//go:build integration` tag, skipped by default,
run in a dedicated CI job (Docker-backed). Not stood up in this spike: this dev
box has no guaranteed local Docker, and the write path stays within the
miniredis-faithful subset.

**Go / no-go criterion for triggering the fallback** — add the integration job
when **any** of:
1. the driver introduces `cjson` or any command outside
   `HGET/HSET/HEXISTS/HGETALL/EXPIRE/PTTL/DEL`;
2. the driver relies on Lua `nil`/`false`/number coercion beyond the proven
   `HGET`-miss branch;
3. a miniredis behavioral surprise appears during driver development;
4. the merge logic grows a branch not covered by the PoC's apply+drop matrix.

Until one fires, miniredis is the trustworthy gate for this write path.

---

## 6. Accepted residuals (recorded, not closed by this spike)

- **Post-EXPIRE late-`SetPending` status regression.** Once a key expires (or is
  evicted), a late `SetPending`/`SetProcessing` for that JobID finds no record and
  **recreates it at a lower status** — a job that was `done` can reappear as
  `pending`. This is **accepted at Redis parity with mem** (`store.go:18-29`): the
  strict-rank guard only protects a *resident* record. The Redis `EXPIRE` has the
  identical hole by design. **Not closed here.**
- **Crash strictly between writes** and the concurrent-second-worker gap are
  dedup-layer residuals (`internal/dedup/redis.go:33-38`), out of scope for the
  jobstore encoding.

---

## 7. Handoff to VISMOD-19

The driver can be built with **zero hidden design work**: encoding table (§1),
the single parameterised `luaCASMerge` script + per-writer arg mapping (§2),
create-only-`EXPIRE` mechanism (§3), `wire.go` client reuse (§4), the fidelity
go/no-go (§5), and the accepted residuals (§6). Lift `spike_redis_test.go`
verbatim as the driver's starting test + Lua source. Findings posted to VISMOD-19.
