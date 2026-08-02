package queue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// countingFailDLQ fails every write and reports a fixed depth, standing in
// for a dead-letter sink that has gone bad.
type countingFailDLQ struct {
	depth    int
	depthErr error
	writes   atomic.Int32
}

func (d *countingFailDLQ) Write(context.Context, DeadLetterEntry) error {
	d.writes.Add(1)
	return errors.New("dlq write refused")
}

func (d *countingFailDLQ) Depth(context.Context) (int, error) {
	return d.depth, d.depthErr
}

// TestQueueConfigDefaults: an unset (or nonsensical) config must land on
// the documented defaults rather than on zero values — Workers 0 would
// start no workers at all and the queue would accept jobs forever.
func TestQueueConfigDefaults(t *testing.T) {
	c := QueueConfig{MaxRetries: -5}
	c.applyDefaults()

	if c.Workers != 4 || c.Buffer != 1024 || c.DeadLetterMax != 1000 {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.MaxRetries != 0 {
		t.Errorf("MaxRetries = %d, want 0: a negative retry budget must clamp, not underflow", c.MaxRetries)
	}
	if c.RetryBackoff <= 0 || c.DrainTimeout <= 0 || c.JobTimeout <= 0 {
		t.Errorf("zero durations left unset: %+v", c)
	}
}

// TestMemqEnqueueSurfacesDLQDepthFailure: the depth check is the
// fail-safe backpressure gate. If it cannot be evaluated, the enqueue must
// fail rather than proceed past a guard that never ran.
func TestMemqEnqueueSurfacesDLQDepthFailure(t *testing.T) {
	q := NewMemq(QueueConfig{Workers: 1, Buffer: 4, DeadLetterMax: 10}, nil)
	q.cfg.DeadLetter = &countingFailDLQ{depthErr: errors.New("dlq unreadable")}
	if _, err := q.Enqueue(context.Background(), job("x")); err == nil {
		t.Fatal("enqueue proceeded with an unreadable dead-letter depth")
	}
}

// TestMemqEnqueueRejectsWhenBufferFull: a full buffer is backpressure, not
// a silent drop. ErrQueueFull is what the intake turns into 503+Retry-After.
func TestMemqEnqueueRejectsWhenBufferFull(t *testing.T) {
	q := NewMemq(QueueConfig{Workers: 0, Buffer: 1, DeadLetterMax: 10}, nil)
	if _, err := q.Enqueue(context.Background(), job("a")); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), job("b")); err != ErrQueueFull {
		t.Errorf("second enqueue = %v, want ErrQueueFull", err)
	}
}

// TestMemqStartTwiceIsRefused mirrors redisq: a second Start would double
// the pool against one config.
func TestMemqStartTwiceIsRefused(t *testing.T) {
	q := NewMemq(QueueConfig{Workers: 1, Buffer: 2, DrainTimeout: time.Second}, nil)
	h := func(context.Context, Job) (Disposition, error) { return Ack, nil }
	if err := q.Start(context.Background(), h); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := q.Start(context.Background(), h); err == nil {
		t.Error("second Start was accepted; the worker pool would be doubled")
	}
	_ = q.Close(context.Background())
}

// TestMemqWorkersExitOnContextCancel: cancelling the run context must stop
// the pool even without Close, so a cancelled process never keeps scoring.
func TestMemqWorkersExitOnContextCancel(t *testing.T) {
	q := NewMemq(QueueConfig{Workers: 2, Buffer: 2, DrainTimeout: 3 * time.Second}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Start(ctx, func(context.Context, Job) (Disposition, error) { return Ack, nil }); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	if err := q.Close(context.Background()); err != nil {
		t.Errorf("Close after context cancel = %v, want a clean drain", err)
	}
}

// TestMemqDeadLetterWriteFailureIsLoggedNotSwallowed: memq cannot defer the
// decision the way redisq does (nothing is durable), but the write must
// still be ATTEMPTED and its failure must not stop the pool.
func TestMemqDeadLetterWriteFailureKeepsThePoolRunning(t *testing.T) {
	dlq := &countingFailDLQ{}
	q := NewMemq(QueueConfig{
		Workers: 1, Buffer: 4, MaxRetries: 0, DrainTimeout: 2 * time.Second,
		JobTimeout: time.Second, DeadLetterMax: 10,
	}, nil)
	q.cfg.DeadLetter = dlq

	var handled atomic.Int32
	if err := q.Start(context.Background(), func(_ context.Context, j Job) (Disposition, error) {
		handled.Add(1)
		if j.ID == "bad" {
			return DeadLetter, errors.New("unscorable")
		}
		return Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), job("bad")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return dlq.writes.Load() == 1 }, "dead-letter write attempted")

	// The pool survives the failed write: a later job still runs.
	if _, err := q.Enqueue(context.Background(), job("good")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return handled.Load() == 2 }, "pool still processing after a failed dead-letter write")
	_ = q.Close(context.Background())
}

// TestMemqActiveWorkers is the UI/metrics worker count: 0 idle, non-zero
// while a handler is running.
func TestMemqActiveWorkers(t *testing.T) {
	q := NewMemq(QueueConfig{Workers: 1, Buffer: 2, DrainTimeout: time.Second, JobTimeout: 5 * time.Second}, nil)
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

// TestMemqCloseTwiceIsSafe: shutdown paths race (signal handler plus the
// deferred close). A second Close must be a no-op, not a panic on the
// already-closed stop channel.
func TestMemqCloseTwiceIsSafe(t *testing.T) {
	q := NewMemq(QueueConfig{Workers: 1, Buffer: 2, DrainTimeout: time.Second}, nil)
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := q.Close(context.Background()); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestMemqCloseHonorsCallerCancellation: an already-cancelled shutdown
// context aborts the drain immediately instead of waiting out the budget.
func TestMemqCloseHonorsCallerCancellation(t *testing.T) {
	q := NewMemq(QueueConfig{Workers: 1, Buffer: 2, DrainTimeout: time.Minute, JobTimeout: time.Minute}, nil)
	started := make(chan struct{})
	release := make(chan struct{})
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
	if err := q.Close(ctx); err == nil {
		t.Error("Close with a cancelled context must report the aborted drain")
	}
	close(release)
}

// TestMemqPendingRetryAbandonedOnShutdown documents the non-durable
// contract explicitly: a retry still waiting out its backoff when the
// queue closes is LOST, and that loss is logged rather than silently
// swallowed. This is exactly why production requires driver=redis.
func TestMemqPendingRetryAbandonedOnShutdown(t *testing.T) {
	q := NewMemq(QueueConfig{
		Workers: 1, Buffer: 4, MaxRetries: 5,
		RetryBackoff: 30 * time.Second, // long enough that shutdown wins
		DrainTimeout: 2 * time.Second, JobTimeout: time.Second, DeadLetterMax: 10,
	}, nil)

	retried := make(chan struct{})
	var once atomic.Bool
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		if once.CompareAndSwap(false, true) {
			close(retried)
		}
		return Retry, errors.New("transient")
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), job("retry-me")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	<-retried

	// Close while the retry timer is still pending: the drain must not hang
	// waiting for a backoff it will never serve.
	if err := q.Close(context.Background()); err != nil {
		t.Errorf("Close with a pending retry = %v, want a clean drain", err)
	}
	if d, _ := q.QueueDepth(context.Background()); d != 0 {
		t.Errorf("queue depth = %d after drain, want 0 (the abandoned retry must be accounted for)", d)
	}
}
