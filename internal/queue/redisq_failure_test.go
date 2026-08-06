package queue

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// failingDLQ stands in for a Redis dead-letter write that fails. Swapped in
// after construction (NewRedisq always installs the real one) so the
// never-drop-a-dead-letter path can be exercised deterministically.
type failingDLQ struct{ depth int }

func (f failingDLQ) Write(context.Context, DeadLetterEntry) error {
	return errors.New("dlq write refused")
}
func (f failingDLQ) Depth(context.Context) (int, error) { return f.depth, nil }

// processingLen counts this replica's in-flight payloads. The processing
// list is per-replica, so there is no single shared key to count.
func processingLen(t *testing.T, q *Redisq) int {
	t.Helper()
	n, err := q.rdb.LLen(context.Background(), q.processingKey()).Result()
	if err != nil {
		t.Fatalf("LLEN %s: %v", q.processingKey(), err)
	}
	return int(n)
}

// TestRedisqDefaultPrefix: an empty prefix must not produce bare keys like
// ":pending" — two vismod deployments sharing a Redis would then collide.
func TestRedisqDefaultPrefix(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "", nil)
	if got := q.key("pending"); got != "vismod:pending" {
		t.Errorf("key = %q, want vismod:pending", got)
	}
	if q.log == nil {
		t.Error("a nil logger must fall back to the default, not stay nil")
	}
}

// TestRedisqEnqueueAfterClose: once drain has begun, new work must be
// refused rather than accepted into a queue nothing will consume.
func TestRedisqEnqueueAfterClose(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second}, rdb, "vismod-test", nil)
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), job("late")); err != ErrQueueClosed {
		t.Errorf("Enqueue after Close = %v, want ErrQueueClosed", err)
	}
	// Close is idempotent: a second call must not panic on the closed stop
	// channel or report a spurious drain failure.
	if err := q.Close(context.Background()); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestRedisqEnqueueSurfacesRedisFailure: an enqueue that cannot reach Redis
// must return the error. Reporting success would tell the submitter the
// asset is queued when nothing holds it.
func TestRedisqEnqueueSurfacesRedisFailure(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DeadLetterMax: 10}, rdb, "vismod-test", nil)
	mr.SetError("redis is down")
	if _, err := q.Enqueue(context.Background(), job("x")); err == nil {
		t.Fatal("enqueue reported success with Redis unreachable")
	}
}

// TestRedisqEnqueueSurfacesLPushFailure: the DLQ depth check succeeds but
// the push itself fails — the job is not queued and the caller must learn
// that, not receive an ID for work that does not exist.
func TestRedisqEnqueueSurfacesLPushFailure(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DeadLetterMax: 10}, rdb, "vismod-test", nil)
	q.cfg.DeadLetter = failingDLQ{depth: 0} // depth check passes without Redis
	mr.SetError("redis is down")
	if _, err := q.Enqueue(context.Background(), job("x")); err == nil {
		t.Fatal("enqueue reported success though LPUSH failed")
	}
}

// TestRedisqStartTwiceIsRefused: a second Start would double the worker
// pool against one config, silently doubling vendor spend.
func TestRedisqStartTwiceIsRefused(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second}, rdb, "vismod-test", nil)
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	}); err == nil {
		t.Error("second Start was accepted; the worker pool would be doubled")
	}
	_ = q.Close(context.Background())
}

// TestRedisqStartFailsWhenOrphanRecoveryFails: orphan recovery is how
// at-least-once survives a crash. Starting workers when it fails would
// leave a previous replica's in-flight jobs stranded in processing with
// nothing reporting it.
func TestRedisqStartFailsWhenOrphanRecoveryFails(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second}, rdb, "vismod-test", nil)
	mr.SetError("redis is down")
	err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	})
	if err == nil {
		t.Fatal("Start succeeded despite failed orphan recovery")
	}
}

