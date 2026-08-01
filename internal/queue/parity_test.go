package queue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The behavior-preserving-swap suite: the same handler Disposition must
// produce the same retry/DLQ outcome on memq and redisq (§D.6).

type driverCase struct {
	name  string
	build func(t *testing.T, cfg QueueConfig) Queue
	dlq   func(q Queue) DeadLetterSink
}

func drivers() []driverCase {
	return []driverCase{
		{
			name: "memq",
			build: func(_ *testing.T, cfg QueueConfig) Queue {
				return NewMemq(cfg, nil)
			},
			dlq: func(q Queue) DeadLetterSink { return q.(*Memq).DLQ() },
		},
		{
			name: "redisq",
			build: func(t *testing.T, cfg QueueConfig) Queue {
				mr := miniredis.RunT(t)
				rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
				t.Cleanup(func() { _ = rdb.Close() })
				return NewRedisq(cfg, rdb, "vismod-test", nil)
			},
			dlq: func(q Queue) DeadLetterSink { return q.(*Redisq).DLQ() },
		},
	}
}

func dlqDepth(t *testing.T, s DeadLetterSink) int {
	t.Helper()
	n, err := s.Depth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestParityFIFO(t *testing.T) {
	for _, d := range drivers() {
		t.Run(d.name, func(t *testing.T) {
			q := d.build(t, QueueConfig{Workers: 1, Buffer: 100})
			var mu sync.Mutex
			var got []string
			ids := []string{"z-9", "a-1", "m-5", "b-2"} // lexicographic != arrival
			for _, id := range ids {
				if _, err := q.Enqueue(context.Background(), job(id)); err != nil {
					t.Fatal(err)
				}
			}
			_ = q.Start(context.Background(), func(_ context.Context, j Job) (Disposition, error) {
				mu.Lock()
				got = append(got, string(j.ID))
				mu.Unlock()
				return Ack, nil
			})
			waitFor(t, 3*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == len(ids) }, "all processed")
			_ = q.Close(context.Background())
			for i := range ids {
				if got[i] != ids[i] {
					t.Fatalf("FIFO violated on %s: got %v want %v", d.name, got, ids)
				}
			}
		})
	}
}

func TestParityRetryThenDLQ(t *testing.T) {
	for _, d := range drivers() {
		t.Run(d.name, func(t *testing.T) {
			q := d.build(t, QueueConfig{Workers: 2, MaxRetries: 2, RetryBackoff: 10 * time.Millisecond})
			var attempts atomic.Int32
			_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
				attempts.Add(1)
				return Retry, errors.New("transient")
			})
			_, _ = q.Enqueue(context.Background(), job("j1"))
			waitFor(t, 5*time.Second, func() bool { return dlqDepth(t, d.dlq(q)) == 1 }, "dead-lettered after bounded retries")
			_ = q.Close(context.Background())
			if got := attempts.Load(); got != 3 {
				t.Errorf("%s: attempts = %d, want 3 (1 + MaxRetries)", d.name, got)
			}
		})
	}
}

func TestParityDeadLetterImmediate(t *testing.T) {
	for _, d := range drivers() {
		t.Run(d.name, func(t *testing.T) {
			q := d.build(t, QueueConfig{Workers: 1})
			var attempts atomic.Int32
			_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
				attempts.Add(1)
				return DeadLetter, errors.New("terminal")
			})
			_, _ = q.Enqueue(context.Background(), job("j1"))
			waitFor(t, 3*time.Second, func() bool { return dlqDepth(t, d.dlq(q)) == 1 }, "immediate DLQ")
			_ = q.Close(context.Background())
			if attempts.Load() != 1 {
				t.Errorf("%s: DeadLetter must not retry (attempts=%d)", d.name, attempts.Load())
			}
		})
	}
}

func TestParityPanicDeadLettersPoolSurvives(t *testing.T) {
	for _, d := range drivers() {
		t.Run(d.name, func(t *testing.T) {
			q := d.build(t, QueueConfig{Workers: 1})
			var ok atomic.Int32
			_ = q.Start(context.Background(), func(_ context.Context, j Job) (Disposition, error) {
				if j.ID == "boom" {
					panic("kaboom")
				}
				ok.Add(1)
				return Ack, nil
			})
			_, _ = q.Enqueue(context.Background(), job("boom"))
			_, _ = q.Enqueue(context.Background(), job("after"))
			waitFor(t, 3*time.Second, func() bool { return ok.Load() == 1 && dlqDepth(t, d.dlq(q)) == 1 }, "panic dead-lettered, pool survived")
			_ = q.Close(context.Background())
		})
	}
}

func TestParityDLQCapRejectsEnqueue(t *testing.T) {
	for _, d := range drivers() {
		t.Run(d.name, func(t *testing.T) {
			q := d.build(t, QueueConfig{Workers: 1, DeadLetterMax: 1})
			_ = q.Start(context.Background(), func(context.Context, Job) (Disposition, error) {
				return DeadLetter, errors.New("bad")
			})
			_, _ = q.Enqueue(context.Background(), job("j1"))
			waitFor(t, 3*time.Second, func() bool { return dlqDepth(t, d.dlq(q)) == 1 }, "first dead-lettered")
			_, err := q.Enqueue(context.Background(), job("j2"))
			if !errors.Is(err, ErrDeadLetterFull) {
				t.Errorf("%s: want ErrDeadLetterFull, got %v", d.name, err)
			}
			_ = q.Close(context.Background())
		})
	}
}

func TestParityQueueDepth(t *testing.T) {
	for _, d := range drivers() {
		t.Run(d.name, func(t *testing.T) {
			q := d.build(t, QueueConfig{Workers: 1, Buffer: 100})
			for i := range 5 {
				_, _ = q.Enqueue(context.Background(), job(string(rune('a'+i))))
			}
			depth, err := q.QueueDepth(context.Background())
			if err != nil || depth != 5 {
				t.Errorf("%s: depth = %d (%v), want 5", d.name, depth, err)
			}
			_ = q.Close(context.Background())
		})
	}
}
