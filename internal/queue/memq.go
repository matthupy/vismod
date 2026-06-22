package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matthupy/vismod/internal/result"
)

// ErrQueueClosed is returned by Enqueue after Close has begun.
var ErrQueueClosed = errors.New("queue: closed")

// ErrDeadLetterFull is returned by Enqueue when the DLQ is at capacity.
// At capacity we reject new intake (never drop, never auto-allow).
var ErrDeadLetterFull = errors.New("queue: dead-letter queue at capacity")

// jobState tracks a job's lifecycle for observability.
type jobState string

const (
	statePending    jobState = "pending"
	stateProcessing jobState = "processing"
	stateAcked      jobState = "acked"
	stateDeadLetter jobState = "dead_letter"
)

// memQueue is the prototype in-memory FIFO queue.
//
// DURABILITY BOUNDARY: at-most-once, non-durable, single-process — dev/CLI
// only. A crash loses enqueued AND in-flight jobs. Production intake MUST use
// driver=redis (M5). serve warns at boot and reports not-ready when this driver
// backs a long-running worker.
type memQueue struct {
	cfg QueueConfig
	log *slog.Logger

	ch       chan Job // buffered => FIFO by construction (dequeue order == enqueue order)
	seq      atomic.Uint64
	dlqLen   atomic.Int64
	inflight atomic.Int64 // jobs currently in process() (in-flight, incl. retry backoff)

	mu     sync.Mutex
	states map[result.JobID]jobState
	closed bool

	wg       sync.WaitGroup
	startCtx context.Context
}

// NewMemQueue builds an in-memory FIFO queue. DeadLetter must be non-nil.
func NewMemQueue(cfg QueueConfig, log *slog.Logger) (*memQueue, error) {
	if cfg.DeadLetter == nil {
		return nil, errors.New("queue: QueueConfig.DeadLetter sink is required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = 64
	}
	if cfg.DeadLetterMax <= 0 {
		cfg.DeadLetterMax = 1024
	}
	if log == nil {
		log = slog.Default()
	}
	return &memQueue{
		cfg:    cfg,
		log:    log,
		ch:     make(chan Job, cfg.Buffer),
		states: make(map[result.JobID]jobState),
	}, nil
}

func (q *memQueue) setState(id result.JobID, s jobState) {
	q.mu.Lock()
	q.states[id] = s
	q.mu.Unlock()
}

// Enqueue appends a job in arrival order. Blocks on a full buffer until space
// frees or ctx is done. Rejects when closed or the DLQ is at capacity.
func (q *memQueue) Enqueue(ctx context.Context, j Job) (result.JobID, error) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return "", ErrQueueClosed
	}
	q.mu.Unlock()

	if q.dlqLen.Load() >= int64(q.cfg.DeadLetterMax) {
		return "", ErrDeadLetterFull
	}

	if j.ID == "" {
		j.ID = result.JobID(fmt.Sprintf("job-%d", q.seq.Add(1)))
	}
	if j.SubmittedAt.IsZero() {
		j.SubmittedAt = time.Now().UTC()
	}
	q.setState(j.ID, statePending)

	select {
	case q.ch <- j:
		return j.ID, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Start launches the worker pool. It returns immediately; workers run until
// Close. startCtx cancels the pulling of NEW work; per-job processing uses a
// separate child ctx so in-flight jobs can finish during drain.
func (q *memQueue) Start(ctx context.Context, handler Handler) error {
	if handler == nil {
		return errors.New("queue: nil handler")
	}
	q.startCtx = ctx
	for i := 0; i < q.cfg.Workers; i++ {
		q.wg.Add(1)
		go q.worker(handler)
	}
	return nil
}

func (q *memQueue) worker(handler Handler) {
	defer q.wg.Done()
	for {
		select {
		case <-q.startCtx.Done():
			return
		case j, ok := <-q.ch:
			if !ok {
				return
			}
			q.process(handler, j)
		}
	}
}

// process runs a job with bounded retry, per-job timeout and panic recovery.
// A retry-exhausted, terminal, or panicked job is dead-lettered with an error
// envelope (Verdict could-not-evaluate — never allow).
func (q *memQueue) process(handler Handler, j Job) {
	q.inflight.Add(1)
	defer q.inflight.Add(-1)
	q.setState(j.ID, stateProcessing)

	attempts := 0
	for {
		disp, err := q.invoke(handler, j)
		switch disp {
		case Ack:
			q.setState(j.ID, stateAcked)
			recordCompleted(q.cfg.Metrics)
			return
		case DeadLetter:
			q.deadLetter(j, err)
			return
		case Retry:
			if attempts >= q.cfg.MaxRetries {
				q.deadLetter(j, fmt.Errorf("retry exhausted after %d attempts: %w", attempts, err))
				return
			}
			attempts++
			q.backoff(attempts)
			continue
		default:
			q.deadLetter(j, fmt.Errorf("unknown disposition %v: %w", disp, err))
			return
		}
	}
}

// invoke calls the handler under panic recovery and an optional per-job
// timeout. A panic is converted to a DeadLetter disposition (poison message) so
// the pool never crashes.
func (q *memQueue) invoke(handler Handler, j Job) (disp Disposition, err error) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if q.cfg.JobTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, q.cfg.JobTimeout)
		defer cancel()
	}

	defer func() {
		if r := recover(); r != nil {
			disp = DeadLetter
			err = fmt.Errorf("panic in handler: %v", r)
		}
	}()

	return handler(ctx, j)
}

