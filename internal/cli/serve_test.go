package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/queue"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// intakeConfig has one known workflow and a loopback intake address. Port 0
// keeps the listener serveIntake starts off any fixed port; the tests drive
// srv.Handler directly rather than over the socket.
func intakeConfig() config.Config {
	return config.Config{
		IntakeAddr: "127.0.0.1:0",
		FFmpeg: config.FFmpegConfig{
			Workflows: map[string]config.WorkflowConfig{"keyframes": {Description: "test"}},
		},
		// Matches config.Defaults(): the per-job dedup ceiling comes from
		// here, so leaving it zero would test a stricter bound than any
		// real deployment runs.
		Frames: config.FramesConfig{
			Concurrency: 4,
			Dedup:       config.DedupConfig{Enabled: false, HammingThreshold: 8},
		},
	}
}

// openBackpressure is Ready: no sustained provider failure.
func openBackpressure() *observe.Backpressure {
	return observe.NewBackpressure(5, 100, time.Minute, 1)
}

func newIntake(t *testing.T, cfg config.Config, q queue.Queue, bp *observe.Backpressure, sw *intakeSwitch) http.Handler {
	t.Helper()
	srv := serveIntake(cfg, q, bp, sw, testLogger())
	if srv == nil {
		t.Fatal("serveIntake returned nil for a configured intake_addr")
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv.Handler
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postJob posts body to a fresh intake handler backed by a real memq and
// returns the response together with any job that reached a worker. Each
// call posts exactly one request, so the slice holds at most one job.
//
// The queue is drained either way: on a rejection this is what makes
// "the job was not enqueued" a real assertion instead of one that passes
// merely because the caller never looked. A short bound is enough there
// since a rejection never enqueues, so no delivery is ever actually
// pending; the longer bound on acceptance allows for real scheduling
// latency.
func postJob(t *testing.T, body string) (*httptest.ResponseRecorder, []queue.Job) {
	t.Helper()
	q := testMemq(t)
	h := newIntake(t, intakeConfig(), q, openBackpressure(), &intakeSwitch{})

	got := make(chan queue.Job, 1)
	if err := q.Start(t.Context(), func(_ context.Context, j queue.Job) (queue.Disposition, error) {
		got <- j
		return queue.Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	rec := post(h, body)
	wait := 200 * time.Millisecond
	if rec.Code == http.StatusAccepted {
		wait = 5 * time.Second
	}
	select {
	case j := <-got:
		return rec, []queue.Job{j}
	case <-time.After(wait):
		if rec.Code == http.StatusAccepted {
			t.Fatal("accepted job never reached a worker")
		}
		return rec, nil
	}
}

func testMemq(t *testing.T) *queue.Memq {
	t.Helper()
	q := queue.NewMemq(queue.QueueConfig{Workers: 1, Buffer: 4, DeadLetterMax: 10}, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = q.Close(ctx)
	})
	return q
}

func depth(t *testing.T, q queue.Queue) int {
	t.Helper()
	d, err := q.QueueDepth(context.Background())
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	return d
}

// TestServeIntakeDisabledWithoutAddr: intake is opt-in. An empty
// intake_addr must not quietly open a port.
func TestServeIntakeDisabledWithoutAddr(t *testing.T) {
	if srv := serveIntake(config.Config{}, testMemq(t), openBackpressure(), nil, testLogger()); srv != nil {
		_ = srv.Close()
		t.Error("serveIntake opened a server with no intake_addr configured")
	}
}

func TestIntakeAcceptsJob(t *testing.T) {
	q := testMemq(t)
	h := newIntake(t, intakeConfig(), q, openBackpressure(), &intakeSwitch{})

	rec := post(h, `{"kind":"file","ref":"clip.mp4"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["job_id"] == "" {
		t.Error("accepted job returned no job_id; the submitter cannot trace its result")
	}
	if got := depth(t, q); got != 1 {
		t.Errorf("queue depth = %d, want 1", got)
	}
}

// TestIntakeNormalizesJob: the queued payload must carry an ABSOLUTE path
// and a media type. A worker resolving a relative ref against its own
// working directory would scan the wrong file (or none), and a missing
// media type would skip frame extraction on a video.
func TestIntakeNormalizesJob(t *testing.T) {
	q := testMemq(t)
	h := newIntake(t, intakeConfig(), q, openBackpressure(), &intakeSwitch{})

	if rec := post(h, `{"kind":"file","ref":"clip.mp4","workflows":["keyframes"]}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	got := make(chan queue.Job, 1)
	if err := q.Start(t.Context(), func(_ context.Context, j queue.Job) (queue.Disposition, error) {
		got <- j
		return queue.Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case j := <-got:
		if !filepath.IsAbs(j.Source.Ref) {
			t.Errorf("queued ref %q is not absolute", j.Source.Ref)
		}
		if j.Source.MediaType != "video" {
			t.Errorf("media_type = %q, want video (inferred from .mp4)", j.Source.MediaType)
		}
		if j.Source.Kind != "file" {
			t.Errorf("kind = %q, want file", j.Source.Kind)
		}
		if len(j.Workflows) != 1 || j.Workflows[0] != "keyframes" {
			t.Errorf("workflows = %v, want [keyframes]", j.Workflows)
		}
		if j.SubmittedAt.IsZero() {
			t.Error("submitted_at not stamped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("job never reached a worker")
	}
}

// TestIntakeRejectsBadRequests: every rejection is explicit. v1 accepts file
// refs only, and an unknown workflow or out-of-range dedup override must be
// caught at INTAKE — accepting it here would surface as a job failure much
// later, after the submitter is gone.
func TestIntakeRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{"kind":`},
		{"empty body", ``},
		{"missing kind", `{"ref":"a.jpg"}`},
		{"unsupported kind", `{"kind":"s3","ref":"s3://bucket/a.jpg"}`},
		{"url with a denied scheme", `{"kind":"url","ref":"http://example.invalid/a.jpg"}`},
		{"missing ref", `{"kind":"file","ref":""}`},
		{"unknown workflow", `{"kind":"file","ref":"a.mp4","workflows":["nope"]}`},
		{"dedup threshold too high", `{"kind":"file","ref":"a.mp4","dedup_threshold":65}`},
		{"dedup threshold below -1", `{"kind":"file","ref":"a.mp4","dedup_threshold":-2}`},
		// Within the dHash width but ABOVE the configured ceiling of 8. At 64
		// every frame is a near-duplicate of frame 0, so honoring this would
		// scan one frame and let it decide the whole video's verdict.
		{"dedup threshold loosens past the config ceiling", `{"kind":"file","ref":"a.mp4","dedup_threshold":64}`},
		{"dedup threshold one past the config ceiling", `{"kind":"file","ref":"a.mp4","dedup_threshold":9}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := testMemq(t)
			h := newIntake(t, intakeConfig(), q, openBackpressure(), &intakeSwitch{})

			rec := post(h, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
			if got := depth(t, q); got != 0 {
				t.Errorf("rejected request still enqueued %d job(s)", got)
			}
		})
	}
}

// TestIntakeAcceptsValidDedupOverrides: -1 (disable) and 0..ceiling are the
// documented per-job range, and the override must reach the job unchanged —
// a dropped override silently reverts to the global config. The ceiling is
// frames.dedup.hamming_threshold (8 in intakeConfig); a job may tighten
// dedup or switch it off, never loosen it.
func TestIntakeAcceptsValidDedupOverrides(t *testing.T) {
	for _, v := range []int{-1, 0, 4, 8} {
		q := testMemq(t)
		h := newIntake(t, intakeConfig(), q, openBackpressure(), &intakeSwitch{})

		body := `{"kind":"file","ref":"a.mp4","dedup_threshold":` + strconv.Itoa(v) + `}`
		if rec := post(h, body); rec.Code != http.StatusAccepted {
			t.Errorf("dedup_threshold=%d: status = %d, want 202: %s", v, rec.Code, rec.Body)
		}
	}
}

// TestIntakeRejectsWhenBackpressureEngaged: under sustained provider failure
// intake sheds load with a RETRYABLE signal (503 + Retry-After). Accepting
// the job instead would pile work onto a provider that is already failing,
// and every one of those jobs ends at verdict=error.
func TestIntakeRejectsWhenBackpressureEngaged(t *testing.T) {
	bp := observe.NewBackpressure(2, 100, time.Minute, 1)
	bp.Record(false)
	bp.Record(false)
	if bp.Ready() {
		t.Fatal("backpressure did not engage after consecutive failures; test premise is wrong")
	}

	q := testMemq(t)
	h := newIntake(t, intakeConfig(), q, bp, &intakeSwitch{})

	rec := post(h, `{"kind":"file","ref":"a.jpg"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After: the rejection reads as permanent, so submitters drop the asset instead of resubmitting")
	}
	if got := depth(t, q); got != 0 {
		t.Errorf("shed request still enqueued %d job(s)", got)
	}
}

// TestIntakePauseResume covers the operator control surface the UI exposes:
// paused intake rejects retryably, resume restores acceptance.
func TestIntakePauseResume(t *testing.T) {
	sw := &intakeSwitch{}
	q := testMemq(t)
	h := newIntake(t, intakeConfig(), q, openBackpressure(), sw)

	if sw.IntakePaused() {
		t.Fatal("intake starts paused")
	}

	sw.PauseIntake()
	if !sw.IntakePaused() {
		t.Fatal("PauseIntake did not take effect")
	}
	rec := post(h, `{"kind":"file","ref":"a.jpg"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("paused status = %d, want 503: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("an operator pause is temporary by definition; it must carry Retry-After")
	}

	sw.ResumeIntake()
	if sw.IntakePaused() {
		t.Fatal("ResumeIntake did not take effect")
	}
	if rec := post(h, `{"kind":"file","ref":"a.jpg"}`); rec.Code != http.StatusAccepted {
		t.Errorf("resumed status = %d, want 202: %s", rec.Code, rec.Body)
	}
}

// TestIntakeRejectsWhenQueueFull: a full buffer is backpressure, not an
// error — 503 + Retry-After, and never a silent drop.
func TestIntakeRejectsWhenQueueFull(t *testing.T) {
	q := queue.NewMemq(queue.QueueConfig{Workers: 0, Buffer: 1, DeadLetterMax: 10}, testLogger())
	h := newIntake(t, intakeConfig(), q, openBackpressure(), &intakeSwitch{})

	if rec := post(h, `{"kind":"file","ref":"a.jpg"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("first job status = %d, want 202: %s", rec.Code, rec.Body)
	}
	rec := post(h, `{"kind":"file","ref":"b.jpg"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("full-queue status = %d, want 503: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a full queue is transient; the rejection must carry Retry-After")
	}
}

// TestIntakeRejectsWhenQueueClosed: after drain begins, new work is refused
// rather than accepted into a queue that will never run it.
func TestIntakeRejectsWhenQueueClosed(t *testing.T) {
	q := queue.NewMemq(queue.QueueConfig{Workers: 1, Buffer: 4, DeadLetterMax: 10}, testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h := newIntake(t, intakeConfig(), q, openBackpressure(), &intakeSwitch{})

	if rec := post(h, `{"kind":"file","ref":"a.jpg"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("closed-queue status = %d, want 503: %s", rec.Code, rec.Body)
	}
}

// TestNewQueueDrivers: the driver name is operator config, and an
// unrecognized one must fail at boot rather than default to the
// non-durable in-memory driver behind the operator's back.
func TestNewQueueDrivers(t *testing.T) {
	base := config.Config{Queue: config.QueueConfig{
		Driver: "memory", Workers: 2, Buffer: 8, DeadLetterMax: 10,
		RetryBackoff: time.Millisecond, DrainTimeout: time.Second, JobTimeout: time.Second,
	}}

	q, err := newQueue(base, testLogger())
	if err != nil {
		t.Fatalf("memory driver: %v", err)
	}
	if _, ok := q.(*queue.Memq); !ok {
		t.Errorf("driver=memory built %T, want *queue.Memq", q)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = q.Close(ctx)

	bad := base
	bad.Queue.Driver = "kafka"
	if _, err := newQueue(bad, testLogger()); err == nil {
		t.Error("an unknown queue.driver must fail fast, not silently fall back")
	}

	empty := base
	empty.Queue.Driver = ""
	if _, err := newQueue(empty, testLogger()); err == nil {
		t.Error("an empty queue.driver must fail fast")
	}
}

// TestQueueAccessorsForMemq: dlqOf/statesOf/activeOf feed the UI and the
// depth metrics. A nil return where a driver does support the capability
// blanks that surface with nothing logged.
func TestQueueAccessorsForMemq(t *testing.T) {
	q := testMemq(t)

	if dlqOf(q) == nil {
		t.Error("dlqOf returned nil for memq; dead-letter depth would never be exported")
	}
	states := statesOf(q)
	if states == nil {
		t.Fatal("statesOf returned nil for memq; the UI job table would be empty")
	}
	if got := states(); len(got) != 0 {
		t.Errorf("fresh queue reports %d job states, want 0", len(got))
	}
	active := activeOf(q)
	if active == nil {
		t.Fatal("activeOf returned nil for memq")
	}
	if got := active(); got != 0 {
		t.Errorf("active workers = %d on an idle queue, want 0", got)
	}
}

// TestQueueAccessorsForUnknownDriver: a driver that cannot answer must
// return nil so callers omit the surface rather than report a fake zero.
func TestQueueAccessorsForUnknownDriver(t *testing.T) {
	var q queue.Queue = stubQueue{}
	if dlqOf(q) != nil {
		t.Error("dlqOf invented a DLQ for a driver that has none")
	}
	if statesOf(q) != nil {
		t.Error("statesOf invented a state map for a driver that has none")
	}
	if activeOf(q) != nil {
		t.Error("activeOf invented a worker count for a driver that has none")
	}
}

type stubQueue struct{}

func (stubQueue) Enqueue(context.Context, queue.Job) (queue.JobID, error) { return "", nil }
func (stubQueue) Start(context.Context, queue.Handler) error              { return nil }
func (stubQueue) QueueDepth(context.Context) (int, error)                 { return 0, nil }
func (stubQueue) Close(context.Context) error                             { return nil }

// urlIntakeConfig is intakeConfig with one allow-listed media host.
func urlIntakeConfig() config.Config {
	cfg := intakeConfig()
	cfg.Source.URL.AllowHosts = []string{"media.example.com"}
	return cfg
}

// With no allow_hosts configured a url job is accepted: the feature works
// out of the box, and the address policy — not the host list — is what
// keeps a job off non-public infrastructure.
func TestIntakeAcceptsURLKindWithAnEmptyAllowList(t *testing.T) {
	q := testMemq(t)
	h := newIntake(t, intakeConfig(), q, openBackpressure(), &intakeSwitch{})
	rec := post(h, `{"kind":"url","ref":"https://media.example.com/clip.mp4","media_type":"video"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if got := depth(t, q); got != 1 {
		t.Errorf("queue depth = %d, want 1", got)
	}
}

// A populated allow_hosts narrows: a host that is not on it is refused.
func TestIntakeRejectsHostOutsideAPopulatedAllowList(t *testing.T) {
	h := newIntake(t, urlIntakeConfig(), testMemq(t), openBackpressure(), &intakeSwitch{})
	rec := post(h, `{"kind":"url","ref":"https://evil.example.com/clip.mp4","media_type":"video"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "source.url.allow_hosts") {
		t.Errorf("body should name the allow-list: %s", rec.Body.String())
	}
}

// http is accepted only for a host in allow_private_hosts.
func TestIntakeAcceptsHTTPForAPrivateHostOnly(t *testing.T) {
	cfg := intakeConfig()
	cfg.Source.URL.AllowPrivateHosts = []string{"host.docker.internal"}

	q := testMemq(t)
	h := newIntake(t, cfg, q, openBackpressure(), &intakeSwitch{})
	if rec := post(h, `{"kind":"url","ref":"http://host.docker.internal:8000/clip.mp4","media_type":"video"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, `{"kind":"url","ref":"http://media.example.com/clip.mp4","media_type":"video"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("http from a non-private host: status = %d, want 400", rec.Code)
	}
	if got := depth(t, q); got != 1 {
		t.Errorf("queue depth = %d, want 1", got)
	}
}

func TestIntakeAcceptsAllowListedURL(t *testing.T) {
	q := testMemq(t)
	h := newIntake(t, urlIntakeConfig(), q, openBackpressure(), &intakeSwitch{})
	rec := post(h, `{"kind":"url","ref":"https://media.example.com/clip.mp4","media_type":"video"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if got := depth(t, q); got != 1 {
		t.Errorf("queue depth = %d, want 1", got)
	}
}

// The queued url must be the FULL original: the fetcher needs the
// presigned query string to authenticate. Redaction happens at the
// recording boundary (pipeline), not at intake.
func TestIntakeQueuesTheFullURLUnmodified(t *testing.T) {
	q := testMemq(t)
	h := newIntake(t, urlIntakeConfig(), q, openBackpressure(), &intakeSwitch{})
	const ref = "https://media.example.com/clip.mp4?sig=secret"
	if rec := post(h, `{"kind":"url","ref":"`+ref+`"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	got := make(chan queue.Job, 1)
	if err := q.Start(t.Context(), func(_ context.Context, j queue.Job) (queue.Disposition, error) {
		got <- j
		return queue.Ack, nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case j := <-got:
		if j.Source.Ref != ref {
			t.Errorf("queued ref = %q, want the untouched url", j.Source.Ref)
		}
		if j.Source.Kind != "url" {
			t.Errorf("kind = %q, want url", j.Source.Kind)
		}
		if j.Source.MediaType != "video" {
			t.Errorf("media_type = %q, want video (inferred from the redacted path)", j.Source.MediaType)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("job never reached a worker")
	}
}

func TestIntakeRejectsBadURLs(t *testing.T) {
	for name, ref := range map[string]string{
		"not allow-listed": "https://evil.example.com/clip.mp4",
		"http scheme":      "http://media.example.com/clip.mp4",
		"userinfo":         "https://u:p@media.example.com/clip.mp4",
		"metadata ip":      "https://169.254.169.254/clip.mp4",
	} {
		t.Run(name, func(t *testing.T) {
			h := newIntake(t, urlIntakeConfig(), testMemq(t), openBackpressure(), &intakeSwitch{})
			rec := post(h, `{"kind":"url","ref":"`+ref+`","media_type":"video"}`)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %s", rec.Code, ref)
			}
		})
	}
}

func TestIntakeDoesNotEchoTheQueryString(t *testing.T) {
	h := newIntake(t, urlIntakeConfig(), testMemq(t), openBackpressure(), &intakeSwitch{})
	rec := post(h, `{"kind":"url","ref":"https://evil.example.com/c.mp4?sig=secret","media_type":"video"}`)
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("error response echoed the credential: %s", rec.Body.String())
	}
}

func TestIntakeStillRejectsUnknownKinds(t *testing.T) {
	h := newIntake(t, urlIntakeConfig(), testMemq(t), openBackpressure(), &intakeSwitch{})
	if rec := post(h, `{"kind":"s3","ref":"s3://b/k","media_type":"video"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// Metadata is optional, opaque, and capped. Accepted metadata is stored
// COMPACTED, because the cap must measure content, not indentation.
func TestIntakeMetadata(t *testing.T) {
	t.Run("accepted and compacted", func(t *testing.T) {
		body := `{"kind":"file","ref":"/data/a.png","metadata":{ "ticket" : "T-1" }}`
		rec, jobs := postJob(t, body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("got %d, want 202: %s", rec.Code, rec.Body.String())
		}
		if len(jobs) != 1 {
			t.Fatalf("want 1 enqueued job, got %d", len(jobs))
		}
		if string(jobs[0].Metadata) != `{"ticket":"T-1"}` {
			t.Errorf("metadata must be stored compacted, got %s", jobs[0].Metadata)
		}
	})

	t.Run("omitted stays nil", func(t *testing.T) {
		rec, jobs := postJob(t, `{"kind":"file","ref":"/data/a.png"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("got %d, want 202", rec.Code)
		}
		if jobs[0].Metadata != nil {
			t.Errorf("absent metadata must stay nil, got %s", jobs[0].Metadata)
		}
	})

	for name, meta := range map[string]string{
		"array":    `["a"]`,
		"scalar":   `"a"`,
		"oversize": `{"k":"` + strings.Repeat("a", queue.MaxMetadataBytes) + `"}`,
	} {
		t.Run("rejected: "+name, func(t *testing.T) {
			body := `{"kind":"file","ref":"/data/a.png","metadata":` + meta + `}`
			rec, jobs := postJob(t, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", rec.Code)
			}
			if len(jobs) != 0 {
				t.Errorf("a rejected job must not be enqueued, got %d", len(jobs))
			}
		})
	}
}

// url sources are usable with no source.url block at all.
func TestFetcherBootsFromDefaults(t *testing.T) {
	f, err := newFetcher(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.Fatal("the default config must still yield a usable fetcher")
	}
}

// gaugeValue reads a Prometheus gauge's current value.
//
// Uses client_golang's own testutil rather than reaching for client_model
// directly: that would promote client_model from an indirect dependency to
// a direct one for the sake of a test assertion.
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	return testutil.ToFloat64(g)
}

// depthQueue is a queue whose depths are scripted, including failures.
type depthQueue struct {
	queue.Queue
	depth, processing int
	depthErr, procErr error
}

func (d *depthQueue) QueueDepth(context.Context) (int, error) {
	return d.depth, d.depthErr
}
func (d *depthQueue) ProcessingDepth(context.Context) (int, error) {
	return d.processing, d.procErr
}

// Jobs parked in processing are excluded from the autoscaling signal by
// design, so they need their own gauge — otherwise a payload stranded by a
// failed dead-letter write is in a queue nothing counts, while queue_depth
// reads 0 and the autoscaler scales to zero on top of it.
func TestPublishDepthGaugesReportsProcessingSeparately(t *testing.T) {
	m := observe.NewMetrics()
	q := &depthQueue{depth: 7, processing: 3}

	publishDepthGauges(context.Background(), q, m)

	if got := gaugeValue(t, m.QueueDepth); got != 7 {
		t.Errorf("queue depth gauge = %v, want 7", got)
	}
	if got := gaugeValue(t, m.ProcessingDepth); got != 3 {
		t.Errorf("processing depth gauge = %v, want 3", got)
	}
}

// A driver with no in-flight concept (memq) must simply not publish the
// gauge, rather than publishing a misleading zero.
func TestPublishDepthGaugesSkipsDriversWithoutProcessing(t *testing.T) {
	m := observe.NewMetrics()
	m.ProcessingDepth.Set(42) // a previous sample

	q := testMemq(t)
	publishDepthGauges(context.Background(), q, m)

	if got := gaugeValue(t, m.ProcessingDepth); got != 42 {
		t.Errorf("processing gauge = %v, want the previous 42 left untouched", got)
	}
}

// A failed sample must leave the last known value alone. Writing 0 on a
// Redis blip is indistinguishable from an empty queue and can talk the
// autoscaler into scaling down mid-backlog.
func TestPublishDepthGaugesLeavesStaleValuesOnError(t *testing.T) {
	m := observe.NewMetrics()
	m.QueueDepth.Set(11)
	m.ProcessingDepth.Set(5)

	q := &depthQueue{
		depth: 0, processing: 0,
		depthErr: errors.New("redis is down"),
		procErr:  errors.New("redis is down"),
	}
	publishDepthGauges(context.Background(), q, m)

	if got := gaugeValue(t, m.QueueDepth); got != 11 {
		t.Errorf("queue depth gauge = %v, want 11 (a failed sample must not write 0)", got)
	}
	if got := gaugeValue(t, m.ProcessingDepth); got != 5 {
		t.Errorf("processing gauge = %v, want 5 (a failed sample must not write 0)", got)
	}
}
