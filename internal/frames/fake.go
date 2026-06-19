package frames

import "context"

// FakeFrameSource returns a fixed set of frames and a no-op (idempotent)
// cleanup. Used by tests and by the M0 prototype where no real extraction is
// wired yet. An Err, if set, is returned instead (to exercise the fail-safe
// extraction-error path).
type FakeFrameSource struct {
	Result []Frame
	Err    error

	cleaned bool
}

// Frames implements FrameSource.
func (f *FakeFrameSource) Frames(_ context.Context, _ string) ([]Frame, func() error, error) {
	cleanup := func() error {
		f.cleaned = true // idempotent: safe to call repeatedly
		return nil
	}
	if f.Err != nil {
		return nil, cleanup, f.Err
	}
	return f.Result, cleanup, nil
}

// Cleaned reports whether cleanup ran (test assertion helper).
func (f *FakeFrameSource) Cleaned() bool { return f.cleaned }