// TestRedisqPoisonPayloadIsDeadLettered: a payload that cannot be decoded
// is never silently dropped — it lands in the DLQ for an operator, and the
// worker keeps running.
func TestRedisqPoisonPayloadIsDeadLettered(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: 2 * time.Second, DeadLetterMax: 10}, rdb, "vismod-test", nil)
	if err := rdb.LPush(context.Background(), q.key("pending"), "{not json").Err(); err != nil {
		t.Fatalf("seed poison payload: %v", err)
	}
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		t.Error("an undecodable payload must never reach the handler")
		return Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		d, err := q.DLQ().Depth(context.Background())
		return err == nil && d == 1
	}, "poison payload dead-lettered")
	_ = q.Close(context.Background())

	if n := processingLen(t, q); n != 0 {
		t.Errorf("processing holds %d payloads after dead-lettering, want 0 (it was acked)", n)
	}
}

// TestRedisqFailedDeadLetterWriteLeavesJobInProcessing: a dead letter that
// cannot be written must NOT be acked away. Leaving the payload in
// processing makes it redeliver after restart instead of vanishing — the
// decision is deferred, never dropped.
func TestRedisqFailedDeadLetterWriteLeavesJobInProcessing(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: 2 * time.Second, DeadLetterMax: 10}, rdb, "vismod-test", nil)
	q.cfg.DeadLetter = failingDLQ{}
	if err := rdb.LPush(context.Background(), q.key("pending"), "{not json").Err(); err != nil {
		t.Fatalf("seed poison payload: %v", err)
	}
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return processingLen(t, q) == 1 }, "undeliverable dead letter stays in processing")
	_ = q.Close(context.Background())
}

// TestRedisqRetryScheduleFailureLeavesJobInProcessing: same contract on the
// retry path. If the retry ZSET cannot be written, the payload must stay
// unacked so a restart redelivers it.
func TestRedisqRetryScheduleFailureLeavesJobInProcessing(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{
		Workers: 1, MaxRetries: 3, RetryBackoff: time.Millisecond,
		DrainTimeout: 2 * time.Second, DeadLetterMax: 10,
	}, rdb, "vismod-test", nil)

	var handled atomic.Int32
	if err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		// Break Redis from INSIDE the handler: scheduleRetry then runs
		// against a dead server with no race about when the failure lands.
		if handled.Add(1) == 1 {
			mr.SetError("redis is down")
		}
		return Retry, errors.New("transient")
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := q.Enqueue(context.Background(), job("retry-me")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return handled.Load() >= 1 }, "job handled once")

	// Let the worker loop hit its own dequeue failure against the dead
	// server too — it must log and back off, not spin or exit.
	time.Sleep(200 * time.Millisecond)
	mr.SetError("") // recover so the assertions below can read Redis

	if n := processingLen(t, q); n != 1 {
		t.Errorf("processing holds %d payloads, want 1 (an unschedulable retry must not be acked)", n)
	}
	_ = q.Close(context.Background())
}

// TestRedisqQueueDepthSurfacesFailure: depth is the autoscaling signal. A
// failed read must be an error, not a silent 0 that scales the fleet down
// while work is pending.
func TestRedisqQueueDepthSurfacesFailure(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	mr.SetError("redis is down")
	if d, err := q.QueueDepth(context.Background()); err == nil {
		t.Errorf("QueueDepth = %d with Redis down, want an error", d)
	}
}

// TestRedisqActiveWorkers tracks jobs inside a handler — the number the UI
// and the metrics export. It must be 0 when idle and non-zero in flight.
func TestRedisqActiveWorkers(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second, JobTimeout: 5 * time.Second}, rdb, "vismod-test", nil)
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

