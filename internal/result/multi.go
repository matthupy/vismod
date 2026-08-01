package result

import (
	"context"
	"fmt"
)

// MultiSink fans one envelope out to every configured sink.
//
// Failure policy: EVERY sink is attempted and the FIRST error is
// returned. Not fail-fast — a webhook outage must never suppress the
// local JSONL record. The returned error reaches the pipeline's queue
// Retry disposition, so the job redelivers; the sinks that already
// succeeded are no-ops on the second pass because each is idempotent per
// JobID.
type MultiSink struct {
	sinks  []Sink
	names  []string
	onFail func(sinkType string)
}

// NewMultiSink pairs each sink with its config type name (used for error
// messages and the failure metric). names must be the same length as
// sinks — a mismatch is an error, not a tolerated defect, because the
// name is what tells an operator WHICH destination failed during an
// outage. onFail may be nil.
func NewMultiSink(sinks []Sink, names []string, onFail func(sinkType string)) (*MultiSink, error) {
	if len(sinks) != len(names) {
		return nil, fmt.Errorf("result: MultiSink needs one name per sink, got %d sinks and %d names", len(sinks), len(names))
	}
	return &MultiSink{sinks: sinks, names: names, onFail: onFail}, nil
}

func (m *MultiSink) Write(ctx context.Context, env ResultEnvelope) error {
	var firstErr error
	for i, s := range m.sinks {
		if err := s.Write(ctx, env); err != nil {
			// Indexing is safe without a bounds fallback: NewMultiSink is
			// the only constructor and it refuses a length mismatch.
			if m.onFail != nil {
				m.onFail(m.names[i])
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("result: sink %s: %w", m.names[i], err)
			}
		}
	}
	return firstErr
}

var _ Sink = (*MultiSink)(nil)
