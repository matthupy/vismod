package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// taskType is the single asynq task type this worker handles.
const taskType = "vismod:moderate"

// jobPayload is what gets serialized into Redis for each job.
//
// PAYLOAD HYGIENE (§D.3/§G.2): this carries ONLY opaque IDs/refs — never media
// bytes, Raw free-text, OCR or captions. Source.Ref is a file path/URI, not
// content. A durable Redis payload and any asynqmon UI must stay free of media.
type jobPayload struct {
	ID          result.JobID      `json:"id"`
	Source      moderation.Source `json:"source"`
	SubmittedAt time.Time         `json:"submitted_at"`
}

// asynqQueue is the Redis-backed FIFO queue driver (M5).
//
// Unlike memq it is durable, at-least-once and multi-process: a crash redelivers
// in-flight jobs rather than losing them. At-least-once REQUIRES idempotency to
// avoid double-writes on redelivery. The in-memory Sink/audit `seen` maps dedupe
// only within one live process; cross-process once-only (a redelivery landing on
// a fresh process or a second replica) is provided by the pipeline's Deduper
// gate — a Redis SETNX `vismod:done:<id>` claim committed after the Sink+audit
// writes succeed (internal/dedup, wired for driver=redis in internal/cli). So a
// redelivery to a fresh process no longer double-writes the result line or the
// audit-chain seq (§L, issue #9).
//
// Per-queue dequeue is FIFO; with >1 worker completion order is not guaranteed
// (same caveat as memq).
type asynqQueue struct {
	cfg       QueueConfig
	log       *slog.Logger
	qname     string
	redisOpt  asynq.RedisClientOpt
	client    *asynq.Client
	inspector *asynq.Inspector
	srv       *asynq.Server
}