// TestRedisqWorkersExitOnContextCancel: the run context is the other exit
// path (the stop channel is the first). Workers that ignored it would keep
// consuming after the process decided to shut down.
func TestRedisqWorkersExitOnContextCancel(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 2, DrainTimeout: 3 * time.Second}, rdb, "vismod-test", nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Start(ctx, func(context.Context, Job) (Disposition, error) { return Ack, nil }); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	if err := q.Close(context.Background()); err != nil {
		t.Errorf("Close after context cancel = %v, want a clean drain", err)
	}
}

// TestRedisqCloseHonorsCallerCancellation: an already-cancelled shutdown
// context must abort the drain with an error rather than blocking for the
// full drain timeout.
func TestRedisqCloseHonorsCallerCancellation(t *testing.T) {
	rdb, _ := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Minute, JobTimeout: time.Minute}, rdb, "vismod-test", nil)
	release := make(chan struct{})
	started := make(chan struct{})
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
	err := q.Close(ctx)
	if err == nil {
		t.Error("Close with a cancelled context must report the aborted drain")
	}
	close(release)
}

// The reaper is the only path that returns a crashed replica's work, so a
// Redis failure during the sweep must be survivable and must not deregister
// an instance whose payloads were never moved — that would strand them
// permanently.
func TestRedisqReaperSurvivesRedisFailure(t *testing.T) {
	rdb, mr := newMini(t)
	ctx := context.Background()

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	mr.Close() // Redis goes away mid-sweep
	q.reapDeadInstances(ctx)
	// The assertion is that this returned rather than panicking or hanging.
}

// A replica must never reclaim its own in-flight jobs from under itself,
// even if its own heartbeat is stale (a long GC pause, a Redis blip).
func TestRedisqReaperSkipsItself(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if _, err := q.Enqueue(ctx, job("mine")); err != nil {
		t.Fatal(err)
	}
	if _, err := rdb.LMove(ctx, "vismod-test:pending", q.processingKey(), "RIGHT", "LEFT").Result(); err != nil {
		t.Fatal(err)
	}
	// Our own heartbeat is stale.
	if err := rdb.ZAdd(ctx, "vismod-test:instances", redis.Z{
		Score: float64(time.Now().Add(-10 * instanceReclaimAfter).UnixMilli()), Member: q.instance,
	}).Err(); err != nil {
		t.Fatal(err)
	}

	q.reapDeadInstances(ctx)

	n, err := rdb.LLen(ctx, q.processingKey()).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("in-flight payloads = %d, want 1: the replica reaped its own running job", n)
	}
}

// registerLegacyIfPresent must be a no-op when there is nothing in the
// pre-upgrade key, or every fresh deployment registers a phantom instance.
func TestRedisqDoesNotRegisterAnEmptyLegacyKey(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if err := q.registerLegacyIfPresent(ctx); err != nil {
		t.Fatal(err)
	}
	members, err := rdb.ZRange(ctx, "vismod-test:instances", 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m == legacyInstance {
			t.Error("registered a legacy instance with no payloads behind it")
		}
	}
}

// Start must fail loudly when Redis is unreachable: a replica that cannot
// register is invisible to every other replica's reaper, so its jobs would
// never be reclaimed if it died.
func TestRedisqStartFailsWhenItCannotRegister(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	mr.Close()

	err := q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	})
	if err == nil {
		t.Fatal("Start must fail when the instance cannot be registered")
	}
}

// ProcessingDepth is a diagnostic; a Redis failure must surface as an error
// rather than a plausible-looking zero.
func TestRedisqProcessingDepthReportsRedisFailure(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	mr.Close()

	if _, err := q.ProcessingDepth(context.Background()); err == nil {
		t.Error("ProcessingDepth returned success with Redis down; a zero here reads as 'nothing stranded'")
	}
}

