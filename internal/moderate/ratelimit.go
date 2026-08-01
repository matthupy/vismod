package moderate

import (
	"context"
	"sync"
	"time"
)

// Limiter is a minimal token-bucket (pacing) rate limiter. It is owned by
// the single active Moderator and SHARED across all workers and all
// per-job frame fan-out, so the aggregate request rate equals the limiter
// rate regardless of workers × frames.concurrency.
//
// Multi-replica caveat (documented in README/deploy): a per-process
// limiter × N replicas can overshoot a vendor quota by N. Either budget
// global_limit / replicas per process, or use a shared (Redis-backed)
// limiter.
type Limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

// NewLimiter allows rps requests per second; rps <= 0 disables limiting.
func NewLimiter(rps float64) *Limiter {
	if rps <= 0 {
		return &Limiter{}
	}
	return &Limiter{interval: time.Duration(float64(time.Second) / rps)}
}

// Wait blocks until the next request slot (FIFO pacing) or ctx ends.
func (l *Limiter) Wait(ctx context.Context) error {
	if l.interval == 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	wait := l.next.Sub(now)
	l.next = l.next.Add(l.interval)
	l.mu.Unlock()

	if wait <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
