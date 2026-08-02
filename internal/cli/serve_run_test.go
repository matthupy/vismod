package cli

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// serveConfig is a credential-free, port-free serve setup: the scripted
// adapter, the memory driver, and every listener on an ephemeral loopback
// port so a test never collides with a real deployment or another test.
func serveConfig(t *testing.T) config.Config {
	t.Helper()
	c := scanConfig()
	c.MetricsAddr = "127.0.0.1:0"
	c.IntakeAddr = "127.0.0.1:0"
	c.Queue.Workers = 1
	c.Queue.Buffer = 8
	c.Queue.MaxRetries = 0
	c.Queue.RetryBackoff = 10 * time.Millisecond
	c.Queue.DrainTimeout = 3 * time.Second
	c.Queue.JobTimeout = 10 * time.Second
	c.Output.Sinks = []config.SinkConfig{{Type: "file", Path: filepath.Join(t.TempDir(), "out.jsonl")}}
	return c
}

func sinkPath(c config.Config) string { return c.Output.Sinks[0].Path }

// TestNewServerRefusesToBoot: every one of these is an operator error that
// must surface at boot. A worker that starts anyway would accept jobs it
// cannot score, cannot record, or cannot deliver.
func TestNewServerRefusesToBoot(t *testing.T) {
	cases := []struct {
		name  string
		mutID func(*config.Config)
	}{
		{"unknown adapter", func(c *config.Config) { c.Adapter.Name = "not-a-vendor" }},
		{"unknown queue driver", func(c *config.Config) { c.Queue.Driver = "kafka" }},
		{"no sinks", func(c *config.Config) { c.Output.Sinks = nil }},
		{"unwritable sink path", func(c *config.Config) {
			c.Output.Sinks = []config.SinkConfig{{Type: "file", Path: filepath.Join(t.TempDir(), "nope", "out.jsonl")}}
		}},
		{"unopenable audit path", func(c *config.Config) {
			c.Audit = config.AuditConfig{Enabled: true, Path: t.TempDir()} // a directory
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := serveConfig(t)
			tc.mutID(&c)
			s, err := newServer(c)
			if err == nil {
				s.close()
				t.Fatal("boot succeeded; this configuration cannot run correctly")
			}
			if s != nil {
				t.Error("newServer returned both a server and an error")
			}
		})
	}
}

// TestNewServerMemoryDriverWarnsInReadyz: memq is non-durable and
// single-process. Booting it is allowed (it is the dev default) but the
// operator has to be able to SEE that choice from outside the process,
// without it flipping readiness and stopping ingress.
func TestNewServerMemoryDriverWarnsInReadyz(t *testing.T) {
	s, err := newServer(serveConfig(t))
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()

	rec := httptest.NewRecorder()
	s.health.Readyz(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 {
		t.Errorf("readyz = %d, want 200: the memory driver is a warning, not an outage", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "NON-DURABLE") {
		t.Errorf("readyz does not disclose the non-durable driver: %s", rec.Body)
	}
}

// TestNewServerCloseIsSafe: a booted-but-never-run server still owns an
// adapter, an audit log and open sinks. close() must release all of them.
func TestNewServerCloseIsSafe(t *testing.T) {
	c := serveConfig(t)
	c.Audit = config.AuditConfig{Enabled: true, Path: filepath.Join(t.TempDir(), "audit.log")}
	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if s.auditLog == nil {
		t.Fatal("audit.enabled=true produced no log")
	}
	s.close()
}

// runServer starts s.run in the background and returns a stop func that
// cancels and waits for the drain to finish. A run that does not return
// after cancellation is a hang in the shutdown path, so the wait is
// bounded and fatal.
func runServer(t *testing.T, s *server) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run returned %v; a clean cancel must drain without error", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("run did not return after context cancellation: the drain path is stuck")
		}
	}
}

// waitForFile polls path until it has content or the deadline passes. The
// worker pool is asynchronous, so there is no synchronous point to assert
// against — the observable outcome is the envelope reaching the sink.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing reached the sink at %s within the deadline", path)
	return ""
}

