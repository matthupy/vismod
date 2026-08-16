package result

import "errors"

// ErrDeliveryFailed marks an envelope that reached NONE of the bytes of
// its destination — the retry budget ran out, the receiver rejected the
// payload, or the local write failed.
//
// It exists so a caller can ask "did this destination get the result?"
// with errors.Is instead of matching on message text. Every Sink in this
// package wraps it, so the question has the same answer regardless of
// transport.
//
// It is deliberately NOT a substitute for the retry classification in
// moderate.DoJSON: whether the failure is worth retrying at the QUEUE
// level is still decided by moderation.IsRetryable, which this sentinel
// leaves intact by wrapping rather than replacing.
var ErrDeliveryFailed = errors.New("result: delivered nothing")

// Bounded reasons for a delivery failure. These reach log fields, so they
// are a closed set — an error string would be unbounded and could carry a
// webhook URL, which for Discord is a credential.
const (
	deliveryReasonExhausted = "exhausted" // retry budget ran out
	deliveryReasonRejected  = "rejected"  // receiver refused; retrying will not help
	deliveryReasonFormat    = "format"    // the envelope could not be rendered
	deliveryReasonWrite     = "write"     // local write failed
)
