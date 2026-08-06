package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
//	<p>:pending          LIST  — arrival-ordered payloads (FIFO: LPUSH + tail-pop)
//	<p>:processing:<id>  LIST  — in-flight payloads for ONE replica
//	<p>:instances        ZSET  — replica id → last heartbeat unix ms
//	<p>:retry            ZSET  — delayed retries, score = ready-at unix ms
//	<p>:dead             LIST  — dead-letter entries
//
// The processing list is per-replica because it is a claim, not a queue.
// When it was one shared key, every Start moved the whole thing back to
// pending — so any second replica booting (KEDA scale-up, rolling deploy,
// crashloop) re-queued jobs that live replicas were running at that moment,
// paying for the vendor call and the webhook POST twice while looking
// exactly like ordinary at-least-once redelivery. A replica now reclaims
// only its own key, and work from a replica that has genuinely stopped
// heartbeating is reclaimed by the reaper.
//
// At-least-once ⇒ redelivery is possible (crash between handler and ack;
// retry-mover races). Sink.Write and the audit append are idempotent per
// JobID, so redelivery never double-writes.
//
// Payload hygiene: elements carry the Job (opaque file refs), NEVER media
// bytes. The Redis instance must be access-controlled (§G.2).
type Redisq struct {
	cfg      QueueConfig
	rdb      redis.UniversalClient
	log      *slog.Logger
	prefix   string
	instance string // identifies this replica's processing list

	stop    chan struct{}
	closed  atomic.Bool
	started atomic.Bool
	active  atomic.Int64
	workers sync.WaitGroup
}

// Timing for instance liveness. Vars, not consts, so tests can drive the
// keeper and the reaper without sleeping for real seconds.
var (
	// instanceHeartbeat is how often a replica refreshes its liveness.
	instanceHeartbeat = 10 * time.Second
	// instanceReclaimAfter is how stale a heartbeat must be before another
	// replica reclaims that instance's in-flight jobs.
	//
	// Liveness is the heartbeat, NOT job duration: a replica keeps
	// heartbeating while it holds a job, so this does not need to exceed
	// JobTimeout. It only needs to survive a few missed beats — a GC pause,
	// a brief Redis blip — because reclaiming too early is what re-runs a
	// job that is still executing.
	instanceReclaimAfter = 60 * time.Second
	// reaperInterval is how often stale instances are swept.
	reaperInterval = 15 * time.Second
)

// legacyInstance names the pre-upgrade shared processing key, so payloads
// left there by an older vismod are reclaimed by the same reaper path
// instead of being stranded forever.
const legacyInstance = "legacy"

// newInstanceID identifies this replica. Random rather than hostname-based:
// two replicas must never share a processing list, and a restarted pod must
// not inherit a predecessor's claim implicitly — the reaper hands that work
// back explicitly.
// randRead is a seam so the entropy-failure fallback can be tested. Two
// replicas sharing an instance id would share a processing list, which is
// the exact bug the per-replica key exists to prevent, so the fallback has
// to actually produce something usable.
var randRead = rand.Read

func newInstanceID() string {
	var b [8]byte
	if _, err := randRead(b[:]); err != nil {
		return fmt.Sprintf("i-%d", time.Now().UnixNano())
	}
	return "i-" + hex.EncodeToString(b[:])
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
	q := &Redisq{
		cfg: cfg, rdb: rdb, prefix: prefix, log: log,
		instance: newInstanceID(),
		stop:     make(chan struct{}),
	}
	q.cfg.DeadLetter = &redisDLQ{rdb: rdb, key: q.key("dead")}
	return q
}

func (q *Redisq) key(s string) string { return q.prefix + ":" + s }

// processingKey is this replica's in-flight list. Only this replica pops
// from it; only the reaper may reclaim it, and only once this replica has
// stopped heartbeating.
func (q *Redisq) processingKey() string { return q.key("processing:" + q.instance) }

func (q *Redisq) instanceProcessingKey(instance string) string {
	if instance == legacyInstance {
		return q.key("processing") // the pre-upgrade shared key
	}
	return q.key("processing:" + instance)
}

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
	// Register before touching anything: an unregistered replica that
	// starts claiming jobs is invisible to every other replica's reaper.
	if err := q.heartbeat(ctx); err != nil {
		return fmt.Errorf("redisq: register instance: %w", err)
	}
	// Note what a pre-upgrade vismod may have left in the shared key, so
	// the reaper can age it out rather than stranding it.
	if err := q.registerLegacyIfPresent(ctx); err != nil {
		return fmt.Errorf("redisq: legacy processing scan: %w", err)
	}
	// Reclaim only OUR key. A previous process using this instance id is
	// impossible (ids are random), so this is normally a no-op; it stays
	// because it is the correct scope, and Start may be reached after a
	// partial failure.
	if err := q.recoverOrphans(ctx); err != nil {
		return fmt.Errorf("redisq: orphan recovery: %w", err)
	}
	// Work stranded by replicas that have stopped heartbeating — including
	// this pod's own previous life — comes back through the reaper.
	q.reapDeadInstances(ctx)

	for range q.cfg.Workers {
		q.workers.Add(1)
		go q.worker(ctx, handler)
	}
	q.workers.Add(1)
	go q.retryMover(ctx)
	q.workers.Add(1)
	go q.instanceKeeper(ctx)
	return nil
}

// recoverOrphans requeues anything left in THIS replica's processing list.
func (q *Redisq) recoverOrphans(ctx context.Context) error {
	n, err := q.drainProcessing(ctx, q.processingKey())
	if err != nil {
		return err
	}
	if n > 0 {
		q.log.Warn("recovered in-flight jobs from this instance's previous run",
			"instance", q.instance, "jobs", n)
	}
	return nil
}

