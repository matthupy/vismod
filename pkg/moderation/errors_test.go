package moderation

import (
	"errors"
	"fmt"
	"testing"
)

// TestRetryableRoundTrip: the retryable mark is what separates "back off and
// retry" from "fail now". Both directions matter — a lost mark turns a
// transient 429 into a terminal failure, and a false positive burns retries
// (and vendor spend) on a request that can never succeed.
func TestRetryableRoundTrip(t *testing.T) {
	base := errors.New("provider returned 503")
	marked := Retryable(base)

	if !IsRetryable(marked) {
		t.Fatal("Retryable(err) is not reported as retryable")
	}
	if !errors.Is(marked, base) {
		t.Error("Retryable dropped the underlying error; callers lose the cause")
	}
	if IsRetryable(base) {
		t.Error("an unmarked error must not be retryable")
	}
}

// TestRetryableNilStaysNil: Retryable is applied on paths that may not have
// failed. Marking a nil error would invent a failure out of a success.
func TestRetryableNilStaysNil(t *testing.T) {
	if err := Retryable(nil); err != nil {
		t.Errorf("Retryable(nil) = %v, want nil", err)
	}
	if IsRetryable(nil) {
		t.Error("IsRetryable(nil) must be false")
	}
}

// TestIsRetryableThroughWrapping: DoJSON marks the error and callers wrap it
// again with context on the way up. The mark must survive that wrapping or
// the fail-safe classification is decided by how many %w's it passed through.
func TestIsRetryableThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("frame 3: %w", fmt.Errorf("analyze: %w", Retryable(errors.New("timeout"))))
	if !IsRetryable(wrapped) {
		t.Error("retryable mark lost through two layers of wrapping")
	}
}
