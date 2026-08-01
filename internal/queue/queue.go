// Package queue defines the FIFO job queue contract and its drivers.
//
// FIFO is a property of the queue: dequeue order = enqueue order. The
// pending set is never ordered by a sortable key (UUID/job-ID string) —
// lexicographic ordering silently starves jobs past the pivot. Drivers
// must use an insertion-ordered structure or a monotonic enqueue sequence.
//
// Start order != completion order: FIFO governs dequeue/start only. With
// more than one worker, completion order is not guaranteed. Strict
// end-to-end ordering needs workers=1 or per-key serialization.
package queue

import (
	"context"
	"errors"
	"time"

	"github.com/vismod/vismod/pkg/moderation"
)

// Disposition is the explicit handler outcome. Both drivers honor the same
// Disposition identically (behavior-preserving swap):
// Ack -> success; Retry -> bounded backoff then DLQ; DeadLetter -> DLQ now.
type Disposition int

const (
	Ack Disposition = iota
	Retry
	DeadLetter
)

// JobID identifies a job. At-least-once delivery (redisq) means consumers
// (Sink, audit) must be idempotent per JobID.
type JobID string

// Job is a queued unit of work. Payloads carry opaque IDs/refs, never
// media bytes.
type Job struct {
	ID     JobID             `json:"id"`
	Source moderation.Source `json:"source"`
	// Workflows optionally names the FFmpeg extraction workflows to run
	// for a video job (in order; frames are the union). Empty means the
	// configured default workflow. Names must exist in the validated
	// ffmpeg.workflows set — validated at intake, never trusted at
	// execution time alone.
	Workflows []string `json:"workflows,omitempty"`
	// DedupThreshold optionally overrides the frames.dedup config for
	// this job: nil inherits the config; 0..64 enables dedup at that
	// Hamming distance (even when disabled globally); a negative value
	// disables dedup for this job.
	DedupThreshold *int      `json:"dedup_threshold,omitempty"`
	SubmittedAt    time.Time `json:"submitted_at"`
}

// Handler processes one job and reports its Disposition. A non-nil error
// accompanies Retry/DeadLetter for logging; it never changes the mapping.
type Handler func(ctx context.Context, j Job) (Disposition, error)

// Queue is the driver contract. Swapping drivers via config must be
// behavior-preserving with zero call-site changes.
type Queue interface {
	Enqueue(ctx context.Context, j Job) (JobID, error)
	// Start launches the fixed worker pool and returns. Cancelling ctx
	// stops pulling new work (it does not cancel in-flight jobs; per-job
	// timeouts do that).
	Start(ctx context.Context, handler Handler) error
	// QueueDepth is uniform across drivers and drives autoscaling
	// (exported as the vismod_queue_depth metric).
	QueueDepth(ctx context.Context) (int, error)
	// Close gracefully drains: stop enqueues, stop pulling, give in-flight
	// jobs the drain budget, never ack jobs that did not finish.
	Close(ctx context.Context) error
}

// DeadLetterEntry is a dead-lettered job plus why it died.
type DeadLetterEntry struct {
	Job      Job       `json:"job"`
	Reason   string    `json:"reason"`
	Attempts int       `json:"attempts"`
	At       time.Time `json:"at"`
}

// DeadLetterSink receives dead-lettered jobs. It must exist in the
// prototype: a dead-lettered job is never silently dropped.
type DeadLetterSink interface {
	Write(ctx context.Context, e DeadLetterEntry) error
	Depth(ctx context.Context) (int, error)
}

// QueueConfig is shared driver tuning.
type QueueConfig struct {
	Workers       int           // worker goroutines per replica (fixed pool)
	Buffer        int           // pending buffer size (memq)
	MaxRetries    int           // Retry attempts before DLQ
	RetryBackoff  time.Duration // base backoff between retries
	DrainTimeout  time.Duration // graceful-drain budget for in-flight jobs
	JobTimeout    time.Duration // per-job processing timeout
	DeadLetterMax int           // DLQ depth cap; at capacity reject enqueues + alert
	DeadLetter    DeadLetterSink
}

func (c *QueueConfig) applyDefaults() {
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.Buffer <= 0 {
		c.Buffer = 1024
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = 2 * time.Second
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 30 * time.Second
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = 5 * time.Minute
	}
	if c.DeadLetterMax <= 0 {
		c.DeadLetterMax = 1000
	}
}

// ErrQueueClosed is returned by Enqueue after Close.
var ErrQueueClosed = errors.New("queue: closed")

// ErrDeadLetterFull is returned by Enqueue while the DLQ is at capacity:
// new work is rejected (retryable signal to producers) rather than risking
// unrecorded failures.
var ErrDeadLetterFull = errors.New("queue: dead-letter queue at capacity; rejecting new work")

// ErrQueueFull is returned by memq when the pending buffer is full.
var ErrQueueFull = errors.New("queue: pending buffer full")
