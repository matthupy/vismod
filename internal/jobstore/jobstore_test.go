package jobstore

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// ---- helpers ----------------------------------------------------------------

func newStore(t *testing.T, maxEntries int, ttl time.Duration, opts ...MemOption) *MemStore {
	t.Helper()
	return NewMemStore(maxEntries, ttl, opts...)
}

var ctx = context.Background()

func src() moderation.Source {
	return moderation.Source{Kind: "file", Ref: "/tmp/img.jpg", MediaType: "image"}
}

func now() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func envelopeDone(id result.JobID) result.ResultEnvelope {
	v := moderation.VerdictAllow
	f := false
	cat := moderation.CategorySexual
	score := 0.1
	conf := 0.9
	return result.ResultEnvelope{
		JobID:      id,
		Source:     src(),
		StartedAt:  "2026-01-02T03:04:05Z",
		FinishedAt: "2026-01-02T03:04:06Z",
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{
				Verdict:     v,
				Flagged:     f,
				TopCategory: &cat,
				MaxScore:    &score,
				Confidence:  &conf,
			},
		},
	}
}

func envelopeDeadLetterErr(id result.JobID) result.ResultEnvelope {
	return result.ResultEnvelope{
		JobID:  id,
		Source: src(),
		Error:  "provider unavailable",
	}
}

func envelopeDeadLetterNilResult(id result.JobID) result.ResultEnvelope {
	return result.ResultEnvelope{
		JobID:  id,
		Source: src(),
		// Result is nil and Error is "" — still dead_letter per spec
	}
}

// ---- compile-time interface assertions --------------------------------------

var _ JobStore = (*MemStore)(nil)
var _ result.Sink = (*StoreSink)(nil)

// ---- Positive: recordFromEnvelope parses timestamps -------------------------

func TestRecordFromEnvelopeParsesTimestamps(t *testing.T) {
	id := result.JobID("j-ts")
	env := result.ResultEnvelope{
		JobID:      id,
		Source:     src(),
		StartedAt:  "2026-01-02T03:04:05Z",
		FinishedAt: "2026-01-02T03:04:06Z",
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow},
		},
	}
	rec := recordFromEnvelope(env)

	wantStarted := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	wantFinished := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)

	if rec.StartedAt == nil || !rec.StartedAt.Equal(wantStarted) {
		t.Errorf("StartedAt: got %v want %v", rec.StartedAt, wantStarted)
	}
	if rec.FinishedAt == nil || !rec.FinishedAt.Equal(wantFinished) {
		t.Errorf("FinishedAt: got %v want %v", rec.FinishedAt, wantFinished)
	}
}

// ---- Negative/Edge: empty timestamps -----------------------------------------

func TestEmptyTimestampsYieldNilNoLog(t *testing.T) {
	// Empty string must yield nil without any log/panic.
	rec := recordFromEnvelope(result.ResultEnvelope{
		JobID:  "j-empty-ts",
		Source: src(),
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow},
		},
		StartedAt:  "",
		FinishedAt: "",
	})
	if rec.StartedAt != nil {
		t.Errorf("StartedAt: expected nil for empty string, got %v", rec.StartedAt)
	}
	if rec.FinishedAt != nil {
		t.Errorf("FinishedAt: expected nil for empty string, got %v", rec.FinishedAt)
	}
}

// ---- Negative/Edge: TTL eviction --------------------------------------------

func TestTTLEviction(t *testing.T) {
	fakeNow := now()
	clock := func() time.Time { return fakeNow }

	m := newStore(t, 100, 10*time.Minute, WithClock(clock))
	id := result.JobID("j-ttl")
	_ = m.SetPending(ctx, id, src(), fakeNow)

	// Before TTL elapses.
	_, found, _ := m.Get(ctx, id)
	if !found {
		t.Fatal("expected to find record before TTL")
	}

	// Advance clock past TTL.
	fakeNow = fakeNow.Add(11 * time.Minute)
	_, found, err := m.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after TTL: %v", err)
	}
	if found {
		t.Error("expected found=false after TTL elapsed")
	}

	// Key should be gone from map (lazy eviction).
	m.mu.Lock()
	_, stillThere := m.entries[id]
	m.mu.Unlock()
	if stillThere {
		t.Error("TTL-expired entry still in map after Get")
	}
}

// ---- Negative/Edge: max-entries eviction ------------------------------------

