package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/matthupy/vismod/pkg/moderation"
)

func TestQueueDepthReflectsBufferedJobs(t *testing.T) {
	q, _ := newTestQueue(t, QueueConfig{Workers: 1, Buffer: 16, DrainTimeout: 2 * time.Second})

	// Block the single worker so enqueued jobs stay buffered.
	release := make(chan struct{})
	var once sync.Once
	_ = q.Start(context.Background(), func(_ context.Context, _ Job) (Disposition, error) {
		<-release
		return Ack, nil
	})

	// First enqueue is pulled by the worker (which then blocks); the rest buffer.
	for i := 0; i < 4; i++ {
		if _, err := q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	// Give the worker a moment to pull the first job.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if d, _ := q.QueueDepth(context.Background()); d >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	d, err := q.QueueDepth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d < 1 {
		t.Fatalf("QueueDepth should report buffered jobs, got %d", d)
	}

	once.Do(func() { close(release) })
	_ = q.Close(context.Background())
}

func TestDeadLetterFullRejectsEnqueue(t *testing.T) {
	q, _ := newTestQueue(t, QueueConfig{
		Workers: 1, Buffer: 8, MaxRetries: 0, DeadLetterMax: 2, DrainTimeout: 2 * time.Second,
	})
	// Handler always dead-letters immediately.
	_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return DeadLetter, nil
	})

	// Fill the DLQ to capacity.
	for i := 0; i < 2; i++ {
		_, _ = q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "x"}})
	}
	// Wait until the DLQ reaches capacity.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if q.DeadLetterDepth() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if q.DeadLetterDepth() < 2 {
		t.Fatalf("DLQ should be full, depth=%d", q.DeadLetterDepth())
	}

	// Further enqueues are rejected (never dropped silently, never auto-allowed).
	if _, err := q.Enqueue(context.Background(), Job{Source: moderation.Source{Ref: "y"}}); err != ErrDeadLetterFull {
		t.Fatalf("want ErrDeadLetterFull at capacity, got %v", err)
	}
	_ = q.Close(context.Background())
}

func TestNewMemQueueRequiresDeadLetter(t *testing.T) {
	if _, err := NewMemQueue(QueueConfig{Workers: 1}, nil); err == nil {
		t.Fatal("NewMemQueue must require a DeadLetter sink")
	}
}
