package queue

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// fakeRecorder counts terminal-disposition calls (queue.Recorder).
type fakeRecorder struct{ completed, failed atomic.Int64 }

func (r *fakeRecorder) RecordJobCompleted() { r.completed.Add(1) }
func (r *fakeRecorder) RecordJobFailed()    { r.failed.Add(1) }

// captureSink records envelopes written to it (used as a DLQ sink).
type captureSink struct {
	mu   sync.Mutex
	envs []result.ResultEnvelope
}

func (c *captureSink) Write(_ context.Context, env result.ResultEnvelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envs = append(c.envs, env)
	return nil
}

func (c *captureSink) ids() []result.JobID {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]result.JobID, len(c.envs))
	for i, e := range c.envs {
		out[i] = e.JobID
	}
	return out
}

func newTestQueue(t *testing.T, cfg QueueConfig) (*memQueue, *captureSink) {
	t.Helper()
	dlq := &captureSink{}
	cfg.DeadLetter = dlq
	q, err := NewMemQueue(cfg, nil)
	if err != nil {
		t.Fatalf("NewMemQueue: %v", err)
	}
	return q, dlq
}

func TestMemQueueFIFOSingleWorker(t *testing.T) {
	q, _ := newTestQueue(t, QueueConfig{Workers: 1, Buffer: 16, DrainTimeout: 2 * time.Second})

	var mu sync.Mutex
	var order []result.JobID
	handler := func(_ context.Context, j Job) (Disposition, error) {
		mu.Lock()
		order = append(order, j.ID)
		mu.Unlock()
		return Ack, nil
	}
	if err := q.Start(context.Background(), handler); err != nil {
		t.Fatal(err)
	}

	want := make([]result.JobID, 0, 20)
	for i := 0; i < 20; i++ {
		id, err := q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "x"}})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(order) != len(want) {
		t.Fatalf("processed %d, want %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("FIFO violated at %d: got %s want %s", i, order[i], want[i])
		}
	}
}

func TestMemQueuePanicDeadLettersAndPoolSurvives(t *testing.T) {
	q, dlq := newTestQueue(t, QueueConfig{Workers: 2, Buffer: 16, DrainTimeout: 2 * time.Second})

	var processed int
	var mu sync.Mutex
	handler := func(_ context.Context, j Job) (Disposition, error) {
		if j.Source.Ref == "poison" {
			panic("boom")
		}
		mu.Lock()
		processed++
		mu.Unlock()
		return Ack, nil
	}
	_ = q.Start(context.Background(), handler)

	_, _ = q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "poison"}})
	for i := 0; i < 5; i++ {
		_, _ = q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "ok"}})
	}
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := dlq.ids(); len(got) != 1 {
		t.Fatalf("want 1 dead-lettered job, got %d", len(got))
	}
	if processed != 5 {
		t.Fatalf("pool must survive panic and process the other 5, processed=%d", processed)
	}
	// Dead-lettered envelope must be fail-safe: Result nil, Error set (never allow).
	if dlq.envs[0].Result != nil || dlq.envs[0].Error == "" {
		t.Fatalf("dead-letter envelope must carry an error and no result: %+v", dlq.envs[0])
	}
}

func TestMemQueueRetryExhaustionDeadLetters(t *testing.T) {
	q, dlq := newTestQueue(t, QueueConfig{
		Workers: 1, Buffer: 8, MaxRetries: 2, RetryBackoff: time.Millisecond, DrainTimeout: 2 * time.Second,
	})

	var attempts int
	handler := func(_ context.Context, _ Job) (Disposition, error) {
		attempts++
		return Retry, nil // always transient
	}
	_ = q.Start(context.Background(), handler)
	_, _ = q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "x"}})
	_ = q.Close(context.Background())

	// 1 initial + 2 retries = 3 attempts, then dead-letter.
	if attempts != 3 {
		t.Fatalf("want 3 attempts, got %d", attempts)
	}
	if len(dlq.ids()) != 1 {
		t.Fatalf("retry-exhausted job must dead-letter, got %d", len(dlq.ids()))
	}
}

