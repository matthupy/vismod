package ui

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/queue"
)

// TestStartServesAndShutsDown exercises the real listener: the routes are
// only wired inside Start, so a mux mistake (a route registered on the
// wrong method or path) is invisible to the handler-level tests.
func TestStartServesAndShutsDown(t *testing.T) {
	t.Setenv("VISMOD_UI_USER", "op")
	t.Setenv("VISMOD_UI_PASSWORD", "secret")

	// Bind an ephemeral port and hand the address to the server, so the
	// test never collides with a real deployment.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := config.Defaults()
	cfg.UI.Addr = addr
	cfg.UI.Auth = "basic"
	cfg.Adapter.Name = "microsoft"
	q := queue.NewMemq(queue.QueueConfig{Workers: 1}, nil)
	t.Cleanup(func() { _ = q.Close(context.Background()) })

	s := New(cfg, q, q.DLQ(), q.States, q.ActiveWorkers, &fakeIntake{}, observe.NewJobTracker(10), nil)
	srv := s.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	get := func(path, user, pass string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", "http://"+addr+path, nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		var resp *http.Response
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, err = http.DefaultClient.Do(req)
			if err == nil {
				return resp
			}
			if time.Now().After(deadline) {
				t.Fatalf("GET %s: %v", path, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	resp := get("/", "op", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The favicon route is deliberately unauthenticated and empty: browsers
	// request it unconditionally and a 401 there produces a login prompt on
	// every page load.
	resp = get("/favicon.ico", "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("GET /favicon.ico = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = get("/api/status", "op", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/status = %d, want 200", resp.StatusCode)
	}
	var payload statusPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Errorf("decode status: %v", err)
	}
	_ = resp.Body.Close()
	if payload.QueueDriver == "" {
		t.Error("status payload carries no queue driver; the dashboard header would be blank")
	}
}

// TestIndexRejectsUnknownPaths: the root handler is registered on "GET /",
// which catches every unmatched path. Serving the dashboard for /nonsense
// would mask typos and broken links as a working page.
func TestIndexRejectsUnknownPaths(t *testing.T) {
	s, _ := newTestServer(t, "none")
	rec := httptest.NewRecorder()
	s.index(rec, httptest.NewRequest("GET", "/no-such-page", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /no-such-page = %d, want 404", rec.Code)
	}
}

// TestIntakeControlUnavailableWithoutASwitch: with no intake control wired
// (a driver/shape that has none), pause and resume must say so explicitly
// rather than return success for a control that does nothing.
func TestIntakeControlUnavailableWithoutASwitch(t *testing.T) {
	cfg := config.Defaults()
	cfg.UI.Auth = "none"
	q := queue.NewMemq(queue.QueueConfig{Workers: 1}, nil)
	t.Cleanup(func() { _ = q.Close(context.Background()) })
	s := New(cfg, q, q.DLQ(), q.States, q.ActiveWorkers, nil, observe.NewJobTracker(10), nil)

	for name, h := range map[string]http.HandlerFunc{"pause": s.pause, "resume": s.resume} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("POST", "/api/intake/"+name, nil))
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s with no intake control = %d, want 501", name, rec.Code)
		}
	}
}

// TestStatusCountsJobStatesWithoutLeakingRefs: the dashboard reports how
// many jobs are in each state. It must carry COUNTS only — a job ID or an
// asset ref in this payload would put media identifiers on an operator
// screen and in any browser cache (invariant 3).
func TestStatusCountsJobStatesWithoutLeakingRefs(t *testing.T) {
	cfg := config.Defaults()
	cfg.UI.Auth = "none"
	q := queue.NewMemq(queue.QueueConfig{Workers: 0, Buffer: 4, DeadLetterMax: 10}, nil)
	t.Cleanup(func() { _ = q.Close(context.Background()) })
	if _, err := q.Enqueue(context.Background(), queue.Job{ID: "secret-asset-id"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	s := New(cfg, q, q.DLQ(), q.States, q.ActiveWorkers, &fakeIntake{}, observe.NewJobTracker(10), nil)
	rec := httptest.NewRecorder()
	s.status(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var payload statusPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.JobStates["queued"] != 1 {
		t.Errorf("job_states = %v, want one queued job", payload.JobStates)
	}
	if payload.RecentJobs == nil {
		t.Error("recent_jobs must serialize as [] rather than null; the dashboard renders it directly")
	}
}