// TestServerRunProcessesQueuedJob is the whole worker path end to end:
// queued job -> pipeline -> sink -> audit -> metrics -> job tracker, then a
// clean drain. It asserts the verdict reached the SINK, not just that the
// handler ran, because a job that is acked without an emitted envelope is
// indistinguishable from a scan that never happened.
func TestServerRunProcessesQueuedJob(t *testing.T) {
	c := serveConfig(t)
	auditPath := filepath.Join(t.TempDir(), "audit.log")
	c.Audit = config.AuditConfig{Enabled: true, Path: auditPath}

	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()

	input := writeInput(t, "bad.jpg", "BLOCK")
	if _, err := s.queue.Enqueue(context.Background(), queue.Job{
		ID:          "serve-test-1",
		Source:      moderation.Source{Kind: "file", Ref: input, MediaType: "image"},
		SubmittedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	stop := runServer(t, s)
	body := waitForFile(t, sinkPath(c))
	stop()

	if !strings.Contains(body, string(moderation.VerdictBlock)) {
		t.Errorf("sink envelope carries no block verdict: %s", body)
	}
	recent, stats := s.tracker.Snapshot()
	if len(recent) != 1 || recent[0].Verdict != string(moderation.VerdictBlock) {
		t.Errorf("job tracker holds %+v, want one block record (the UI job feed would be empty)", recent)
	}
	if stats.Total != 1 {
		t.Errorf("verdict stats total = %d, want 1", stats.Total)
	}
	if b, err := os.ReadFile(auditPath); err != nil || len(b) == 0 {
		t.Errorf("the decision produced no audit record (%v)", err)
	}
}

// TestServerRunDeadLettersUnscorableJob: a job the model cannot score must
// end at verdict=error in the DLQ, never acked as allow, and the dead-letter
// depth must reach the gauge the autoscaler and the UI both read.
func TestServerRunDeadLettersUnscorableJob(t *testing.T) {
	c := serveConfig(t)
	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()

	input := writeInput(t, "boom.jpg", "ERROR")
	if _, err := s.queue.Enqueue(context.Background(), queue.Job{
		ID:          "serve-test-err",
		Source:      moderation.Source{Kind: "file", Ref: input, MediaType: "image"},
		SubmittedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	stop := runServer(t, s)
	body := waitForFile(t, sinkPath(c))

	// The dead-letter sink is what the depth poller exports every 2s and
	// what the operator reviews. An unscorable job that never lands here
	// is a job that vanished.
	dlq := dlqOf(s.queue)
	if dlq == nil {
		t.Fatal("memq exposes no dead-letter sink")
	}
	deadline := time.Now().Add(15 * time.Second)
	var dlqDepth int
	for time.Now().Before(deadline) {
		if d, err := dlq.Depth(context.Background()); err == nil && d > 0 {
			dlqDepth = d
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Outlive one tick of the 2s depth poller so the gauge export path runs
	// with a non-zero depth rather than only on an empty queue.
	time.Sleep(2100 * time.Millisecond)
	stop()

	if !strings.Contains(body, string(moderation.VerdictError)) {
		t.Errorf("unscorable job produced %s, want verdict=error", body)
	}
	if strings.Contains(body, `"verdict":"allow"`) {
		t.Error("unscorable job reported as allow; invariant 1 is broken")
	}
	if dlqDepth < 1 {
		t.Errorf("dead-letter depth gauge = %v, want >=1; the autoscaling and UI signal never updated", dlqDepth)
	}
}

// TestServerRunWithUIEnabled: the dashboard is off by default, and turning
// it on must not change the shutdown contract — a UI server left running
// after drain would hold the process open past its drain timeout.
func TestServerRunWithUIEnabled(t *testing.T) {
	c := serveConfig(t)
	c.UI = config.UIConfig{Enabled: true, Addr: "127.0.0.1:0", Auth: "basic"}
	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()

	stop := runServer(t, s)
	stop()
}

// TestServerRunReturnsWithoutIntake: intake is opt-in. With no intake_addr
// the worker still runs and still drains — the queue is fed by another
// replica's Redis in that shape.
func TestServerRunReturnsWithoutIntake(t *testing.T) {
	c := serveConfig(t)
	c.IntakeAddr = ""
	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()

	stop := runServer(t, s)
	stop()
}

// TestRunServeUsesTheLoadedConfig covers the cobra-facing wrapper: it must
// read the package-level config, boot from it, and return the boot error
// rather than starting a worker.
func TestRunServeFailsFastOnBadConfig(t *testing.T) {
	c := serveConfig(t)
	c.Adapter.Name = "not-a-vendor"
	withConfig(t, c)
	if err := runServe(context.Background()); err == nil {
		t.Fatal("runServe started with an unregistered adapter")
	}
}

// TestRunServeDrainsOnCancel: the signal path. runServe wraps the parent
// context with signal.NotifyContext, so cancelling the parent must still
// reach the drain — a lost cancellation would leave SIGTERM unhandled and
// the pod would be killed mid-job.
func TestRunServeDrainsOnCancel(t *testing.T) {
	withConfig(t, serveConfig(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()

	time.Sleep(200 * time.Millisecond) // let the worker pool come up
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runServe = %v, want a clean drain", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServe ignored parent cancellation")
	}
}
