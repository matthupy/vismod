package moderate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestLimiterPacesRequests: the limiter exists to stay BELOW a vendor quota,
// so N requests at R rps must take at least (N-1)/R. The first slot is free.
func TestLimiterPacesRequests(t *testing.T) {
	l := NewLimiter(50) // 20ms apart
	ctx := context.Background()

	start := time.Now()
	for range 4 {
		if err := l.Wait(ctx); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed < 55*time.Millisecond {
		t.Errorf("4 requests at 50rps took %v, want >= ~60ms; pacing is not being applied", elapsed)
	}
}

// TestLimiterIsSharedAcrossGoroutines pins the property the whole design
// rests on: the aggregate rate is the limiter's rate regardless of
// workers × frames.concurrency. A per-goroutine limiter would multiply the
// vendor request rate by the fan-out width.
func TestLimiterIsSharedAcrossGoroutines(t *testing.T) {
	l := NewLimiter(100) // 10ms apart
	ctx := context.Background()

	start := time.Now()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := l.Wait(ctx); err != nil {
				t.Errorf("Wait: %v", err)
			}
		})
	}
	wg.Wait()

	// 8 slots at 10ms apart: the last one cannot land before ~70ms even
	// though all eight goroutines asked at once.
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Errorf("8 concurrent waits at 100rps completed in %v; the limiter is not shared", elapsed)
	}
}

// TestLimiterDisabledWhenRateNonPositive: rps <= 0 means "no limiting", the
// documented opt-out. It must not block and must not panic on a zero
// interval.
func TestLimiterDisabledWhenRateNonPositive(t *testing.T) {
	for _, rps := range []float64{0, -1} {
		l := NewLimiter(rps)
		start := time.Now()
		for range 100 {
			if err := l.Wait(context.Background()); err != nil {
				t.Fatalf("rps=%v Wait: %v", rps, err)
			}
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("rps=%v: 100 waits took %v, want ~0 (limiting disabled)", rps, elapsed)
		}
	}
}

// TestLimiterHonorsContext: the limiter sits inside the per-attempt request
// builder, so a shutdown or job timeout must unblock a queued waiter instead
// of holding a worker for the rest of the pacing schedule.
func TestLimiterHonorsContext(t *testing.T) {
	l := NewLimiter(1) // one per second
	ctx, cancel := context.WithCancel(context.Background())

	if err := l.Wait(ctx); err != nil { // takes the free first slot
		t.Fatalf("first Wait: %v", err)
	}

	cancel()
	start := time.Now()
	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait after cancel = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("cancelled Wait blocked for %v; it must return promptly", elapsed)
	}
}

// TestDisabledLimiterReportsContextError: even with limiting off, a
// cancelled context is still a cancelled context — callers check this error
// before spending a vendor request.
func TestDisabledLimiterReportsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewLimiter(0).Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("disabled limiter Wait = %v, want context.Canceled", err)
	}
}
