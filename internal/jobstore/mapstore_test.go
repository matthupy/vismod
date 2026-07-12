package jobstore

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// mapStore is a minimal map-based JobStore implementation for testing.
// It reuses recordFromEnvelope and statusRank helpers from the package,
// maintaining the same monotonic-rank + SubmittedAt-fallback semantics as MemStore,
// but with NO TTL, NO capacity eviction, and NO insertOrder tracking.
type mapStore struct {
	mu      sync.Mutex
	entries map[result.JobID]JobRecord
}

// newMapStore returns a new mapStore.
func newMapStore() *mapStore {
	return &mapStore{entries: make(map[result.JobID]JobRecord)}
}

// SetPending records a newly submitted job. No-ops if a record with equal or
// higher rank already exists.
func (m *mapStore) SetPending(_ context.Context, id result.JobID, src moderation.Source, submittedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.entries[id]
	if ok && statusRank(existing.Status) >= statusRank(StatusPending) {
		// Equal or higher rank exists — drop (monotonicity rule).
		return nil
	}

	rec := JobRecord{
		JobID:       id,
		Status:      StatusPending,
		Source:      src,
		SubmittedAt: submittedAt,
	}
	m.entries[id] = rec
	return nil
}

// SetProcessing advances a job to processing state. No-ops if a record with
// equal or higher rank already exists. Preserves SubmittedAt and Source from
// any existing pending record, or falls back to startedAt if no record exists.
func (m *mapStore) SetProcessing(_ context.Context, id result.JobID, workerID string, startedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.entries[id]
	if ok && statusRank(existing.Status) >= statusRank(StatusProcessing) {
		// Equal or higher rank (processing/done/dead_letter) — drop.
		return nil
	}

	rec := JobRecord{
		JobID:     id,
		Status:    StatusProcessing,
		WorkerID:  workerID,
		StartedAt: &startedAt,
	}
	if ok {
		// Preserve fields from the existing pending record.
		rec.Source = existing.Source
		rec.SubmittedAt = existing.SubmittedAt
	} else {
		// Race-ahead guard: SetProcessing landed before any SetPending.
		rec.SubmittedAt = startedAt
	}
	m.entries[id] = rec
	return nil
}

// Complete transitions a job to done or dead_letter. If a record already exists
// at any rank, it merges: terminal fields from recordFromEnvelope overlay the
// existing record while preserving SubmittedAt, WorkerID, and StartedAt.
// Idempotent: replaying a terminal envelope on an already-terminal record is
// rank 3 vs 3 ⇒ dropped ⇒ record unchanged.
func (m *mapStore) Complete(_ context.Context, env result.ResultEnvelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.entries[env.JobID]
	if ok && statusRank(existing.Status) >= statusRank(StatusDone) {
		// Already terminal — idempotent drop (rank 3 vs 3).
		return nil
	}

	rec := recordFromEnvelope(env)
	if ok {
		// Merge: preserve prior SubmittedAt, WorkerID, StartedAt (if set).
		rec.SubmittedAt = existing.SubmittedAt
		if existing.WorkerID != "" {
			rec.WorkerID = existing.WorkerID
		}
		if existing.StartedAt != nil {
			rec.StartedAt = existing.StartedAt
		}
	}
	// Race-ahead guard: if Complete lands before any SetPending ever ran (no
	// existing record, or an existing record whose SubmittedAt was itself never
	// set), SubmittedAt would otherwise serialize as the confusing epoch-zero
	// value "0001-01-01T00:00:00Z". Fall back to a sensible non-zero time:
	// prefer FinishedAt, else StartedAt, else leave zero (both unset is only
	// possible with unparseable/empty timestamps on a race-ahead Complete).
	if rec.SubmittedAt.IsZero() {
		if rec.FinishedAt != nil {
			rec.SubmittedAt = *rec.FinishedAt
		} else if rec.StartedAt != nil {
			rec.SubmittedAt = *rec.StartedAt
		}
	}
	m.entries[env.JobID] = rec
	return nil
}

// Get returns the record for id. Returns found=false when no record exists.
// No TTL expiry (mapStore is unbounded test-only storage).
func (m *mapStore) Get(_ context.Context, id result.JobID) (JobRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.entries[id]
	return rec, ok, nil
}

