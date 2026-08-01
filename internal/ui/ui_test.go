package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/queue"
)

type fakeIntake struct{ paused atomic.Bool }

func (f *fakeIntake) PauseIntake()       { f.paused.Store(true) }
func (f *fakeIntake) ResumeIntake()      { f.paused.Store(false) }
func (f *fakeIntake) IntakePaused() bool { return f.paused.Load() }

func newTestServer(t *testing.T, auth string) (*Server, *fakeIntake) {
	t.Helper()
	cfg := config.Defaults()
	cfg.UI.Auth = auth
	cfg.Adapter.Name = "microsoft"
	q := queue.NewMemq(queue.QueueConfig{Workers: 1}, nil)
	t.Cleanup(func() { _ = q.Close(t.Context()) })
	intake := &fakeIntake{}
	tracker := observe.NewJobTracker(50)
	s := New(cfg, q, q.DLQ(), q.States, q.ActiveWorkers, intake, tracker, nil)
	return s, intake
}

func do(t *testing.T, s *Server, method, path, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	var h http.HandlerFunc
	switch path {
	case "/api/status":
		h = s.auth(s.status)
	case "/api/intake/pause":
		h = s.auth(s.pause)
	case "/api/intake/resume":
		h = s.auth(s.resume)
	default:
		h = s.auth(s.index)
	}
	h(rec, req)
	return rec
}

func TestAuthRequired(t *testing.T) {
	t.Setenv("VISMOD_UI_USER", "op")
	t.Setenv("VISMOD_UI_PASSWORD", "secret")
	s, _ := newTestServer(t, "basic")

	if rec := do(t, s, "GET", "/api/status", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credentials must 401, got %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/api/status", "op", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad password must 401, got %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/api/status", "op", "secret"); rec.Code != http.StatusOK {
		t.Errorf("valid credentials must 200, got %d", rec.Code)
	}
}

func TestBasicAuthWithoutCredentialsRefusesService(t *testing.T) {
	t.Setenv("VISMOD_UI_USER", "")
	t.Setenv("VISMOD_UI_PASSWORD", "")
	s, _ := newTestServer(t, "basic")
	if rec := do(t, s, "GET", "/api/status", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("basic auth without configured credentials must refuse (503), got %d", rec.Code)
	}
}

func TestStatusExposesCountsNeverSecretsOrMedia(t *testing.T) {
	t.Setenv("VISMOD_MICROSOFT_API_KEY", "super-secret-key")
	s, _ := newTestServer(t, "none")
	rec := do(t, s, "GET", "/api/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "super-secret-key") {
		t.Fatal("UI leaked a secret")
	}
	var p statusPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Adapter != "microsoft" || p.QueueDriver != "memory" {
		t.Errorf("payload = %+v", p)
	}
	if len(p.Thresholds) == 0 {
		t.Error("thresholds should be visible (read-only)")
	}
	// Standard workflows surface with full detail in the config tab.
	wf, ok := p.Workflows["scene-detect"]
	if !ok || wf.Description == "" || len(wf.Args) == 0 {
		t.Errorf("workflow detail missing: %+v", p.Workflows)
	}
	if p.DefaultWF != "scene-detect" || p.MaxFrames <= 0 {
		t.Errorf("extraction config missing: default=%q max_frames=%d", p.DefaultWF, p.MaxFrames)
	}
}

func TestStatusIncludesVerdictsAndRecentJobs(t *testing.T) {
	s, _ := newTestServer(t, "none")
	now := time.Now().UTC()
	score := 0.42
	for _, v := range []string{"allow", "allow", "flag", "block", "error"} {
		rec := observe.JobRecord{
			ID: "j-" + v, Ref: "/data/" + v + ".png", MediaType: "image",
			Verdict: v, FinishedAt: now, DurationMS: 42, FramesScanned: 1,
		}
		if v != "error" {
			rec.MaxScore, rec.Confidence = &score, &score
		}
		s.tracker.Record(rec)
	}
	s.tracker.Record(observe.JobRecord{
		ID: "j-vid", Ref: "/data/v.mp4", MediaType: "video", Verdict: "flag",
		FinishedAt: now, FramesScanned: 12, FramesFlagged: 1,
	})
	rec := do(t, s, "GET", "/api/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var p statusPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Verdicts.Total != 6 || p.Verdicts.Evaluated != 5 {
		t.Errorf("total=%d evaluated=%d, want 6/5", p.Verdicts.Total, p.Verdicts.Evaluated)
	}
	// flag/block over the 5 evaluated (2 allow, 2 flag, 1 block); the
	// errored job doesn't dilute them. Error rate over all 6.
	if p.Verdicts.FlagRate != 0.6 {
		t.Errorf("flag_rate = %v, want 0.6", p.Verdicts.FlagRate)
	}
	if p.Verdicts.BlockRate != 0.2 || p.Verdicts.ErrorRate != 1.0/6.0 {
		t.Errorf("block_rate=%v error_rate=%v", p.Verdicts.BlockRate, p.Verdicts.ErrorRate)
	}
	if len(p.RecentJobs) != 6 {
		t.Fatalf("recent = %d, want 6", len(p.RecentJobs))
	}
	if p.RecentJobs[0].Verdict != "flag" || p.RecentJobs[0].FramesScanned != 12 {
		t.Errorf("newest-first with frame counts, got %+v", p.RecentJobs[0])
	}
	// error job (index 1 now): nil scores (never 0); scored jobs carry them.
	if p.RecentJobs[1].MaxScore != nil {
		t.Error("error job must have nil max_score")
	}
	if p.RecentJobs[2].MaxScore == nil || *p.RecentJobs[2].MaxScore != 0.42 ||
		p.RecentJobs[2].Confidence == nil {
		t.Errorf("scored job must carry max_score/confidence: %+v", p.RecentJobs[2])
	}
	// Frame-extraction aggregates surface for workflow tuning.
	if p.Frames.TotalFrames != 17 || p.Frames.VideoJobs != 1 {
		t.Errorf("frames = %+v, want total 17 / 1 video job", p.Frames)
	}
	if p.Frames.AvgFramesPerVideo != 12 || p.Frames.SingleFrameFlagVideos != 1 {
		t.Errorf("frames = %+v, want avg 12 / 1 single-frame flag", p.Frames)
	}
}

func TestPauseResume(t *testing.T) {
	s, intake := newTestServer(t, "none")
	if rec := do(t, s, "POST", "/api/intake/pause", "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("pause = %d", rec.Code)
	}
	if !intake.IntakePaused() {
		t.Fatal("pause did not flip the intake switch")
	}
	if rec := do(t, s, "POST", "/api/intake/resume", "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("resume = %d", rec.Code)
	}
	if intake.IntakePaused() {
		t.Fatal("resume did not clear the intake switch")
	}
}

func TestIndexServesEmbeddedPage(t *testing.T) {
	s, _ := newTestServer(t, "none")
	rec := do(t, s, "GET", "/", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "vismod") {
		t.Errorf("index = %d", rec.Code)
	}
}
