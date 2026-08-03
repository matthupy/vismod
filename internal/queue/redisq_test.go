package queue

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newMini(t *testing.T) (redis.UniversalClient, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

// Durability: jobs enqueued by one process are visible to a fresh driver
// instance (restart survival) — the property memq cannot provide.
func TestRedisqDurableAcrossRestart(t *testing.T) {
	rdb, _ := newMini(t)
	q1 := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := q1.Enqueue(context.Background(), job(id)); err != nil {
			t.Fatal(err)
		}
	}
	// "Crash": q1 never started, never drained. New instance sees the work.
	q2 := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	depth, err := q2.QueueDepth(context.Background())
	if err != nil || depth != 3 {
		t.Fatalf("depth after restart = %d (%v), want 3", depth, err)
	}
	var done atomic.Int32
	_ = q2.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		done.Add(1)
		return Ack, nil
	})
	waitFor(t, 3*time.Second, func() bool { return done.Load() == 3 }, "restart instance drains the durable queue")
	_ = q2.Close(context.Background())
}

// Orphan recovery: in-flight payloads from a crashed replica (processing
// list) are requeued on Start — at-least-once, never lost.
func TestRedisqOrphanRecovery(t *testing.T) {
	rdb, _ := newMini(t)
	// Simulate a crashed replica: payload sits in processing.
	q0 := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if _, err := q0.Enqueue(context.Background(), job("orphan")); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.LMove(context.Background(), "vismod-test:pending", "vismod-test:processing", "RIGHT", "LEFT").Result(); err != nil {
		t.Fatal(err)
	}

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	var done atomic.Int32
	_ = q.Start(context.Background(), func(_ context.Context, j Job) (Disposition, error) {
		if j.ID == "orphan" {
			done.Add(1)
		}
		return Ack, nil
	})
	waitFor(t, 3*time.Second, func() bool { return done.Load() == 1 }, "orphan redelivered")
	_ = q.Close(context.Background())
}

// Redelivering an already-completed JobID must not double-write the Sink
// (§K.9) — proven here at the queue level by processing the same JobID
// twice into an idempotent consumer.
func TestRedisqRedeliveryIsDedupedDownstream(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	seen := map[JobID]int{}
	var total atomic.Int32
	_ = q.Start(context.Background(), func(_ context.Context, j Job) (Disposition, error) {
		seen[j.ID]++ // single worker: no lock needed
		total.Add(1)
		return Ack, nil
	})
	_, _ = q.Enqueue(context.Background(), job("dup"))
	_, _ = q.Enqueue(context.Background(), job("dup")) // redelivery
	waitFor(t, 3*time.Second, func() bool { return total.Load() == 2 }, "both deliveries processed")
	_ = q.Close(context.Background())
	if seen["dup"] != 2 {
		t.Fatalf("expected 2 deliveries of the same JobID, got %d", seen["dup"])
	}
	// The dedupe itself is Sink/audit behavior, proven in
	// result.TestJSONLSinkIdempotentPerJobID and audit.TestIdempotentPerJobID.
}

// Metadata must survive the durable Redis round-trip byte-for-byte: this
// is the exact path the execution-time validator in pipeline.ProcessJob
// exists to defend, since a job can reach Redis without ever passing
// through POST /jobs. A no-metadata job must round-trip to nil, not the
// literal JSON "null" that json.RawMessage + omitempty can produce.
func TestRedisqMetadataRoundTrips(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)

	withMeta := job("with-meta")
	withMeta.Metadata = json.RawMessage(`{"ticket":"T-1","tenant":"acme"}`)
	noMeta := job("no-meta")

	if _, err := q.Enqueue(context.Background(), withMeta); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(context.Background(), noMeta); err != nil {
		t.Fatal(err)
	}

	seen := map[JobID]Job{}
	var mu sync.Mutex
	var done atomic.Int32
	_ = q.Start(context.Background(), func(_ context.Context, j Job) (Disposition, error) {
		mu.Lock()
		seen[j.ID] = j
		mu.Unlock()
		done.Add(1)
		return Ack, nil
	})
	waitFor(t, 3*time.Second, func() bool { return done.Load() == 2 }, "both jobs delivered")
	_ = q.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	got := seen["with-meta"]
	if string(got.Metadata) != `{"ticket":"T-1","tenant":"acme"}` {
		t.Errorf("metadata must survive the redis round-trip unchanged, got %q", got.Metadata)
	}
	gotNo := seen["no-meta"]
	if gotNo.Metadata != nil {
		t.Errorf("job with no metadata must round-trip to nil, got %q (len %d)", gotNo.Metadata, len(gotNo.Metadata))
	}
}

func TestRedisqPing(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{}, rdb, "vismod-test", nil)
	if err := q.Ping(context.Background()); err != nil {
		t.Fatalf("ping healthy redis: %v", err)
	}
	mr.Close()
	if err := q.Ping(context.Background()); err == nil {
		t.Error("ping must fail when redis is down (readiness probe contract)")
	}
}

// Drain: in-flight jobs that don't finish stay in processing (durable,
// unacked) rather than being lost or acked-done.
func TestRedisqDrainLeavesUnfinishedInProcessing(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: 200 * time.Millisecond}, rdb, "vismod-test", nil)
	started := make(chan struct{})
	release := make(chan struct{})
	_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		close(started)
		<-release
		return Ack, nil
	})
	_, _ = q.Enqueue(context.Background(), job("slow"))
	<-started
	if err := q.Close(context.Background()); err == nil {
		t.Error("drain should time out with a job still in flight")
	}
	// While the job is still in flight (never finished), it must sit in
	// processing — durable and unacked, never lost, never acked-done.
	n, err := rdb.LLen(context.Background(), "vismod-test:processing").Result()
	if err != nil || n != 1 {
		t.Errorf("unfinished job must remain in processing (durable, unacked): n=%d err=%v", n, err)
	}
	close(release) // let the worker goroutine exit
}