// recoverOrphans logs and requeues whatever this instance left behind.
func TestRedisqRecoverOrphansRequeuesOwnPayloads(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	for _, id := range []string{"a", "b"} {
		if _, err := q.Enqueue(ctx, job(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := rdb.LMove(ctx, "vismod-test:pending", q.processingKey(), "RIGHT", "LEFT").Result(); err != nil {
			t.Fatal(err)
		}
	}

	if err := q.recoverOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if n := processingLen(t, q); n != 0 {
		t.Errorf("processing holds %d payloads after recovery, want 0", n)
	}
	depth, err := q.QueueDepth(ctx)
	if err != nil || depth != 2 {
		t.Errorf("depth = %d (%v), want 2: recovered jobs must be back on pending", depth, err)
	}
}

// withFastInstanceTiming shortens the liveness timers so the keeper and the
// reaper can be observed without sleeping for real seconds.
func withFastInstanceTiming(t *testing.T, beat, reclaim, reap time.Duration) {
	t.Helper()
	ob, orc, orp := instanceHeartbeat, instanceReclaimAfter, reaperInterval
	instanceHeartbeat, instanceReclaimAfter, reaperInterval = beat, reclaim, reap
	t.Cleanup(func() {
		instanceHeartbeat, instanceReclaimAfter, reaperInterval = ob, orc, orp
	})
}

// The heartbeat is what keeps a live replica's in-flight jobs from being
// reclaimed out from under it, so it has to actually keep ticking for the
// life of the process — not just fire once at Start.
func TestRedisqHeartbeatKeepsRefreshing(t *testing.T) {
	withFastInstanceTiming(t, 20*time.Millisecond, time.Hour, time.Hour)
	rdb, _ := newMini(t)
	ctx := context.Background()

	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second}, rdb, "vismod-test", nil)
	if err := q.Start(ctx, func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close(ctx) }()

	first, err := rdb.ZScore(ctx, "vismod-test:instances", q.instance).Result()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		s, err := rdb.ZScore(ctx, "vismod-test:instances", q.instance).Result()
		return err == nil && s > first
	}, "heartbeat score advances")
}

// The reaper must keep running on its interval, not only at Start: a
// replica that dies later is only recovered by a periodic sweep.
func TestRedisqReaperRunsPeriodically(t *testing.T) {
	withFastInstanceTiming(t, time.Hour, time.Millisecond, 20*time.Millisecond)
	rdb, _ := newMini(t)
	ctx := context.Background()

	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second}, rdb, "vismod-test", nil)
	if err := q.Start(ctx, func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close(ctx) }()

	// A replica dies AFTER we started: payload in its list, stale heartbeat.
	dead := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if err := rdb.LPush(ctx, dead.processingKey(), `{"job":{"id":"late"},"attempts":0}`).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.ZAdd(ctx, "vismod-test:instances", redis.Z{
		Score: float64(time.Now().Add(-time.Hour).UnixMilli()), Member: dead.instance,
	}).Err(); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool {
		n, err := rdb.LLen(ctx, dead.processingKey()).Result()
		return err == nil && n == 0
	}, "periodic reaper reclaims a replica that died after startup")
}

// instanceKeeper must exit on context cancellation, or Close hangs for the
// full drain timeout on every shutdown.
func TestRedisqInstanceKeeperStopsOnContextCancel(t *testing.T) {
	withFastInstanceTiming(t, 10*time.Millisecond, time.Hour, 10*time.Millisecond)
	rdb, _ := newMini(t)
	ctx, cancel := context.WithCancel(context.Background())

	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: 2 * time.Second}, rdb, "vismod-test", nil)
	if err := q.Start(ctx, func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	}); err != nil {
		t.Fatal(err)
	}
	cancel()

	done := make(chan error, 1)
	go func() { done <- q.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close after cancel = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung: a keeper goroutine did not exit on context cancel")
	}
}

