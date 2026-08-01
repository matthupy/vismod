package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redisq is the production driver: Redis-backed, DURABLE, at-least-once,
// and the substrate for multi-replica autoscaling (every replica consumes
// the same pending list; scale = add replicas).
//
// Dependency note: raw go-redis (not asynq) because the FIFO contract is
// load-bearing — a Redis LIST with LPUSH (enqueue) + LMOVE tail→processing
// (dequeue) preserves arrival order by construction and gives an honest
// LLEN for QueueDepth, whereas a scheduling framework does not guarantee
// strict FIFO and hides the Disposition mapping.
//
// Layout (all keys under prefix):
//
//	<p>:pending    LIST  — arrival-ordered payloads (FIFO: LPUSH + tail-pop)
//	<p>:processing LIST  — in-flight payloads (crash recovery source)
//	<p>:retry      ZSET  — delayed retries, score = ready-at unix ms
//	<p>:dead       LIST  — dead-letter entries
//
// At-least-once ⇒ redelivery is possible (crash between handler and ack;
// retry-mover races). Sink.Write and the audit append are idempotent per
// JobID, so redelivery never double-writes.
//
// Payload hygiene: elements carry the Job (opaque file refs), NEVER media
// bytes. The Redis instance must be access-controlled (§G.2).
type Redisq struct {
	cfg    QueueConfig
	rdb    redis.UniversalClient
	log    *slog.Logger
	prefix string

	stop    chan struct{}
	closed  atomic.Bool
	started atomic.Bool
	active  atomic.Int64
	workers sync.WaitGroup
}

type redisPayload struct {
	Job      Job `json:"job"`
	Attempts int `json:"attempts"`
}

// NewRedisq builds the driver. The DLQ lives in Redis (cfg.DeadLetter is
// ignored; the Redis DLQ is authoritative so all replicas share it).
func NewRedisq(cfg QueueConfig, rdb redis.UniversalClient, prefix string, log *slog.Logger) *Redisq {
	cfg.applyDefaults()
	if prefix == "" {
		prefix = "vismod"
	}
	if log == nil {
		log = slog.Default()
	}
	q := &Redisq{cfg: cfg, rdb: rdb, prefix: prefix, log: log, stop: make(chan struct{})}
	q.cfg.DeadLetter = &redisDLQ{rdb: rdb, key: q.key("dead")}
	return q
}

func (q *Redisq) key(s string) string { return q.prefix + ":" + s }

// Ping validates Redis reachability (boot validation and /readyz — Redis
// is the SPOF; an outage must flip readiness, never black-hole jobs).
func (q *Redisq) Ping(ctx context.Context) error {
	return q.rdb.Ping(ctx).Err()
}

// DLQ exposes the shared Redis dead-letter sink.
func (q *Redisq) DLQ() DeadLetterSink { return q.cfg.DeadLetter }

func (q *Redisq) Enqueue(ctx context.Context, j Job) (JobID, error) {
	if q.closed.Load() {
		return "", ErrQueueClosed
	}
	depth, err := q.cfg.DeadLetter.Depth(ctx)
	if err != nil {
		return "", fmt.Errorf("redisq: dlq depth: %w", err)
	}
	if depth >= q.cfg.DeadLetterMax {
		q.log.Error("dead-letter queue at capacity; rejecting enqueue",
			"dlq_depth", depth, "dlq_max", q.cfg.DeadLetterMax)
		return "", ErrDeadLetterFull
	}
	b, err := json.Marshal(redisPayload{Job: j})
	if err != nil {
		return "", err
	}
	if err := q.rdb.LPush(ctx, q.key("pending"), b).Err(); err != nil {
		return "", fmt.Errorf("redisq: enqueue: %w", err)
	}
	return j.ID, nil
}

func (q *Redisq) Start(ctx context.Context, handler Handler) error {
	if !q.started.CompareAndSwap(false, true) {
		return fmt.Errorf("redisq: Start called twice")
	}
	// Crash recovery: anything still in processing belonged to a dead
	// replica of this consumer group; move it back to pending
	// (at-least-once redelivery).
	if err := q.recoverOrphans(ctx); err != nil {
		return fmt.Errorf("redisq: orphan recovery: %w", err)
	}
	for i := 0; i < q.cfg.Workers; i++ {
		q.workers.Add(1)
		go q.worker(ctx, handler)
	}
	q.workers.Add(1)
	go q.retryMover(ctx)
	return nil
}

func (q *Redisq) recoverOrphans(ctx context.Context) error {
	for {
		res, err := q.rdb.LMove(ctx, q.key("processing"), q.key("pending"), "RIGHT", "RIGHT").Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		q.log.Warn("recovered orphaned in-flight job from a previous run", "payload_len", len(res))
	}
}