// TestNewMemStoreClampsNonPositiveMaxEntries covers the defensive lower bound
// on maxEntries. NewMemStore is exported and config validation is not part of
// this change, so a caller-supplied maxEntries<=0 must not be trusted
// verbatim: at maxEntries=0 (or negative) every insert would evict on
// insertion and the store would thrash at effective capacity 1. Instead,
// NewMemStore must clamp to defaultMaxEntries so the store behaves sanely
// until upstream config validation lands.
func TestNewMemStoreClampsNonPositiveMaxEntries(t *testing.T) {
	m := NewMemStore(0, 0)

	const inserted = 10
	var ids []result.JobID
	for i := 0; i < inserted; i++ {
		id := result.JobID(fmt.Sprintf("job-clamp-%d", i))
		ids = append(ids, id)
		_ = m.SetPending(ctx, id, src(), now())
	}

	for _, id := range ids {
		_, found, _ := m.Get(ctx, id)
		if !found {
			t.Errorf("entry %v should still be retained under the clamped default capacity, not evicted", id)
		}
	}
}

func TestMaxEntriesEviction(t *testing.T) {
	const max = 5
	const extras = 3

	// Use monotonically advancing clock so insertion order is deterministic.
	var mu sync.Mutex
	fakeNow := now()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := fakeNow
		fakeNow = fakeNow.Add(time.Millisecond)
		return t
	}

	m := newStore(t, max, 0, WithClock(clock))

	var ids []result.JobID
	for i := 0; i < max+extras; i++ {
		id := result.JobID(fmt.Sprintf("job-evict-%d", i))
		ids = append(ids, id)
		_ = m.SetPending(ctx, id, src(), now())
	}

	// Map must not exceed max.
	m.mu.Lock()
	size := len(m.entries)
	m.mu.Unlock()
	if size > max {
		t.Errorf("map size %d exceeds maxEntries %d", size, max)
	}

	// Oldest entries (first max entries inserted minus evicted) must be gone.
	for i := 0; i < extras; i++ {
		_, found, _ := m.Get(ctx, ids[i])
		if found {
			t.Errorf("oldest entry %v should have been evicted", ids[i])
		}
	}

	// Newest entries must still be present.
	for i := max; i < max+extras; i++ {
		_, found, _ := m.Get(ctx, ids[i])
		if !found {
			t.Errorf("newest entry %v should still be present", ids[i])
		}
	}
}

// ---- P0: concurrent out-of-order race (headline test) ----------------------
//
// This test fires SetPending, SetProcessing, and Complete for the same JobID
// in random order from concurrent goroutines, then asserts the record is
// terminal (done or dead_letter), never regressed. The test is written to
// exercise the race detector when run under -race in CI.
//
// A single `go test -race` invocation (no -count needed) already runs
// numJobs * rounds = 50 * 20 = 1000 shuffled 5-op sequences (~5000 op-runs
// total, each against a fresh MemStore), so CI gets meaningful volume on
// every run without relying on a manual -count flag. Pass -count=N to
// multiply that further for extended manual fuzzing.

func TestConcurrentOutOfOrder(t *testing.T) {
	const numJobs = 50
	const rounds = 20

	for job := 0; job < numJobs; job++ {
		for round := 0; round < rounds; round++ {
			id := result.JobID(fmt.Sprintf("job-concurrent-%d-%d", job, round))
			m := NewMemStore(1000, 0)
			env := envelopeDone(id)

			ops := []func(){
				func() { _ = m.SetPending(ctx, id, src(), now()) },
				func() { _ = m.SetProcessing(ctx, id, "worker-x", now()) },
				func() { _ = m.Complete(ctx, env) },
				// Extra late-arriving duplicates to stress idempotency.
				func() { _ = m.SetPending(ctx, id, src(), now()) },
				func() { _ = m.Complete(ctx, env) },
			}

			// Shuffle to randomize order.
			rand.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })

			var wg sync.WaitGroup
			for _, op := range ops {
				wg.Add(1)
				op := op
				go func() {
					defer wg.Done()
					op()
				}()
			}
			wg.Wait()

			rec, found, err := m.Get(ctx, id)
			if err != nil {
				t.Errorf("job %d round %d: Get error: %v", job, round, err)
				continue
			}
			if !found {
				t.Errorf("job %d round %d: record not found after concurrent ops", job, round)
				continue
			}
			// Final status MUST be terminal — never pending or processing.
			if rec.Status != StatusDone && rec.Status != StatusDeadLetter {
				t.Errorf("job %d round %d: status regressed to %v (want terminal)", job, round, rec.Status)
			}
		}
	}
}

// ---- FIX 1: insertOrder/entries desync (TTL evict leak + capacity FIFO) ----

