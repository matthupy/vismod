package jobstore

// Test taxonomy (VISMOD-17)
//
// jobstore_test.go originally held all 26 test funcs (+3 TestCompleteSetsStatusCorrectly
// subcases) against a single driver (*MemStore). This file carves out the ones that
// exercise ONLY the JobStore interface contract into a shared, factory-parameterised
// conformance suite that runs against every driver (see the `drivers` table below).
// The remainder — tests that reach into MemStore-internal fields/behaviour, tests of
// unexported package helpers, and the concurrency stress test — stay in
// jobstore_test.go since they have no portable equivalent on other drivers.
//
// Bucket A — shared conformance suite (this file, parameterised over `drivers`):
// these bodies only call the JobStore interface (SetPending/SetProcessing/Complete/Get)
// plus json.Marshal on the returned JobRecord; they hold for any conforming driver.
//   1.  TestLifecyclePendingProcessingDone                          — happy-path lifecycle
//   2.  TestLifecyclePendingProcessingDeadLetter                    — happy-path lifecycle (dead_letter)
//   3.  TestCompleteSetsStatusCorrectly (3 subcases: done /          — Complete → done vs dead_letter
//       dead_letter-on-error / dead_letter-on-nil)
//   4.  TestNullableScalarsMarshalNull                              — nullable JSON contract
//   5.  TestNullableScalarsMarshalValues                            — nullable JSON contract
//   6.  TestCompleteIdempotent                                      — idempotency contract
//   7.  TestMonotonicityDoneBlocksSetPending                        — strict-rank monotonicity
//   8.  TestMonotonicityDoneBlocksSetProcessing                     — strict-rank monotonicity
//   9.  TestMonotonicityDeadLetterBlocksSetPending                  — strict-rank monotonicity
//   10. TestMonotonicityDeadLetterBlocksSetProcessing                — strict-rank monotonicity
//   11. TestMonotonicityProcessingBlocksSetPending                   — strict-rank monotonicity
//   12. TestNoRawOrFramesInJobRecord                                 — payload-hygiene contract
//   13. TestStoreSinkDelegates                                       — StoreSink → JobStore.Complete
//   14. TestCompleteWithNoExistingRecord                             — race-ahead Complete
//   15. TestSetProcessingWithNoExistingRecord                        — race-ahead SetProcessing
//   16. TestSetProcessingWithNoExistingRecordSetsSubmittedAtFallback — SubmittedAt fallback contract
//   17. TestCompleteWithNoExistingRecordSetsSubmittedAtFallback      — SubmittedAt fallback contract
//   18. TestUnparseableTimestampsYieldNilNoPanic                     — bad-timestamp handling contract
//
// Bucket B — mem-only internals, stay in jobstore_test.go (no Redis analog):
//   19. TestNewMemStoreClampsNonPositiveMaxEntries — asserts NewMemStore's constructor clamp
//       (maxEntries<=0 -> defaultMaxEntries); a constructor-argument quirk specific to the
//       MemStore struct literal, not part of the JobStore contract.
//   20. TestTTLEviction — asserts lazy TTL-on-Get eviction via MemStore.mu/m.entries and
//       WithClock; TTL-as-modeled here is a MemStore-specific mechanism (a Redis driver would
//       use native key EXPIRE with different observable timing, not this lazy-check-on-Get path).
//   21. TestMaxEntriesEviction — asserts oldest-first eviction against MemStore.mu/m.entries at
//       a fixed `maxEntries` capacity; capacity-bounded eviction is a MemStore-only concept (a
//       Redis driver has no in-process maxEntries knob).
//   22. TestTTLChurnKeepsInsertOrderBounded — asserts MemStore's internal `insertOrder` slice
//       stays bounded under TTL churn (regression test for the insertOrder/entries desync fix);
//       `insertOrder` is a MemStore-private FIFO bookkeeping structure with no cross-driver
//       equivalent.
//   23. TestEvictRecreateCapacityFIFO — asserts MemStore's `evictOldest` FIFO ordering survives
//       a TTL-evict-then-recreate sequence without evicting the wrong (newer) record; again
//       exercises MemStore-private `insertOrder`/`entries` invariants directly.
//
// Bucket C — unexported package helper, stay in jobstore_test.go (not a JobStore method):
//   24. TestRecordFromEnvelopeParsesTimestamps — exercises the unexported `recordFromEnvelope`
//       helper directly, not any JobStore driver.
//   25. TestEmptyTimestampsYieldNilNoLog — exercises the unexported `recordFromEnvelope` helper
//       directly, not any JobStore driver.
//
// Bucket D — concurrency, mem-only, stay in jobstore_test.go:
//   26. TestConcurrentOutOfOrder — fires SetPending/SetProcessing/Complete concurrently against
//       a single in-process *MemStore under `go test -race` to catch data races on MemStore's
//       mutex-guarded internals. A cross-process driver's concurrency safety (e.g. a future Redis
//       driver's WATCH/MULTI or Lua-script atomicity) cannot be validated this way — it requires
//       a live server and out-of-process races, which is out of scope here (violates the
//       no-network/no-credentials acceptance criterion for this package's tests) and is tracked
//       as a separate follow-up integration ticket, not part of this in-process suite.
//
// (Task 1's TestMapStoreContract in mapstore_test.go is a separate driver-guard test that
// exercises mapStore directly; it is not part of this parity set and is left as-is.)

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// storeFactory constructs a fresh JobStore instance for a single (sub)test.
type storeFactory func(t *testing.T) JobStore

