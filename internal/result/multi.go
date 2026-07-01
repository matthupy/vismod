package result

import "context"

// Compile-time assertion: *MultiSink must satisfy Sink.
var _ Sink = (*MultiSink)(nil)

// MultiSink fans each envelope out to every wrapped Sink. It is the tee that
// lets the result/DLQ pipeline feed both the JSONL sink and the queryable job
// store introduced in VISMOD-6.
type MultiSink struct {
	sinks []Sink
}

// NewMultiSink returns a MultiSink that writes to each of the provided sinks in
// order. Calling Write on a zero-sink MultiSink is a no-op.
func NewMultiSink(sinks ...Sink) *MultiSink {
	return &MultiSink{sinks: sinks}
}

// Write calls Write on every wrapped Sink in order. It always attempts all
// sinks — a failure in one sink does not skip the remaining ones. The first
// non-nil error encountered is returned; subsequent errors are discarded so
// that no single slow or failing sink starves the others.
func (m *MultiSink) Write(ctx context.Context, env ResultEnvelope) error {
	var first error
	for _, s := range m.sinks {
		if err := s.Write(ctx, env); err != nil && first == nil {
			first = err
		}
	}
	return first
}
