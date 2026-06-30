package jobstore

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"
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

// ---- Positive: happy-path lifecycle -----------------------------------------

func TestLifecyclePendingProcessingDone(t *testing.T) {
	m := newStore(t, 100, 0)
	id := result.JobID("job-1")
	submitted := now()

	if err := m.SetPending(ctx, id, src(), submitted); err != nil {
		t.Fatalf("SetPending: %v", err)
	}
	rec, found, err := m.Get(ctx, id)
	if err != nil || !found || rec.Status != StatusPending {
		t.Fatalf("after SetPending: found=%v err=%v status=%v", found, err, rec.Status)
	}
	if !rec.SubmittedAt.Equal(submitted) {
		t.Errorf("SubmittedAt: got %v want %v", rec.SubmittedAt, submitted)
	}

	startedAt := now().Add(time.Second)
	if err := m.SetProcessing(ctx, id, "worker-1", startedAt); err != nil {
		t.Fatalf("SetProcessing: %v", err)
	}
	rec, found, err = m.Get(ctx, id)
	if err != nil || !found || rec.Status != StatusProcessing {
		t.Fatalf("after SetProcessing: found=%v err=%v status=%v", found, err, rec.Status)
	}
	if rec.WorkerID != "worker-1" {
		t.Errorf("WorkerID: got %q want %q", rec.WorkerID, "worker-1")
	}
	// SubmittedAt preserved from pending.
	if !rec.SubmittedAt.Equal(submitted) {
		t.Errorf("SubmittedAt preserved: got %v want %v", rec.SubmittedAt, submitted)
	}

	env := envelopeDone(id)
	if err := m.Complete(ctx, env); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rec, found, err = m.Get(ctx, id)
	if err != nil || !found || rec.Status != StatusDone {
		t.Fatalf("after Complete: found=%v err=%v status=%v", found, err, rec.Status)
	}
	// SubmittedAt and WorkerID preserved through Complete merge.
	if !rec.SubmittedAt.Equal(submitted) {
		t.Errorf("SubmittedAt preserved through Complete: got %v want %v", rec.SubmittedAt, submitted)
	}
	if rec.WorkerID != "worker-1" {
		t.Errorf("WorkerID preserved through Complete: got %q want %q", rec.WorkerID, "worker-1")
	}
	// StartedAt preserved from SetProcessing.
	if rec.StartedAt == nil || !rec.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt preserved: got %v want %v", rec.StartedAt, startedAt)
	}
}

func TestLifecyclePendingProcessingDeadLetter(t *testing.T) {
	m := newStore(t, 100, 0)
	id := result.JobID("job-dl")
	submitted := now()

	_ = m.SetPending(ctx, id, src(), submitted)
	_ = m.SetProcessing(ctx, id, "w1", now().Add(time.Second))
	env := envelopeDeadLetterErr(id)
	if err := m.Complete(ctx, env); err != nil {
		t.Fatalf("Complete dead_letter: %v", err)
	}
	rec, found, _ := m.Get(ctx, id)
	if !found || rec.Status != StatusDeadLetter {
		t.Fatalf("expected dead_letter, got found=%v status=%v", found, rec.Status)
	}
	if rec.Error != "provider unavailable" {
		t.Errorf("Error: got %q", rec.Error)
	}
}

// ---- Positive: Complete sets done vs dead_letter ----------------------------

func TestCompleteSetsStatusCorrectly(t *testing.T) {
	tests := []struct {
		name       string
		env        func(result.JobID) result.ResultEnvelope
		wantStatus JobStatus
	}{
		{"done on non-nil result", envelopeDone, StatusDone},
		{"dead_letter on error", envelopeDeadLetterErr, StatusDeadLetter},
		{"dead_letter on nil result", envelopeDeadLetterNilResult, StatusDeadLetter},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newStore(t, 100, 0)
			id := result.JobID("j-" + tc.name)
			if err := m.Complete(ctx, tc.env(id)); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			rec, found, _ := m.Get(ctx, id)
			if !found || rec.Status != tc.wantStatus {
				t.Errorf("got found=%v status=%v, want %v", found, rec.Status, tc.wantStatus)
			}
		})
	}
}

