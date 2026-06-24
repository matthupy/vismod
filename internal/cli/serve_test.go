package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/hashmatch"
	"github.com/matthupy/vismod/internal/observe"
	"github.com/matthupy/vismod/internal/pipeline"
	"github.com/matthupy/vismod/internal/queue"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

const testWorkerFP = "fp-worker"

func TestJobHandlerAcksOnSuccess(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	_ = os.WriteFile(img, []byte("bytes"), 0o600)

	// Successful sink write => Ack (even when the decision itself is an error
	// verdict, that envelope was written successfully). Matching fingerprint.
	p := newTestPipeline(t, result.NewJSONLSink(discardWriter{}))
	disp, err := jobHandler(p, testWorkerFP, observe.NewMetrics(), observe.NewLogger("error"))(context.Background(), queue.Job{
		ID:               "j1",
		Source:           moderation.Source{Kind: "file", Ref: img},
		ModelFingerprint: testWorkerFP,
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
	disp, err := jobHandler(p, testWorkerFP, observe.NewMetrics(), observe.NewLogger("error"))(context.Background(), queue.Job{
		ID:               "j2",
		Source:           moderation.Source{Kind: "file", Ref: img},
		ModelFingerprint: testWorkerFP,
	})
	if err == nil {
		t.Fatal("expected an error from the failing sink")
	}
	if disp != queue.Retry {
		t.Fatalf("infra failure must Retry, got %s", disp)
	}
}

// countSink records how many envelopes were written, to prove a mismatched job
// never reached pipeline.Process (which writes exactly one envelope per job).
type countSink struct{ n int }

func (c *countSink) Write(context.Context, result.ResultEnvelope) error { c.n++; return nil }

// A job whose fingerprint != the worker's must DeadLetter and NEVER call Process
// (no wrong-model verdict is ever emitted — §L / §F.5 fail-safe direction).
func TestJobHandlerDeadLettersOnFingerprintMismatch(t *testing.T) {
	sink := &countSink{}
	p := newTestPipeline(t, sink)
	disp, err := jobHandler(p, testWorkerFP, observe.NewMetrics(), observe.NewLogger("error"))(context.Background(), queue.Job{
		ID:               "j3",
		Source:           moderation.Source{Kind: "file", Ref: "/does/not/matter.jpg"},
		ModelFingerprint: "fp-OTHER-model",
	})
	if disp != queue.DeadLetter {
		t.Fatalf("mismatch must DeadLetter, got %s", disp)
	}
	if err == nil {
		t.Fatal("mismatch must carry a descriptive error for the DLQ envelope")
	}
	if sink.n != 0 {
		t.Fatalf("Process must NOT run on mismatch (sink writes=%d, want 0)", sink.n)
	}
}

// An empty fingerprint (pre-feature older-binary job) is processed normally —
// the guard is skipped, never silently dropped.
func TestJobHandlerProcessesEmptyFingerprint(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	_ = os.WriteFile(img, []byte("bytes"), 0o600)

	sink := &countSink{}
	p := newTestPipeline(t, sink)
	disp, err := jobHandler(p, testWorkerFP, observe.NewMetrics(), observe.NewLogger("error"))(context.Background(), queue.Job{
		ID:               "j4",
		Source:           moderation.Source{Kind: "file", Ref: img},
		ModelFingerprint: "",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if disp != queue.Ack {
		t.Fatalf("empty fingerprint must process (Ack), got %s", disp)
	}
	if sink.n != 1 {
		t.Fatalf("empty fingerprint must run Process (sink writes=%d, want 1)", sink.n)
	}
}

// An unstamped (pre-feature) job that retries on infra failure must count the
// "unstamped" model-mismatch signal ONCE (on the first attempt), not once per
// dequeue. The handler gates the accounting+WARN to j.Attempt == 0 so the
// counter reflects distinct jobs, not retry-dequeue attempts.
func TestJobHandlerUnstampedCountsFirstAttemptOnly(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "x.jpg")
	_ = os.WriteFile(img, []byte("bytes"), 0o600)

	// Failing sink => the handler returns Retry, so in production memq/asynq would
	// re-dispatch the SAME job with an incremented Attempt. Simulate that here by
	// invoking the handler across a sequence of attempts for one job.
	m := observe.NewMetrics()
	p := newTestPipeline(t, failSink{err: errors.New("disk full")})
	h := jobHandler(p, testWorkerFP, m, observe.NewLogger("error"))

	for attempt := 0; attempt < 4; attempt++ {
		disp, err := h(context.Background(), queue.Job{
			ID:               "j-unstamped",
			Source:           moderation.Source{Kind: "file", Ref: img},
			ModelFingerprint: "",
			Attempt:          attempt,
		})
		if err == nil {
			t.Fatalf("attempt %d: expected infra error", attempt)
		}
		if disp != queue.Retry {
			t.Fatalf("attempt %d: want Retry, got %s", attempt, disp)
		}
	}

	const want = `# HELP vismod_jobs_model_mismatch_total Jobs whose stamped model fingerprint did not match the worker's loaded model (reason=mismatch) or carried no fingerprint (reason=unstamped).
# TYPE vismod_jobs_model_mismatch_total counter
vismod_jobs_model_mismatch_total{reason="unstamped"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "vismod_jobs_model_mismatch_total"); err != nil {
		t.Errorf("unstamped must count once across retries, not per dequeue: %v", err)
	}
}

// The single enqueue helper stamps the worker fingerprint onto every job, so an
// empty fingerprint downstream can only mean a pre-feature job.
func TestEnqueueJobStampsFingerprint(t *testing.T) {
	rec := &recordingQueue{}
	if _, err := enqueueJob(context.Background(), rec, moderation.Source{Kind: "file", Ref: "x.jpg"}, testWorkerFP); err != nil {
		t.Fatalf("enqueueJob: %v", err)
	}
	if rec.last.ModelFingerprint != testWorkerFP {
		t.Fatalf("enqueueJob must stamp fingerprint, got %q", rec.last.ModelFingerprint)
	}
	if rec.last.Source.Ref != "x.jpg" {
		t.Fatalf("enqueueJob must carry the source, got %q", rec.last.Source.Ref)
	}
}

// recordingQueue captures the last enqueued job. Only Enqueue is exercised.
type recordingQueue struct {
	queue.Queue
	last queue.Job
}

func (r *recordingQueue) Enqueue(_ context.Context, j queue.Job) (result.JobID, error) {
	r.last = j
	return j.ID, nil
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
