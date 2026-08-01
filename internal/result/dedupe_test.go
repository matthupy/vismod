package result

import (
	"sync"
	"testing"

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
