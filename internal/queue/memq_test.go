package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vismod/vismod/pkg/moderation"
)

func job(id string) Job {
	return Job{ID: JobID(id), Source: moderation.Source{Kind: "file", Ref: id, MediaType: "image"}, SubmittedAt: time.Now()}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout waiting for: " + msg)
}

// FIFO: with one worker, dequeue order must equal enqueue order.
func TestMemqFIFO(t *testing.T) {
	q := NewMemq(QueueConfig{Workers: 1, Buffer: 100}, nil)
	var mu sync.Mutex
	var got []string

	// Enqueue IDs whose lexicographic order differs from arrival order to
	// catch sorted-key ordering bugs.
	ids := []string{"z-9", "a-1", "m-5", "b-2", "y-8"}
	for _, id := range ids {
		if _, err := q.Enqueue(context.Background(), job(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Start(context.Background(), func(_ context.Context, j Job) (Disposition, error) {
		mu.Lock()
		got = append(got, string(j.ID))
		mu.Unlock()
		return Ack, nil
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == len(ids) }, "all jobs processed")
	_ = q.Close(context.Background())

	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("FIFO violated: got %v, want %v", got, ids)
		}
	}
}

func TestMemqRetryThenDLQ(t *testing.T) {
	dlq := NewMemDLQ()
	q := NewMemq(QueueConfig{Workers: 2, MaxRetries: 2, RetryBackoff: 5 * time.Millisecond, DeadLetter: dlq}, nil)
	var attempts atomic.Int32
	_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		attempts.Add(1)
		return Retry, errors.New("transient")
	})
	if _, err := q.Enqueue(context.Background(), job("j1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return len(dlq.Entries()) == 1 }, "job dead-lettered after retries")
	_ = q.Close(context.Background())

	// 1 initial + 2 retries = 3 attempts, then DLQ.
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	e := dlq.Entries()[0]
	if e.Job.ID != "j1" || e.Attempts != 3 {
		t.Errorf("unexpected DLQ entry: %+v", e)
	}
}

func TestMemqDeadLetterDisposition(t *testing.T) {
	dlq := NewMemDLQ()
	q := NewMemq(QueueConfig{Workers: 1, DeadLetter: dlq}, nil)
	_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return DeadLetter, errors.New("terminal")
	})
	_, _ = q.Enqueue(context.Background(), job("j1"))
	waitFor(t, 2*time.Second, func() bool { return len(dlq.Entries()) == 1 }, "immediate dead-letter")
	_ = q.Close(context.Background())
	if e := dlq.Entries()[0]; e.Attempts != 1 {
		t.Errorf("DeadLetter disposition should not retry, attempts=%d", e.Attempts)
	}
}

// A panicking handler dead-letters the job and the pool keeps running.
func TestMemqPanicDeadLettersPoolSurvives(t *testing.T) {
	dlq := NewMemDLQ()
	q := NewMemq(QueueConfig{Workers: 1, DeadLetter: dlq}, nil)
	var processed atomic.Int32
	_ = q.Start(context.Background(), func(_ context.Context, j Job) (Disposition, error) {
		if j.ID == "boom" {
			panic("kaboom")
		}
		processed.Add(1)
		return Ack, nil
	})
	_, _ = q.Enqueue(context.Background(), job("boom"))
	_, _ = q.Enqueue(context.Background(), job("after"))
	waitFor(t, 2*time.Second, func() bool { return processed.Load() == 1 && len(dlq.Entries()) == 1 }, "panic dead-lettered, next job processed")
	_ = q.Close(context.Background())
	if e := dlq.Entries()[0]; e.Job.ID != "boom" {
		t.Errorf("wrong job dead-lettered: %+v", e)
	}
}

// DLQ at capacity: enqueues are rejected with a retryable error; dead
// letters are never dropped.
func TestMemqDLQCapRejectsEnqueue(t *testing.T) {
	dlq := NewMemDLQ()
	q := NewMemq(QueueConfig{Workers: 1, DeadLetterMax: 1, DeadLetter: dlq}, nil)
	_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return DeadLetter, errors.New("bad")
	})
	_, _ = q.Enqueue(context.Background(), job("j1"))
	waitFor(t, 2*time.Second, func() bool { return len(dlq.Entries()) == 1 }, "first job dead-lettered")

	_, err := q.Enqueue(context.Background(), job("j2"))
	if !errors.Is(err, ErrDeadLetterFull) {
		t.Errorf("want ErrDeadLetterFull, got %v", err)
	}
	_ = q.Close(context.Background())
}