// TestMapStoreContract exercises mapStore directly, covering the full lifecycle,
// monotonicity, idempotency, and SubmittedAt fallback semantics.
func TestMapStoreContract(t *testing.T) {
	t.Run("full lifecycle SetPending→SetProcessing→Complete→Get", func(t *testing.T) {
		store := newMapStore()
		id := result.JobID("map-job-1")
		submitted := now()

		// SetPending
		if err := store.SetPending(ctx, id, src(), submitted); err != nil {
			t.Fatalf("SetPending: %v", err)
		}
		rec, found, err := store.Get(ctx, id)
		if err != nil || !found || rec.Status != StatusPending {
			t.Fatalf("after SetPending: found=%v err=%v status=%v", found, err, rec.Status)
		}
		if !rec.SubmittedAt.Equal(submitted) {
			t.Errorf("SubmittedAt: got %v want %v", rec.SubmittedAt, submitted)
		}

		// SetProcessing
		startedAt := now().Add(time.Second)
		if err := store.SetProcessing(ctx, id, "worker-1", startedAt); err != nil {
			t.Fatalf("SetProcessing: %v", err)
		}
		rec, found, err = store.Get(ctx, id)
		if err != nil || !found || rec.Status != StatusProcessing {
			t.Fatalf("after SetProcessing: found=%v err=%v status=%v", found, err, rec.Status)
		}
		if rec.WorkerID != "worker-1" {
			t.Errorf("WorkerID: got %q want %q", rec.WorkerID, "worker-1")
		}
		// SubmittedAt must be preserved from pending.
		if !rec.SubmittedAt.Equal(submitted) {
			t.Errorf("SubmittedAt preserved: got %v want %v", rec.SubmittedAt, submitted)
		}
		if rec.StartedAt == nil || !rec.StartedAt.Equal(startedAt) {
			t.Errorf("StartedAt: got %v want %v", rec.StartedAt, startedAt)
		}

		// Complete
		env := envelopeDone(id)
		if err := store.Complete(ctx, env); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		rec, found, err = store.Get(ctx, id)
		if err != nil || !found || rec.Status != StatusDone {
			t.Fatalf("after Complete: found=%v err=%v status=%v", found, err, rec.Status)
		}
		// SubmittedAt and WorkerID must be preserved through Complete merge.
		if !rec.SubmittedAt.Equal(submitted) {
			t.Errorf("SubmittedAt preserved through Complete: got %v want %v", rec.SubmittedAt, submitted)
		}
		if rec.WorkerID != "worker-1" {
			t.Errorf("WorkerID preserved through Complete: got %q want %q", rec.WorkerID, "worker-1")
		}
		if rec.StartedAt == nil || !rec.StartedAt.Equal(startedAt) {
			t.Errorf("StartedAt preserved: got %v want %v", rec.StartedAt, startedAt)
		}
	})

	t.Run("monotonicity: late SetPending after Complete is no-op", func(t *testing.T) {
		store := newMapStore()
		id := result.JobID("map-job-2")

		// Complete first (races ahead of SetPending).
		env := envelopeDone(id)
		if err := store.Complete(ctx, env); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		rec0, _, _ := store.Get(ctx, id)
		if rec0.Status != StatusDone {
			t.Fatalf("setup: expected done, got %v", rec0.Status)
		}

		// Late SetPending must be a no-op.
		if err := store.SetPending(ctx, id, src(), now()); err != nil {
			t.Fatalf("SetPending returned error: %v", err)
		}
		rec1, _, _ := store.Get(ctx, id)
		if rec1.Status != StatusDone {
			t.Errorf("SetPending regressed done → %v", rec1.Status)
		}
	})

	t.Run("Complete idempotent: replaying done envelope leaves record byte-identical", func(t *testing.T) {
		store := newMapStore()
		id := result.JobID("map-job-3")
		env := envelopeDone(id)

		if err := store.Complete(ctx, env); err != nil {
			t.Fatalf("first Complete: %v", err)
		}
		rec1, _, _ := store.Get(ctx, id)

		// Replay the same envelope.
		if err := store.Complete(ctx, env); err != nil {
			t.Fatalf("second Complete: %v", err)
		}
		rec2, _, _ := store.Get(ctx, id)

		b1, _ := json.Marshal(rec1)
		b2, _ := json.Marshal(rec2)
		if string(b1) != string(b2) {
			t.Errorf("record changed on replay:\nbefore: %s\nafter:  %s", b1, b2)
		}
	})

	t.Run("SubmittedAt fallback: SetProcessing before any SetPending", func(t *testing.T) {
		store := newMapStore()
		id := result.JobID("map-job-4")
		startedAt := now()

		// SetProcessing with no prior SetPending.
		if err := store.SetProcessing(ctx, id, "w", startedAt); err != nil {
			t.Fatalf("SetProcessing: %v", err)
		}

		rec, found, _ := store.Get(ctx, id)
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

	t.Run("SubmittedAt fallback: Complete before any SetPending uses FinishedAt", func(t *testing.T) {
		store := newMapStore()
		id := result.JobID("map-job-5")
		env := envelopeDone(id) // has StartedAt and FinishedAt

		if err := store.Complete(ctx, env); err != nil {
			t.Fatalf("Complete: %v", err)
		}

		rec, found, _ := store.Get(ctx, id)
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

// Compile-time assertion that mapStore satisfies JobStore.
var _ JobStore = (*mapStore)(nil)