// ---- Positive: nullable scalars marshal to JSON null -------------------------

func TestNullableScalarsMarshalNull(t *testing.T) {
	// A dead-letter record has all five verdict scalars nil + nil timestamps.
	m := newStore(t, 100, 0)
	id := result.JobID("j-null")
	_ = m.Complete(ctx, envelopeDeadLetterNilResult(id))
	rec, _, _ := m.Get(ctx, id)

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(data)

	// All five verdict pointers must appear as null, not 0 and not absent.
	for _, key := range []string{`"verdict":null`, `"flagged":null`, `"top_category":null`, `"max_score":null`, `"confidence":null`} {
		if !strings.Contains(js, key) {
			t.Errorf("expected %s in JSON, got: %s", key, js)
		}
	}
	// Nullable timestamps: started_at and finished_at must appear as null.
	for _, key := range []string{`"started_at":null`, `"finished_at":null`} {
		if !strings.Contains(js, key) {
			t.Errorf("expected %s in JSON, got: %s", key, js)
		}
	}
	// Must NOT contain literal 0 for the numeric nulls or missing keys.
	if strings.Contains(js, `"max_score":0`) || strings.Contains(js, `"confidence":0`) {
		t.Errorf("unexpected numeric zero in nullable field: %s", js)
	}
}

func TestNullableScalarsMarshalValues(t *testing.T) {
	// A done record with real verdict scalars must marshal real values.
	m := newStore(t, 100, 0)
	id := result.JobID("j-vals")
	env := envelopeDone(id)
	_ = m.Complete(ctx, env)
	rec, _, _ := m.Get(ctx, id)

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(data)

	if !strings.Contains(js, `"verdict":"allow"`) {
		t.Errorf("expected verdict allow in JSON: %s", js)
	}
	if !strings.Contains(js, `"flagged":false`) {
		t.Errorf("expected flagged false in JSON: %s", js)
	}
	if !strings.Contains(js, `"top_category":"SEXUAL"`) {
		t.Errorf("expected top_category SEXUAL in JSON: %s", js)
	}
	if !strings.Contains(js, `"max_score":0.1`) {
		t.Errorf("expected max_score 0.1 in JSON: %s", js)
	}
	if !strings.Contains(js, `"confidence":0.9`) {
		t.Errorf("expected confidence 0.9 in JSON: %s", js)
	}
}

// ---- Positive: Complete idempotent ------------------------------------------

func TestCompleteIdempotent(t *testing.T) {
	m := newStore(t, 100, 0)
	id := result.JobID("j-idem")
	env := envelopeDone(id)

	if err := m.Complete(ctx, env); err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	rec1, _, _ := m.Get(ctx, id)

	// Replay the same envelope.
	if err := m.Complete(ctx, env); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	rec2, _, _ := m.Get(ctx, id)

	b1, _ := json.Marshal(rec1)
	b2, _ := json.Marshal(rec2)
	if string(b1) != string(b2) {
		t.Errorf("record changed on replay:\nbefore: %s\nafter:  %s", b1, b2)
	}
}

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

// ---- Negative/Edge: status monotonicity (P0) --------------------------------

func TestMonotonicityDoneBlocksSetPending(t *testing.T) {
	m := newStore(t, 100, 0)
	id := result.JobID("j-mono-1")

	_ = m.Complete(ctx, envelopeDone(id))
	rec0, _, _ := m.Get(ctx, id)
	if rec0.Status != StatusDone {
		t.Fatalf("setup: expected done, got %v", rec0.Status)
	}

	// Late SetPending must be a no-op.
	if err := m.SetPending(ctx, id, src(), now()); err != nil {
		t.Fatalf("SetPending returned error: %v", err)
	}
	rec1, _, _ := m.Get(ctx, id)
	if rec1.Status != StatusDone {
		t.Errorf("SetPending regressed done → %v", rec1.Status)
	}
}

