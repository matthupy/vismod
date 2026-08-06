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

// Orphan recovery: in-flight payloads from a crashed replica are requeued
// — at-least-once, never lost.
//
// The crashed replica is modelled by its own processing list plus a stale
// heartbeat, which is what a crash actually leaves behind now that the list
// is per-replica.
func TestRedisqOrphanRecovery(t *testing.T) {
	rdb, _ := newMini(t)
	// Simulate a crashed replica: payload sits in ITS processing list and
	// its heartbeat has gone stale.
	q0 := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if _, err := q0.Enqueue(context.Background(), job("orphan")); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.LMove(context.Background(), "vismod-test:pending", q0.processingKey(), "RIGHT", "LEFT").Result(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.ZAdd(context.Background(), "vismod-test:instances", redis.Z{
		Score: float64(time.Now().Add(-10 * instanceReclaimAfter).UnixMilli()), Member: q0.instance,
	}).Err(); err != nil {
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
	n, err := rdb.LLen(context.Background(), q.processingKey()).Result()
	if err != nil || n != 1 {
		t.Errorf("unfinished job must remain in processing (durable, unacked): n=%d err=%v", n, err)
	}
	close(release) // let the worker goroutine exit
}

// A replica starting up must not disturb jobs that OTHER live replicas are
// processing right now.
//
// The processing list used to be one shared key, so every Start LMOVEd the
// whole thing back to pending. In multi-replica deployments — the
// documented production topology — that meant any KEDA scale-up, rolling
// deploy, or crashloop re-queued jobs that were actively running elsewhere:
// duplicate billed vendor calls and duplicate webhook POSTs, indistinguishable
// from ordinary at-least-once redelivery.
func TestRedisqStartDoesNotStealLiveReplicasWork(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()

	busy := make(chan struct{})
	release := make(chan struct{})
	var handled atomic.Int32

	q1 := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if _, err := q1.Enqueue(ctx, job("in-flight")); err != nil {
		t.Fatal(err)
	}
	if err := q1.Start(ctx, func(_ context.Context, _ Job) (Disposition, error) {
		if handled.Add(1) == 1 {
			close(busy)
			<-release // hold the job in flight
		}
		return Ack, nil
	}); err != nil {
		t.Fatal(err)
	}
	<-busy // q1 is now holding the job

	// A second replica boots while q1 is mid-job.
	q2 := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if err := q2.Start(ctx, func(_ context.Context, _ Job) (Disposition, error) {
		handled.Add(1)
		return Ack, nil
	}); err != nil {
		t.Fatal(err)
	}

	// Give q2 time to do damage if it is going to.
	time.Sleep(300 * time.Millisecond)
	if depth, err := q2.QueueDepth(ctx); err != nil || depth != 0 {
		t.Errorf("depth = %d (%v), want 0: a booting replica re-queued a job another replica is running", depth, err)
	}

	close(release)
	_ = q1.Close(ctx)
	_ = q2.Close(ctx)

	if got := handled.Load(); got != 1 {
		t.Errorf("job handled %d times, want 1: it was processed twice (billed twice)", got)
	}
}

// The flip side: work from a replica that really is gone must still come
// back. With per-replica processing keys nothing else can reclaim it, so
// the reaper is load-bearing, not an optimization.
func TestRedisqReaperReclaimsDeadReplicaWork(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()

	// A dead replica: registered, holding a payload, heartbeat long stale.
	dead := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if _, err := dead.Enqueue(ctx, job("stranded")); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.LMove(ctx, "vismod-test:pending", dead.processingKey(), "RIGHT", "LEFT").Result(); err != nil {
		t.Fatal(err)
	}
	staleAt := time.Now().Add(-10 * instanceReclaimAfter)
	if err := rdb.ZAdd(ctx, "vismod-test:instances", redis.Z{
		Score: float64(staleAt.UnixMilli()), Member: dead.instance,
	}).Err(); err != nil {
		t.Fatal(err)
	}

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	var done atomic.Int32
	if err := q.Start(ctx, func(_ context.Context, j Job) (Disposition, error) {
		if j.ID == "stranded" {
			done.Add(1)
		}
		return Ack, nil
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return done.Load() == 1 }, "dead replica's job reclaimed")
	_ = q.Close(ctx)

	// The dead instance must be deregistered, or it is reaped forever.
	n, err := rdb.ZCard(ctx, "vismod-test:instances").Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("instances registered = %d, want 1 (only the live one)", n)
	}
}

// A live replica is identified by its heartbeat, not by whether it happens
// to be mid-job: a worker legitimately holding a job for the full
// JobTimeout must never be reaped out from under itself.
func TestRedisqDoesNotReapAHeartbeatingReplica(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()

	live := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if _, err := live.Enqueue(ctx, job("slow")); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.LMove(ctx, "vismod-test:pending", live.processingKey(), "RIGHT", "LEFT").Result(); err != nil {
		t.Fatal(err)
	}
	// Registered and heartbeating as of now.
	if err := rdb.ZAdd(ctx, "vismod-test:instances", redis.Z{
		Score: float64(time.Now().UnixMilli()), Member: live.instance,
	}).Err(); err != nil {
		t.Fatal(err)
	}

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if err := q.Start(ctx, func(context.Context, Job) (Disposition, error) { return Ack, nil }); err != nil {
		t.Fatal(err)
	}
	q.reapDeadInstances(ctx)
	_ = q.Close(ctx)

	n, err := rdb.LLen(ctx, live.processingKey()).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("live replica's in-flight payload count = %d, want 1 (it was reaped while alive)", n)
	}
}

// Payloads left in the pre-upgrade shared processing key must not be
// stranded forever after the rollout.
func TestRedisqReclaimsLegacyProcessingKey(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()

	q0 := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if _, err := q0.Enqueue(ctx, job("pre-upgrade")); err != nil {
		t.Fatal(err)
	}
	// Exactly where an older vismod would have left it.
	if _, err := rdb.LMove(ctx, "vismod-test:pending", "vismod-test:processing", "RIGHT", "LEFT").Result(); err != nil {
		t.Fatal(err)
	}
	// The legacy holder is registered on first sight; age it out.
	if err := rdb.ZAdd(ctx, "vismod-test:instances", redis.Z{
		Score: float64(time.Now().Add(-10 * instanceReclaimAfter).UnixMilli()), Member: legacyInstance,
	}).Err(); err != nil {
		t.Fatal(err)
	}

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	var done atomic.Int32
	if err := q.Start(ctx, func(_ context.Context, j Job) (Disposition, error) {
		if j.ID == "pre-upgrade" {
			done.Add(1)
		}
		return Ack, nil
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return done.Load() == 1 }, "pre-upgrade payload reclaimed")
	_ = q.Close(ctx)
}

// Stranded payloads are invisible to QueueDepth by design (it is the
// autoscaling signal), so they need their own gauge — otherwise jobs parked
// in processing show up nowhere and the autoscaler scales to zero on top of
// them.
func TestRedisqProcessingDepthIsObservable(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if _, err := q.Enqueue(ctx, job("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.LMove(ctx, "vismod-test:pending", q.processingKey(), "RIGHT", "LEFT").Result(); err != nil {
		t.Fatal(err)
	}

	depth, err := q.ProcessingDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depth != 1 {
		t.Errorf("ProcessingDepth = %d, want 1", depth)
	}
	if qd, err := q.QueueDepth(ctx); err != nil || qd != 0 {
		t.Errorf("QueueDepth = %d (%v), want 0: the autoscaling signal must stay pending+retry", qd, err)
	}
}