// The driver threads a 0-based attempt counter into Job.Attempt so a handler can
// distinguish a first dequeue from a retry re-dispatch. memq increments it across
// its in-process retry loop: first attempt = 0, then 1, 2, ...
func TestMemQueueThreadsAttemptAcrossRetries(t *testing.T) {
	q, _ := newTestQueue(t, QueueConfig{
		Workers: 1, Buffer: 8, MaxRetries: 2, RetryBackoff: time.Millisecond, DrainTimeout: 2 * time.Second,
	})

	var mu sync.Mutex
	var seen []int
	handler := func(_ context.Context, j Job) (Disposition, error) {
		mu.Lock()
		seen = append(seen, j.Attempt)
		mu.Unlock()
		return Retry, nil // always transient => exhausts retries
	}
	_ = q.Start(context.Background(), handler)
	_, _ = q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "x"}})
	_ = q.Close(context.Background())

	mu.Lock()
	defer mu.Unlock()
	want := []int{0, 1, 2} // 1 initial + 2 retries
	if len(seen) != len(want) {
		t.Fatalf("attempts seen = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("attempt[%d] = %d, want %d (full=%v)", i, seen[i], want[i], seen)
		}
	}
}

func TestMemQueueDeadLetterImmediate(t *testing.T) {
	q, dlq := newTestQueue(t, QueueConfig{Workers: 1, Buffer: 4, MaxRetries: 5, DrainTimeout: time.Second})
	handler := func(_ context.Context, _ Job) (Disposition, error) { return DeadLetter, nil }
	_ = q.Start(context.Background(), handler)
	_, _ = q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "x"}})
	_ = q.Close(context.Background())
	if len(dlq.ids()) != 1 {
		t.Fatalf("DeadLetter disposition must dead-letter immediately, got %d", len(dlq.ids()))
	}
}

func TestMemQueueActiveDepthAndLifecycleCounters(t *testing.T) {
	rec := &fakeRecorder{}
	q, _ := newTestQueue(t, QueueConfig{
		Workers: 1, Buffer: 8, MaxRetries: 0, DrainTimeout: 2 * time.Second, Metrics: rec,
	})

	release := make(chan struct{})
	_ = q.Start(context.Background(), func(_ context.Context, j Job) (Disposition, error) {
		if j.Source.Ref == "fail" {
			return DeadLetter, nil
		}
		<-release // block so the job stays in-flight (Active)
		return Ack, nil
	})

	// Enqueue a job that blocks in the handler => ActiveDepth rises to 1.
	if _, err := q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "ok"}}); err != nil {
		t.Fatal(err)
	}
	waitForInt(t, func() int { return q.ActiveDepth() }, 1, "ActiveDepth should be 1 while a job is in-flight")

	// Release it => acked => ActiveDepth back to 0, completed counter == 1.
	close(release)
	waitForInt(t, func() int { return q.ActiveDepth() }, 0, "ActiveDepth should return to 0 after ack")
	waitForInt(t, func() int { return int(rec.completed.Load()) }, 1, "completed counter should be 1 after ack")

	// A dead-lettered job bumps the failed counter and never the completed one.
	if _, err := q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "fail"}}); err != nil {
		t.Fatal(err)
	}
	waitForInt(t, func() int { return int(rec.failed.Load()) }, 1, "failed counter should be 1 after dead-letter")
	if got := rec.completed.Load(); got != 1 {
		t.Fatalf("completed counter must stay 1 (dead-letter is not a completion), got %d", got)
	}
	waitForInt(t, func() int { return q.ActiveDepth() }, 0, "ActiveDepth should be 0 after the dead-lettered job finishes")

	_ = q.Close(context.Background())
}

// waitForInt polls f until it equals want or a short deadline passes.
func waitForInt(t *testing.T, f func() int, want int, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s (last=%d, want=%d)", msg, f(), want)
}

func TestMemQueueRejectsAfterClose(t *testing.T) {
	q, _ := newTestQueue(t, QueueConfig{Workers: 1, Buffer: 4, DrainTimeout: time.Second})
	_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) { return Ack, nil })
	_ = q.Close(context.Background())
	if _, err := q.Enqueue(context.Background(), Job{}); err != ErrQueueClosed {
		t.Fatalf("want ErrQueueClosed, got %v", err)
	}
}
