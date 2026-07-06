// THROWAWAY SPIKE POC — VISMOD-16. NOT production code; NOT wired into any
// driver. This file is retained in-repo so the VISMOD-19 Redis-jobstore driver
// story can lift the proven encoding, the Lua CAS-merge script, and the
// miniredis harness verbatim. Delete it once VISMOD-19 ships the real driver.
//
// What it proves (see docs/design/VISMOD-16-redis-jobstore-encoding.md):
//  1. Hash encoding round-trip: nil pointer -> field omitted on HSET ->
//     HGET-miss on reload -> nil in Go -> JSON `null` (never 0/""), under
//     miniredis. Disambiguates HGET-miss vs written-nil vs written-empty.
//  2. ONE parameterised Lua CAS-merge script driven per writer (SetPending /
//     SetProcessing / Complete via an ARGV protocol, not three scripts — see
//     design doc §2.1), each writer proving BOTH the apply path AND the
//     equal-or-higher-rank DROP no-op.
//  3. Complete merge-preserve (SubmittedAt always / WorkerID if non-empty /
//     StartedAt if non-nil) + rank-3-vs-3 idempotent drop + race-ahead
//     SubmittedAt fallbacks, reusing the mandated Go-side recordFromEnvelope.
//  4. EXPIRE fixed-from-create: an advance-write does NOT slide the TTL.
//  5. Concurrent SetProcessing-vs-Complete resolves deterministically under
//     the Lua CAS with no lost terminal status.
//
// The Go/Lua boundary mirrors sink.go:24: RFC3339 parse + verdict extraction
// stay Go-side (recordFromEnvelope); Lua does HGET -> rank-branch ->
// HSET/HEXISTS/EXPIRE only. No cjson on the write path.
package jobstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// ---------------------------------------------------------------------------
// Encoding contract (Go side)
// ---------------------------------------------------------------------------
//
// One Redis hash per JobID at key "vismod:job:<id>". Every field is a string.
// moderation.Source is stored as a FLAT 3-field hash (source_kind/source_ref/
// source_media_type), NOT a nested JSON blob — keeps every value a plain string
// so Lua can compare/copy fields without cjson.
//
// NULL SEMANTICS (the crux). Redis hashes hold strings only and HGETALL returns
// an absent field as if it never existed, so the scheme must disambiguate three
// states. The contract collapses them safely:
//
//   - written-nil   : a nil *T is ENCODED AS FIELD-OMITTED. There is no null
//     sentinel string; absence IS nil. So "written-nil" == "HGET-miss".
//   - HGET-miss     : decode -> nil pointer -> JSON `null` (never 0 / "").
//   - written-empty : the two omitempty strings (WorkerID, Error) also omit on
//     empty, so an empty value is never written. Therefore a PRESENT field
//     ALWAYS denotes a real, non-nil value. No ""-vs-nil ambiguity can arise.
//
// SubmittedAt is the one non-null time: always written (Go supplies the
// race-ahead fallback value before encoding, mirroring mem.go).

const spikeKeyPrefix = "vismod:job:"

func spikeKey(id result.JobID) string { return spikeKeyPrefix + string(id) }

// encodeFields turns a JobRecord into the ordered field/value pairs to HSET.
// Nil pointers and empty omitempty strings are OMITTED (never written), which
// is the whole null-vs-zero contract. Returns a flat [f0,v0,f1,v1,...] slice.
func encodeFields(rec JobRecord) []string {
	var out []string
	put := func(f, v string) { out = append(out, f, v) }

	put("status", string(rec.Status))
	// Source: flat 3-field. Always present (may be empty strings on race-ahead;
	// those empties are meaningful zero values of a non-pointer struct, so we
	// still write them to keep Source a stable 3-field shape — decode reads them
	// straight back. This differs from the pointer fields, which OMIT on nil.)
	put("source_kind", rec.Source.Kind)
	put("source_ref", rec.Source.Ref)
	put("source_media_type", rec.Source.MediaType)

	// SubmittedAt: non-null, always written (RFC3339 UTC).
	put("submitted_at", rec.SubmittedAt.UTC().Format(time.RFC3339Nano))

	// omitempty strings: omit on empty.
	if rec.WorkerID != "" {
		put("worker_id", rec.WorkerID)
	}
	if rec.Error != "" {
		put("error", rec.Error)
	}

	// Nullable pointers: OMIT on nil.
	if rec.StartedAt != nil {
		put("started_at", rec.StartedAt.UTC().Format(time.RFC3339Nano))
	}
	if rec.FinishedAt != nil {
		put("finished_at", rec.FinishedAt.UTC().Format(time.RFC3339Nano))
	}
	if rec.Verdict != nil {
		put("verdict", string(*rec.Verdict))
	}
	if rec.Flagged != nil {
		put("flagged", strconv.FormatBool(*rec.Flagged))
	}
	if rec.TopCategory != nil {
		put("top_category", string(*rec.TopCategory))
	}
	if rec.MaxScore != nil {
		put("max_score", strconv.FormatFloat(*rec.MaxScore, 'g', -1, 64))
	}
	if rec.Confidence != nil {
		put("confidence", strconv.FormatFloat(*rec.Confidence, 'g', -1, 64))
	}
	return out
}