// A heartbeat that cannot reach Redis must be survivable — the keeper logs
// and keeps going rather than dying, since the process is still holding
// jobs it needs to finish.
func TestRedisqHeartbeatFailureDoesNotKillTheKeeper(t *testing.T) {
	withFastInstanceTiming(t, 10*time.Millisecond, time.Hour, time.Hour)
	rdb, mr := newMini(t)
	ctx := context.Background()

	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: 2 * time.Second}, rdb, "vismod-test", nil)
	if err := q.Start(ctx, func(context.Context, Job) (Disposition, error) {
		return Ack, nil
	}); err != nil {
		t.Fatal(err)
	}
	mr.SetError("redis is down")
	time.Sleep(80 * time.Millisecond) // several failed beats

	mr.SetError("")
	waitFor(t, 3*time.Second, func() bool {
		_, err := rdb.ZScore(ctx, "vismod-test:instances", q.instance).Result()
		return err == nil
	}, "keeper resumes heartbeating after Redis recovers")
	_ = q.Close(ctx)
}

// drainProcessing must surface a Redis failure rather than reporting that it
// moved everything: a silent partial drain strands jobs.
func TestRedisqDrainProcessingSurfacesRedisFailure(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	mr.SetError("redis is down")

	if _, err := q.drainProcessing(context.Background(), q.processingKey()); err == nil {
		t.Error("drainProcessing reported success with Redis down")
	}
	if err := q.recoverOrphans(context.Background()); err == nil {
		t.Error("recoverOrphans reported success with Redis down")
	}
}

// registerLegacyIfPresent must surface a Redis failure at Start rather than
// leaving pre-upgrade payloads silently unreclaimable.
func TestRedisqRegisterLegacySurfacesRedisFailure(t *testing.T) {
	rdb, mr := newMini(t)
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	mr.SetError("redis is down")

	if err := q.registerLegacyIfPresent(context.Background()); err == nil {
		t.Error("registerLegacyIfPresent reported success with Redis down")
	}
}

// The legacy pseudo-instance is registered once, and re-registering must not
// refresh its timestamp — otherwise every restart pushes the reclaim
// deadline out and the pre-upgrade payloads are never recovered.
func TestRedisqLegacyRegistrationIsNotRefreshed(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)

	if err := rdb.LPush(ctx, q.key("processing"), `{"job":{"id":"old"},"attempts":0}`).Err(); err != nil {
		t.Fatal(err)
	}
	if err := q.registerLegacyIfPresent(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := rdb.ZScore(ctx, "vismod-test:instances", legacyInstance).Result()
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)
	if err := q.registerLegacyIfPresent(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := rdb.ZScore(ctx, "vismod-test:instances", legacyInstance).Result()
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("legacy registration was refreshed (%v -> %v); the reclaim deadline would never arrive", first, again)
	}
}

// A reaper that cannot deregister an instance must NOT report the jobs as
// reclaimed and move on — leaving it registered is what makes the next sweep
// try again.
func TestRedisqReaperLeavesInstanceRegisteredWhenDrainFails(t *testing.T) {
	rdb, mr := newMini(t)
	ctx := context.Background()

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	dead := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if err := rdb.ZAdd(ctx, "vismod-test:instances", redis.Z{
		Score: float64(time.Now().Add(-10 * instanceReclaimAfter).UnixMilli()), Member: dead.instance,
	}).Err(); err != nil {
		t.Fatal(err)
	}

	mr.SetError("redis is down")
	q.reapDeadInstances(ctx) // must not panic
	mr.SetError("")

	// Still registered, so a later sweep can retry it.
	n, err := rdb.ZScore(ctx, "vismod-test:instances", dead.instance).Result()
	if err != nil {
		t.Fatalf("dead instance was deregistered despite a failed sweep: %v", err)
	}
	_ = n
}

// If the system entropy source fails, the fallback still has to produce a
// usable, non-empty id: two replicas sharing an id share a processing list,
// which is exactly the bug per-replica keys exist to prevent.
func TestNewInstanceIDFallsBackWhenEntropyFails(t *testing.T) {
	orig := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	t.Cleanup(func() { randRead = orig })

	id := newInstanceID()
	if id == "" || id == "i-" {
		t.Fatalf("fallback instance id = %q, want a usable id", id)
	}
	if !strings.HasPrefix(id, "i-") {
		t.Errorf("fallback instance id = %q, want the i- prefix", id)
	}
}

