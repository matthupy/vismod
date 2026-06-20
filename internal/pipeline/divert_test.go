package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/review"
	"github.com/matthupy/vismod/pkg/moderation"
)

type fakeDiverter struct {
	mu    sync.Mutex
	items []review.Item
}

func (f *fakeDiverter) Divert(_ context.Context, it review.Item) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, it)
	return nil
}

// failDiverter always errors, simulating a review channel that is down.
type failDiverter struct{}

func (failDiverter) Divert(context.Context, review.Item) error {
	return errors.New("review channel down")
}

// countingRecorder implements both JobRecorder and DivertFailureRecorder so the
// pipeline can observe a dropped divert without a real prometheus registry.
type countingRecorder struct {
	jobs        int
	divertFails int
}

func (c *countingRecorder) RecordJob(moderation.Verdict) { c.jobs++ }
func (c *countingRecorder) RecordDivertFailure()         { c.divertFails++ }

func sexualFrame(score float64) moderation.NormalizedResult {
	return moderation.NormalizedResult{Frames: []moderation.FrameResult{okFrame(scoreCat(moderation.CategorySexual, score))}}
}

func TestProcessDivertsPotentialCSAM(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	if err := os.WriteFile(img, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	// SEXUAL score 0.7 >= default potential_csam 0.667 => divert.
	mod := &fakeMod{caps: moderation.Caps{MaxImageBytes: 1024}, result: sexualFrame(0.7)}
	div := &fakeDiverter{}
	p := newPipe(t, mod, &frames.FakeFrameSource{}, &capSink{})
	p.Diverter = div

	if err := p.Process(context.Background(), "job-1", moderation.Source{Kind: "file", Ref: img}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(div.items) != 1 {
		t.Fatalf("want 1 divert, got %d", len(div.items))
	}
	sum := sha256.Sum256([]byte("data"))
	if div.items[0].FrameSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("divert must carry SHA-256(frame), got %q", div.items[0].FrameSHA256)
	}
	if div.items[0].JobID != "job-1" {
		t.Fatalf("divert JobID mismatch: %q", div.items[0].JobID)
	}
}

func TestProcessBelowPotentialCSAMNoDivert(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	_ = os.WriteFile(img, []byte("data"), 0o600)
	// SEXUAL score 0.5 < 0.667 => no divert.
	mod := &fakeMod{caps: moderation.Caps{MaxImageBytes: 1024}, result: sexualFrame(0.5)}
	div := &fakeDiverter{}
	p := newPipe(t, mod, &frames.FakeFrameSource{}, &capSink{})
	p.Diverter = div

	_ = p.Process(context.Background(), "job-1", moderation.Source{Kind: "file", Ref: img})
	if len(div.items) != 0 {
		t.Fatalf("below-threshold must not divert, got %d", len(div.items))
	}
}

// A divert that fails must NOT fail the job (fail-safe), but MUST bump the
// divert-failure counter so a lost potential-CSAM frame is observable.
func TestProcessDivertFailureIsObservableButNonFatal(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	if err := os.WriteFile(img, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	mod := &fakeMod{caps: moderation.Caps{MaxImageBytes: 1024}, result: sexualFrame(0.7)}
	rec := &countingRecorder{}
	p := newPipe(t, mod, &frames.FakeFrameSource{}, &capSink{})
	p.Diverter = failDiverter{}
	p.Metrics = rec

	if err := p.Process(context.Background(), "job-1", moderation.Source{Kind: "file", Ref: img}); err != nil {
		t.Fatalf("divert failure must NOT fail the job (fail-safe): %v", err)
	}
	if rec.divertFails != 1 {
		t.Fatalf("dropped divert must bump the counter, got %d", rec.divertFails)
	}
	if rec.jobs != 1 {
		t.Fatalf("job must still be recorded once, got %d", rec.jobs)
	}
}
