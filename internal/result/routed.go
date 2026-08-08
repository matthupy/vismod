package result

import "context"

// RoutedSink applies a Predicate in front of a Sink, so "which results go
// here" is config rather than a bespoke Sink implementation per rule.
//
// A predicate MISS returns nil, not an error. A miss is a routing
// decision, not a delivery failure: MultiSink turns the first error into
// the queue's Retry disposition, which re-runs the whole job including a
// BILLED vendor call, so treating "this sink didn't want it" as failure
// would bill an operator for every allow verdict.
//
// A RoutedSink can only ever NARROW delivery. A set of predicates that
// collectively misses an envelope routes it nowhere, which is a silent
// drop of exactly the kind this project exists to prevent — so
// config.validateOutput requires at least one sink with no predicate.
type RoutedSink struct {
	inner Sink
	pred  Predicate
}

// NewRoutedSink wraps inner. A zero Predicate matches everything, so
// wrapping is always safe.
func NewRoutedSink(inner Sink, pred Predicate) *RoutedSink {
	return &RoutedSink{inner: inner, pred: pred}
}

func (r *RoutedSink) Write(ctx context.Context, env ResultEnvelope) error {
	if !r.pred.Match(env) {
		return nil
	}
	return r.inner.Write(ctx, env)
}

var _ Sink = (*RoutedSink)(nil)
