package result

import (
	"sync"

	"github.com/vismod/vismod/internal/queue"
)

// dedupe is the per-JobID idempotency state every Sink needs.
//
// Queue delivery is at-least-once, so the same job can arrive more than
// once and a sink must not write it twice. Claim reports whether THIS
// call owns the write.
//
// Release exists for sinks whose write can fail after the claim (the
// webhook): without it, a failed send would mark the job written and the
// queue's redelivery would silently skip that sink forever.
//
// The zero value is ready to use.
type dedupe struct {
	mu      sync.Mutex
	written map[queue.JobID]bool
}

// Claim reports whether the caller owns the write for id. It returns
// false if the id was already claimed.
func (d *dedupe) Claim(id queue.JobID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.written[id] {
		return false
	}
	if d.written == nil {
		d.written = map[queue.JobID]bool{}
	}
	d.written[id] = true
	return true
}

// Release undoes a Claim so a later delivery can retry. Releasing an
// unclaimed id is a no-op.
func (d *dedupe) Release(id queue.JobID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.written, id)
}