// drainProcessing moves every payload in key back onto the tail of pending,
// preserving arrival order.
func (q *Redisq) drainProcessing(ctx context.Context, key string) (int, error) {
	var n int
	for {
		_, err := q.rdb.LMove(ctx, key, q.key("pending"), "RIGHT", "RIGHT").Result()
		if errors.Is(err, redis.Nil) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		n++
	}
}

// heartbeat refreshes this replica's liveness.
func (q *Redisq) heartbeat(ctx context.Context) error {
	return q.rdb.ZAdd(ctx, q.key("instances"), redis.Z{
		Score: float64(time.Now().UnixMilli()), Member: q.instance,
	}).Err()
}

// registerLegacyIfPresent records the pre-upgrade shared processing key as
// a pseudo-instance the first time it is seen with payloads in it.
//
// During a rolling upgrade an old replica may still be writing there, so
// the entry is dated NOW rather than reclaimed immediately: it ages out
// like any other instance, by which point the old replicas are gone. That
// is strictly safer than the old unconditional drain, which requeued live
// work on every single Start.
func (q *Redisq) registerLegacyIfPresent(ctx context.Context) error {
	n, err := q.rdb.LLen(ctx, q.key("processing")).Result()
	if err != nil || n == 0 {
		return err
	}
	added, err := q.rdb.ZAddNX(ctx, q.key("instances"), redis.Z{
		Score: float64(time.Now().UnixMilli()), Member: legacyInstance,
	}).Result()
	if err != nil {
		return err
	}
	if added > 0 {
		q.log.Warn("found in-flight jobs in the pre-upgrade shared processing list; they will be reclaimed once stale",
			"jobs", n, "reclaim_after", instanceReclaimAfter)
	}
	return nil
}

// instanceKeeper refreshes this replica's heartbeat and reaps replicas that
// have stopped refreshing theirs.
func (q *Redisq) instanceKeeper(ctx context.Context) {
	defer q.workers.Done()
	beat := time.NewTicker(instanceHeartbeat)
	defer beat.Stop()
	reap := time.NewTicker(reaperInterval)
	defer reap.Stop()
	for {
		select {
		case <-q.stop:
			return
		case <-ctx.Done():
			return
		case <-beat.C:
			if err := q.heartbeat(ctx); err != nil {
				// Losing the heartbeat means other replicas will eventually
				// reclaim jobs this one is still running.
				q.log.Error("instance heartbeat failed; in-flight jobs may be reclaimed by another replica",
					"instance", q.instance, "err", err)
			}
		case <-reap.C:
			q.reapDeadInstances(ctx)
		}
	}
}

// reapDeadInstances returns work held by replicas whose heartbeat has gone
// stale. This is the only path that recovers a crashed replica's jobs, so
// it must run — a missing reaper strands them permanently.
func (q *Redisq) reapDeadInstances(ctx context.Context) {
	cutoff := time.Now().Add(-instanceReclaimAfter).UnixMilli()
	stale, err := q.rdb.ZRangeByScore(ctx, q.key("instances"), &redis.ZRangeBy{
		Min: "-inf", Max: fmt.Sprintf("%d", cutoff), Count: 100,
	}).Result()
	if err != nil {
		q.log.Error("scan for dead replicas failed", "err", err)
		return
	}
	for _, instance := range stale {
		if instance == q.instance {
			continue // never reap ourselves
		}
		n, err := q.drainProcessing(ctx, q.instanceProcessingKey(instance))
		if err != nil {
			q.log.Error("reclaiming a dead replica's jobs failed", "instance", instance, "err", err)
			continue
		}
		// Deregister only after the payloads are back on pending: crashing
		// in between must leave the instance reapable, not forget it.
		if err := q.rdb.ZRem(ctx, q.key("instances"), instance).Err(); err != nil {
			q.log.Error("deregistering a dead replica failed", "instance", instance, "err", err)
			continue
		}
		if n > 0 {
			q.log.Warn("reclaimed in-flight jobs from a replica that stopped heartbeating",
				"instance", instance, "jobs", n)
		}
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
		raw, err := q.rdb.LMove(ctx, q.key("pending"), q.processingKey(), "RIGHT", "LEFT").Result()
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
	if err := q.rdb.LRem(context.Background(), q.processingKey(), 1, raw).Err(); err != nil {
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

// ProcessingDepth counts payloads claimed by replicas but not yet acked,
// across all of them.
//
// Deliberately NOT part of QueueDepth: that is the autoscaling signal and
// must stay pending+retry, or scaling would chase its own in-flight work.
// But jobs can park here — finish() leaves a payload in processing when a
// dead-letter write fails — and without a gauge of its own that is a queue
// nothing counts, alerts on, or times out, while the autoscaler reads zero
// and scales to zero on top of it.
func (q *Redisq) ProcessingDepth(ctx context.Context) (int, error) {
	var total int64
	// SCAN rather than walking the instances ZSET: a key whose owner was
	// deregistered (or never registered) is exactly the stranded case this
	// gauge exists to expose, and the ZSET would not list it.
	var cursor uint64
	for {
		keys, next, err := q.rdb.Scan(ctx, cursor, q.key("processing:")+"*", 100).Result()
		if err != nil {
			return 0, err
		}
		for _, k := range keys {
			n, err := q.rdb.LLen(ctx, k).Result()
			if err != nil {
				return 0, err
			}
			total += n
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	// Plus anything left in the pre-upgrade shared key.
	legacy, err := q.rdb.LLen(ctx, q.key("processing")).Result()
	if err != nil {
		return 0, err
	}
	return int(total + legacy), nil
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
