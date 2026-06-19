// Package result defines the output envelope and the Sink interface that
// receives every job's decision. Sinks MUST be idempotent per JobID so that
// at-least-once redelivery (asynq, M5) never double-writes.
package result

import (
	"context"

	"github.com/matthupy/vismod/pkg/moderation"
)

// JobID uniquely identifies a job through the queue, pipeline, sink and audit.
type JobID string

// ModelIdentity binds a decision to the exact model + threshold config that
// produced it. ConfigHash is a SHA-256 over the canonicalized verdict-affecting
// config (adapter name + ModelVersion + resolved threshold map; secrets/addrs
// excluded). Stamped on every envelope and audit record for reproducibility.
type ModelIdentity struct {
	Adapter      string `json:"adapter"`
	ModelVersion string `json:"model_version"`
	ConfigHash   string `json:"config_hash"`
}

// ResultEnvelope is one job's full output record. Result is nil on a hard
// error (Error set instead).
type ResultEnvelope struct {
	JobID      JobID                        `json:"job_id"`
	Source     moderation.Source            `json:"source"`
	ModelID    ModelIdentity                `json:"model_id"`
	Result     *moderation.NormalizedResult `json:"result,omitempty"`
	Error      string                       `json:"error,omitempty"`
	StartedAt  string                       `json:"started_at"`  // RFC3339 UTC
	FinishedAt string                       `json:"finished_at"` // RFC3339 UTC
}

// Sink receives result envelopes. Implementations MUST be idempotent per
// JobID: re-writing a completed JobID must not produce a duplicate output.
type Sink interface {
	Write(ctx context.Context, env ResultEnvelope) error
}