func TestMonotonicityDoneBlocksSetProcessing(t *testing.T) {
	m := newStore(t, 100, 0)
	id := result.JobID("j-mono-2")
	_ = m.Complete(ctx, envelopeDone(id))

	if err := m.SetProcessing(ctx, id, "w", now()); err != nil {
		t.Fatalf("SetProcessing returned error: %v", err)
	}
	rec, _, _ := m.Get(ctx, id)
	if rec.Status != StatusDone {
		t.Errorf("SetProcessing regressed done → %v", rec.Status)
	}
}

func TestMonotonicityDeadLetterBlocksSetPending(t *testing.T) {
	m := newStore(t, 100, 0)
	id := result.JobID("j-mono-3")
	_ = m.Complete(ctx, envelopeDeadLetterErr(id))

	if err := m.SetPending(ctx, id, src(), now()); err != nil {
		t.Fatalf("SetPending returned error: %v", err)
	}
	rec, _, _ := m.Get(ctx, id)
	if rec.Status != StatusDeadLetter {
		t.Errorf("SetPending regressed dead_letter → %v", rec.Status)
	}
}

func TestMonotonicityDeadLetterBlocksSetProcessing(t *testing.T) {
	m := newStore(t, 100, 0)
	id := result.JobID("j-mono-4")
	_ = m.Complete(ctx, envelopeDeadLetterErr(id))

	if err := m.SetProcessing(ctx, id, "w", now()); err != nil {
		t.Fatalf("SetProcessing returned error: %v", err)
	}
	rec, _, _ := m.Get(ctx, id)
	if rec.Status != StatusDeadLetter {
		t.Errorf("SetProcessing regressed dead_letter → %v", rec.Status)
	}
}

func TestMonotonicityProcessingBlocksSetPending(t *testing.T) {
	m := newStore(t, 100, 0)
	id := result.JobID("j-mono-5")
	_ = m.SetPending(ctx, id, src(), now())
	_ = m.SetProcessing(ctx, id, "w", now())

	// Late second SetPending must be a no-op.
	if err := m.SetPending(ctx, id, src(), now()); err != nil {
		t.Fatalf("SetPending returned error: %v", err)
	}
	rec, _, _ := m.Get(ctx, id)
	if rec.Status != StatusProcessing {
		t.Errorf("SetPending regressed processing → %v", rec.Status)
	}
}

// ---- Negative/Edge: unparseable timestamps ----------------------------------

func TestUnparseableTimestampsYieldNilNoPanic(t *testing.T) {
	id := result.JobID("j-badts")
	env := result.ResultEnvelope{
		JobID:      id,
		Source:     src(),
		StartedAt:  "not-a-time",
		FinishedAt: "also-bad",
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow},
		},
	}
	// Must not panic, must return nil, must still store the record.
	m := newStore(t, 100, 0)
	err := m.Complete(ctx, env)
	if err != nil {
		t.Fatalf("Complete with bad timestamps: %v", err)
	}
	rec, found, _ := m.Get(ctx, id)
	if !found {
		t.Fatal("record not found after Complete with bad timestamps")
	}
	if rec.StartedAt != nil {
		t.Errorf("StartedAt should be nil for unparseable input, got %v", rec.StartedAt)
	}
	if rec.FinishedAt != nil {
		t.Errorf("FinishedAt should be nil for unparseable input, got %v", rec.FinishedAt)
	}
}

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
		id := result.JobID(time.Now().String() + string(rune('a'+i)))
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

// ---- Negative/Edge: no frame/Raw/OCR data stored ----------------------------