// decodeFields reverses encodeFields. A MISSING field decodes to the Go zero
// value: nil for every pointer (the null contract), "" for omitempty strings.
func decodeFields(h map[string]string) (JobRecord, error) {
	var rec JobRecord
	rec.Status = JobStatus(h["status"])
	rec.Source = moderation.Source{
		Kind:      h["source_kind"],
		Ref:       h["source_ref"],
		MediaType: h["source_media_type"],
	}
	if s, ok := h["submitted_at"]; ok {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return rec, fmt.Errorf("submitted_at: %w", err)
		}
		rec.SubmittedAt = t.UTC()
	}
	rec.WorkerID = h["worker_id"] // absent -> ""
	rec.Error = h["error"]

	parseTimePtr := func(f string) (*time.Time, error) {
		s, ok := h[f]
		if !ok {
			return nil, nil // HGET-miss -> nil (the contract)
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, err
		}
		u := t.UTC()
		return &u, nil
	}
	var err error
	if rec.StartedAt, err = parseTimePtr("started_at"); err != nil {
		return rec, err
	}
	if rec.FinishedAt, err = parseTimePtr("finished_at"); err != nil {
		return rec, err
	}
	if s, ok := h["verdict"]; ok {
		rec.Verdict = moderation.Ptr(moderation.Verdict(s))
	}
	if s, ok := h["flagged"]; ok {
		b, perr := strconv.ParseBool(s)
		if perr != nil {
			return rec, perr
		}
		rec.Flagged = &b
	}
	if s, ok := h["top_category"]; ok {
		rec.TopCategory = moderation.Ptr(moderation.Category(s))
	}
	if s, ok := h["max_score"]; ok {
		v, perr := strconv.ParseFloat(s, 64)
		if perr != nil {
			return rec, perr
		}
		rec.MaxScore = &v
	}
	if s, ok := h["confidence"]; ok {
		v, perr := strconv.ParseFloat(s, 64)
		if perr != nil {
			return rec, perr
		}
		rec.Confidence = &v
	}
	return rec, nil
}

// ---------------------------------------------------------------------------
// Lua scripts (write side). One generic CAS-merge script parameterised per
// writer. Lua does HGET(status) -> rank branch -> HSET / EXPIRE only.
//
// ARGV protocol:
//   ARGV[1] = target rank (1|2|3)
//   ARGV[2] = ttl seconds (applied ONLY on create; 0 = no ttl)
//   ARGV[3] = N overwrite pairs; then 2N args (always HSET)
//   ARGV[3+2N+1] = M preserve pairs; then 2M args (HSET only if field absent)
//
// Returns 1 on apply, 0 on rank-drop no-op.
// ---------------------------------------------------------------------------

const luaCASMerge = `
local cur = redis.call('HGET', KEYS[1], 'status')
local ranks = {pending=1, processing=2, done=3, dead_letter=3}
local curRank = 0
if cur then curRank = ranks[cur] or 0 end
local target = tonumber(ARGV[1])
if cur and curRank >= target then
  return 0   -- equal-or-higher rank -> DROP (monotonic + idempotent)
end
local isCreate = (not cur)

local i = 3
local nOver = tonumber(ARGV[i]); i = i + 1
for k = 1, nOver do
  redis.call('HSET', KEYS[1], ARGV[i], ARGV[i+1]); i = i + 2
end
local nPres = tonumber(ARGV[i]); i = i + 1
for k = 1, nPres do
  local f = ARGV[i]; local v = ARGV[i+1]; i = i + 2
  if redis.call('HEXISTS', KEYS[1], f) == 0 then
    redis.call('HSET', KEYS[1], f, v)
  end
end

-- EXPIRE fixed-from-create: only touch TTL when the key is first created, so an
-- advance-write never slides the window (mirrors mem.go immutable createdAt).
if isCreate then
  local ttl = tonumber(ARGV[2])
  if ttl > 0 then redis.call('EXPIRE', KEYS[1], ttl) end
end
return 1
`

