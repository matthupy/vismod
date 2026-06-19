package frames

import (
	"context"
	"errors"
	"testing"
)

func TestFakeFrameSourceReturnsFramesAndCleanup(t *testing.T) {
	f := &FakeFrameSource{Result: []Frame{{Index: 0, Path: "a.png"}}}
	got, cleanup, err := f.Frames(context.Background(), "v.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "a.png" {
		t.Fatalf("frames = %+v", got)
	}
	if f.Cleaned() {
		t.Fatal("not cleaned before cleanup() called")
	}
	// cleanup must be idempotent.
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal("cleanup must be idempotent")
	}
	if !f.Cleaned() {
		t.Fatal("Cleaned() must report true after cleanup")
	}
}

func TestFakeFrameSourceErrStillReturnsCleanup(t *testing.T) {
	f := &FakeFrameSource{Err: errors.New("extract failed")}
	_, cleanup, err := f.Frames(context.Background(), "v.mp4")
	if err == nil {
		t.Fatal("expected error")
	}
	if cleanup == nil {
		t.Fatal("cleanup must be non-nil even on error (caller defers it)")
	}
	_ = cleanup()
}
