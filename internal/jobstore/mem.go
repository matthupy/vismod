package jobstore

import (
	"context"
	"sync"
	"time"

	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// memEntry wraps a JobRecord with the metadata needed for TTL and eviction.
type memEntry struct {
	record    JobRecord
	createdAt time.Time // immutable after first insert; TTL measured from here
}

// defaultMaxEntries is the capacity NewMemStore clamps to when called with a
// non-positive maxEntries. Config validation that should normally enforce
// maxEntries > 0 lives upstream and is not part of this package, and
// NewMemStore is exported, so a caller-supplied value <= 0 must not be trusted
// verbatim — it would otherwise evict on every insert and thrash at effective
// capacity 1.
const defaultMaxEntries = 10000

// MemOption is a functional option for NewMemStore.
type MemOption func(*MemStore)

// WithClock injects a custom clock into the store for deterministic TTL tests.
// Defaults to time.Now.
func WithClock(now func() time.Time) MemOption {
	return func(m *MemStore) {
		m.now = now
	}
}

// MemStore is a bounded, TTL-expiring, in-memory JobStore. It is the dev/CLI
// driver — non-durable, single-process only. Production uses the future Redis
// driver.
//
// All exported methods are safe for concurrent use. Every write acquires the
// mutex before applying the strict-rank monotonicity rule, so concurrent
// out-of-order SetPending / SetProcessing / Complete calls can never regress
// a terminal record.
type MemStore struct {
	mu          sync.Mutex
	entries     map[result.JobID]*memEntry
	insertOrder []result.JobID // tracks insertion order for oldest-first eviction

	maxEntries int
	ttl        time.Duration
	now        func() time.Time
}

// NewMemStore creates a MemStore with the given capacity and TTL.
// maxEntries is expected to be > 0 (validated by config upstream); a
// non-positive maxEntries is clamped to defaultMaxEntries rather than
// allowed to thrash the store at effective capacity 1. ttl > 0 enables lazy
// TTL expiry on Get.
func NewMemStore(maxEntries int, ttl time.Duration, opts ...MemOption) *MemStore {
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	m := &MemStore{
		entries:    make(map[result.JobID]*memEntry),
		maxEntries: maxEntries,
		ttl:        ttl,
		now:        time.Now,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// compile-time assertion: *MemStore must satisfy JobStore.
var _ JobStore = (*MemStore)(nil)

// SetPending records a newly submitted job. No-ops if a record with equal or
// higher rank already exists.
func (m *MemStore) SetPending(_ context.Context, id result.JobID, src moderation.Source, submittedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.entries[id]
	if ok && statusRank(existing.record.Status) >= statusRank(StatusPending) {
		// Equal or higher rank exists — drop (monotonicity rule).
		return nil
	}

	rec := JobRecord{
		JobID:       id,
		Status:      StatusPending,
		Source:      src,
		SubmittedAt: submittedAt,
	}
	m.upsert(id, rec, ok)
	return nil
}

// SetProcessing advances a job to processing state. No-ops if a record with
// equal or higher rank already exists. Preserves SubmittedAt and Source from
// any existing pending record. Note: because of the no-op-on-equal-rank rule,
// a redelivered job picked up by a second worker after the first already
// reached processing will NOT overwrite WorkerID/StartedAt — the stored
// WorkerID reflects the first worker to reach processing, not necessarily
// the one currently (or still) running the job.
func (m *MemStore) SetProcessing(_ context.Context, id result.JobID, workerID string, startedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.entries[id]
	if ok && statusRank(existing.record.Status) >= statusRank(StatusProcessing) {
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
		rec.Source = existing.record.Source
		rec.SubmittedAt = existing.record.SubmittedAt
	}
	m.upsert(id, rec, ok)
	return nil
}

// Complete transitions a job to done or dead_letter. If a record already exists
// at any rank, it merges: terminal fields from recordFromEnvelope overlay the
// existing record while preserving SubmittedAt, WorkerID, and StartedAt.
// Idempotent: replaying a terminal envelope on an already-terminal record is
// rank 3 vs 3 ⇒ dropped ⇒ record unchanged.
func (m *MemStore) Complete(_ context.Context, env result.ResultEnvelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.entries[env.JobID]
	if ok && statusRank(existing.record.Status) >= statusRank(StatusDone) {
		// Already terminal — idempotent drop (rank 3 vs 3).
		return nil
	}

	rec := recordFromEnvelope(env)
	if ok {
		// Merge: preserve prior SubmittedAt, WorkerID, StartedAt (if set).
		rec.SubmittedAt = existing.record.SubmittedAt
		if existing.record.WorkerID != "" {
			rec.WorkerID = existing.record.WorkerID
		}
		if existing.record.StartedAt != nil {
			rec.StartedAt = existing.record.StartedAt
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
	m.upsert(env.JobID, rec, ok)
	return nil
}

// Get returns the record for id. Performs lazy TTL expiry: if the entry's TTL
// has elapsed, it is deleted and found=false is returned. Returns found=false
// when no record exists.
func (m *MemStore) Get(_ context.Context, id result.JobID) (JobRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[id]
	if !ok {
		return JobRecord{}, false, nil
	}

	// Lazy TTL eviction.
	if m.ttl > 0 && m.now().Sub(entry.createdAt) > m.ttl {
		m.evict(id)
		return JobRecord{}, false, nil
	}

	return entry.record, true, nil
}

// upsert stores a record. If isUpdate is false (new key), it handles max-entries
// eviction before inserting. If isUpdate is true, it only overwrites the record
// without changing the createdAt or insertion order.
func (m *MemStore) upsert(id result.JobID, rec JobRecord, isUpdate bool) {
	if isUpdate {
		m.entries[id].record = rec
		return
	}
	// New key: evict oldest if at capacity.
	if len(m.entries) >= m.maxEntries {
		m.evictOldest()
	}
	m.entries[id] = &memEntry{
		record:    rec,
		createdAt: m.now(),
	}
	m.insertOrder = append(m.insertOrder, id)
}

// evictOldest removes the oldest entry by insertion order (FIFO approximation).
// It scans insertOrder, skipping any IDs that no longer exist in the map
// (already evicted by TTL or a previous eviction).
func (m *MemStore) evictOldest() {
	for len(m.insertOrder) > 0 {
		oldest := m.insertOrder[0]
		m.insertOrder = m.insertOrder[1:]
		if _, exists := m.entries[oldest]; exists {
			delete(m.entries, oldest)
			return
		}
	}
}

// evict removes a single entry by id from the map only; it does NOT touch
// insertOrder. The stale id is left in insertOrder and is skipped lazily the
// next time evictOldest scans past it (evictOldest is the only place that
// cleans up insertOrder). Removing from the map here is sufficient so that
// subsequent Gets see the entry as gone.
func (m *MemStore) evict(id result.JobID) {
	delete(m.entries, id)
}
