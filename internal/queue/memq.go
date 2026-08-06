package queue

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Memq is the in-process dev/CLI driver: a buffered channel (FIFO by
// construction) plus a fixed worker pool, with a real DLQ and bounded
// in-memory retry.
//
// Memq is NON-DURABLE, at-most-once, single-process. A crash loses
// enqueued and in-flight jobs. It is not for production intake; serve mode
// warns at boot and in /readyz when driver=memory.
type Memq struct {
	cfg QueueConfig
	log *slog.Logger

	jobs chan queuedJob
	stop chan struct{} // closed on Close: workers stop pulling

	closed  atomic.Bool
	started atomic.Bool
	pending atomic.Int64 // buffered + scheduled-for-retry
	active  atomic.Int64 // jobs currently in a handler

	workers sync.WaitGroup
	timers  sync.WaitGroup

	mu     sync.Mutex
	states map[JobID]string // job states: queued|running|retrying|done|dead
	// finished is a FIFO of the ids that have reached a terminal state,
	// oldest first, bounding how many of them states retains.
	finished []JobID
}

// maxFinishedStates caps the terminal (done|dead) entries kept in States.
//
// Nothing ever deleted from this map, so it held every job id the process
// had ever seen: memory grew with uptime, and States() copies the whole map
// under the same mutex every setState takes — so opening the operator
// dashboard during an incident stalled every worker's state transition, and
// the stall got worse the longer the process had been up.
//
// In-flight jobs (queued|running|retrying) are always retained; only
// finished ones age out, matching observe.JobTracker's bounded ring of
// recent outcomes.
const maxFinishedStates = 500

func isTerminalState(s string) bool { return s == "done" || s == "dead" }

type queuedJob struct {
	job      Job
	attempts int // completed handler attempts so far
}

// NewMemq builds the in-memory driver. A nil DLQ gets a MemDLQ.
func NewMemq(cfg QueueConfig, log *slog.Logger) *Memq {
	cfg.applyDefaults()
	if cfg.DeadLetter == nil {
		cfg.DeadLetter = NewMemDLQ()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Memq{
		cfg:    cfg,
		log:    log,
		jobs:   make(chan queuedJob, cfg.Buffer),
		stop:   make(chan struct{}),
		states: map[JobID]string{},
	}
}

// DLQ exposes the dead-letter sink (for depth metrics and tests).
func (q *Memq) DLQ() DeadLetterSink { return q.cfg.DeadLetter }

func (q *Memq) setState(id JobID, s string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	prev, existed := q.states[id]
	q.states[id] = s
	if !isTerminalState(s) || (existed && isTerminalState(prev)) {
		return // still in flight, or already counted as finished
	}
	q.finished = append(q.finished, id)
	for len(q.finished) > maxFinishedStates {
		// The oldest finished job ages out. Guard against a redelivered id
		// that has since gone back in flight.
		oldest := q.finished[0]
		q.finished = q.finished[1:]
		if isTerminalState(q.states[oldest]) {
			delete(q.states, oldest)
		}
	}
}

// States returns a snapshot of job states (for the UI/tests).
func (q *Memq) States() map[JobID]string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[JobID]string, len(q.states))
	for k, v := range q.states {
		out[k] = v
	}
	return out
}

func (q *Memq) Enqueue(ctx context.Context, j Job) (JobID, error) {
	if q.closed.Load() {
		return "", ErrQueueClosed
	}
	depth, err := q.cfg.DeadLetter.Depth(ctx)
	if err != nil {
		return "", fmt.Errorf("memq: dlq depth: %w", err)
	}
	if depth >= q.cfg.DeadLetterMax {
		// Fail-safe backpressure: never drop dead letters, never
		// auto-allow; reject intake with a retryable signal instead.
		q.log.Error("dead-letter queue at capacity; rejecting enqueue",
			"dlq_depth", depth, "dlq_max", q.cfg.DeadLetterMax)
		return "", ErrDeadLetterFull
	}
	select {
	case q.jobs <- queuedJob{job: j}:
		q.pending.Add(1)
		q.setState(j.ID, "queued")
		return j.ID, nil
	default:
		return "", ErrQueueFull
	}
}

func (q *Memq) Start(ctx context.Context, handler Handler) error {
	if !q.started.CompareAndSwap(false, true) {
		return fmt.Errorf("memq: Start called twice")
	}
	for i := 0; i < q.cfg.Workers; i++ {
		q.workers.Add(1)
		go q.worker(ctx, handler)
	}
	return nil
}

func (q *Memq) worker(ctx context.Context, handler Handler) {
	defer q.workers.Done()
	for {
		select {
		case <-q.stop:
			return
		case <-ctx.Done():
			return
		case qj := <-q.jobs:
			q.pending.Add(-1)
			q.process(qj, handler)
		}
	}
}

