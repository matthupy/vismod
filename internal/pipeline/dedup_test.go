package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/matthupy/vismod/internal/audit"
	"github.com/matthupy/vismod/internal/dedup"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/redis/go-redis/v9"
)

// countingMod wraps a result and counts AnalyzeImage calls, to prove the dedup
// gate short-circuits BEFORE analyze on a known-done job.
type countingMod struct {
	calls  atomic.Int64
	caps   moderation.Caps
	result moderation.NormalizedResult
}

func (m *countingMod) Name() string                  { return "counting" }
func (m *countingMod) Capabilities() moderation.Caps { return m.caps }
func (m *countingMod) Close() error                  { return nil }
func (m *countingMod) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	m.calls.Add(1)
	return m.result, nil
}

// fakeDeduper reports a fixed Done verdict and records Commit calls.
type fakeDeduper struct {
	done    bool
	commits atomic.Int64
	doneErr error
}

func (f *fakeDeduper) Done(context.Context, string) (bool, error) { return f.done, f.doneErr }
func (f *fakeDeduper) Commit(context.Context, string) error       { f.commits.Add(1); return nil }

func newRedisDeduper(t *testing.T) *dedup.RedisDeduper {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return dedup.NewRedisDeduper(client, time.Hour)
}

// TestDedupShortCircuitsBeforeAnalyze proves a job already marked Done skips the
// whole pipeline: no Moderator call, no Sink write.
func TestDedupShortCircuitsBeforeAnalyze(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	if err := os.WriteFile(img, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	mod := &countingMod{
		caps:   moderation.Caps{MaxImageBytes: 1024},
		result: moderation.NormalizedResult{Frames: []moderation.FrameResult{okFrame(scoreCat(moderation.CategoryViolence, 0.95))}},
	}
	sink := &capSink{}
	p := newPipe(t, mod, &frames.FakeFrameSource{}, sink)
	p.Dedup = &fakeDeduper{done: true}

	if err := p.Process(context.Background(), "job-1", moderation.Source{Kind: "file", Ref: img}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if mod.calls.Load() != 0 {
		t.Fatalf("done job must not call the Moderator, got %d calls", mod.calls.Load())
	}
	if len(sink.envs) != 0 {
		t.Fatalf("done job must not write the Sink, got %d", len(sink.envs))
	}
}

// TestDedupCommitsAfterWrite proves a fresh job is processed and then committed.
func TestDedupCommitsAfterWrite(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	_ = os.WriteFile(img, []byte("data"), 0o600)
	mod := &countingMod{caps: moderation.Caps{MaxImageBytes: 1024},
		result: moderation.NormalizedResult{Frames: []moderation.FrameResult{okFrame(scoreCat(moderation.CategoryViolence, 0.95))}}}
	sink := &capSink{}
	p := newPipe(t, mod, &frames.FakeFrameSource{}, sink)
	d := &fakeDeduper{done: false}
	p.Dedup = d

	if err := p.Process(context.Background(), "job-1", moderation.Source{Kind: "file", Ref: img}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(sink.envs) != 1 {
		t.Fatalf("fresh job must write once, got %d", len(sink.envs))
	}
	if d.commits.Load() != 1 {
		t.Fatalf("fresh job must Commit once after write, got %d", d.commits.Load())
	}
}

// TestCrossProcessRedeliveryWritesOnce is the issue-#9 regression: a redelivery
// landing on a FRESH process (new Sink + new audit.Log over the same chain file)
// sharing one durable Deduper must not double-write — exactly one result line
// across both sinks and exactly one audit seq.
func TestCrossProcessRedeliveryWritesOnce(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	if err := os.WriteFile(img, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "audit.log")
	d := newRedisDeduper(t)
	src := moderation.Source{Kind: "file", Ref: img}

	newProc := func(sink result.Sink) *Pipeline {
		mod := &countingMod{caps: moderation.Caps{MaxImageBytes: 1024},
			result: moderation.NormalizedResult{Frames: []moderation.FrameResult{okFrame(scoreCat(moderation.CategoryViolence, 0.95))}}}
		al, err := audit.Open(logPath) // fresh Log over the SAME file = new process
		if err != nil {
			t.Fatal(err)
		}
		p := newPipe(t, mod, &frames.FakeFrameSource{}, sink)
		p.Audit = al
		p.Dedup = d
		return p
	}

	sinkA := &capSink{}
	if err := newProc(sinkA).Process(context.Background(), "job-1", src); err != nil {
		t.Fatalf("process A: %v", err)
	}
	// Fresh process B: empty in-memory Sink seen-set — the cross-process gap.
	sinkB := &capSink{}
	if err := newProc(sinkB).Process(context.Background(), "job-1", src); err != nil {
		t.Fatalf("process B (redelivery): %v", err)
	}

	if total := len(sinkA.envs) + len(sinkB.envs); total != 1 {
		t.Fatalf("redelivery double-wrote the Sink: got %d result lines, want 1", total)
	}
	recs, err := audit.ReadRecords(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("redelivery double-appended the audit chain: got %d seq, want 1", len(recs))
	}
}