// buildArgs assembles the ARGV slice from target rank, ttl, and the two pair
// lists produced Go-side.
func buildArgs(targetRank int, ttlSec int, overwrite, preserve []string) []any {
	if len(overwrite)%2 != 0 || len(preserve)%2 != 0 {
		panic("pairs must be even")
	}
	args := []any{targetRank, ttlSec, len(overwrite) / 2}
	for _, s := range overwrite {
		args = append(args, s)
	}
	args = append(args, len(preserve)/2)
	for _, s := range preserve {
		args = append(args, s)
	}
	return args
}

// ---- Go-side writers (encode + EVAL). Mirror mem.go SetPending/SetProcessing/Complete.

type spikeStore struct {
	rdb    *redis.Client
	script *redis.Script
	ttlSec int
}

func newSpikeStore(t *testing.T, ttlSec int) *spikeStore {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &spikeStore{rdb: rdb, script: redis.NewScript(luaCASMerge), ttlSec: ttlSec}
}

// runE executes the CAS-merge script and returns (applied, err) WITHOUT touching
// *testing.T — safe to call from a spawned goroutine (t.Fatalf must only be
// called from the test's own goroutine). run is the t.Fatalf-on-error wrapper
// for the single-goroutine tests.
func (s *spikeStore) runE(ctx context.Context, id result.JobID, target, ttl int, over, pres []string) (int64, error) {
	return s.script.Run(ctx, s.rdb, []string{spikeKey(id)}, buildArgs(target, ttl, over, pres)...).Int64()
}

func (s *spikeStore) run(t *testing.T, id result.JobID, target, ttl int, over, pres []string) int64 {
	t.Helper()
	n, err := s.runE(t.Context(), id, target, ttl, over, pres)
	if err != nil {
		t.Fatalf("EVAL: %v", err)
	}
	return n
}

func (s *spikeStore) setPending(t *testing.T, id result.JobID, src moderation.Source, submitted time.Time) int64 {
	over := []string{
		"status", string(StatusPending),
		"source_kind", src.Kind, "source_ref", src.Ref, "source_media_type", src.MediaType,
		"submitted_at", submitted.UTC().Format(time.RFC3339Nano),
	}
	return s.run(t, id, statusRank(StatusPending), s.ttlSec, over, nil)
}

func (s *spikeStore) setProcessing(t *testing.T, id result.JobID, workerID string, started time.Time) int64 {
	t.Helper()
	n, err := s.setProcessingE(t.Context(), id, workerID, started)
	if err != nil {
		t.Fatalf("EVAL: %v", err)
	}
	return n
}

// setProcessingE is the goroutine-safe core of setProcessing (no *testing.T).
func (s *spikeStore) setProcessingE(ctx context.Context, id result.JobID, workerID string, started time.Time) (int64, error) {
	over := []string{
		"status", string(StatusProcessing),
		"worker_id", workerID,
		"started_at", started.UTC().Format(time.RFC3339Nano),
	}
	// Race-ahead fallback: if no pending exists, SubmittedAt := started (mem.go:140).
	// preserve => set only if absent, so an existing pending's SubmittedAt wins.
	pres := []string{"submitted_at", started.UTC().Format(time.RFC3339Nano)}
	return s.runE(ctx, id, statusRank(StatusProcessing), s.ttlSec, over, pres)
}

func (s *spikeStore) complete(t *testing.T, env result.ResultEnvelope) int64 {
	t.Helper()
	n, err := s.completeE(t.Context(), env)
	if err != nil {
		t.Fatalf("EVAL: %v", err)
	}
	return n
}

