package jobstore

import (
	"context"
	"log/slog"
	"time"

	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// recordFromEnvelope builds the terminal-state fields of a JobRecord from env.
//
// Status is dead_letter when env.Error != "" OR env.Result == nil; otherwise done.
//
// StartedAt and FinishedAt are parsed from RFC3339 strings into *time.Time.
// An unparseable non-empty value leaves that field nil and logs once at WARN
// (carrying job_id and field name ONLY — never media bytes, Raw, or captions).
// An empty string yields nil with no log.
//
// Verdict scalars are populated from env.Result.Overall when env.Result != nil;
// on dead_letter all five verdict fields are left nil and Error is set.
//
// This is the sole RFC3339 parsing site in the jobstore package. The Redis driver
// (WI-2) and any future driver MUST call this helper rather than re-implement it.
func recordFromEnvelope(env result.ResultEnvelope) JobRecord {
	rec := JobRecord{
		JobID:  env.JobID,
		Source: env.Source,
	}

	// Determine terminal status.
	if env.Error != "" || env.Result == nil {
		rec.Status = StatusDeadLetter
		rec.Error = env.Error
	} else {
		rec.Status = StatusDone
	}

	// Parse RFC3339 timestamps — the sole parsing site.
	rec.StartedAt = parseRFC3339(env.JobID, "started_at", env.StartedAt)
	rec.FinishedAt = parseRFC3339(env.JobID, "finished_at", env.FinishedAt)

	// Populate verdict scalars from Overall — only on success path.
	if env.Result != nil {
		ov := env.Result.Overall
		rec.Verdict = moderation.Ptr(ov.Verdict)
		rec.Flagged = moderation.Ptr(ov.Flagged)
		rec.TopCategory = ov.TopCategory // already *Category
		rec.MaxScore = ov.MaxScore       // already *float64
		rec.Confidence = ov.Confidence   // already *float64
	}

	return rec
}

// parseRFC3339 parses s as RFC3339 UTC. Returns nil on empty string (no log).
// Returns nil on parse error and logs a WARN carrying job_id and fieldName only.
func parseRFC3339(id result.JobID, fieldName, s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		slog.Warn("jobstore: unparseable timestamp field",
			"job_id", id,
			"field", fieldName,
		)
		return nil
	}
	t = t.UTC()
	return &t
}

// StoreSink is a result.Sink adapter that tees each pipeline result envelope
// into a JobStore via Complete. Because Complete is idempotent (at-least-once
// rank rule) redelivery is safe.
type StoreSink struct {
	store JobStore
}

// NewStoreSink returns a StoreSink backed by store.
func NewStoreSink(store JobStore) *StoreSink {
	return &StoreSink{store: store}
}

// Write calls store.Complete with env. It is idempotent per JobID.
func (s *StoreSink) Write(ctx context.Context, env result.ResultEnvelope) error {
	return s.store.Complete(ctx, env)
}

// compile-time assertion: *StoreSink must satisfy result.Sink.
var _ result.Sink = (*StoreSink)(nil)
