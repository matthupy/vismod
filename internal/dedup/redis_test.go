package dedup

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestDeduper(t *testing.T, ttl time.Duration) (*RedisDeduper, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisDeduper(client, ttl), mr
}

func TestRedisDeduperDoneAfterCommit(t *testing.T) {
	d, _ := newTestDeduper(t, time.Hour)
	ctx := context.Background()

	if done, err := d.Done(ctx, "job-1"); err != nil || done {
		t.Fatalf("fresh id must be not-done: done=%v err=%v", done, err)
	}
	if err := d.Commit(ctx, "job-1"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if done, err := d.Done(ctx, "job-1"); err != nil || !done {
		t.Fatalf("committed id must be done: done=%v err=%v", done, err)
	}
}

func TestRedisDeduperCommitIdempotent(t *testing.T) {
	d, _ := newTestDeduper(t, time.Hour)
	ctx := context.Background()
	if err := d.Commit(ctx, "job-1"); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if err := d.Commit(ctx, "job-1"); err != nil {
		t.Fatalf("second commit must be a no-op, got: %v", err)
	}
}

func TestRedisDeduperTTLExpires(t *testing.T) {
	d, mr := newTestDeduper(t, time.Hour)
	ctx := context.Background()
	if err := d.Commit(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(2 * time.Hour)
	if done, err := d.Done(ctx, "job-1"); err != nil || done {
		t.Fatalf("after TTL the claim must expire: done=%v err=%v", done, err)
	}
}

func TestRedisDeduperRedisDownErrors(t *testing.T) {
	d, mr := newTestDeduper(t, time.Hour)
	ctx := context.Background()
	mr.Close() // redis unreachable

	if _, err := d.Done(ctx, "job-1"); err == nil {
		t.Fatal("Done must surface a redis error, not swallow it (fail-safe)")
	}
	if err := d.Commit(ctx, "job-1"); err == nil {
		t.Fatal("Commit must surface a redis error, not swallow it (fail-safe)")
	}
}
