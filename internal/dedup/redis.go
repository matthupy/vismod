// Package dedup provides durable, cross-process once-only job recording for the
// at-least-once redis/asynq queue driver (M5 §L, issue #9).
//
// The pipeline's in-memory Sink/audit `seen` maps dedupe only within one live
// process. Under at-least-once redelivery a job that finished its Sink+audit
// writes but died before the asynq ack is redelivered to a fresh process or a
// second replica, where those maps start empty — double-writing a result line
// and an audit-chain seq. A RedisDeduper centralizes the "this job is recorded"
// fact in Redis so the dedup check survives restart and spans replicas.
package dedup

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyPrefix namespaces the dedup keys. The JobID is an opaque ID — never media,
// Raw text or PII (§G.2 payload hygiene).
const keyPrefix = "vismod:done:"

// RedisDeduper records committed JobIDs in Redis with a bounded TTL.
//
// ORDERING: the pipeline checks Done BEFORE writing and calls Commit AFTER the
// Sink+audit writes succeed (write-then-commit). This is fail-safe — a crash
// before Commit redelivers and redoes the job (never a silent loss). The only
// residual duplicate window is a crash strictly between the writes and Commit,
// which is far narrower than the status quo (every fresh-process redelivery
// double-writes today).
type RedisDeduper struct {
	client *redis.Client
	ttl    time.Duration
}

// NewRedisDeduper wraps client. The caller owns the client's lifecycle. ttl
// bounds key growth and MUST exceed the maximum redelivery window (asynq
// retention + retry backoff budget).
func NewRedisDeduper(client *redis.Client, ttl time.Duration) *RedisDeduper {
	return &RedisDeduper{client: client, ttl: ttl}
}

func key(jobID string) string { return keyPrefix + jobID }

// Done reports whether jobID has already been durably committed. A Redis error
// is surfaced (never swallowed) so the caller can fail safe — retry/dead-letter
// rather than risk skipping or double-writing on an unknown state.
func (d *RedisDeduper) Done(ctx context.Context, jobID string) (bool, error) {
	n, err := d.client.Exists(ctx, key(jobID)).Result()
	if err != nil {
		return false, fmt.Errorf("dedup: done check: %w", err)
	}
	return n > 0, nil
}

// Commit durably marks jobID recorded. Idempotent: a repeat Commit (SET NX) is a
// no-op and not an error, so retrying the commit step is safe.
func (d *RedisDeduper) Commit(ctx context.Context, jobID string) error {
	if err := d.client.SetNX(ctx, key(jobID), 1, d.ttl).Err(); err != nil {
		return fmt.Errorf("dedup: commit: %w", err)
	}
	return nil
}