// completeE is the goroutine-safe core of complete (no *testing.T).
func (s *spikeStore) completeE(ctx context.Context, env result.ResultEnvelope) (int64, error) {
	// Go-side: reuse the mandated helper (sink.go:24) for RFC3339 parse + verdict
	// extraction. Lua never parses time or ranks verdicts.
	rec := recordFromEnvelope(env)

	// Overwrite = terminal fields. encodeFields already omits nil pointers.
	over := encodeFields(rec)
	// Strip the merge-preserve fields out of overwrite; they go to preserve so an
	// existing record's values win (mem.go:162-170).
	over, submitted := splitOut(over, "submitted_at")
	over, worker := splitOut(over, "worker_id")   // absent if rec.WorkerID=="" (already omitted)
	over, started := splitOut(over, "started_at") // absent if rec.StartedAt==nil (already omitted)

	// Race-ahead SubmittedAt fallback already applied by mem-style Go logic:
	// recordFromEnvelope leaves SubmittedAt zero, so encodeFields wrote the
	// zero-formatted "0001-01-01T00:00:00Z". Replace it with a sensible non-zero
	// time (FinishedAt -> StartedAt), mirroring mem.go:178-184, before it becomes
	// the create-time value. (submitted is never "" — encodeFields always writes
	// submitted_at — so isZeroRFC3339 is the only trigger.)
	subVal := submitted
	if isZeroRFC3339(subVal) {
		if rec.FinishedAt != nil {
			subVal = rec.FinishedAt.UTC().Format(time.RFC3339Nano)
		} else if rec.StartedAt != nil {
			subVal = rec.StartedAt.UTC().Format(time.RFC3339Nano)
		}
	}

	var pres []string
	if subVal != "" {
		pres = append(pres, "submitted_at", subVal)
	}
	if worker != "" {
		pres = append(pres, "worker_id", worker)
	}
	if started != "" {
		pres = append(pres, "started_at", started)
	}
	// target derived from rec.Status (done and dead_letter both rank 3) rather
	// than hard-coded StatusDone, so the rank guard reads correctly when lifted.
	return s.runE(ctx, env.JobID, statusRank(rec.Status), s.ttlSec, over, pres)
}

// splitOut removes field f (and its value) from a flat pair slice, returning the
// remainder and the removed value ("" if absent).
func splitOut(pairs []string, f string) ([]string, string) {
	out := pairs[:0:0]
	var val string
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i] == f {
			val = pairs[i+1]
			continue
		}
		out = append(out, pairs[i], pairs[i+1])
	}
	return out, val
}

func isZeroRFC3339(s string) bool {
	t, err := time.Parse(time.RFC3339Nano, s)
	return err == nil && t.IsZero()
}

func (s *spikeStore) get(t *testing.T, id result.JobID) (JobRecord, bool) {
	t.Helper()
	h, err := s.rdb.HGetAll(t.Context(), spikeKey(id)).Result()
	if err != nil {
		t.Fatalf("HGETALL: %v", err)
	}
	if len(h) == 0 {
		return JobRecord{}, false
	}
	rec, derr := decodeFields(h)
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	return rec, true
}

// ===========================================================================
// PROOFS
// ===========================================================================

func rfc3339(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC().Format(time.RFC3339)
}

