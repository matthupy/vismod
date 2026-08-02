package queue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// failingDLQ stands in for a Redis dead-letter write that fails. Swapped in
// after construction (NewRedisq always installs the real one) so the
// never-drop-a-dead-letter path can be exercised deterministically.
type failingDLQ struct{ depth int }

func (f failingDLQ) Write(context.Context, DeadLetterEntry) error {
	return errors.New("dlq write refused")
}
func (f failingDLQ) Depth(context.Context) (int, error) { return f.depth, nil }

func llen(t *testing.T, q *Redisq, list string) int {
	t.Helper()
	n, err := q.rdb.LLen(context.Background(), q.key(list)).Result()
	if err != nil {
		t.Fatalf("LLEN %s: %v", list, err)
	}
	return int(n)
}

// TestRedisqDefaultPrefix: an empty prefix must not produce bare keys like
// ":pending" — two vismod deployments sharing a Redis would then collide.
func TestRedisqDefaultPrefix(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "", nil)
	if got := q.key("pending"); got != "vismod:pending" {
		t.Errorf("key = %q, want vismod:pending", got)
	}
	if q.log == nil {
		t.Error("a nil logger must fall back to the default, not stay nil")
	}
}

// TestRedisqEnqueueAfterClose: once drain has begun, new work must be
// refused rather than accepted into a queue nothing will consume.
func TestRedisqEnqueueAfterClose(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second}, rdb, "vismod-test", nil)
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), job("late")); err != ErrQueueClosed {
		t.Errorf("Enqueue after Close = %v, want ErrQueueClosed", err)
	}
	// Close is idempotent: a second call must not panic on the closed stop
	// channel or report a spurious drain failure.
	if err := q.Close(context.Background()); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestRedisqEnqueueSurfacesRedisFailure: an enqueue that cannot reach Redis
// must return the error. Reporting success would tell the submitter the
// asset is queued when nothing holds it.
func TestRedisqEnqueueSurfacesRedisFailure(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DeadLetterMax: 10}, rdb, "vismod-test", nil)
	mr.SetError("redis is down")
	if _, err := q.Enqueue(context.Background(), job("x")); err == nil {
		t.Fatal("enqueue reported success with Redis unreachable")
	}
}

// TestRedisqEnqueueSurfacesLPushFailure: the DLQ depth check succeeds but
// the push itself fails — the job is not queued and the caller must learn
// that, not receive an ID for work that does not exist.
func TestRedisqEnqueueSurfacesLPushFailure(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DeadLetterMax: 10}, rdb, "vismod-test", nil)
	q.cfg.DeadLetter = failingDLQ{depth: 0} // depth check passes without Redis
	mr.SetError("redis is down")
	if _, err := q.Enqueue(context.Background(), job("x")); err == nil {
		t.Fatal("enqueue reported success though LPUSH failed")
	}
}

// TestRedisqStartTwiceIsRefused: a second Start would double the worker
// pool against one config, silently doubling vendor spend.
func TestRedisqStartTwiceIsRefused(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second}, rdb, "vismod-test", nil)
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	}); err == nil {
		t.Error("second Start was accepted; the worker pool would be doubled")
	}
	_ = q.Close(context.Background())
}

// TestRedisqStartFailsWhenOrphanRecoveryFails: orphan recovery is how
// at-least-once survives a crash. Starting workers when it fails would
// leave a previous replica's in-flight jobs stranded in processing with
// nothing reporting it.
func TestRedisqStartFailsWhenOrphanRecoveryFails(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second}, rdb, "vismod-test", nil)
	mr.SetError("redis is down")
	err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	})
	if err == nil {
		t.Fatal("Start succeeded despite failed orphan recovery")
	}
}

// TestRedisqPoisonPayloadIsDeadLettered: a payload that cannot be decoded
// is never silently dropped — it lands in the DLQ for an operator, and the
// worker keeps running.
func TestRedisqPoisonPayloadIsDeadLettered(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: 2 * time.Second, DeadLetterMax: 10}, rdb, "vismod-test", nil)
	if err := rdb.LPush(context.Background(), q.key("pending"), "{not json").Err(); err != nil {
		t.Fatalf("seed poison payload: %v", err)
	}
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		t.Error("an undecodable payload must never reach the handler")
		return Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		d, err := q.DLQ().Depth(context.Background())
		return err == nil && d == 1
	}, "poison payload dead-lettered")
	_ = q.Close(context.Background())

	if n := llen(t, q, "processing"); n != 0 {
		t.Errorf("processing holds %d payloads after dead-lettering, want 0 (it was acked)", n)
	}
}

