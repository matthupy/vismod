// Package frames extracts still frames from video for image-only
// moderators. The real implementation (FFmpegSource) shells out to
// ffmpeg/ffprobe via os/exec — never a shell — with configurable,
// guardrailed workflows.
package frames

import "context"

// Frame is a pipeline-owned extracted frame: an absolute path to a PNG on
// local disk inside the extraction WorkDir.
type Frame struct {
	Index        int
	TimestampSec float64
	Path         string
}

// FrameSource extracts frames from a local video file. workflows names
// the extraction workflows to run (in order; the result is the union of
// their frames, subject to the max_frames hard cap); empty means the
// configured default workflow.
//
// Lifecycle contract: the implementation creates and owns an absolute
// WorkDir and returns a cleanup closure that deletes it. The caller MUST
// `defer cleanup()` immediately after Frames returns (before any fan-out)
// so the WorkDir is deleted on every exit path — error, ctx-cancel, panic.
// cleanup is idempotent; its error is logged but never changes the verdict.
//
// Fail-safe: any extraction error, a missing binary at runtime, or zero
// frames extracted is a could-not-evaluate condition (Verdict=error +
// dead-letter). Zero frames is NEVER treated as clean/allow.
type FrameSource interface {
	Frames(ctx context.Context, videoPath string, workflows []string) (frames []Frame, cleanup func() error, err error)
}