// AC: round-trip proof (CLAUDE.md P0). nil pointer -> field-omit -> HGET-miss
// -> nil in Go -> JSON null. And a dead_letter record leaves all 5 verdict
// scalars nil.
func TestSpike_EncodingRoundTrip_NilIsNull(t *testing.T) {
	s := newSpikeStore(t, 0)

	// dead_letter envelope: Result==nil => all verdict scalars nil; empty
	// timestamps => StartedAt/FinishedAt nil.
	env := result.ResultEnvelope{
		JobID:  "dl-1",
		Source: moderation.Source{Kind: "file", Ref: "x.jpg", MediaType: "image"},
		Error:  "boom",
	}
	if n := s.complete(t, env); n != 1 {
		t.Fatalf("apply want 1 got %d", n)
	}

	// The five verdict fields + started_at + finished_at must be ABSENT in Redis.
	for _, f := range []string{"verdict", "flagged", "top_category", "max_score", "confidence", "started_at", "finished_at"} {
		if ex, _ := s.rdb.HExists(t.Context(), spikeKey("dl-1"), f).Result(); ex {
			t.Errorf("field %q must be omitted for nil, but present", f)
		}
	}

	rec, ok := s.get(t, "dl-1")
	if !ok {
		t.Fatal("record missing")
	}
	if rec.Verdict != nil || rec.Flagged != nil || rec.TopCategory != nil || rec.MaxScore != nil || rec.Confidence != nil {
		t.Errorf("verdict scalars must decode nil, got %+v", rec)
	}
	if rec.StartedAt != nil || rec.FinishedAt != nil {
		t.Errorf("times must decode nil")
	}
	if rec.Status != StatusDeadLetter || rec.Error != "boom" {
		t.Errorf("status/error wrong: %+v", rec)
	}

	// JSON contract: nil -> `null`, never 0/"".
	b, _ := json.Marshal(rec)
	js := string(b)
	for _, want := range []string{`"verdict":null`, `"flagged":null`, `"top_category":null`, `"max_score":null`, `"confidence":null`, `"started_at":null`, `"finished_at":null`} {
		if !contains(js, want) {
			t.Errorf("JSON missing %s in %s", want, js)
		}
	}
	// Never a bare zero for these.
	for _, bad := range []string{`"max_score":0`, `"confidence":0`, `"flagged":false`} {
		if contains(js, bad) {
			t.Errorf("JSON leaked zero value %s", bad)
		}
	}
}

// AC: success round-trip — non-nil scalars survive the hash round-trip exactly.
func TestSpike_EncodingRoundTrip_SuccessScalars(t *testing.T) {
	s := newSpikeStore(t, 0)
	env := result.ResultEnvelope{
		JobID:      "ok-1",
		Source:     moderation.Source{Kind: "file", Ref: "y.png", MediaType: "image"},
		StartedAt:  rfc3339("2026-07-01T10:00:00Z"),
		FinishedAt: rfc3339("2026-07-01T10:00:05Z"),
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{
				Verdict:     moderation.VerdictBlock,
				Flagged:     true,
				TopCategory: moderation.Ptr(moderation.CategoryOther),
				MaxScore:    moderation.Ptr(0.667),
				Confidence:  moderation.Ptr(0.9),
			},
		},
	}
	s.complete(t, env)
	rec, ok := s.get(t, "ok-1")
	if !ok {
		t.Fatal("missing")
	}
	if rec.Status != StatusDone || *rec.Verdict != moderation.VerdictBlock || *rec.Flagged != true ||
		*rec.TopCategory != moderation.CategoryOther || *rec.MaxScore != 0.667 || *rec.Confidence != 0.9 {
		t.Errorf("scalar round-trip mismatch: %+v", rec)
	}
	if rec.StartedAt == nil || rec.FinishedAt == nil {
		t.Fatal("times should be set")
	}
}

// AC: SetPending apply + equal/higher-rank drop.
func TestSpike_SetPending_ApplyAndDrop(t *testing.T) {
	s := newSpikeStore(t, 0)
	src := moderation.Source{Kind: "file", Ref: "a", MediaType: "image"}
	sub := rfc3339parse("2026-07-01T09:00:00Z")

	if n := s.setPending(t, "p1", src, sub); n != 1 {
		t.Fatalf("first pending should apply, got %d", n)
	}
	if n := s.setPending(t, "p1", src, sub.Add(time.Hour)); n != 0 {
		t.Fatalf("second pending (equal rank) should DROP, got %d", n)
	}
	// Advance to processing, then a late pending must still drop (lower rank).
	s.setProcessing(t, "p1", "w1", sub.Add(time.Minute))
	if n := s.setPending(t, "p1", src, sub); n != 0 {
		t.Fatalf("pending after processing should DROP, got %d", n)
	}
	rec, _ := s.get(t, "p1")
	if rec.Status != StatusProcessing {
		t.Errorf("status regressed to %s", rec.Status)
	}
}

