// Package review implements the §G.8 flagged-for-review divert seam.
//
// A frame whose category score lands in the flag band ([flag_at, block_at)) is
// flagged for manual review: the frame is routed to a human-review channel
// BEFORE Sink.Write, and the ordinary result/audit storage keeps only
// SHA-256(frame) + verdict, never the frame bytes or provider Raw (§G.2
// transient-handling + encrypt-at-rest). This is a generic flagged-frame seam,
// NOT a CSAM determination — CSAM is detected by hash-match in the Matcher
// pre-stage, not inferred from a classifier score.
//
// A Diverter carries ONLY derived metadata (hash, timestamp, score) — never
// media bytes. v1 ships LogDiverter as the safe default (it records the event
// for an operator to action). A production deployment supplies its own Diverter
// writing to an encrypted, access-controlled review queue under §G.2 rules.
package review

import (
	"context"
	"log/slog"
)

// Item is the divert payload for one flagged frame. It deliberately holds NO
// frame bytes and NO provider Raw — only derived identifiers so the human-review
// handoff never persists illegal media through this path.
type Item struct {
	JobID        string
	AssetID      string
	FrameSHA256  string
	TimestampSec *float64
	Category     string
	Score        *float64
	Reason       string
}

// Diverter routes a flagged frame to human review. Implementations MUST NOT
// persist or transmit media bytes; they receive only the Item metadata.
//
// Delivery is AT-LEAST-ONCE: the pipeline diverts before Sink.Write, and a
// retried job re-runs the divert (redelivery may hit a different worker, so the
// pipeline cannot dedup). A production Diverter MUST treat (Item.JobID,
// Item.FrameSHA256, Item.Category) as the idempotency key and collapse repeats —
// otherwise a transient retry enqueues the same flagged frame twice for
// reviewers. The Category component is load-bearing: one frame flagged in N
// categories emits N Items sharing (JobID, FrameSHA256) but differing in
// Category, so a key without Category would collapse all but one and a reviewer
// would lose every category but one.
type Diverter interface {
	Divert(ctx context.Context, it Item) error
}

// LogDiverter is the v1 default: it emits a structured WARN event so an
// operator is alerted to a flagged frame. It records the frame hash and score,
// never the bytes. A production deployment supplies an encrypted, access-
// controlled review channel — see RESPONSIBLE_USE.md.
type LogDiverter struct{ log *slog.Logger }

// NewLogDiverter builds the default log-only Diverter.
func NewLogDiverter(log *slog.Logger) *LogDiverter { return &LogDiverter{log: log} }

// Divert logs the review event. It never returns an error (logging is
// best-effort) so a divert never blocks the fail-safe pipeline.
func (d *LogDiverter) Divert(_ context.Context, it Item) error {
	if d == nil || d.log == nil {
		return nil
	}
	fields := []any{
		"event", "review_divert",
		"job_id", it.JobID,
		"frame_sha256", it.FrameSHA256,
		"category", it.Category,
		"reason", it.Reason,
	}
	// Score is an optional pointer; log the value (not the address) only when set
	// so the WARN matches the doc claim "records the frame hash and score".
	if it.Score != nil {
		fields = append(fields, "score", *it.Score)
	}
	d.log.Warn("flagged frame diverted to human review", fields...)
	return nil
}
