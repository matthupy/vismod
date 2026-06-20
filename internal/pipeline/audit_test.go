package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/matthupy/vismod/internal/audit"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/pkg/moderation"
)

func TestProcessAppendsAuditRecord(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	if err := os.WriteFile(img, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "audit.log")
	al, err := audit.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}

	mod := &fakeMod{
		caps:   moderation.Caps{MaxImageBytes: 1024},
		result: moderation.NormalizedResult{Frames: []moderation.FrameResult{okFrame(scoreCat(moderation.CategoryViolence, 0.95))}},
	}
	p := newPipe(t, mod, &frames.FakeFrameSource{}, &capSink{})
	p.Audit = al

	if err := p.Process(context.Background(), "job-1", moderation.Source{Kind: "file", Ref: img}); err != nil {
		t.Fatalf("Process: %v", err)
	}

	recs, err := audit.ReadRecords(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 audit record, got %d", len(recs))
	}
	if recs[0].Payload.JobID != "job-1" || recs[0].Payload.Verdict != string(moderation.VerdictBlock) {
		t.Fatalf("audit payload mismatch: %+v", recs[0].Payload)
	}
	if broken, err := audit.Verify(logPath); err != nil {
		t.Fatalf("chain must verify: broken@%d %v", broken, err)
	}
}

func TestProcessAuditIdempotentOnReprocess(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	_ = os.WriteFile(img, []byte("data"), 0o600)
	logPath := filepath.Join(dir, "audit.log")
	al, _ := audit.Open(logPath)

	mod := &fakeMod{caps: moderation.Caps{MaxImageBytes: 1024},
		result: moderation.NormalizedResult{Frames: []moderation.FrameResult{okFrame(scoreCat(moderation.CategoryViolence, 0.95))}}}
	p := newPipe(t, mod, &frames.FakeFrameSource{}, &capSink{})
	p.Audit = al

	src := moderation.Source{Kind: "file", Ref: img}
	_ = p.Process(context.Background(), "job-1", src)
	_ = p.Process(context.Background(), "job-1", src) // redelivery

	recs, _ := audit.ReadRecords(logPath)
	if len(recs) != 1 {
		t.Fatalf("redelivery must not double-append: got %d records", len(recs))
	}
}
