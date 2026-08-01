// Package ui is the STRETCH read-mostly operator dashboard, served by
// the same binary behind ui.enabled (default off) and auth.
//
// Hard rules (§J): the UI never renders media bytes, provider Raw
// payloads, OCR text, captions, or PII — hashes, verdicts, counts, and
// configuration metadata only. Secrets are never exposed. Config is
// read-only (changes are restart-to-apply). "Manage workers" means
// operational controls only: pause/resume intake through the
// backpressure seam — never arbitrary code or config mutation. The
// pipeline is fully functional headless without this package.
package ui

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/queue"
)

//go:embed static/index.html
var static embed.FS

// IntakeControl is the operational pause/resume seam shared with the
// intake endpoint.
type IntakeControl interface {
	PauseIntake()
	ResumeIntake()
	IntakePaused() bool
}

// Server serves the dashboard.
type Server struct {
	cfg       config.Config
	q         queue.Queue
	dlq       queue.DeadLetterSink
	states    func() map[queue.JobID]string // nil when the driver has no local state view
	active    func() int
	intake    IntakeControl
	tracker   *observe.JobTracker
	log       *slog.Logger
	user      string
	pass      string
	startedAt time.Time
}

// New builds the UI server. Basic-auth credentials are env-only
// (VISMOD_UI_USER / VISMOD_UI_PASSWORD); with auth "basic" and no
// credentials set, the UI refuses every request rather than serving
// unauthenticated.
func New(cfg config.Config, q queue.Queue, dlq queue.DeadLetterSink, states func() map[queue.JobID]string, active func() int, intake IntakeControl, tracker *observe.JobTracker, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg: cfg, q: q, dlq: dlq, states: states, active: active,
		intake: intake, tracker: tracker, log: log,
		user: os.Getenv("VISMOD_UI_USER"), pass: os.Getenv("VISMOD_UI_PASSWORD"),
		startedAt: time.Now().UTC(),
	}
}

// Start launches the UI HTTP server; returns it for graceful shutdown.
func (s *Server) Start() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.auth(s.index))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/status", s.auth(s.status))
	mux.HandleFunc("POST /api/intake/pause", s.auth(s.pause))
	mux.HandleFunc("POST /api/intake/resume", s.auth(s.resume))
	srv := &http.Server{Addr: s.cfg.UI.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Error("ui server failed", "addr", s.cfg.UI.Addr, "err", err)
		}
	}()
	s.log.Info("ui enabled", "addr", s.cfg.UI.Addr, "auth", s.cfg.UI.Auth)
	return srv
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	if s.cfg.UI.Auth == "none" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if s.user == "" || s.pass == "" {
			http.Error(w, "ui auth is 'basic' but VISMOD_UI_USER/VISMOD_UI_PASSWORD are not set", http.StatusServiceUnavailable)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(s.user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(s.pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="vismod"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := static.ReadFile("static/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// statusPayload carries counts and metadata ONLY — no media, no Raw, no
// asset refs, no secrets.
type statusPayload struct {
	UptimeSec     int64             `json:"uptime_sec"`
	QueueDriver   string            `json:"queue_driver"`
	QueueDepth    int               `json:"queue_depth"`
	DLQDepth      int               `json:"dlq_depth"`
	WorkersActive int               `json:"workers_active"`
	WorkersPool   int               `json:"workers_pool"`
	IntakePaused  bool              `json:"intake_paused"`
	JobStates     map[string]int    `json:"job_states"`
	Adapter       string            `json:"adapter"`
	Thresholds    config.Thresholds `json:"thresholds"`
	// Workflows carries the full standard + custom workflow definitions
	// (name, description, arg template) — configuration metadata only.
	Workflows   map[string]config.WorkflowConfig `json:"workflows"`
	DefaultWF   string                           `json:"default_workflow"`
	MaxFrames   int                              `json:"max_frames"`
	MaxWidth    int                              `json:"max_width"`
	MetricsAddr string                           `json:"metrics_addr"`
	// Verdict outcomes: lifetime aggregate rates plus the most recent
	// finished jobs (opaque refs + verdict metadata only).
	Verdicts   observe.VerdictStats `json:"verdicts"`
	Frames     observe.FrameStats   `json:"frames"`
	RecentJobs []observe.JobRecord  `json:"recent_jobs"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	p := statusPayload{
		UptimeSec:   int64(time.Since(s.startedAt).Seconds()),
		QueueDriver: s.cfg.Queue.Driver,
		WorkersPool: s.cfg.Queue.Workers,
		Adapter:     s.cfg.Adapter.Name,
		Thresholds:  s.cfg.Thresholds,
		Workflows:   s.cfg.FFmpeg.Workflows,
		DefaultWF:   s.cfg.FFmpeg.DefaultWorkflow,
		MaxFrames:   s.cfg.FFmpeg.MaxFrames,
		MaxWidth:    s.cfg.FFmpeg.MaxWidth,
		MetricsAddr: s.cfg.MetricsAddr,
		JobStates:   map[string]int{},
	}
	if d, err := s.q.QueueDepth(ctx); err == nil {
		p.QueueDepth = d
	}
	if s.dlq != nil {
		if d, err := s.dlq.Depth(ctx); err == nil {
			p.DLQDepth = d
		}
	}
	if s.active != nil {
		p.WorkersActive = s.active()
	}
	if s.intake != nil {
		p.IntakePaused = s.intake.IntakePaused()
	}
	if s.states != nil {
		for _, st := range s.states() {
			p.JobStates[st]++ // counts only; job IDs/refs never leave the process
		}
	}
	if s.tracker != nil {
		p.RecentJobs, p.Verdicts = s.tracker.Snapshot()
		p.Frames = s.tracker.FrameSnapshot()
		if p.RecentJobs == nil {
			p.RecentJobs = []observe.JobRecord{}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

func (s *Server) pause(w http.ResponseWriter, _ *http.Request) {
	if s.intake == nil {
		http.Error(w, "intake control unavailable", http.StatusNotImplemented)
		return
	}
	s.intake.PauseIntake()
	s.log.Warn("intake PAUSED via operator UI")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resume(w http.ResponseWriter, _ *http.Request) {
	if s.intake == nil {
		http.Error(w, "intake control unavailable", http.StatusNotImplemented)
		return
	}
	s.intake.ResumeIntake()
	s.log.Warn("intake RESUMED via operator UI")
	w.WriteHeader(http.StatusNoContent)
}