func TestNoRawOrFramesInJobRecord(t *testing.T) {
	m := newStore(t, 100, 0)
	id := result.JobID("j-hygiene")

	rawData := json.RawMessage(`{"sensitive":"csam-detection-output"}`)
	cat := moderation.CategorySexual
	score := 0.99
	env := result.ResultEnvelope{
		JobID:  id,
		Source: src(),
		Result: &moderation.NormalizedResult{
			Raw: rawData,
			Frames: []moderation.FrameResult{
				{
					Categories: []moderation.CategoryResult{
						{Category: cat, Score: &score},
					},
				},
			},
			Overall: moderation.OverallVerdict{
				Verdict:     moderation.VerdictBlock,
				TopCategory: &cat,
				MaxScore:    &score,
			},
		},
	}

	_ = m.Complete(ctx, env)
	rec, _, _ := m.Get(ctx, id)

	// Marshal the record and ensure no raw/frames keys are present.
	data, _ := json.Marshal(rec)
	js := string(data)

	if strings.Contains(js, "sensitive") {
		t.Errorf("Raw data leaked into JobRecord JSON: %s", js)
	}
	if strings.Contains(js, `"frames"`) {
		t.Errorf("frames leaked into JobRecord JSON: %s", js)
	}
	if strings.Contains(js, `"raw"`) {
		t.Errorf("raw leaked into JobRecord JSON: %s", js)
	}
	if strings.Contains(js, "csam-detection-output") {
		t.Errorf("Raw content leaked into JobRecord JSON: %s", js)
	}
}

// ---- Negative/Edge: StoreSink delegates to store.Complete -------------------

func TestStoreSinkDelegates(t *testing.T) {
	m := newStore(t, 100, 0)
	sink := NewStoreSink(m)
	id := result.JobID("j-sink")
	env := envelopeDone(id)

	if err := sink.Write(ctx, env); err != nil {
		t.Fatalf("StoreSink.Write: %v", err)
	}
	rec, found, _ := m.Get(ctx, id)
	if !found || rec.Status != StatusDone {
		t.Errorf("expected done record via StoreSink, got found=%v status=%v", found, rec.Status)
	}
}

// ---- P0: concurrent out-of-order race (headline test) ----------------------
//
// Run with: go test ./internal/jobstore/ -run TestConcurrentOutOfOrder -count=200
//
// This test fires SetPending, SetProcessing, and Complete for the same JobID
// in random order from concurrent goroutines, then asserts the record is
// terminal (done or dead_letter), never regressed. The test is written to
// exercise the race detector when run under -race in CI.

func TestConcurrentOutOfOrder(t *testing.T) {
	const numJobs = 50

	for job := 0; job < numJobs; job++ {
		id := result.JobID(time.Now().String() + string(rune('A'+job%26)) + string(rune('a'+job%26)))
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
			t.Errorf("job %d: Get error: %v", job, err)
			continue
		}
		if !found {
			t.Errorf("job %d: record not found after concurrent ops", job)
			continue
		}
		// Final status MUST be terminal — never pending or processing.
		if rec.Status != StatusDone && rec.Status != StatusDeadLetter {
			t.Errorf("job %d: status regressed to %v (want terminal)", job, rec.Status)
		}
	}
}

// ---- Edge: Complete/SetProcessing on absent record creates it ---------------

func TestCompleteWithNoExistingRecord(t *testing.T) {
	// Complete races ahead of SetPending — legal (record created at done).
	m := newStore(t, 100, 0)
	id := result.JobID("j-race-ahead")
	_ = m.Complete(ctx, envelopeDone(id))
	rec, found, _ := m.Get(ctx, id)
	if !found || rec.Status != StatusDone {
		t.Errorf("expected done record, got found=%v status=%v", found, rec.Status)
	}
}

func TestSetProcessingWithNoExistingRecord(t *testing.T) {
	m := newStore(t, 100, 0)
	id := result.JobID("j-proc-first")
	_ = m.SetProcessing(ctx, id, "w", now())
	rec, found, _ := m.Get(ctx, id)
	if !found || rec.Status != StatusProcessing {
		t.Errorf("expected processing, got found=%v status=%v", found, rec.Status)
	}
}