// AC: SetProcessing apply + drop-when-terminal + race-ahead SubmittedAt fallback.
func TestSpike_SetProcessing_ApplyDrop_RaceAheadFallback(t *testing.T) {
	s := newSpikeStore(t, 0)
	started := rfc3339parse("2026-07-01T09:05:00Z")

	// Race-ahead: SetProcessing with no prior pending -> SubmittedAt := started.
	if n := s.setProcessing(t, "pr1", "w1", started); n != 1 {
		t.Fatalf("apply want 1 got %d", n)
	}
	rec, _ := s.get(t, "pr1")
	if !rec.SubmittedAt.Equal(started) {
		t.Errorf("race-ahead SubmittedAt fallback want %v got %v", started, rec.SubmittedAt)
	}
	if rec.WorkerID != "w1" || rec.StartedAt == nil {
		t.Errorf("processing fields wrong: %+v", rec)
	}

	// A second processing (equal rank) drops; WorkerID stays w1.
	if n := s.setProcessing(t, "pr1", "w2", started.Add(time.Minute)); n != 0 {
		t.Fatalf("second processing should DROP, got %d", n)
	}
	rec, _ = s.get(t, "pr1")
	if rec.WorkerID != "w1" {
		t.Errorf("WorkerID must stay first worker, got %s", rec.WorkerID)
	}
}

// AC: SetProcessing preserves an existing pending's SubmittedAt + Source.
func TestSpike_SetProcessing_PreservesPending(t *testing.T) {
	s := newSpikeStore(t, 0)
	src := moderation.Source{Kind: "file", Ref: "b", MediaType: "video"}
	sub := rfc3339parse("2026-07-01T08:00:00Z")
	started := rfc3339parse("2026-07-01T08:10:00Z")
	s.setPending(t, "pp1", src, sub)
	s.setProcessing(t, "pp1", "w1", started)
	rec, _ := s.get(t, "pp1")
	if !rec.SubmittedAt.Equal(sub) {
		t.Errorf("SubmittedAt should be preserved from pending, want %v got %v", sub, rec.SubmittedAt)
	}
	if rec.Source != src {
		t.Errorf("Source should be preserved, got %+v", rec.Source)
	}
}

// AC: Complete idempotent rank-3-vs-3 drop + merge-preserve
// (SubmittedAt always / WorkerID if non-empty / StartedAt if non-nil).
func TestSpike_Complete_MergePreserve_IdempotentDrop(t *testing.T) {
	s := newSpikeStore(t, 0)
	src := moderation.Source{Kind: "file", Ref: "c", MediaType: "image"}
	sub := rfc3339parse("2026-07-01T07:00:00Z")
	started := rfc3339parse("2026-07-01T07:01:00Z")
	s.setPending(t, "c1", src, sub)
	s.setProcessing(t, "c1", "w1", started)

	// Envelope carries DIFFERENT started_at + no worker; merge must keep the
	// processing record's SubmittedAt, WorkerID=w1, and StartedAt.
	env := result.ResultEnvelope{
		JobID:      "c1",
		Source:     src,
		StartedAt:  rfc3339("2026-07-01T07:30:00Z"), // must be ignored (existing wins)
		FinishedAt: rfc3339("2026-07-01T07:05:00Z"),
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow, Flagged: false},
		},
	}
	if n := s.complete(t, env); n != 1 {
		t.Fatalf("complete should apply, got %d", n)
	}
	rec, _ := s.get(t, "c1")
	if rec.Status != StatusDone {
		t.Fatalf("want done, got %s", rec.Status)
	}
	if !rec.SubmittedAt.Equal(sub) {
		t.Errorf("SubmittedAt must be preserved, want %v got %v", sub, rec.SubmittedAt)
	}
	if rec.WorkerID != "w1" {
		t.Errorf("WorkerID must be preserved, got %s", rec.WorkerID)
	}
	if rec.StartedAt == nil || !rec.StartedAt.Equal(started) {
		t.Errorf("StartedAt must be preserved (existing wins), want %v got %v", started, rec.StartedAt)
	}
	if rec.Verdict == nil || *rec.Verdict != moderation.VerdictAllow {
		t.Errorf("verdict overlay wrong: %+v", rec.Verdict)
	}

	// Idempotent replay: rank 3 vs 3 -> DROP, record unchanged. Compare by JSON
	// value (decode allocates fresh pointers each Get, so %+v prints differing
	// addresses for identical content).
	beforeJSON := mustJSON(t, "c1", s)
	if n := s.complete(t, env); n != 0 {
		t.Fatalf("replay should DROP, got %d", n)
	}
	if afterJSON := mustJSON(t, "c1", s); beforeJSON != afterJSON {
		t.Errorf("idempotent replay changed record:\n%s\n%s", beforeJSON, afterJSON)
	}
}

