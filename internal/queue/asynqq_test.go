package queue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// capturingSink records every envelope written, for asserting DLQ output.
type capturingSink struct {
	mu   sync.Mutex
	envs []result.ResultEnvelope
}

func (s *capturingSink) Write(_ context.Context, env result.ResultEnvelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envs = append(s.envs, env)
	return nil
}

func (s *capturingSink) snapshot() []result.ResultEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]result.ResultEnvelope, len(s.envs))
	copy(out, s.envs)
	return out
}

// waitFor polls cond until true or the deadline passes.
func waitFor(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestAsynqQueue(t *testing.T, cfg QueueConfig) *asynqQueue {
	t.Helper()
	mr := miniredis.RunT(t)
	if cfg.DeadLetter == nil {
		cfg.DeadLetter = result.NewJSONLSink(io.Discard)
	}
	q, err := NewAsynqQueue(cfg, mr.Addr(), "vismod_test", testLogger())
	if err != nil {
		t.Fatalf("NewAsynqQueue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close(context.Background()) })
	return q
}

// Smoke test: the riskiest assumption is that asynq's server loop runs against
// miniredis at all. Enqueue one job, prove the handler runs and the job acks.
func TestAsynqQueue_ProcessesAndAcks(t *testing.T) {
	q := newTestAsynqQueue(t, QueueConfig{Workers: 1, MaxRetries: 2})

	processed := make(chan Job, 1)
	ctx := context.Background()
	if err := q.Start(ctx, func(_ context.Context, j Job) (Disposition, error) {
		processed <- j
		return Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	id, err := q.Enqueue(ctx, Job{Source: moderation.Source{Kind: "file", Ref: "x.jpg"}})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("Enqueue returned empty JobID")
	}

	select {
	case got := <-processed:
		if got.ID != id {
			t.Fatalf("processed JobID = %q, want %q", got.ID, id)
		}
		if got.Source.Ref != "x.jpg" {
			t.Fatalf("processed Source.Ref = %q, want x.jpg", got.Source.Ref)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("job was not processed within 10s")
	}
}

// A retry-exhausted job lands in the DLQ sink as an error envelope (never allow)
// and is archived in asynq (DeadLetterDepth == 1).
func TestAsynqQueue_RetryExhaustedDeadLetters(t *testing.T) {
	sink := &capturingSink{}
	q := newTestAsynqQueue(t, QueueConfig{
		Workers:      1,
		MaxRetries:   1,
		RetryBackoff: 10 * time.Millisecond,
		DeadLetter:   sink,
	})

	ctx := context.Background()
	if err := q.Start(ctx, func(_ context.Context, _ Job) (Disposition, error) {
		return Retry, errors.New("boom")
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id, err := q.Enqueue(ctx, Job{Source: moderation.Source{Kind: "file", Ref: "x.jpg"}})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if !waitFor(func() bool { return len(sink.snapshot()) >= 1 }, 12*time.Second) {
		t.Fatal("no DLQ envelope written after retry exhaustion")
	}
	env := sink.snapshot()[0]
	if env.JobID != id {
		t.Fatalf("DLQ envelope JobID = %q, want %q", env.JobID, id)
	}
	if env.Result != nil {
		t.Fatal("DLQ envelope must have nil Result (could-not-evaluate, never allow)")
	}
	if env.Error == "" {
		t.Fatal("DLQ envelope must carry an error message")
	}
	if !waitFor(func() bool { return q.DeadLetterDepth() == 1 }, 5*time.Second) {
		t.Fatalf("DeadLetterDepth = %d, want 1", q.DeadLetterDepth())
	}
}

// A DeadLetter disposition archives immediately (no retry) and writes the DLQ envelope.
func TestAsynqQueue_DeadLetterDispositionSkipsRetry(t *testing.T) {
	sink := &capturingSink{}
	q := newTestAsynqQueue(t, QueueConfig{Workers: 1, MaxRetries: 5, DeadLetter: sink})

	var attempts int
	var mu sync.Mutex
	ctx := context.Background()
	if err := q.Start(ctx, func(_ context.Context, _ Job) (Disposition, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		return DeadLetter, errors.New("terminal")
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := q.Enqueue(ctx, Job{Source: moderation.Source{Kind: "file", Ref: "x.jpg"}}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if !waitFor(func() bool { return len(sink.snapshot()) >= 1 }, 10*time.Second) {
		t.Fatal("no DLQ envelope for DeadLetter disposition")
	}
	// Give any erroneous retry a chance to run, then assert it never did.
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("handler ran %d times, want 1 (DeadLetter must skip retry)", got)
	}
}

// A handler panic dead-letters the job (poison message) and the pool keeps
// processing subsequent jobs.
func TestAsynqQueue_PanicDeadLettersAndPoolSurvives(t *testing.T) {
	sink := &capturingSink{}
	q := newTestAsynqQueue(t, QueueConfig{Workers: 1, MaxRetries: 3, DeadLetter: sink})

	processed := make(chan result.JobID, 2)
	ctx := context.Background()
	if err := q.Start(ctx, func(_ context.Context, j Job) (Disposition, error) {
		if j.Source.Ref == "poison.jpg" {
			panic("kaboom")
		}
		processed <- j.ID
		return Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := q.Enqueue(ctx, Job{Source: moderation.Source{Kind: "file", Ref: "poison.jpg"}}); err != nil {
		t.Fatalf("Enqueue poison: %v", err)
	}
	goodID, err := q.Enqueue(ctx, Job{Source: moderation.Source{Kind: "file", Ref: "good.jpg"}})
	if err != nil {
		t.Fatalf("Enqueue good: %v", err)
	}

	if !waitFor(func() bool { return len(sink.snapshot()) >= 1 }, 10*time.Second) {
		t.Fatal("panicking job was not dead-lettered")
	}
	select {
	case got := <-processed:
		if got != goodID {
			t.Fatalf("processed %q, want good job %q", got, goodID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pool did not survive the panic to process the good job")
	}
}

// Idempotent intake: enqueuing the same JobID twice is deduped by asynq's
// TaskID, so only one task ever enters the queue. Proven deterministically via
// QueueDepth (no server, no timing race).
func TestAsynqQueue_DuplicateJobIDIsIdempotent(t *testing.T) {
	q := newTestAsynqQueue(t, QueueConfig{Workers: 1, MaxRetries: 1})
	ctx := context.Background()

	job := Job{ID: "fixed-id", Source: moderation.Source{Kind: "file", Ref: "x.jpg"}}
	id1, err := q.Enqueue(ctx, job)
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	id2, err := q.Enqueue(ctx, job)
	if err != nil {
		t.Fatalf("duplicate Enqueue must be idempotent, got: %v", err)
	}
	if id1 != id2 || id1 != "fixed-id" {
		t.Fatalf("ids = %q,%q want both fixed-id", id1, id2)
	}

	n, err := q.QueueDepth(ctx)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if n != 1 {
		t.Fatalf("QueueDepth = %d after duplicate enqueue, want 1 (deduped)", n)
	}
}

// QueueDepth reports pending (not-yet-started) tasks — the redis analogue of
// memq's buffered length. With no server consuming, all enqueued jobs are pending.
func TestAsynqQueue_QueueDepthCountsPending(t *testing.T) {
	q := newTestAsynqQueue(t, QueueConfig{Workers: 1, MaxRetries: 1})
	ctx := context.Background()

	// No Start() => nothing dequeues.
	for range 3 {
		if _, err := q.Enqueue(ctx, Job{Source: moderation.Source{Kind: "file", Ref: "x.jpg"}}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	n, err := q.QueueDepth(ctx)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if n != 3 {
		t.Fatalf("QueueDepth = %d, want 3", n)
	}
}

// FIFO: with a single worker, dequeue order == enqueue order (§D.3).
func TestAsynqQueue_FIFOSingleWorker(t *testing.T) {
	q := newTestAsynqQueue(t, QueueConfig{Workers: 1, MaxRetries: 1})
	ctx := context.Background()

	const n = 5
	order := make(chan string, n)
	if err := q.Start(ctx, func(_ context.Context, j Job) (Disposition, error) {
		order <- j.Source.Ref
		return Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	want := []string{"a", "b", "c", "d", "e"}
	for _, ref := range want {
		if _, err := q.Enqueue(ctx, Job{Source: moderation.Source{Kind: "file", Ref: ref}}); err != nil {
			t.Fatalf("Enqueue %s: %v", ref, err)
		}
	}

	var got []string
	deadline := time.After(15 * time.Second)
	for len(got) < n {
		select {
		case ref := <-order:
			got = append(got, ref)
		case <-deadline:
			t.Fatalf("only got %v within timeout", got)
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dequeue order = %v, want %v", got, want)
		}
	}
}

// Ping honors context cancellation so a hung Redis never blocks /readyz or boot.
func TestAsynqQueue_PingHonorsContext(t *testing.T) {
	q := newTestAsynqQueue(t, QueueConfig{Workers: 1})

	// Healthy: a live ping against miniredis succeeds.
	if err := q.Ping(context.Background()); err != nil {
		t.Fatalf("Ping against live miniredis: %v", err)
	}

	// Already-cancelled ctx returns promptly with the ctx error, not a hang.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- q.Ping(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Ping with cancelled ctx returned nil, want ctx error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ping did not honor cancelled ctx within 2s")
	}
}
