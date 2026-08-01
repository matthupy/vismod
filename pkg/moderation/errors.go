package moderation

import (
	"errors"
	"fmt"
)

// errRetryable is the sentinel wrapped by Retryable.
var errRetryable = errors.New("retryable")

// Retryable marks err as transient (429, 5xx, timeouts, transient network
// failure). Retryable errors get bounded backoff and then dead-letter;
// terminal errors (4xx validation, unsupported/oversize media) fail without
// retry. Either way the fail-safe policy applies: never "allow" on error.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", errRetryable, err)
}

// IsRetryable reports whether err was marked with Retryable.
func IsRetryable(err error) bool {
	return errors.Is(err, errRetryable)
}
