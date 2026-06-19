// Package frames defines the FrameSource seam: how the pipeline obtains the
// frames of a video for per-image moderation. The pipeline owns its own Frame
// type and MUST NOT embed or alias videosift types (that would re-leak the
// quarantined dependency through the public seam).
//
// The videosift-backed implementation lands in M2. v1/M0 ships the interface
// plus a fake for tests.
package frames

import "context"

// Frame is a pipeline-owned reference to one extracted PNG on disk.
type Frame struct {
	Index        int
	TimestampSec float64
	Path         string // absolute path to a PNG
}

// FrameSource extracts the frames of a video. The returned cleanup deletes the
// working directory that holds the PNGs.
//
// LIFECYCLE CONTRACT: the caller MUST `defer cleanup()` immediately after
// Frames returns (before any fan-out) so the working dir is deleted on every
// exit path — error, ctx-cancel, panic. cleanup must be idempotent; its error
// is logged but does not change the verdict.
type FrameSource interface {
	Frames(ctx context.Context, videoPath string) (frames []Frame, cleanup func() error, err error)
}