// drivers is the factory table every Bucket A conformance test runs against.
var drivers = []struct {
	name    string
	factory storeFactory
}{
	{"mem", func(t *testing.T) JobStore { return NewMemStore(100, 0) }},
	{"map", func(t *testing.T) JobStore { return newMapStore() }},
}

// ---- Positive: happy-path lifecycle -----------------------------------------

func TestLifecyclePendingProcessingDone(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("job-1")
			submitted := now()

			if err := s.SetPending(ctx, id, src(), submitted); err != nil {
				t.Fatalf("SetPending: %v", err)
			}
			rec, found, err := s.Get(ctx, id)
			if err != nil || !found || rec.Status != StatusPending {
				t.Fatalf("after SetPending: found=%v err=%v status=%v", found, err, rec.Status)
			}
			if !rec.SubmittedAt.Equal(submitted) {
				t.Errorf("SubmittedAt: got %v want %v", rec.SubmittedAt, submitted)
			}

			startedAt := now().Add(time.Second)
			if err := s.SetProcessing(ctx, id, "worker-1", startedAt); err != nil {
				t.Fatalf("SetProcessing: %v", err)
			}
			rec, found, err = s.Get(ctx, id)
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
			if err := s.Complete(ctx, env); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			rec, found, err = s.Get(ctx, id)
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
		})
	}
}

func TestLifecyclePendingProcessingDeadLetter(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("job-dl")
			submitted := now()

			_ = s.SetPending(ctx, id, src(), submitted)
			_ = s.SetProcessing(ctx, id, "w1", now().Add(time.Second))
			env := envelopeDeadLetterErr(id)
			if err := s.Complete(ctx, env); err != nil {
				t.Fatalf("Complete dead_letter: %v", err)
			}
			rec, found, _ := s.Get(ctx, id)
			if !found || rec.Status != StatusDeadLetter {
				t.Fatalf("expected dead_letter, got found=%v status=%v", found, rec.Status)
			}
			if rec.Error != "provider unavailable" {
				t.Errorf("Error: got %q", rec.Error)
			}
		})
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
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					s := d.factory(t)
					id := result.JobID("j-" + tc.name)
					if err := s.Complete(ctx, tc.env(id)); err != nil {
						t.Fatalf("Complete: %v", err)
					}
					rec, found, _ := s.Get(ctx, id)
					if !found || rec.Status != tc.wantStatus {
						t.Errorf("got found=%v status=%v, want %v", found, rec.Status, tc.wantStatus)
					}
				})
			}
		})
	}
}

// ---- Positive: nullable scalars marshal to JSON null -------------------------

func TestNullableScalarsMarshalNull(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			// A dead-letter record has all five verdict scalars nil + nil timestamps.
			s := d.factory(t)
			id := result.JobID("j-null")
			_ = s.Complete(ctx, envelopeDeadLetterNilResult(id))
			rec, _, _ := s.Get(ctx, id)

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
		})
	}
}

func TestNullableScalarsMarshalValues(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			// A done record with real verdict scalars must marshal real values.
			s := d.factory(t)
			id := result.JobID("j-vals")
			env := envelopeDone(id)
			_ = s.Complete(ctx, env)
			rec, _, _ := s.Get(ctx, id)

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
		})
	}
}

