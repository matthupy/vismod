package result

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
)

// JSONLSink writes one JSON object per line to an io.Writer (stdout or a file).
// It is idempotent per JobID: a JobID already written is skipped, so
// at-least-once redelivery never double-writes.
type JSONLSink struct {
	mu   sync.Mutex
	w    *bufio.Writer
	seen map[JobID]struct{}
}

// NewJSONLSink wraps w. The caller owns w's lifecycle (closing a file, etc.).
func NewJSONLSink(w io.Writer) *JSONLSink {
	return &JSONLSink{
		w:    bufio.NewWriter(w),
		seen: make(map[JobID]struct{}),
	}
}

// Write emits env as a JSON line. Idempotent per JobID. It does not retain any
// reference to frame files.
func (s *JSONLSink) Write(_ context.Context, env ResultEnvelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, dup := s.seen[env.JobID]; dup {
		return nil
	}

	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	if err := s.w.WriteByte('\n'); err != nil {
		return err
	}
	if err := s.w.Flush(); err != nil {
		return err
	}

	s.seen[env.JobID] = struct{}{}
	return nil
}
