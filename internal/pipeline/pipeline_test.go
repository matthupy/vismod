package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/hashmatch"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

type fakeMod struct {
	caps   moderation.Caps
	result moderation.NormalizedResult
	err    error
}

func (f *fakeMod) Name() string                  { return "fake" }
func (f *fakeMod) Capabilities() moderation.Caps { return f.caps }
func (f *fakeMod) Close() error                  { return nil }
func (f *fakeMod) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	return f.result, f.err
}

type capSink struct {
	mu   sync.Mutex
	envs []result.ResultEnvelope
}

func (c *capSink) Write(_ context.Context, e result.ResultEnvelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envs = append(c.envs, e)
	return nil
}

func newPipe(t *testing.T, mod moderation.Moderator, fs frames.FrameSource, sink result.Sink) *Pipeline {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return &Pipeline{Moderator: mod, Frames: fs, Matcher: hashmatch.NoOp{}, Sink: sink, Cfg: cfg, Log: testLogger()}
}

func TestProcessImageHappyPath(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	if err := os.WriteFile(img, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	mod := &fakeMod{
		caps: moderation.Caps{MaxImageBytes: 1024},
		result: moderation.NormalizedResult{Frames: []moderation.FrameResult{okFrame(
			scoreCat(moderation.CategoryViolence, 0.95), // block
		)}},
	}
	sink := &capSink{}
	p := newPipe(t, mod, &frames.FakeFrameSource{}, sink)

	if err := p.Process(context.Background(), "job-1", moderation.Source{Kind: "file", Ref: img}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(sink.envs) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(sink.envs))
	}
	got := sink.envs[0].Result
	if got.Overall.Verdict != moderation.VerdictBlock {
		t.Fatalf("want block, got %s", got.Overall.Verdict)
	}
	if got.SchemaVersion != moderation.SchemaVersion {
		t.Fatalf("schema_version must be stamped, got %q", got.SchemaVersion)
	}
	if got.AssetID != img {
		t.Fatalf("asset_id must be Source.Ref, got %q", got.AssetID)
	}
}

func TestProcessOversizeImagePreflightError(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "big.jpg")
	if err := os.WriteFile(img, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	mod := &fakeMod{caps: moderation.Caps{MaxImageBytes: 10}} // 100 > 10 => pre-flight error
	sink := &capSink{}
	p := newPipe(t, mod, &frames.FakeFrameSource{}, sink)

	_ = p.Process(context.Background(), "job-1", moderation.Source{Kind: "file", Ref: img})
	if v := sink.envs[0].Result.Overall.Verdict; v != moderation.VerdictError {
		t.Fatalf("oversize must be error (never allow), got %s", v)
	}
}

func TestProcessVideoExtractionFailureIsError(t *testing.T) {
	fs := &frames.FakeFrameSource{Err: errors.New("ffmpeg exploded")}
	mod := &fakeMod{caps: moderation.Caps{MaxImageBytes: 1024}}
	sink := &capSink{}
	p := newPipe(t, mod, fs, sink)

	_ = p.Process(context.Background(), "job-v", moderation.Source{Kind: "file", Ref: "clip.mp4"})
	if v := sink.envs[0].Result.Overall.Verdict; v != moderation.VerdictError {
		t.Fatalf("extraction failure must be error (never allow), got %s", v)
	}
	if !fs.Cleaned() {
		t.Fatal("cleanup must run on the extraction-error path")
	}
}

func TestProcessVideoFrameByFrame(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "f1.png")
	f2 := filepath.Join(dir, "f2.png")
	_ = os.WriteFile(f1, []byte("a"), 0o600)
	_ = os.WriteFile(f2, []byte("b"), 0o600)

	fs := &frames.FakeFrameSource{Result: []frames.Frame{
		{Index: 0, TimestampSec: 0.0, Path: f1},
		{Index: 1, TimestampSec: 1.5, Path: f2},
	}}
	mod := &fakeMod{
		caps:   moderation.Caps{MaxImageBytes: 1024},
		result: moderation.NormalizedResult{Frames: []moderation.FrameResult{okFrame(scoreCat(moderation.CategoryViolence, 0.6))}},
	}
	sink := &capSink{}
	p := newPipe(t, mod, fs, sink)

	_ = p.Process(context.Background(), "job-v", moderation.Source{Kind: "file", Ref: "clip.mp4", MediaType: "video"})
	res := sink.envs[0].Result
	if len(res.Frames) != 2 {
		t.Fatalf("want 2 frames, got %d", len(res.Frames))
	}
	if res.Frames[1].TimestampSec == nil || *res.Frames[1].TimestampSec != 1.5 {
		t.Fatalf("frame timestamp must be carried, got %v", res.Frames[1].TimestampSec)
	}
	if res.Overall.Verdict != moderation.VerdictFlag {
		t.Fatalf("want flag, got %s", res.Overall.Verdict)
	}
	if !fs.Cleaned() {
		t.Fatal("cleanup must run after successful video processing")
	}
}