// ---- Positive: Complete idempotent ------------------------------------------

func TestCompleteIdempotent(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("j-idem")
			env := envelopeDone(id)

			if err := s.Complete(ctx, env); err != nil {
				t.Fatalf("first Complete: %v", err)
			}
			rec1, _, _ := s.Get(ctx, id)

			// Replay the same envelope.
			if err := s.Complete(ctx, env); err != nil {
				t.Fatalf("second Complete: %v", err)
			}
			rec2, _, _ := s.Get(ctx, id)

			b1, _ := json.Marshal(rec1)
			b2, _ := json.Marshal(rec2)
			if string(b1) != string(b2) {
				t.Errorf("record changed on replay:\nbefore: %s\nafter:  %s", b1, b2)
			}
		})
	}
}

// ---- Negative/Edge: status monotonicity (P0) --------------------------------

func TestMonotonicityDoneBlocksSetPending(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("j-mono-1")

			_ = s.Complete(ctx, envelopeDone(id))
			rec0, _, _ := s.Get(ctx, id)
			if rec0.Status != StatusDone {
				t.Fatalf("setup: expected done, got %v", rec0.Status)
			}

			// Late SetPending must be a no-op.
			if err := s.SetPending(ctx, id, src(), now()); err != nil {
				t.Fatalf("SetPending returned error: %v", err)
			}
			rec1, _, _ := s.Get(ctx, id)
			if rec1.Status != StatusDone {
				t.Errorf("SetPending regressed done → %v", rec1.Status)
			}
		})
	}
}

func TestMonotonicityDoneBlocksSetProcessing(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("j-mono-2")
			_ = s.Complete(ctx, envelopeDone(id))

			if err := s.SetProcessing(ctx, id, "w", now()); err != nil {
				t.Fatalf("SetProcessing returned error: %v", err)
			}
			rec, _, _ := s.Get(ctx, id)
			if rec.Status != StatusDone {
				t.Errorf("SetProcessing regressed done → %v", rec.Status)
			}
		})
	}
}

func TestMonotonicityDeadLetterBlocksSetPending(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("j-mono-3")
			_ = s.Complete(ctx, envelopeDeadLetterErr(id))

			if err := s.SetPending(ctx, id, src(), now()); err != nil {
				t.Fatalf("SetPending returned error: %v", err)
			}
			rec, _, _ := s.Get(ctx, id)
			if rec.Status != StatusDeadLetter {
				t.Errorf("SetPending regressed dead_letter → %v", rec.Status)
			}
		})
	}
}

func TestMonotonicityDeadLetterBlocksSetProcessing(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("j-mono-4")
			_ = s.Complete(ctx, envelopeDeadLetterErr(id))

			if err := s.SetProcessing(ctx, id, "w", now()); err != nil {
				t.Fatalf("SetProcessing returned error: %v", err)
			}
			rec, _, _ := s.Get(ctx, id)
			if rec.Status != StatusDeadLetter {
				t.Errorf("SetProcessing regressed dead_letter → %v", rec.Status)
			}
		})
	}
}

func TestMonotonicityProcessingBlocksSetPending(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("j-mono-5")
			_ = s.SetPending(ctx, id, src(), now())
			_ = s.SetProcessing(ctx, id, "w", now())

			// Late second SetPending must be a no-op.
			if err := s.SetPending(ctx, id, src(), now()); err != nil {
				t.Fatalf("SetPending returned error: %v", err)
			}
			rec, _, _ := s.Get(ctx, id)
			if rec.Status != StatusProcessing {
				t.Errorf("SetPending regressed processing → %v", rec.Status)
			}
		})
	}
}

// ---- Negative/Edge: unparseable timestamps ----------------------------------

func TestUnparseableTimestampsYieldNilNoPanic(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
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
			s := d.factory(t)
			err := s.Complete(ctx, env)
			if err != nil {
				t.Fatalf("Complete with bad timestamps: %v", err)
			}
			rec, found, _ := s.Get(ctx, id)
			if !found {
				t.Fatal("record not found after Complete with bad timestamps")
			}
			if rec.StartedAt != nil {
				t.Errorf("StartedAt should be nil for unparseable input, got %v", rec.StartedAt)
			}
			if rec.FinishedAt != nil {
				t.Errorf("FinishedAt should be nil for unparseable input, got %v", rec.FinishedAt)
			}
		})
	}
}

