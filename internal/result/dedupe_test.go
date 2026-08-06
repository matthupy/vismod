package result

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/queue"
)

func TestDedupeClaimIsOncePerJobID(t *testing.T) {
	var d dedupe
	if !d.Claim("job-1") {
		t.Fatal("first claim must succeed")
	}
	if d.Claim("job-1") {
		t.Error("second claim of the same id must fail")
	}
	if !d.Claim("job-2") {
		t.Error("a distinct id must claim independently")
	}
}

func TestDedupeReleaseAllowsReclaim(t *testing.T) {
	var d dedupe
	if !d.Claim("job-1") {
		t.Fatal("first claim must succeed")
	}
	d.Release("job-1")
	if !d.Claim("job-1") {
		t.Error("after Release the id must be claimable again — a failed send must be retriable on redelivery")
	}
}

func TestDedupeReleaseOfUnclaimedIsSafe(t *testing.T) {
	var d dedupe
	d.Release("never-claimed") // must not panic
}

func TestDedupeConcurrentClaimYieldsExactlyOneWinner(t *testing.T) {
	var d dedupe
	const n = 100
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.Claim(queue.JobID("job-1")) {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("exactly one goroutine must win the claim, got %d", wins)
	}
}

// The claim map must not grow with uptime. Left unbounded it OOM-killed
// long-running serve pods, and the restart reset idempotency — so the leak
// caused the duplicate writes the map exists to prevent.
func TestDedupeExpiresOldClaims(t *testing.T) {
	now := time.Now()
	d := dedupe{
		now:    func() time.Time { return now },
		retain: 10 * time.Minute,
	}

	for i := range 1000 {
		if !d.Claim(queue.JobID(fmt.Sprintf("job-%d", i))) {
			t.Fatalf("claim %d must succeed", i)
		}
	}
	if got := len(d.written); got != 1000 {
		t.Fatalf("claims held = %d, want 1000", got)
	}

	// Past the retention window, a sweep must drop them.
	now = now.Add(11 * time.Minute)
	d.Claim("trigger-sweep")
	if got := len(d.written); got != 1 {
		t.Errorf("claims held after expiry = %d, want 1 (only the trigger)", got)
	}
}

// Expiry must not be so eager that it breaks idempotency: within the
// window a redelivery is still recognized as a duplicate.
func TestDedupeStillDedupesWithinTheWindow(t *testing.T) {
	now := time.Now()
	d := dedupe{
		now:    func() time.Time { return now },
		retain: time.Hour,
	}
	if !d.Claim("job-1") {
		t.Fatal("first claim must succeed")
	}
	// Well past the sweep interval but inside the retention window: this
	// is the ordinary queue-retry case.
	now = now.Add(5 * time.Minute)
	if d.Claim("job-1") {
		t.Error("a redelivery inside the retention window must not re-claim")
	}
}

// The sweep is O(n) under the lock, so it must be rate-limited rather than
// running on every write.
func TestDedupeSweepIsRateLimited(t *testing.T) {
	now := time.Now()
	d := dedupe{now: func() time.Time { return now }, retain: time.Nanosecond}

	d.Claim("job-1")
	now = now.Add(time.Second) // past retention, but under the sweep interval
	d.Claim("job-2")
	if len(d.written) != 2 {
		t.Errorf("claims held = %d, want 2: the sweep ran before its interval elapsed", len(d.written))
	}
}
