package cli

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/queue"
)

// TestWorkflowsListMarksTheDefault: the listing is how an operator learns
// which extraction runs when a job names no workflow. An unmarked default
// makes every custom-workflow decision guesswork.
func TestWorkflowsListMarksTheDefault(t *testing.T) {
	c := scanConfig()
	c.FFmpeg.DefaultWorkflow = "keyframes"
	c.FFmpeg.Workflows = map[string]config.WorkflowConfig{
		"keyframes": {Description: "I-frames only"},
		"uniform":   {Description: "one frame per second"},
	}
	withConfig(t, c)

	var out bytes.Buffer
	workflowsListCmd.SetOut(&out)
	if err := workflowsListCmd.RunE(workflowsListCmd, nil); err != nil {
		t.Fatalf("workflows list: %v", err)
	}
	got := out.String()
	for _, want := range []string{"keyframes", "uniform", "I-frames only", "one frame per second"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing omits %q:\n%s", want, got)
		}
	}
	for line := range strings.SplitSeq(strings.TrimSpace(got), "\n") {
		if strings.Contains(line, "keyframes") && !strings.HasPrefix(line, "*") {
			t.Errorf("the default workflow is not marked: %q", line)
		}
		if strings.Contains(line, "uniform") && strings.HasPrefix(line, "*") {
			t.Errorf("a non-default workflow is marked as default: %q", line)
		}
	}
}

func TestWorkflowsListWithNoneConfigured(t *testing.T) {
	c := scanConfig()
	c.FFmpeg.Workflows = nil
	withConfig(t, c)

	var out bytes.Buffer
	workflowsListCmd.SetOut(&out)
	if err := workflowsListCmd.RunE(workflowsListCmd, nil); err != nil {
		t.Fatalf("workflows list: %v", err)
	}
	if !strings.Contains(out.String(), "no workflows") {
		t.Errorf("an empty workflow set must say so, got: %q", out.String())
	}
}

// TestWorkflowsValidateReportsMissingBinaries: `workflows validate` is the
// gate operators run before shipping a custom workflow. It must fail when
// ffmpeg is absent rather than reporting the templates OK.
func TestWorkflowsValidateReportsMissingBinaries(t *testing.T) {
	c := scanConfig()
	c.FFmpeg.FFmpegPath = filepath.Join(t.TempDir(), "no-such-ffmpeg")
	c.FFmpeg.FFprobePath = filepath.Join(t.TempDir(), "no-such-ffprobe")
	withConfig(t, c)

	var out bytes.Buffer
	workflowsValidateCmd.SetOut(&out)
	if err := workflowsValidateCmd.RunE(workflowsValidateCmd, nil); err == nil {
		t.Fatal("validate passed with no ffmpeg present")
	}
}

// TestWorkflowsValidateRejectsAGuardrailViolation: the guardrails are the
// no-shell trust boundary (invariant 5). A workflow that escapes the
// pipeline-owned WorkDir must fail this command, not fail later per job.
func TestWorkflowsValidateRejectsAGuardrailViolation(t *testing.T) {
	c := scanConfig()
	c.FFmpeg.Workflows = map[string]config.WorkflowConfig{
		"escapes": {Description: "writes outside WorkDir", Args: []string{
			"-i", "{{.Input}}", "http://example.invalid/out.jpg",
		}},
	}
	withConfig(t, c)

	var out bytes.Buffer
	workflowsValidateCmd.SetOut(&out)
	if err := workflowsValidateCmd.RunE(workflowsValidateCmd, nil); err == nil {
		t.Fatal("validate accepted a workflow whose output leaves the WorkDir")
	}
}

// TestAdaptersListsRegisteredNames: the adapters listing is how an operator
// discovers what adapter.name accepts. It must name every registered
// adapter even when one cannot be constructed without credentials —
// hiding it would read as "that vendor is unsupported".
func TestAdaptersLists(t *testing.T) {
	withConfig(t, scanConfig())

	var out bytes.Buffer
	adaptersCmd.SetOut(&out)
	if err := adaptersCmd.RunE(adaptersCmd, nil); err != nil {
		t.Fatalf("adapters: %v", err)
	}
	got := out.String()
	for _, name := range []string{"microsoft", "google", "hive", "shieldgemma"} {
		if !strings.Contains(got, name) {
			t.Errorf("adapter %q missing from the listing:\n%s", name, got)
		}
	}
	// Adapters that need credentials cannot be constructed here; the
	// listing has to say why instead of dropping them.
	if !strings.Contains(got, "capabilities unavailable") {
		t.Errorf("no adapter reported an unavailable-capabilities reason; the listing may be hiding construction failures:\n%s", got)
	}
	// The scripted test adapter constructs cleanly, so its capabilities
	// line proves the success branch also renders.
	if !strings.Contains(got, "max_image_bytes") {
		t.Errorf("no adapter rendered its capabilities:\n%s", got)
	}
}

// TestNewQueueRedisDriver: the redis driver is what production runs. Boot
// must reach the server (Redis is the SPOF — an unreachable instance is a
// fatal operator error, not a per-job failure).
func TestNewQueueRedisDriver(t *testing.T) {
	mr := miniredis.RunT(t)
	c := scanConfig()
	c.Queue.Driver = "redis"
	c.Queue.Redis = config.RedisConfig{Addr: mr.Addr()}

	q, err := newQueue(c, testLogger())
	if err != nil {
		t.Fatalf("newQueue(redis): %v", err)
	}
	if _, ok := q.(*queue.Redisq); !ok {
		t.Fatalf("driver=redis built %T, want *queue.Redisq", q)
	}
	if dlqOf(q) == nil {
		t.Error("dlqOf returned nil for redisq; dead-letter depth would never be exported")
	}
	if activeOf(q) == nil {
		t.Error("activeOf returned nil for redisq; the UI worker count would be blank")
	}
	if statesOf(q) != nil {
		t.Error("statesOf must be nil for redisq: it has no local state view to report")
	}
	_ = q.Close(t.Context())
}

// TestNewQueueRedisUnreachableFailsBoot: silently starting with an
// unreachable Redis would accept jobs into a queue that cannot hold them.
func TestNewQueueRedisUnreachableFailsBoot(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close() // nothing listening now

	c := scanConfig()
	c.Queue.Driver = "redis"
	c.Queue.Redis = config.RedisConfig{Addr: addr}

	if _, err := newQueue(c, testLogger()); err == nil {
		t.Fatal("an unreachable Redis must fail boot")
	}
}

// TestNewServerAddsRedisReadinessProbe: with the redis driver, /readyz has
// to track Redis health so an outage stops ingress routing instead of
// black-holing jobs.
func TestNewServerAddsRedisReadinessProbe(t *testing.T) {
	mr := miniredis.RunT(t)
	c := serveConfig(t)
	c.Queue.Driver = "redis"
	c.Queue.Redis = config.RedisConfig{Addr: mr.Addr()}

	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()

	rec := httptest.NewRecorder()
	s.health.Readyz(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("readyz = %d with a healthy Redis, want 200: %s", rec.Code, rec.Body)
	}

	mr.Close() // the outage
	rec = httptest.NewRecorder()
	s.health.Readyz(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Errorf("readyz = %d with Redis down, want 503; ingress would keep routing to a worker that cannot queue", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "redis") {
		t.Errorf("readyz does not name the failing probe: %s", rec.Body)
	}
}