// NewAsynqQueue builds a Redis-backed queue against redisAddr, processing the
// named queue. DeadLetter must be non-nil (mirrors memq); it receives the
// error-verdict envelope when a job is dead-lettered.
func NewAsynqQueue(cfg QueueConfig, redisAddr, qname string, log *slog.Logger) (*asynqQueue, error) {
	if cfg.DeadLetter == nil {
		return nil, errors.New("queue: QueueConfig.DeadLetter sink is required")
	}
	if redisAddr == "" {
		return nil, errors.New("queue: redis address is required for the redis driver")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if qname == "" {
		qname = "vismod"
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.DeadLetterMax > 0 {
		// memq enforces DeadLetterMax as enqueue backpressure (ErrDeadLetterFull,
		// never drop). The asynq driver does NOT: archived/dead-lettered tasks
		// live in Redis and are bounded by asynq Retention + ops, not this cap.
		// Warn so a configured safety cap is not silently swallowed.
		log.Warn("queue: deadletter_max is not enforced under the redis driver; "+
			"archive depth is bounded by asynq retention/ops, not this cap",
			"deadletter_max", cfg.DeadLetterMax)
	}
	opt := asynq.RedisClientOpt{Addr: redisAddr}
	return &asynqQueue{
		cfg:       cfg,
		log:       log,
		qname:     qname,
		redisOpt:  opt,
		client:    asynq.NewClient(opt),
		inspector: asynq.NewInspector(opt),
	}, nil
}

// newJobID returns a process-unique opaque ID. Unlike memq's per-process atomic
// counter, redis is multi-process, so a globally-unique random ID is used. It is
// NOT a sortable key the queue orders by — FIFO is asynq's pending-list property
// (§D.3), so a random ID does not perturb ordering.
func newJobID() result.JobID {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return result.JobID("job-" + hex.EncodeToString(b[:]))
}

// Enqueue appends a job. The JobID doubles as the asynq TaskID so a duplicate
// enqueue of the same JobID is deduped by asynq (idempotent intake).
func (q *asynqQueue) Enqueue(ctx context.Context, j Job) (result.JobID, error) {
	if j.ID == "" {
		j.ID = newJobID()
	}
	if j.SubmittedAt.IsZero() {
		j.SubmittedAt = time.Now().UTC()
	}

	payload, err := json.Marshal(jobPayload(j))
	if err != nil {
		return "", fmt.Errorf("queue: marshal payload: %w", err)
	}

	opts := []asynq.Option{
		asynq.Queue(q.qname),
		asynq.TaskID(string(j.ID)),
		asynq.MaxRetry(q.cfg.MaxRetries),
	}
	if q.cfg.JobTimeout > 0 {
		opts = append(opts, asynq.Timeout(q.cfg.JobTimeout))
	}

	_, err = q.client.EnqueueContext(ctx, asynq.NewTask(taskType, payload), opts...)
	if err != nil {
		// A conflicting/duplicate TaskID means this JobID is already queued or
		// processed — treat intake as idempotently successful, not an error.
		if errors.Is(err, asynq.ErrTaskIDConflict) || errors.Is(err, asynq.ErrDuplicateTask) {
			return j.ID, nil
		}
		return "", fmt.Errorf("queue: enqueue: %w", err)
	}
	return j.ID, nil
}

// Start launches the asynq server. It returns immediately; cancelling ctx stops
// pulling NEW work (mirrors memq's start ctx). Graceful drain of in-flight jobs
// happens in Close.
func (q *asynqQueue) Start(ctx context.Context, handler Handler) error {
	if handler == nil {
		return errors.New("queue: nil handler")
	}

	q.srv = asynq.NewServer(q.redisOpt, asynq.Config{
		Concurrency:     q.cfg.Workers,
		Queues:          map[string]int{q.qname: 1},
		ShutdownTimeout: q.drainTimeout(),
		RetryDelayFunc:  q.retryDelay,
		ErrorHandler:    asynq.ErrorHandlerFunc(q.onError),
		Logger:          slogAsynqLogger{q.log},
	})

	mux := asynq.NewServeMux()
	mux.HandleFunc(taskType, q.processor(handler))

	if err := q.srv.Start(mux); err != nil {
		return fmt.Errorf("queue: start server: %w", err)
	}

	// Cancelling the worker ctx stops pulling new work (drain in-flight via Close),
	// mirroring memq's start-ctx semantics. This is intentionally redundant with
	// Close: Close calls srv.Shutdown() (= Stop + wait). asynq's Stop is
	// idempotent, so whichever fires first, the second is a harmless no-op. If ctx
	// is never cancelled the goroutine simply blocks until Close stops the server.
	go func() {
		<-ctx.Done()
		q.srv.Stop()
	}()
	return nil
}

// processor adapts the queue Handler to asynq, mapping Disposition -> asynq's
// error contract so the memq->asynq swap is behavior-preserving:
//
//	Ack        -> nil          (success, task removed)
//	Retry      -> err          (asynq retries up to MaxRetry, then archives = DLQ)
//	DeadLetter -> SkipRetry    (archived immediately, no retry)
//
// A handler panic is recovered here and mapped to DeadLetter (poison message) so
// the worker pool never crashes and a deterministic panic does not retry-loop.
func (q *asynqQueue) processor(handler Handler) func(context.Context, *asynq.Task) error {
	return func(ctx context.Context, t *asynq.Task) error {
		var p jobPayload
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			// Undecodable payload is a poison message: archive, do not retry.
			return fmt.Errorf("decode payload: %v: %w", err, asynq.SkipRetry)
		}
		j := Job(p)

		disp, herr := q.invoke(ctx, handler, j)
		switch disp {
		case Ack:
			return nil
		case DeadLetter:
			return fmt.Errorf("dead-letter: %v: %w", errString(herr), asynq.SkipRetry)
		case Retry:
			if herr == nil {
				herr = errors.New("retry requested")
			}
			return herr
		default:
			return fmt.Errorf("unknown disposition %v: %v: %w", disp, errString(herr), asynq.SkipRetry)
		}
	}
}

// invoke runs the handler under panic recovery.
func (q *asynqQueue) invoke(ctx context.Context, handler Handler, j Job) (disp Disposition, err error) {
	defer func() {
		if r := recover(); r != nil {
			disp = DeadLetter
			err = fmt.Errorf("panic in handler: %v", r)
		}
	}()
	return handler(ctx, j)
}

// onError is invoked by asynq on every failed attempt. On the FINAL failure
// (retries exhausted, or SkipRetry) it writes the could-not-evaluate error
// envelope to the DeadLetter sink — mirroring memq's dead-letter behavior so the
// driver swap is behavior-preserving. asynq independently archives the task
// (DeadLetterDepth). The Sink dedupes per JobID within a live process; across a
// crash/restart or replica that is not yet guaranteed (see asynqQueue doc).
func (q *asynqQueue) onError(ctx context.Context, t *asynq.Task, err error) {
	retried, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)
	final := errors.Is(err, asynq.SkipRetry) || retried >= maxRetry
	if !final {
		q.log.Warn("job attempt failed; will retry", "retried", retried, "max_retry", maxRetry, "err", err)
		return
	}

	var p jobPayload
	if uerr := json.Unmarshal(t.Payload(), &p); uerr != nil {
		// A corrupt payload must NOT lose the JobID — the Sink dedupes on it.
		// asynq's TaskID is the JobID (set in Enqueue), so recover it from ctx.
		if id, ok := asynq.GetTaskID(ctx); ok {
			p.ID = result.JobID(id)
		}
		q.log.Error("dead-letter payload decode failed", "job_id", p.ID, "err", uerr)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	env := result.ResultEnvelope{
		JobID:      p.ID,
		Source:     p.Source,
		Error:      err.Error(), // Result is nil => could-not-evaluate, never "allow"
		StartedAt:  now,
		FinishedAt: now,
	}
	if werr := q.cfg.DeadLetter.Write(ctx, env); werr != nil {
		q.log.Error("dead-letter sink write failed", "job_id", p.ID, "err", werr)
	}
	q.log.Warn("job dead-lettered", "job_id", p.ID, "cause", err)
}

func (q *asynqQueue) retryDelay(n int, _ error, _ *asynq.Task) time.Duration {
	d := q.cfg.RetryBackoff
	if d <= 0 {
		return time.Second
	}
	// Linear backoff mirrors memq.
	return time.Duration(n+1) * d
}

func (q *asynqQueue) drainTimeout() time.Duration {
	if q.cfg.DrainTimeout > 0 {
		return q.cfg.DrainTimeout
	}
	return 30 * time.Second
}

// Close gracefully drains in-flight work (up to ShutdownTimeout) then releases
// the Redis clients. In-flight jobs that do not finish are left for redelivery
// (at-least-once) — never acked-done, never dropped.
func (q *asynqQueue) Close(ctx context.Context) error {
	if q.srv != nil {
		done := make(chan struct{})
		go func() {
			q.srv.Shutdown() // Stop + wait up to ShutdownTimeout for in-flight
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			q.log.Warn("queue close ctx done before drain finished")
		}
	}
	_ = q.client.Close()
	_ = q.inspector.Close()
	return nil
}

// QueueDepth returns the outstanding backlog — the redis analogue of memq's
// buffered length. It sums every not-yet-completed state (Pending + Active +
// Scheduled + Retry) so the gauge does not undercount during a retry storm,
// when work sits in Retry/Scheduled rather than Pending. Archived (dead-lettered)
// is excluded — that is terminal and reported by DeadLetterDepth. A missing queue
// (nothing enqueued yet) reports 0, not an error.
func (q *asynqQueue) QueueDepth(_ context.Context) (int, error) {
	info, err := q.inspector.GetQueueInfo(q.qname)
	if err != nil {
		if errors.Is(err, asynq.ErrQueueNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("queue: depth: %w", err)
	}
	return info.Pending + info.Active + info.Scheduled + info.Retry, nil
}

// DeadLetterDepth returns the number of archived (dead-lettered) tasks, for the
// vismod_deadletter_depth gauge. A missing queue reports 0.
func (q *asynqQueue) DeadLetterDepth() int {
	info, err := q.inspector.GetQueueInfo(q.qname)
	if err != nil {
		return 0
	}
	return info.Archived
}

// Ping verifies Redis reachability (§F.2). Used at boot and by the /readyz
// readiness probe so a Redis outage flips readiness rather than black-holing
// jobs. asynq's client Ping does not take a context, so it runs off-goroutine and
// ctx bounds the wait — a hung Redis connection must not block /readyz or boot.
func (q *asynqQueue) Ping(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- q.client.Ping() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

var (
	_ Queue         = (*asynqQueue)(nil)
	_ DepthReporter = (*asynqQueue)(nil)
	_ Pinger        = (*asynqQueue)(nil)
)

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// slogAsynqLogger adapts slog to asynq's Logger interface.
type slogAsynqLogger struct{ log *slog.Logger }

func (l slogAsynqLogger) Debug(args ...any) { l.log.Debug(fmt.Sprint(args...)) }
func (l slogAsynqLogger) Info(args ...any)  { l.log.Info(fmt.Sprint(args...)) }
func (l slogAsynqLogger) Warn(args ...any)  { l.log.Warn(fmt.Sprint(args...)) }
func (l slogAsynqLogger) Error(args ...any) { l.log.Error(fmt.Sprint(args...)) }
func (l slogAsynqLogger) Fatal(args ...any) { l.log.Error(fmt.Sprint(args...)) }