// vismod shares a Redis with whatever else the operator runs there. A key
// collision with the wrong TYPE (someone SETs a string where we expect a
// list) must fail loudly at Start rather than silently skipping recovery and
// stranding in-flight jobs.
func TestRedisqStartFailsOnLegacyKeyTypeCollision(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second}, rdb, "vismod-test", nil)

	if err := rdb.Set(ctx, q.key("processing"), "not-a-list", 0).Err(); err != nil {
		t.Fatal(err)
	}
	err := q.Start(ctx, func(context.Context, Job) (Disposition, error) { return Ack, nil })
	if err == nil {
		_ = q.Close(ctx)
		t.Fatal("Start succeeded despite an unreadable legacy processing key")
	}
	if !strings.Contains(err.Error(), "legacy processing scan") {
		t.Errorf("error %q should name the legacy scan", err)
	}
}

func TestRedisqStartFailsWhenOwnProcessingKeyIsUnreadable(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()
	q := NewRedisq(QueueConfig{Workers: 1, DrainTimeout: time.Second}, rdb, "vismod-test", nil)

	if err := rdb.Set(ctx, q.processingKey(), "not-a-list", 0).Err(); err != nil {
		t.Fatal(err)
	}
	err := q.Start(ctx, func(context.Context, Job) (Disposition, error) { return Ack, nil })
	if err == nil {
		_ = q.Close(ctx)
		t.Fatal("Start succeeded despite an unreadable processing key")
	}
	if !strings.Contains(err.Error(), "orphan recovery") {
		t.Errorf("error %q should name orphan recovery", err)
	}
}

// A dead replica whose list cannot be drained must stay REGISTERED, so the
// next sweep retries it. Deregistering on failure would strand its jobs
// permanently — nothing else ever looks at that key.
func TestRedisqReaperKeepsInstanceWhenItsListIsUnreadable(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()

	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	dead := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)
	if err := rdb.Set(ctx, dead.processingKey(), "not-a-list", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.ZAdd(ctx, "vismod-test:instances", redis.Z{
		Score: float64(time.Now().Add(-10 * instanceReclaimAfter).UnixMilli()), Member: dead.instance,
	}).Err(); err != nil {
		t.Fatal(err)
	}

	q.reapDeadInstances(ctx)

	if _, err := rdb.ZScore(ctx, "vismod-test:instances", dead.instance).Result(); err != nil {
		t.Errorf("dead instance was deregistered despite an unreadable list: %v", err)
	}
}

// ProcessingDepth must report a failure rather than a plausible zero when a
// processing key cannot be counted — a zero here reads as "nothing stranded".
func TestRedisqProcessingDepthFailsOnKeyTypeCollision(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)

	if err := rdb.Set(ctx, q.processingKey(), "not-a-list", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ProcessingDepth(ctx); err == nil {
		t.Error("ProcessingDepth returned success over an uncountable key")
	}
}

// The legacy key is counted too, so a stranded pre-upgrade payload shows up
// on the gauge instead of being invisible.
func TestRedisqProcessingDepthCountsLegacyKey(t *testing.T) {
	rdb, _ := newMini(t)
	ctx := context.Background()
	q := NewRedisq(QueueConfig{Workers: 1}, rdb, "vismod-test", nil)

	if err := rdb.LPush(ctx, q.key("processing"), `{"job":{"id":"old"},"attempts":0}`).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.LPush(ctx, q.processingKey(), `{"job":{"id":"mine"},"attempts":0}`).Err(); err != nil {
		t.Fatal(err)
	}
	d, err := q.ProcessingDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d != 2 {
		t.Errorf("ProcessingDepth = %d, want 2 (own + legacy)", d)
	}
}
