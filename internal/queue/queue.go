// Package queue defines the FIFO job queue seam and a prototype in-memory
// driver (memq). The interface is rich enough that swapping memq for the
// Redis-backed asynq driver (M5) is behavior-preserving: the handler returns an
// explicit Disposition (not a bare error), which both drivers honor identically.
package queue

import (
	"context"
	"time"

	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

// Disposition is the explicit outcome a handler reports for a job. It is the
// key to a behavior-preserving driver swap: a bare error means opposite things
// on memq vs asynq, an explicit Disposition does not.
type Disposition int

const (
	// Ack: job succeeded, remove it.
	Ack Disposition = iota
	// Retry: transient failure, bounded backoff then dead-letter.
	Retry
	// DeadLetter: terminal failure, dead-letter immediately (no retry).
	DeadLetter
)

func (d Disposition) String() string {
	switch d {
	case Ack:
		return "ack"
	case Retry:
		return "retry"
	case DeadLetter:
		return "dead_letter"
	default:
		return "unknown"
	}
}

// Job is one unit of work flowing through the queue.
type Job struct {
	ID          result.JobID
	Source      moderation.Source
	SubmittedAt time.Time
	// ModelFingerprint is the boot-knowable identity of the model that enqueued
	// the job (config.ModelFingerprint, §L). The worker dead-letters a job whose
	// fingerprint != its own loaded model instead of silently moderating with the
	// wrong model under a rolling deploy. It is an opaque hash => payload-hygiene
	// safe (§D.3/§G.2: no media/PII). Empty only for a pre-feature (older-binary)
	// job, since all enqueues go through the single stamping helper.
	ModelFingerprint string
}

// Handler processes a job and reports a Disposition. A returned error is
// advisory detail attached to dead-lettered jobs; the Disposition drives
// control flow.
type Handler func(ctx context.Context, j Job) (Disposition, error)

// QueueConfig tunes a queue driver.
type QueueConfig struct {
	Workers       int
	Buffer        int
	MaxRetries    int           // bounded retry attempts before dead-letter
	RetryBackoff  time.Duration // base backoff between retries
	DrainTimeout  time.Duration // graceful-drain budget for in-flight jobs on Close
	JobTimeout    time.Duration // per-job processing timeout (0 = none)
	DeadLetterMax int           // DLQ depth cap; at capacity reject enqueues + alert
	DeadLetter    result.Sink   // where dead-lettered jobs go (exists in the prototype)
}

// DepthReporter is a Queue that also exposes DLQ depth for metrics. Both the
// memq and asynq drivers implement it, so serve can wire the depth gauges
// uniformly regardless of driver.
type DepthReporter interface {
	Queue
	DeadLetterDepth() int
}

// Pinger is implemented by drivers backed by an external dependency (the redis
// driver) so serve can validate reachability at boot and on /readyz. The memq
// driver is in-process and does not implement it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Queue is the FIFO job queue. Dequeue order == enqueue order. With >1 worker,
// completion order is NOT guaranteed (jobs finish at different speeds); strict
// end-to-end ordering needs Workers=1 or per-key serialization.
type Queue interface {
	Enqueue(ctx context.Context, j Job) (result.JobID, error)
	Start(ctx context.Context, handler Handler) error
	QueueDepth(ctx context.Context) (int, error)
	Close(ctx context.Context) error
}
