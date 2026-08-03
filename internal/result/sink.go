// Package result defines the result envelope and Sink implementations.
package result

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// ModelIdentity is stamped on every job so a verdict can be audited back
// to the exact adapter, model version, and verdict-affecting config.
type ModelIdentity struct {
	Adapter      string `json:"adapter"`
	ModelVersion string `json:"model_version"`
	ConfigHash   string `json:"config_hash"`
}

// ResultEnvelope is the per-job output record.
type ResultEnvelope struct {
	JobID   queue.JobID                  `json:"job_id"`
	Source  moderation.Source            `json:"source"`
	ModelID ModelIdentity                `json:"model_id"`
	Result  *moderation.NormalizedResult `json:"result,omitempty"`
	Error   string                       `json:"error,omitempty"`
	// Metadata is the opaque caller-supplied JSON from queue.Job, passed
	// through untouched. vismod never interprets it and it never
	// influences a verdict. Omitted when the caller supplied none.
	// Present in sink output ONLY — never in the audit log, log lines,
	// or the operator UI.
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
}

// Sink receives result envelopes. Write MUST be idempotent per JobID:
// at-least-once queue delivery means the same job may be redelivered, and
// redelivery must never double-write.
type Sink interface {
	Write(ctx context.Context, env ResultEnvelope) error
}

// JSONLSink writes one JSON line per envelope to an io.Writer (stdout or a
// file). Idempotency is per-process: dedupe suppresses duplicate writes
// within this process's lifetime. There is NO cross-restart dedupe —
// redisq acks but keeps no durable completion marker
// (TestRedisqRedeliveryIsDedupedDownstream processes the same JobID
// twice on purpose), so a redelivery after a restart writes a second
// line. Only the audit log survives a restart, by replaying its file in
// audit.Open. Consumers dedupe on job_id; see README.
type JSONLSink struct {
	mu sync.Mutex // serializes writes to w
	w  io.Writer
	d  dedupe
}

func NewJSONLSink(w io.Writer) *JSONLSink {
	return &JSONLSink{w: w}
}

func (s *JSONLSink) Write(_ context.Context, env ResultEnvelope) error {
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
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		s.d.Release(env.JobID)
		return fmt.Errorf("result: write envelope: %w", err)
	}
	return nil
}

var _ Sink = (*JSONLSink)(nil)
