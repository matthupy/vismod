package pipeline

import (
	"context"
	"testing"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// TestHandlerAdaptsProcessToTheQueue: the queue drives every serve-mode job
// through Handler(), so the disposition it returns is what decides ack vs
// dead-letter. Process is well covered directly; this pins that the adapter
// forwards the disposition instead of flattening it.
func TestHandlerAdaptsProcessToTheQueue(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, buf := newTestPipeline(t, mod, nil)
	path := writeInput(t, "benign")

	disp, err := p.Handler()(context.Background(), imageJob(path))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if disp != queue.Ack {
		t.Errorf("disposition = %v, want Ack for an allow verdict", disp)
	}
	if env := decodeEnvelope(t, buf); env.Result.Overall.Verdict != moderation.VerdictAllow {
		t.Errorf("verdict = %q, want allow", env.Result.Overall.Verdict)
	}
	if _, ok := any(p.Handler()).(queue.Handler); !ok {
		t.Error("Handler() does not satisfy queue.Handler")
	}
}

// TestHandlerDeadLettersErrorVerdicts: fail-safe. A job the model could not
// score must reach the queue as a dead-letter with a non-nil error, never as
// a quiet ack — an acked error verdict is a job nobody reviews.
func TestHandlerDeadLettersErrorVerdicts(t *testing.T) {
	mod := &fakeModerator{errOn: map[string]error{"boom": context.DeadlineExceeded}}
	p, _ := newTestPipeline(t, mod, nil)
	path := writeInput(t, "boom")

	disp, err := p.Handler()(context.Background(), imageJob(path))
	if disp != queue.DeadLetter {
		t.Errorf("disposition = %v, want DeadLetter for a provider failure", disp)
	}
	if err == nil {
		t.Error("a dead-lettered job must carry the reason it died")
	}
}

// TestProcessMatchesProcessJob: Process is the thin two-value form used by
// the queue; it must not diverge from the envelope-returning path the CLI
// uses for exit codes.
func TestProcessMatchesProcessJob(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"spicy": 0.9}}
	p, _ := newTestPipeline(t, mod, nil)
	path := writeInput(t, "spicy")

	disp, err := p.Process(context.Background(), imageJob(path))
	if disp != queue.Ack {
		t.Errorf("disposition = %v, want Ack (a block is a successful evaluation)", disp)
	}
	if err != nil {
		t.Errorf("Process: %v", err)
	}
}
