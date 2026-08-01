package queue

import (
	"context"
	"sync"
)

// MemDLQ is an in-memory DeadLetterSink. Entries are never dropped; the
// depth cap is enforced upstream by rejecting new enqueues (see
// QueueConfig.DeadLetterMax), not by discarding dead letters.
type MemDLQ struct {
	mu      sync.Mutex
	entries []DeadLetterEntry
}

func NewMemDLQ() *MemDLQ { return &MemDLQ{} }

func (d *MemDLQ) Write(_ context.Context, e DeadLetterEntry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries = append(d.entries, e)
	return nil
}

func (d *MemDLQ) Depth(_ context.Context) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries), nil
}

// Entries returns a copy of the dead-lettered entries (for tests/UI).
func (d *MemDLQ) Entries() []DeadLetterEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DeadLetterEntry, len(d.entries))
	copy(out, d.entries)
	return out
}
