package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/hashmatch"
	"github.com/matthupy/vismod/internal/observe"
	"github.com/matthupy/vismod/internal/pipeline"
	"github.com/matthupy/vismod/internal/queue"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
)

type failSink struct{ err error }

func (f failSink) Write(context.Context, result.ResultEnvelope) error { return f.err }

func newTestPipeline(t *testing.T, sink result.Sink) *pipeline.Pipeline {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	mod, err := buildModerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &pipeline.Pipeline{
		Moderator: mod,
		Frames:    &frames.FakeFrameSource{},
		Matcher:   hashmatch.NoOp{},
		Sink:      sink,
		Cfg:       cfg,
		Log:       observe.NewLogger("error"),
	}
}

func TestJobHandlerAcksOnSuccess(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	_ = os.WriteFile(img, []byte("bytes"), 0o600)

	// Successful sink write => Ack (even when the decision itself is an error
	// verdict, that envelope was written successfully).
	p := newTestPipeline(t, result.NewJSONLSink(discardWriter{}))
	disp, err := jobHandler(p)(context.Background(), queue.Job{
		ID:     "j1",
		Source: moderation.Source{Kind: "file", Ref: img},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if disp != queue.Ack {
		t.Fatalf("want Ack, got %s", disp)
	}
}

func TestJobHandlerRetriesOnInfraFailure(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	_ = os.WriteFile(img, []byte("bytes"), 0o600)

	// A sink write failure is an infrastructure failure => Retry.
	p := newTestPipeline(t, failSink{err: errors.New("disk full")})
	disp, err := jobHandler(p)(context.Background(), queue.Job{
		ID:     "j2",
		Source: moderation.Source{Kind: "file", Ref: img},
	})
	if err == nil {
		t.Fatal("expected an error from the failing sink")
	}
	if disp != queue.Retry {
		t.Fatalf("infra failure must Retry, got %s", disp)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