func (q *Memq) process(qj queuedJob, handler Handler) {
	q.active.Add(1)
	defer q.active.Add(-1)
	q.setState(qj.job.ID, "running")

	// The per-job ctx derives from Background, not the lifecycle ctx: a
	// job pulled before shutdown keeps its full JobTimeout to finish,
	// Sink.Write, and ack during the drain window.
	ctx, cancel := context.WithTimeout(context.Background(), q.cfg.JobTimeout)
	disp, err := runHandler(ctx, handler, qj.job)
	cancel()
	qj.attempts++

	switch disp {
	case Ack:
		q.setState(qj.job.ID, "done")
	case Retry:
		if qj.attempts > q.cfg.MaxRetries {
			q.deadLetter(qj, fmt.Sprintf("max retries (%d) exceeded: %v", q.cfg.MaxRetries, err))
			return
		}
		q.scheduleRetry(qj, err)
	default: // DeadLetter (and any unknown disposition fails safe)
		q.deadLetter(qj, fmt.Sprintf("dead-lettered: %v", err))
	}
}

// runHandler isolates panic recovery: a panicking job dead-letters and the
// pool keeps running.
func runHandler(ctx context.Context, handler Handler, j Job) (d Disposition, err error) {
	defer func() {
		if r := recover(); r != nil {
			d = DeadLetter
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return handler(ctx, j)
}

func (q *Memq) scheduleRetry(qj queuedJob, cause error) {
	q.setState(qj.job.ID, "retrying")
	q.pending.Add(1)
	q.timers.Add(1)
	backoff := q.cfg.RetryBackoff * time.Duration(qj.attempts)
	q.log.Warn("job retry scheduled", "job_id", qj.job.ID,
		"attempt", qj.attempts, "backoff", backoff, "cause", fmt.Sprint(cause))
	go func() {
		defer q.timers.Done()
		t := time.NewTimer(backoff)
		defer t.Stop()
		select {
		case <-q.stop:
			q.pending.Add(-1)
			q.log.Warn("pending retry abandoned on shutdown (memq is non-durable)",
				"job_id", qj.job.ID, "attempts", qj.attempts)
		case <-t.C:
			select {
			case q.jobs <- qj:
				q.setState(qj.job.ID, "queued")
			case <-q.stop:
				q.pending.Add(-1)
				q.log.Warn("pending retry abandoned on shutdown (memq is non-durable)",
					"job_id", qj.job.ID, "attempts", qj.attempts)
			}
		}
	}()
}

func (q *Memq) deadLetter(qj queuedJob, reason string) {
	q.setState(qj.job.ID, "dead")
	e := DeadLetterEntry{Job: qj.job, Reason: reason, Attempts: qj.attempts, At: time.Now().UTC()}
	if err := q.cfg.DeadLetter.Write(context.Background(), e); err != nil {
		q.log.Error("dead-letter write failed", "job_id", qj.job.ID, "err", err)
	}
	q.log.Warn("job dead-lettered", "job_id", qj.job.ID, "attempts", qj.attempts, "reason", reason)
}

func (q *Memq) QueueDepth(context.Context) (int, error) {
	return int(q.pending.Load()), nil
}

// ActiveWorkers reports jobs currently inside a handler (metrics).
func (q *Memq) ActiveWorkers() int { return int(q.active.Load()) }

// Close gracefully drains: stop enqueues, stop pulling new work, give
// in-flight jobs the drain budget. Jobs not finished in time are never
// acked-done; buffered-but-unstarted jobs are logged at WARN (memq is
// non-durable, so they are lost).
func (q *Memq) Close(ctx context.Context) error {
	if !q.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(q.stop)

	done := make(chan struct{})
	go func() {
		q.workers.Wait()
		q.timers.Wait()
		close(done)
	}()

	drain := time.NewTimer(q.cfg.DrainTimeout)
	defer drain.Stop()
	var drainErr error
	select {
	case <-done:
	case <-drain.C:
		drainErr = fmt.Errorf("memq: drain timeout after %s; in-flight jobs abandoned unacked", q.cfg.DrainTimeout)
		q.log.Error("drain timeout; in-flight jobs abandoned unacked", "timeout", q.cfg.DrainTimeout)
	case <-ctx.Done():
		drainErr = fmt.Errorf("memq: drain cancelled: %w", ctx.Err())
	}

	// Surface buffered-but-unstarted jobs.
	for {
		select {
		case qj := <-q.jobs:
			q.pending.Add(-1)
			q.log.Warn("buffered job lost on shutdown (memq is non-durable)", "job_id", qj.job.ID)
		default:
			return drainErr
		}
	}
}

var _ Queue = (*Memq)(nil)
