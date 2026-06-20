// Package review implements the §G.8 potential-CSAM divert seam.
//
// v1 has NO CSAM detection: the Sexual classifier will sometimes fire on
// content that is actually CSAM. A high-severity Sexual hit is therefore NOT a
// CSAM determination but MUST be handled as potential-CSAM — the frame is
// routed to a human-review channel BEFORE Sink.Write, and the ordinary
// result/audit storage keeps only SHA-256(frame) + verdict, never the frame
// bytes or provider Raw (§G.2 transient-handling + encrypt-at-rest).
//
// A Diverter carries ONLY derived metadata (hash, timestamp, score) — never
// media bytes. v1 ships LogDiverter as the safe default (it records the event
// for an operator to action). A production deployment supplies its own Diverter
// writing to an encrypted, access-controlled review queue under §G.2 rules; the
// real PDQ/TMK hash matcher is v1.1.
package review

import (
	"context"
	"log/slog"
)

// Item is the divert payload for one potential-CSAM frame. It deliberately
// holds NO frame bytes and NO provider Raw — only derived identifiers so the
// human-review handoff never persists illegal media through this path.
type Item struct {
	JobID        string
	AssetID      string
	FrameSHA256  string
	TimestampSec *float64
	Category     string
	Score        *float64
	Reason       string
}

// Diverter routes a potential-CSAM frame to human review. Implementations MUST
// NOT persist or transmit media bytes; they receive only the Item metadata.
//
// Delivery is AT-LEAST-ONCE: the pipeline diverts before Sink.Write, and a
// retried job re-runs the divert (redelivery may hit a different worker, so the
// pipeline cannot dedup). A production Diverter MUST treat (Item.JobID,
// Item.FrameSHA256) as the idempotency key and collapse repeats — otherwise a
// transient retry enqueues the same potential-CSAM frame twice for reviewers.
type Diverter interface {
	Divert(ctx context.Context, it Item) error
}

// LogDiverter is the v1 default: it emits a structured WARN event so an
// operator is alerted to a potential-CSAM frame. It records the frame hash and
// score, never the bytes. Operators needing real CSAM coverage must supply an
// encrypted review channel and the v1.1 hash matcher — see RESPONSIBLE_USE.md.
type LogDiverter struct{ log *slog.Logger }

// NewLogDiverter builds the default log-only Diverter.
func NewLogDiverter(log *slog.Logger) *LogDiverter { return &LogDiverter{log: log} }

// Divert logs the potential-CSAM event. It never returns an error (logging is
// best-effort) so a divert never blocks the fail-safe pipeline.
func (d *LogDiverter) Divert(_ context.Context, it Item) error {
	if d == nil || d.log == nil {
		return nil
	}
	fields := []any{
		"event", "potential_csam_divert",
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
	d.log.Warn("potential-CSAM frame diverted to human review", fields...)
	return nil
}