// AC: Complete race-ahead — lands before any pending/processing. SubmittedAt
// falls back FinishedAt -> StartedAt (mem.go:178-184).
func TestSpike_Complete_RaceAheadFallback(t *testing.T) {
	s := newSpikeStore(t, 0)
	env := result.ResultEnvelope{
		JobID:      "ca1",
		Source:     moderation.Source{Kind: "file", Ref: "d", MediaType: "image"},
		FinishedAt: rfc3339("2026-07-01T06:00:00Z"),
		Result:     &moderation.NormalizedResult{Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow}},
	}
	s.complete(t, env)
	rec, _ := s.get(t, "ca1")
	if rec.SubmittedAt.Format(time.RFC3339) != "2026-07-01T06:00:00Z" {
		t.Errorf("SubmittedAt should fall back to FinishedAt, got %v", rec.SubmittedAt)
	}
}

// AC: concurrent SetProcessing vs Complete resolve deterministically — terminal
// status is never lost regardless of arrival order (Lua CAS is atomic).
func TestSpike_ConcurrentProcessingVsComplete(t *testing.T) {
	s := newSpikeStore(t, 0)
	src := moderation.Source{Kind: "file", Ref: "e", MediaType: "image"}
	started := rfc3339parse("2026-07-01T05:00:00Z")
	env := result.ResultEnvelope{
		JobID:      "cc1",
		Source:     src,
		FinishedAt: rfc3339("2026-07-01T05:00:02Z"),
		Result:     &moderation.NormalizedResult{Overall: moderation.OverallVerdict{Verdict: moderation.VerdictBlock, Flagged: true}},
	}
	// Goroutines must NOT touch *testing.T (t.Fatalf from a non-test goroutine is
	// testing misuse: it runs Goexit on the wrong goroutine, so the failure goes
	// unrecorded and the test can hang). Use the *E variants and funnel any EVAL
	// error to a channel; assert on the main goroutine after wg.Wait().
	ctx := t.Context()
	errc := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, err := s.setProcessingE(ctx, "cc1", "w1", started); errc <- err }()
	go func() { defer wg.Done(); _, err := s.completeE(ctx, env); errc <- err }()
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatalf("concurrent writer EVAL error: %v", err)
		}
	}

	rec, _ := s.get(t, "cc1")
	if rec.Status != StatusDone {
		t.Fatalf("terminal status LOST — got %s (Lua CAS should guarantee done wins)", rec.Status)
	}
	if rec.Verdict == nil || *rec.Verdict != moderation.VerdictBlock {
		t.Errorf("verdict lost: %+v", rec.Verdict)
	}
}

// AC: EXPIRE fixed-from-create — an advance-write does NOT slide the TTL.
func TestSpike_ExpireNoSlideOnAdvance(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s := &spikeStore{rdb: rdb, script: redis.NewScript(luaCASMerge), ttlSec: 100}

	src := moderation.Source{Kind: "file", Ref: "f", MediaType: "image"}
	sub := rfc3339parse("2026-07-01T04:00:00Z")
	s.setPending(t, "ttl1", src, sub) // create -> EXPIRE 100s

	ttl0 := mr.TTL(spikeKey("ttl1"))
	if ttl0 <= 0 || ttl0 > 100*time.Second {
		t.Fatalf("create TTL want ~100s, got %v", ttl0)
	}

	// Let 40s elapse, then an advance-write (processing).
	mr.FastForward(40 * time.Second)
	s.setProcessing(t, "ttl1", "w1", sub.Add(time.Minute))

	ttl1 := mr.TTL(spikeKey("ttl1"))
	// If EXPIRE slid, ttl1 would be back at ~100s. Fixed-from-create keeps it
	// counting down from the original window (~60s remaining).
	if ttl1 > 65*time.Second {
		t.Fatalf("advance-write SLID the TTL: want ~60s remaining, got %v", ttl1)
	}
	if ttl1 <= 0 {
		t.Fatalf("TTL unexpectedly expired: %v", ttl1)
	}
}

func rfc3339parse(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func contains(hay, needle string) bool { return strings.Contains(hay, needle) }

func mustJSON(t *testing.T, id result.JobID, s *spikeStore) string {
	t.Helper()
	rec, ok := s.get(t, id)
	if !ok {
		t.Fatalf("record %s missing", id)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
