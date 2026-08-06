package result

import (
	"sync"
	"time"

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
	written map[queue.JobID]time.Time

	// now and retain are test seams; their zero values mean time.Now and
	// dedupeRetention.
	now       func() time.Time
	retain    time.Duration
	lastSweep time.Time
}

// dedupeRetention is how long a claimed JobID is remembered.
//
// This map used to grow one entry per job for the life of the process. A
// serve pod at steady throughput accumulated two of them (file sink and
// webhook sink) until it was OOM-killed days in — no error, no metric —
// and the restart reset idempotency, so the crash itself produced the
// duplicate writes the map exists to prevent.
//
// A time window rather than an LRU: evicting by count discards ids under a
// burst, which is precisely when redelivery happens, and the eviction is
// invisible. A window bounds memory by THROUGHPUT instead of by uptime and
// degrades in the already-documented direction — a redelivery arriving
// after it behaves like the post-restart case, which consumers handle by
// deduping on job_id.
//
// An hour is well beyond the real redelivery window: queue.max_retries
// defaults to 3 and moderate.DoJSON honors Retry-After only up to 120s.
const dedupeRetention = time.Hour

// dedupeSweepInterval bounds how often expiry runs. The sweep is O(n) and
// runs inline under the lock, so it must not happen on every write.
const dedupeSweepInterval = time.Minute

func (d *dedupe) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *dedupe) retention() time.Duration {
	if d.retain != 0 {
		return d.retain
	}
	return dedupeRetention
}

// Claim reports whether the caller owns the write for id. It returns
// false if the id was already claimed.
func (d *dedupe) Claim(id queue.JobID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	d.sweepLocked(now)
	if _, ok := d.written[id]; ok {
		return false
	}
	if d.written == nil {
		d.written = map[queue.JobID]time.Time{}
	}
	d.written[id] = now
	return true
}

// Release undoes a Claim so a later delivery can retry. Releasing an
// unclaimed id is a no-op.
func (d *dedupe) Release(id queue.JobID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.written, id)
}

// sweepLocked drops claims older than the retention window.
func (d *dedupe) sweepLocked(now time.Time) {
	if now.Sub(d.lastSweep) < dedupeSweepInterval {
		return
	}
	d.lastSweep = now
	cutoff := now.Add(-d.retention())
	for id, at := range d.written {
		if at.Before(cutoff) {
			delete(d.written, id)
		}
	}
}
