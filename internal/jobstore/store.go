// Package jobstore provides a queryable JobID→record store for the vismod
// serve pipeline. It tracks per-job status + verdict from intake through
// completion and exposes the records for the future GET /v1/jobs/{id} API.
//
// # Status monotonicity (P0)
//
// Intake is at-least-once and the worker is async, so status writes can arrive
// out of order: a fast job may Complete (→ done) BEFORE its SetPending runs.
// Every writer enforces a strict-rank rule:
//
//	pending=1, processing=2, done==dead_letter=3
//
// A write applies ONLY when its target rank is strictly greater than the rank of
// the stored record (or no record exists). Equal or lower rank ⇒ drop (no-op,
// return nil). This gives both monotonicity AND idempotency: a second Complete on
// an already-terminal record is rank 3 vs 3 ⇒ dropped ⇒ record unchanged.
//
// # Eviction can defeat monotonicity for evicted records
//
// The strict-rank guard above only protects a record that is still resident
// in the store. Once a terminal record is evicted — by TTL expiry (lazy, in
// Get) or by oldest-first capacity eviction (evictOldest) — a late
// SetPending/SetProcessing for that same JobID finds no record (ok=false)
// and RECREATES it at a lower status: a job that was already `done` can
// reappear as `pending` or `processing`. This is an inherent limitation of a
// bounded/TTL-expiring store, not a bug in the rank rule itself, and it is
// NOT covered by the monotonicity guarantee above. The same hole applies to
// the future Redis driver's key `EXPIRE`: once a key expires, a late status
// write recreates it with no memory of the prior terminal state.
package jobstore

import (
	"context"
	"time"

	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// JobStatus is the lifecycle status of a moderation job.
type JobStatus string

const (
	StatusPending    JobStatus = "pending"
	StatusProcessing JobStatus = "processing"
	StatusDone       JobStatus = "done"
	StatusDeadLetter JobStatus = "dead_letter"
)

// statusRank returns the monotonic rank for a JobStatus. Higher rank = more
// advanced. pending=1, processing=2, done==dead_letter=3.
func statusRank(s JobStatus) int {
	switch s {
	case StatusPending:
		return 1
	case StatusProcessing:
		return 2
	case StatusDone, StatusDeadLetter:
		return 3
	default:
		return 0
	}
}

// JobRecord is the payload-hygiene-safe per-job record stored in the job store.
// It contains only scalar IDs, source ref, timestamps, worker identity, and
// OverallVerdict scalars. NEVER frames, Raw, OCR, or caption data.
//
// Nullable verdict scalars (Verdict, Flagged, TopCategory, MaxScore, Confidence)
// and nullable timestamps (StartedAt, FinishedAt) are pointers WITHOUT omitempty
// so they serialize to explicit JSON null when unset — never 0, never absent key.
type JobRecord struct {
	JobID  result.JobID      `json:"job_id"`
	Status JobStatus         `json:"status"`
	Source moderation.Source `json:"source"`
	// WorkerID is set by the first worker to reach processing for this job and
	// is NOT updated by later redeliveries (the strict-rank monotonicity rule
	// drops a second SetProcessing once a record is already at rank
	// processing-or-higher). Do not treat WorkerID as "the worker currently
	// running this job" — on a redelivered/retried job it may name a dead or
	// long-finished worker.
	WorkerID    string               `json:"worker_id,omitempty"`
	SubmittedAt time.Time            `json:"submitted_at"`
	StartedAt   *time.Time           `json:"started_at"`
	FinishedAt  *time.Time           `json:"finished_at"`
	Verdict     *moderation.Verdict  `json:"verdict"`
	Flagged     *bool                `json:"flagged"`
	TopCategory *moderation.Category `json:"top_category"`
	MaxScore    *float64             `json:"max_score"`
	Confidence  *float64             `json:"confidence"`
	Error       string               `json:"error,omitempty"`
}

// JobStore is the queryable per-job status and verdict store. All writers are
// MONOTONIC: status only advances pending→processing→{done|dead_letter} and
// never regresses.
//
// SetPending and SetProcessing MUST no-op (return nil, not error) when a
// higher/terminal status already exists — at-least-once + async worker means
// Complete can race ahead of SetPending/SetProcessing.
type JobStore interface {
	// SetPending records a newly submitted job. No-ops (nil) if a record with
	// equal or higher rank already exists.
	SetPending(ctx context.Context, id result.JobID, src moderation.Source, submittedAt time.Time) error

	// SetProcessing advances a job to processing state, recording the worker ID
	// and when it started. No-ops (nil) if a record with equal or higher rank
	// already exists.
	SetProcessing(ctx context.Context, id result.JobID, workerID string, startedAt time.Time) error

	// Complete transitions a job to done or dead_letter and records the verdict
	// and timing from env. Idempotent per JobID: replaying a terminal envelope
	// MUST leave the record unchanged (rank 3 vs 3 ⇒ dropped).
	Complete(ctx context.Context, env result.ResultEnvelope) error

	// Get retrieves the record for id. Returns found=false when the record does
	// not exist (never submitted, or evicted by TTL/max-entries bounds).
	Get(ctx context.Context, id result.JobID) (JobRecord, bool, error)
}