// Graceful drain: in-flight jobs finish and ack within the drain budget.
func TestMemqDrainLetsInFlightFinish(t *testing.T) {
	q := NewMemq(QueueConfig{Workers: 1, DrainTimeout: 2 * time.Second}, nil)
	started := make(chan struct{})
	var done atomic.Bool
	_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		close(started)
		time.Sleep(100 * time.Millisecond)
		done.Store(true)
		return Ack, nil
	})
	_, _ = q.Enqueue(context.Background(), job("slow"))
	<-started
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !done.Load() {
		t.Error("in-flight job was not allowed to finish during drain")
	}
	if _, err := q.Enqueue(context.Background(), job("late")); !errors.Is(err, ErrQueueClosed) {
		t.Errorf("enqueue after close: want ErrQueueClosed, got %v", err)
	}
}

// Jobs still buffered at shutdown are surfaced (logged), never acked-done.
func TestMemqDrainReportsUnstartedJobs(t *testing.T) {
	q := NewMemq(QueueConfig{Workers: 1, Buffer: 10, DrainTimeout: time.Second}, nil)
	block := make(chan struct{})
	_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		<-block
		return Ack, nil
	})
	for i := 0; i < 5; i++ {
		_, _ = q.Enqueue(context.Background(), job(fmt.Sprintf("j%d", i)))
	}
	close(block)
	_ = q.Close(context.Background())
	// After close, no job may be in state "running"; unfinished ones stay
	// queued/lost (memq non-durable), never "done" without running.
	states := q.States()
	for id, s := range states {
		if s == "running" {
			t.Errorf("job %s left in running state after drain", id)
		}
	}
	if d, _ := q.QueueDepth(context.Background()); d != 0 {
		t.Errorf("depth after drain = %d, want 0", d)
	}
}

// States must not grow with uptime. Nothing was ever deleted, so a
// long-running memq held every job id it had ever seen — and States()
// copies the whole map under the mutex that every state transition takes,
// so the dashboard opened to diagnose a stall was making it worse.
func TestMemqBoundsFinishedStates(t *testing.T) {
	q := NewMemq(QueueConfig{Buffer: 4, Workers: 1}, nil)

	for i := range maxFinishedStates * 3 {
		id := JobID(fmt.Sprintf("job-%d", i))
		q.setState(id, "queued")
		q.setState(id, "running")
		q.setState(id, "done")
	}

	got := len(q.States())
	if got > maxFinishedStates {
		t.Errorf("states retained = %d, want <= %d", got, maxFinishedStates)
	}
	if got < maxFinishedStates {
		t.Errorf("states retained = %d, want the full window of %d recent outcomes", got, maxFinishedStates)
	}
}

// Aging out must never drop a job that is still in flight: the UI would
// show it as vanished and the operator would have no way to see it running.
func TestMemqNeverEvictsInFlightStates(t *testing.T) {
	q := NewMemq(QueueConfig{Buffer: 4, Workers: 1}, nil)

	q.setState("long-runner", "running")
	for i := range maxFinishedStates * 2 {
		id := JobID(fmt.Sprintf("job-%d", i))
		q.setState(id, "done")
	}

	states := q.States()
	if s, ok := states["long-runner"]; !ok || s != "running" {
		t.Errorf("in-flight job evicted: state = %q, present = %v", s, ok)
	}
}

// A terminal state set twice for the same id must not consume two slots in
// the window, or the retained history silently shrinks.
func TestMemqTerminalStateIsCountedOnce(t *testing.T) {
	q := NewMemq(QueueConfig{Buffer: 4, Workers: 1}, nil)
	for range 10 {
		q.setState("job-1", "done")
	}
	if got := len(q.finished); got != 1 {
		t.Errorf("finished entries = %d, want 1", got)
	}
	if got := len(q.States()); got != 1 {
		t.Errorf("states = %d, want 1", got)
	}
}