// TestRedisqFailedDeadLetterWriteLeavesJobInProcessing: a dead letter that
// cannot be written must NOT be acked away. Leaving the payload in
// processing makes it redeliver after restart instead of vanishing — the
// decision is deferred, never dropped.
func TestRedisqFailedDeadLetterWriteLeavesJobInProcessing(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: 2 * time.Second, DeadLetterMax: 10}, rdb, "vismod-test", nil)
	q.cfg.DeadLetter = failingDLQ{}
	if err := rdb.LPush(context.Background(), q.key("pending"), "{not json").Err(); err != nil {
		t.Fatalf("seed poison payload: %v", err)
	}
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return llen(t, q, "processing") == 1 }, "undeliverable dead letter stays in processing")
	_ = q.Close(context.Background())
}

// TestRedisqRetryScheduleFailureLeavesJobInProcessing: same contract on the
// retry path. If the retry ZSET cannot be written, the payload must stay
// unacked so a restart redelivers it.
func TestRedisqRetryScheduleFailureLeavesJobInProcessing(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{
		Workers: 1, MaxRetries: 3, RetryBackoff: time.Millisecond,
		DrainTimeout: 2 * time.Second, DeadLetterMax: 10,
	}, rdb, "vismod-test", nil)

	var handled atomic.Int32
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		// Break Redis from INSIDE the handler: scheduleRetry then runs
		// against a dead server with no race about when the failure lands.
		if handled.Add(1) == 1 {
			mr.SetError("redis is down")
		}
		return Retry, errors.New("transient")
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), job("retry-me")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return handled.Load() >= 1 }, "job handled once")

	// Let the worker loop hit its own dequeue failure against the dead
	// server too — it must log and back off, not spin or exit.
	time.Sleep(200 * time.Millisecond)
	mr.SetError("") // recover so the assertions below can read Redis

	if n := llen(t, q, "processing"); n != 1 {
		t.Errorf("processing holds %d payloads, want 1 (an unschedulable retry must not be acked)", n)
	}
	_ = q.Close(context.Background())
}

// TestRedisqQueueDepthSurfacesFailure: depth is the autoscaling signal. A
// failed read must be an error, not a silent 0 that scales the fleet down
// while work is pending.
func TestRedisqQueueDepthSurfacesFailure(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	mr.SetError("redis is down")
	if d, err := q.QueueDepth(context.Background()); err == nil {
		t.Errorf("QueueDepth = %d with Redis down, want an error", d)
	}
}

// TestRedisqActiveWorkers tracks jobs inside a handler — the number the UI
// and the metrics export. It must be 0 when idle and non-zero in flight.
func TestRedisqActiveWorkers(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second, JobTimeout: 5 * time.Second}, rdb, "vismod-test", nil)
	if got := q.ActiveWorkers(); got != 0 {
		t.Errorf("ActiveWorkers = %d on an idle queue, want 0", got)
	}

	inHandler := make(chan struct{})
	release := make(chan struct{})
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		close(inHandler)
		<-release
		return Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), job("busy")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	<-inHandler
	if got := q.ActiveWorkers(); got != 1 {
		t.Errorf("ActiveWorkers = %d with a job in flight, want 1", got)
	}
	close(release)
	_ = q.Close(context.Background())
}

// TestRedisqWorkersExitOnContextCancel: the run context is the other exit
// path (the stop channel is the first). Workers that ignored it would keep
// consuming after the process decided to shut down.
func TestRedisqWorkersExitOnContextCancel(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 2, DrainTimeout: 3 * time.Second}, rdb, "vismod-test", nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Start(ctx, func(context.Context, Job) (Disposition, error) { return Ack, nil }); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	if err := q.Close(context.Background()); err != nil {
		t.Errorf("Close after context cancel = %v, want a clean drain", err)
	}
}

// TestRedisqCloseHonorsCallerCancellation: an already-cancelled shutdown
// context must abort the drain with an error rather than blocking for the
// full drain timeout.
func TestRedisqCloseHonorsCallerCancellation(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Minute, JobTimeout: time.Minute}, rdb, "vismod-test", nil)
	release := make(chan struct{})
	started := make(chan struct{})
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		close(started)
		<-release
		return Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), job("slow")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := q.Close(ctx)
	if err == nil {
		t.Error("Close with a cancelled context must report the aborted drain")
	}
	close(release)
}