// TestTTLChurnKeepsInsertOrderBounded covers consequence (a) of FIX 1:
// under a TTL-dominated, sub-capacity workload (resident set stays well below
// maxEntries because each job is read via Get — triggering lazy TTL evict —
// before the next is inserted), insertOrder must NOT grow unboundedly with
// the number of jobs inserted over time. Before the fix, evict() left the
// stale id in insertOrder forever, since evictOldest (the only pruner) never
// runs under capacity pressure.
func TestTTLChurnKeepsInsertOrderBounded(t *testing.T) {
	const maxEntries = 100
	const ttl = time.Minute
	const numJobs = 1000 // numJobs >> maxEntries

	fakeNow := now()
	clock := func() time.Time { return fakeNow }
	m := newStore(t, maxEntries, ttl, WithClock(clock))

	for i := 0; i < numJobs; i++ {
		id := result.JobID(fmt.Sprintf("job-%d", i))
		_ = m.SetPending(ctx, id, src(), fakeNow)

		// Advance the clock past TTL and read it back, forcing lazy TTL evict
		// before the next insert. Resident set therefore never reaches capacity.
		fakeNow = fakeNow.Add(ttl + time.Second)
		_, found, _ := m.Get(ctx, id)
		if found {
			t.Fatalf("job %d: expected TTL-expired entry to be evicted on Get", i)
		}
	}

	m.mu.Lock()
	orderLen := len(m.insertOrder)
	entriesLen := len(m.entries)
	m.mu.Unlock()

	if orderLen != 0 {
		t.Errorf("insertOrder should be empty after every inserted job was TTL-evicted via Get, got len=%d", orderLen)
	}
	if entriesLen != 0 {
		t.Errorf("entries should be empty after every inserted job was TTL-evicted via Get, got len=%d", entriesLen)
	}
	// Bounded regardless: must never approach numJobs.
	if orderLen >= numJobs {
		t.Errorf("insertOrder grew unboundedly with numJobs: len=%d numJobs=%d", orderLen, numJobs)
	}
}

// TestEvictRecreateCapacityFIFO covers consequence (b) of FIX 1: a
// TTL-evicted record that is RECREATED by a late SetPending must not be
// prematurely evicted by a stale front entry left behind in insertOrder by
// the earlier TTL eviction — eviction order must still follow true insertion
// order. Before the fix, evict() left id A's stale id at the front of
// insertOrder; a different id B inserted between the TTL evict and the
// recreate became the genuinely oldest live record, but the stale front
// entry still named A. The next evictOldest popped the stale front id A,
// found the key DOES exist (the freshly recreated live A), and deleted A —
// the wrong (newer) record — while B, the true oldest, wrongly survived.
func TestEvictRecreateCapacityFIFO(t *testing.T) {
	const maxEntries = 5

	fakeNow := now()
	clock := func() time.Time { return fakeNow }
	m := newStore(t, maxEntries, time.Minute, WithClock(clock))

	idA := result.JobID("job-A")
	_ = m.SetPending(ctx, idA, src(), fakeNow)

	// TTL-evict A via Get.
	fakeNow = fakeNow.Add(2 * time.Minute)
	_, found, _ := m.Get(ctx, idA)
	if found {
		t.Fatalf("expected A to be TTL-evicted before recreation")
	}

	// B is inserted after A's TTL eviction but before A's recreation, so B
	// is genuinely older than the recreated A in true insertion order.
	idB := result.JobID("job-B")
	_ = m.SetPending(ctx, idB, src(), fakeNow)

	// Recreate A via a late SetPending — A is now newer than B.
	_ = m.SetPending(ctx, idA, src(), fakeNow)

	// Fill with other ids to force exactly one evictOldest call. Total
	// resident set reaches maxEntries+1 (A, B, job-0..extras-1), so exactly
	// one eviction occurs; the true oldest live record is B.
	const extras = maxEntries - 1
	for i := 0; i < extras; i++ {
		id := result.JobID(fmt.Sprintf("job-%d", i))
		_ = m.SetPending(ctx, id, src(), fakeNow)
	}

	// A (recreated, newer) must survive; B (true oldest live record) must be
	// the one evicted — NOT a stale front entry wrongly targeting A.
	recA, foundA, _ := m.Get(ctx, idA)
	_, foundB, _ := m.Get(ctx, idB)
	if !foundA {
		t.Errorf("expected recreated record A to still be present after capacity eviction, but it was evicted")
	}
	if foundA && recA.Status != StatusPending {
		t.Errorf("expected recreated record A to be pending, got %v", recA.Status)
	}
	if foundB {
		t.Errorf("expected B (true oldest live record) to be evicted by capacity pressure, but it survived")
	}

	// Invariant: every id in insertOrder is also in entries (no orphans).
	m.mu.Lock()
	for _, id := range m.insertOrder {
		if _, ok := m.entries[id]; !ok {
			t.Errorf("insertOrder contains orphan id %v not present in entries", id)
		}
	}
	m.mu.Unlock()
}