// ---- Negative/Edge: no frame/Raw/OCR data stored ----------------------------

func TestNoRawOrFramesInJobRecord(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
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

			_ = s.Complete(ctx, env)
			rec, _, _ := s.Get(ctx, id)

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
		})
	}
}

// ---- Negative/Edge: StoreSink delegates to store.Complete -------------------

func TestStoreSinkDelegates(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			sink := NewStoreSink(s)
			id := result.JobID("j-sink")
			env := envelopeDone(id)

			if err := sink.Write(ctx, env); err != nil {
				t.Fatalf("StoreSink.Write: %v", err)
			}
			rec, found, _ := s.Get(ctx, id)
			if !found || rec.Status != StatusDone {
				t.Errorf("expected done record via StoreSink, got found=%v status=%v", found, rec.Status)
			}
		})
	}
}

// ---- Edge: Complete/SetProcessing on absent record creates it ---------------

func TestCompleteWithNoExistingRecord(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			// Complete races ahead of SetPending — legal (record created at done).
			s := d.factory(t)
			id := result.JobID("j-race-ahead")
			_ = s.Complete(ctx, envelopeDone(id))
			rec, found, _ := s.Get(ctx, id)
			if !found || rec.Status != StatusDone {
				t.Errorf("expected done record, got found=%v status=%v", found, rec.Status)
			}
		})
	}
}

func TestSetProcessingWithNoExistingRecord(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("j-proc-first")
			_ = s.SetProcessing(ctx, id, "w", now())
			rec, found, _ := s.Get(ctx, id)
			if !found || rec.Status != StatusProcessing {
				t.Errorf("expected processing, got found=%v status=%v", found, rec.Status)
			}
		})
	}
}

// TestSetProcessingWithNoExistingRecordSetsSubmittedAtFallback covers the race
// where SetProcessing lands before any SetPending: no record exists, so
// SubmittedAt is never set from a prior write. Without a fallback this
// serializes as the epoch-zero value "0001-01-01T00:00:00Z" for the duration
// of the processing window (or permanently, if the worker crashes and the job
// stays "processing" — the wedged-job signal). SetProcessing must fall back
// SubmittedAt to startedAt on the processing-first (!ok) path, mirroring the
// fallback already applied in Complete.
func TestSetProcessingWithNoExistingRecordSetsSubmittedAtFallback(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("j-proc-first-fallback")
			startedAt := now()
			_ = s.SetProcessing(ctx, id, "w", startedAt)

			rec, found, _ := s.Get(ctx, id)
			if !found {
				t.Fatalf("expected record to be found")
			}
			if rec.SubmittedAt.IsZero() {
				t.Fatalf("expected non-zero SubmittedAt fallback, got zero value")
			}
			if !rec.SubmittedAt.Equal(startedAt) {
				t.Errorf("expected SubmittedAt == startedAt, got SubmittedAt=%v startedAt=%v", rec.SubmittedAt, startedAt)
			}
		})
	}
}

// TestCompleteWithNoExistingRecordSetsSubmittedAtFallback covers the race
// where Complete lands before any SetPending: no record exists, so
// SubmittedAt is never set from a prior write. Without a fallback this
// serializes as the epoch-zero value "0001-01-01T00:00:00Z", which is a
// confusing value for GET /v1/jobs/{id}. Complete must fall back to
// FinishedAt (preferred) or StartedAt when SubmittedAt would otherwise be
// zero.
func TestCompleteWithNoExistingRecordSetsSubmittedAtFallback(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			s := d.factory(t)
			id := result.JobID("j-race-ahead-fallback")
			env := envelopeDone(id) // has both StartedAt and FinishedAt set
			_ = s.Complete(ctx, env)

			rec, found, _ := s.Get(ctx, id)
			if !found {
				t.Fatalf("expected record to be found")
			}
			if rec.SubmittedAt.IsZero() {
				t.Fatalf("expected non-zero SubmittedAt fallback, got zero value")
			}
			if rec.FinishedAt == nil {
				t.Fatalf("expected FinishedAt to be set on the record")
			}
			if !rec.SubmittedAt.Equal(*rec.FinishedAt) {
				t.Errorf("expected SubmittedAt == *FinishedAt, got SubmittedAt=%v FinishedAt=%v", rec.SubmittedAt, *rec.FinishedAt)
			}
		})
	}
}