func (q *memQueue) backoff(attempt int) {
	d := q.cfg.RetryBackoff
	if d <= 0 {
		return
	}
	// Linear backoff is enough for the in-memory prototype.
	time.Sleep(time.Duration(attempt) * d)
}

func (q *memQueue) deadLetter(j Job, cause error) {
	q.setState(j.ID, stateDeadLetter)
	q.dlqLen.Add(1)
	recordFailed(q.cfg.Metrics)

	msg := "dead-lettered"
	if cause != nil {
		msg = cause.Error()
	}
	now := time.Now().UTC().Format(time.RFC3339)
	env := result.ResultEnvelope{
		JobID:      j.ID,
		Source:     j.Source,
		Error:      msg, // Result is nil => could-not-evaluate, never "allow"
		StartedAt:  now,
		FinishedAt: now,
	}
	if err := q.cfg.DeadLetter.Write(context.Background(), env); err != nil {
		q.log.Error("dead-letter sink write failed", "job_id", j.ID, "err", err)
	}
	q.log.Warn("job dead-lettered", "job_id", j.ID, "cause", msg)
}

// QueueDepth returns the number of buffered, not-yet-started jobs.
func (q *memQueue) QueueDepth(_ context.Context) (int, error) {
	return len(q.ch), nil
}

// DeadLetterDepth returns the number of dead-lettered jobs (for metrics).
func (q *memQueue) DeadLetterDepth() int { return int(q.dlqLen.Load()) }

// ActiveDepth returns jobs currently in process() — in-flight, not yet acked or
// dead-lettered. memq is at-most-once, so this includes retry-backoff sleeps (the
// job is still held). Read live at scrape time.
func (q *memQueue) ActiveDepth() int { return int(q.inflight.Load()) }

// Close stops intake and gracefully drains in-flight work within DrainTimeout.
// Buffered-but-unstarted jobs are logged at WARN (not silently dropped).
// In-flight jobs that do not finish in time are left incomplete and logged —
// never acked-done.
func (q *memQueue) Close(ctx context.Context) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	q.mu.Unlock()

	// Stop intake. Workers drain whatever is already buffered until ch empties
	// or startCtx cancels.
	close(q.ch)

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	timeout := q.cfg.DrainTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		q.reportUnstarted()
		return nil
	case <-timer.C:
		q.reportUnstarted()
		q.log.Warn("queue drain timed out; in-flight jobs left incomplete")
		return context.DeadlineExceeded
	case <-ctx.Done():
		q.reportUnstarted()
		return ctx.Err()
	}
}

func (q *memQueue) reportUnstarted() {
	q.mu.Lock()
	defer q.mu.Unlock()
	var pending []result.JobID
	for id, st := range q.states {
		if st == statePending {
			pending = append(pending, id)
		}
	}
	if len(pending) > 0 {
		q.log.Warn("buffered jobs never started before shutdown", "count", len(pending), "job_ids", pending)
	}
}

var _ Queue = (*memQueue)(nil)