func (q *Redisq) worker(ctx context.Context, handler Handler) {
	defer q.workers.Done()
	for {
		select {
		case <-q.stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		// FIFO: tail-pop of an LPUSH list = arrival order. LMove (not a
		// blocking variant) keeps shutdown prompt and works under
		// miniredis; the short poll interval bounds idle latency.
		raw, err := q.rdb.LMove(ctx, q.key("pending"), q.key("processing"), "RIGHT", "LEFT").Result()
		if errors.Is(err, redis.Nil) {
			sleepOrStop(q.stop, ctx, 100*time.Millisecond)
			continue
		}
		if err != nil {
			q.log.Error("redis dequeue failed", "err", err)
			sleepOrStop(q.stop, ctx, time.Second)
			continue
		}
		q.process(raw, handler)
	}
}

func (q *Redisq) process(raw string, handler Handler) {
	q.active.Add(1)
	defer q.active.Add(-1)

	var p redisPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		// Poison payload: never silently dropped.
		q.log.Error("undecodable job payload; dead-lettering", "err", err)
		q.finish(raw, DeadLetterEntry{Reason: "undecodable payload: " + err.Error(), At: time.Now().UTC()}, true)
		return
	}

	// Per-job ctx derives from Background so draining jobs keep their
	// full timeout (same contract as memq).
	ctx, cancel := context.WithTimeout(context.Background(), q.cfg.JobTimeout)
	disp, err := runHandler(ctx, handler, p.Job)
	cancel()
	p.Attempts++

	switch disp {
	case Ack:
		q.ack(raw)
	case Retry:
		if p.Attempts > q.cfg.MaxRetries {
			q.finish(raw, DeadLetterEntry{Job: p.Job, Attempts: p.Attempts, At: time.Now().UTC(),
				Reason: fmt.Sprintf("max retries (%d) exceeded: %v", q.cfg.MaxRetries, err)}, true)
			return
		}
		q.scheduleRetry(raw, p, err)
	default:
		q.finish(raw, DeadLetterEntry{Job: p.Job, Attempts: p.Attempts, At: time.Now().UTC(),
			Reason: fmt.Sprintf("dead-lettered: %v", err)}, true)
	}
}

func (q *Redisq) ack(raw string) {
	if err := q.rdb.LRem(context.Background(), q.key("processing"), 1, raw).Err(); err != nil {
		q.log.Error("ack (LREM processing) failed; job may redeliver", "err", err)
	}
}

func (q *Redisq) finish(raw string, e DeadLetterEntry, dead bool) {
	ctx := context.Background()
	if dead {
		if err := q.cfg.DeadLetter.Write(ctx, e); err != nil {
			// Never drop a dead letter: leave the payload in processing so
			// it is redelivered after restart instead of vanishing.
			q.log.Error("dead-letter write failed; leaving job in processing for redelivery", "err", err)
			return
		}
		q.log.Warn("job dead-lettered", "job_id", e.Job.ID, "attempts", e.Attempts, "reason", e.Reason)
	}
	q.ack(raw)
}

func (q *Redisq) scheduleRetry(raw string, p redisPayload, cause error) {
	ctx := context.Background()
	b, err := json.Marshal(p)
	if err != nil {
		q.log.Error("marshal retry payload", "err", err)
		return
	}
	readyAt := time.Now().Add(q.cfg.RetryBackoff * time.Duration(p.Attempts))
	if err := q.rdb.ZAdd(ctx, q.key("retry"), redis.Z{Score: float64(readyAt.UnixMilli()), Member: string(b)}).Err(); err != nil {
		q.log.Error("schedule retry failed; leaving job in processing for redelivery", "err", err)
		return
	}
	q.ack(raw)
	q.log.Warn("job retry scheduled", "job_id", p.Job.ID, "attempt", p.Attempts,
		"ready_at", readyAt.UTC(), "cause", fmt.Sprint(cause))
}

// retryMover promotes due retries back onto the pending list.
func (q *Redisq) retryMover(ctx context.Context) {
	defer q.workers.Done()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-q.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			now := float64(time.Now().UnixMilli())
			due, err := q.rdb.ZRangeByScore(ctx, q.key("retry"), &redis.ZRangeBy{
				Min: "-inf", Max: fmt.Sprintf("%f", now), Count: 100,
			}).Result()
			if err != nil || len(due) == 0 {
				continue
			}
			for _, member := range due {
				// LPush before ZRem: a crash in between duplicates the job
				// (at-least-once, deduped downstream per JobID) instead of
				// losing it.
				if err := q.rdb.LPush(ctx, q.key("pending"), member).Err(); err != nil {
					continue
				}
				q.rdb.ZRem(ctx, q.key("retry"), member)
			}
		}
	}
}

// QueueDepth is pending + delayed retries: the uniform autoscaling signal.
func (q *Redisq) QueueDepth(ctx context.Context) (int, error) {
	pending, err := q.rdb.LLen(ctx, q.key("pending")).Result()
	if err != nil {
		return 0, err
	}
	retries, err := q.rdb.ZCard(ctx, q.key("retry")).Result()
	if err != nil {
		return 0, err
	}
	return int(pending + retries), nil
}

// ActiveWorkers reports jobs currently inside a handler (metrics).
func (q *Redisq) ActiveWorkers() int { return int(q.active.Load()) }

// Close stops pulling and gives in-flight jobs the drain budget. Jobs not
// finished stay in the processing list — durable and unacked — and are
// redelivered by orphan recovery on the next Start. Nothing is lost.
func (q *Redisq) Close(ctx context.Context) error {
	if !q.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(q.stop)
	done := make(chan struct{})
	go func() {
		q.workers.Wait()
		close(done)
	}()
	drain := time.NewTimer(q.cfg.DrainTimeout)
	defer drain.Stop()
	select {
	case <-done:
		return nil
	case <-drain.C:
		return fmt.Errorf("redisq: drain timeout after %s; in-flight jobs remain in processing for redelivery", q.cfg.DrainTimeout)
	case <-ctx.Done():
		return fmt.Errorf("redisq: drain cancelled: %w", ctx.Err())
	}
}

func sleepOrStop(stop <-chan struct{}, ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
	case <-ctx.Done():
	case <-t.C:
	}
}

// redisDLQ is the shared, durable dead-letter sink.
type redisDLQ struct {
	rdb redis.UniversalClient
	key string
}

func (d *redisDLQ) Write(ctx context.Context, e DeadLetterEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return d.rdb.LPush(ctx, d.key, b).Err()
}

func (d *redisDLQ) Depth(ctx context.Context) (int, error) {
	n, err := d.rdb.LLen(ctx, d.key).Result()
	return int(n), err
}

var _ Queue = (*Redisq)(nil)
