package result

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// FileSink appends one JSON line per envelope to a file.
//
// Opening happens at construction so an unwritable path is a BOOT error,
// not a surprise on the first verdict.
//
// One process per file. Two replicas appending to one file interleave in
// exactly the way the audit log does — compose gives each replica its own
// volume for that reason, and the same applies here.
//
// KNOWN GAP: idempotency is per PROCESS, not durable. Unlike audit.Open,
// which replays its file and rebuilds the seen-set before appending,
// this sink opens O_APPEND with an empty dedupe. A job redelivered after
// a restart (webhook fails -> MultiSink errors -> job re-queued -> pod
// restarts) is appended a SECOND time with the same job_id. Consumers
// must dedupe on job_id. Replay-on-open would close this and is the
// obvious fix, but it is a design change (cost of reading the whole file
// at boot, unbounded seen-set) and has not been taken.
type FileSink struct {
	mu sync.Mutex // serializes appends so concurrent lines never interleave
	f  *os.File
	d  dedupe
}

func NewFileSink(path string) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("result: open file sink %s: %w", path, err)
	}
	return &FileSink{f: f}, nil
}

func (s *FileSink) Write(_ context.Context, env ResultEnvelope) error {
	if !s.d.Claim(env.JobID) {
		return nil // idempotent per JobID
	}
	b, err := json.Marshal(env)
	if err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: marshal envelope: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: write envelope: %w", err)
	}
	return nil
}

func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

var _ Sink = (*FileSink)(nil)
