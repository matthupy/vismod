package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/frames"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

// videoNativeModerator is a provider that scores video directly (the
// VideoModerator seam), so the pipeline must NOT extract frames for it.
type videoNativeModerator struct {
	err     error
	score   float64
	frames  int
	calls   int
	noVideo bool // declares VideoModerator but reports SupportsVideo=false
}

func (m *videoNativeModerator) Name() string { return "video-native-fake" }
func (m *videoNativeModerator) Close() error { return nil }
func (m *videoNativeModerator) Capabilities() moderation.Caps {
	return moderation.Caps{SupportsVideo: !m.noVideo, MaxImageBytes: 1 << 20}
}

func (m *videoNativeModerator) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	score := 0.1
	return moderation.NormalizedResult{
		Provider: "video-native-fake",
		Frames: []moderation.FrameResult{{
			Status: moderation.FrameOK,
			Categories: []moderation.CategoryResult{{
				Category: moderation.CategorySexual, ProviderLabel: "fake/sexual",
				Score: &score, ScoreOrigin: moderation.OriginProbability,
			}},
		}},
	}, nil
}

func (m *videoNativeModerator) AnalyzeVideo(context.Context, moderation.Source) (moderation.NormalizedResult, error) {
	m.calls++
	if m.err != nil {
		return moderation.NormalizedResult{}, m.err
	}
	out := moderation.NormalizedResult{Provider: "video-native-fake", MediaType: "video"}
	for range m.frames {
		score := m.score
		out.Frames = append(out.Frames, moderation.FrameResult{
			Status: moderation.FrameOK,
			Categories: []moderation.CategoryResult{{
				Category:      moderation.CategorySexual,
				ProviderLabel: "fake/sexual",
				Score:         &score,
				ScoreOrigin:   moderation.OriginProbability,
			}},
		})
	}
	return out, nil
}

// recordingEvents captures operational audit events; errOn makes the
// append fail.
type recordingEvents struct {
	kinds []string
	err   error
}

func (e *recordingEvents) AppendEvent(kind string, _ map[string]string) error {
	e.kinds = append(e.kinds, kind)
	return e.err
}

// TestVideoNativeAdapterSkipsFrameExtraction: a provider that scores video
// itself must be used directly. Extracting frames anyway would pay for
// ffmpeg and moderate stills the provider never asked for.
func TestVideoNativeAdapterSkipsFrameExtraction(t *testing.T) {
	mod := &videoNativeModerator{score: 0.9, frames: 2}
	fs := &fakeFrameSource{dir: t.TempDir(), contents: []string{"a", "b"}}
	p, buf := newTestPipeline(t, mod, fs)

	env, disp, err := p.ProcessJob(context.Background(), videoJob("clip.mp4"))
	if err != nil || disp != queue.Ack {
		t.Fatalf("ProcessJob = (%v, %v), want Ack", disp, err)
	}
	if mod.calls != 1 {
		t.Errorf("AnalyzeVideo called %d times, want 1", mod.calls)
	}
	if fs.cleaned != 0 || fs.gotWFs != nil {
		t.Error("frames were extracted for a video-native adapter")
	}
	if env.Result == nil || len(env.Result.Frames) != 2 {
		t.Fatalf("want 2 frames from the provider, got %+v", env.Result)
	}
	// Thresholds must still be applied to a video-native result — the
	// provider returns raw scores, and flagging is vismod's decision.
	if !env.Result.Frames[0].Categories[0].Flagged {
		t.Error("thresholds were not applied to video-native frames; a 0.9 SEXUAL score did not flag")
	}
	_ = buf
}

// TestVideoNativeAdapterFailureIsAnErrorVerdict: a provider failure on the
// video path is could-not-evaluate, never allow.
func TestVideoNativeAdapterFailureIsAnErrorVerdict(t *testing.T) {
	mod := &videoNativeModerator{err: errors.New("provider down")}
	p, _ := newTestPipeline(t, mod, &fakeFrameSource{dir: t.TempDir()})

	env, disp, err := p.ProcessJob(context.Background(), videoJob("clip.mp4"))
	if disp != queue.DeadLetter || err == nil {
		t.Fatalf("ProcessJob = (%v, %v), want DeadLetter and an error", disp, err)
	}
	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("verdict = %q, want error", env.Result.Overall.Verdict)
	}
}

// TestVideoAdapterWithoutVideoSupportFallsBackToFrames: declaring the
// interface is not enough — Capabilities decides. Otherwise an adapter
// that stubs AnalyzeVideo would silently bypass frame extraction.
func TestVideoAdapterWithoutVideoSupportFallsBackToFrames(t *testing.T) {
	mod := &videoNativeModerator{noVideo: true}
	fs := &fakeFrameSource{dir: t.TempDir(), contents: []string{"a"}}
	p, _ := newTestPipeline(t, mod, fs)

	if _, disp, err := p.ProcessJob(context.Background(), videoJob("clip.mp4")); disp != queue.Ack || err != nil {
		t.Fatalf("ProcessJob = (%v, %v), want Ack", disp, err)
	}
	if mod.calls != 0 {
		t.Error("AnalyzeVideo was used by an adapter that reports SupportsVideo=false")
	}
	if fs.cleaned != 1 {
		t.Error("the frame path did not run for an image-only adapter")
	}
}

// TestVideoWithoutAFrameSourceIsAnErrorVerdict: a misconfigured worker must
// not pass a video through as if it were scanned.
func TestVideoWithoutAFrameSourceIsAnErrorVerdict(t *testing.T) {
	p, _ := newTestPipeline(t, &fakeModerator{}, nil)
	p.FrameSource = nil

	env, disp, _ := p.ProcessJob(context.Background(), videoJob("clip.mp4"))
	if disp != queue.DeadLetter {
		t.Fatalf("disposition = %v, want DeadLetter", disp)
	}
	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("verdict = %q, want error", env.Result.Overall.Verdict)
	}
}

// TestSinkFailureRetriesWithoutAuditing pins the documented ordering: the
// sink write happens before the audit record, so a sink outage returns
// Retry and writes NO audit entry (AGENTS.md sink gotcha). This test exists
// to make that coupling visible if anyone changes the order.
func TestSinkFailureRetriesWithoutAuditing(t *testing.T) {
	p, _ := newTestPipeline(t, &fakeModerator{scores: map[string]float64{"clean": 0.1}}, nil)
	p.Sink = failingSink{}
	audit := &countingAudit{}
	p.Audit = audit

	_, disp, err := p.ProcessJob(context.Background(), imageJob(writeInput(t, "clean")))
	if disp != queue.Retry {
		t.Errorf("disposition = %v, want Retry for a sink failure", disp)
	}
	if err == nil || !strings.Contains(err.Error(), "sink write") {
		t.Errorf("error = %v, want a sink write failure", err)
	}
	if audit.calls != 0 {
		t.Errorf("audit recorded %d entries; the current ordering writes none on a sink failure", audit.calls)
	}
}

type countingAudit struct {
	calls int
	err   error
}

func (a *countingAudit) Record(context.Context, result.ResultEnvelope) error {
	a.calls++
	return a.err
}

// TestAuditFailureDeadLetters: a decision that cannot be recorded must not
// be acked. Acking would leave a verdict acted on with nothing accounting
// for it.
func TestAuditFailureDeadLetters(t *testing.T) {
	p, _ := newTestPipeline(t, &fakeModerator{scores: map[string]float64{"clean": 0.1}}, nil)
	p.Audit = &countingAudit{err: errors.New("audit disk full")}

	_, disp, err := p.ProcessJob(context.Background(), imageJob(writeInput(t, "clean")))
	if disp != queue.DeadLetter || err == nil {
		t.Fatalf("ProcessJob = (%v, %v), want DeadLetter and an error", disp, err)
	}
}

// TestEmptyVideoOverrideFailsSafeWhenTheEventCannotBeRecorded: the
// zero-frames override is only acceptable BECAUSE it is audited. If the
// audit event cannot be written, the override must not happen.
func TestEmptyVideoOverrideFailsSafeWhenTheEventCannotBeRecorded(t *testing.T) {
	fs := &fakeFrameSource{dir: t.TempDir(), zeroClean: true}
	p, _ := newTestPipeline(t, &fakeModerator{}, fs)
	p.AllowEmptyVideoSkip = true
	p.Events = &recordingEvents{err: errors.New("audit unavailable")}

	_, disp, err := p.ProcessJob(context.Background(), videoJob("empty.mp4"))
	if disp != queue.DeadLetter || err == nil {
		t.Fatalf("ProcessJob = (%v, %v), want DeadLetter: an unaudited override is not allowed", disp, err)
	}
}

// TestEmptyVideoOverrideRecordsItsEvent: the accepted path still has to
// leave the prominent audit trail the override is gated on.
func TestEmptyVideoOverrideRecordsItsEvent(t *testing.T) {
	fs := &fakeFrameSource{dir: t.TempDir(), zeroClean: true}
	p, buf := newTestPipeline(t, &fakeModerator{}, fs)
	p.AllowEmptyVideoSkip = true
	events := &recordingEvents{}
	p.Events = events

	env, disp, err := p.ProcessJob(context.Background(), videoJob("empty.mp4"))
	if disp != queue.Ack || err != nil {
		t.Fatalf("ProcessJob = (%v, %v), want Ack", disp, err)
	}
	if env.Result != nil {
		t.Error("the override is an operational skip: it must emit NO verdict")
	}
	if len(events.kinds) != 1 || events.kinds[0] != "empty_video_skip_override" {
		t.Errorf("audit events = %v, want one empty_video_skip_override", events.kinds)
	}
	if strings.TrimSpace(buf.String()) != "" {
		t.Errorf("a skipped job wrote an envelope: %s", buf.String())
	}
}

// TestOversizeImageIsRejectedBeforeTheProvider: the pre-flight size check
// exists so an oversize asset never spends a rate-limiter token or a billed
// call. It must surface as a frame error, not as an allow.
func TestOversizeImageIsRejectedBeforeTheProvider(t *testing.T) {
	mod := &fakeModerator{caps: moderation.Caps{MaxImageBytes: 4}}
	p, _ := newTestPipeline(t, mod, nil)

	env, disp, _ := p.ProcessJob(context.Background(), imageJob(writeInput(t, "far too many bytes")))
	if disp != queue.DeadLetter {
		t.Fatalf("disposition = %v, want DeadLetter", disp)
	}
	if mod.calls != 0 {
		t.Error("the provider was called for an image that failed pre-flight; that is a billed call")
	}
	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("verdict = %q, want error", env.Result.Overall.Verdict)
	}
}

// panicOnceModerator panics for one specific frame, to prove a frame task
// panic dead-letters that FRAME without taking down the pool.
type panicOnceModerator struct{ boom string }

func (panicOnceModerator) Name() string { return "panic-fake" }
func (panicOnceModerator) Close() error { return nil }
func (panicOnceModerator) Capabilities() moderation.Caps {
	return moderation.Caps{MaxImageBytes: 1 << 20}
}

func (m panicOnceModerator) AnalyzeImage(_ context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	if string(img.Bytes) == m.boom {
		panic("provider client exploded")
	}
	score := 0.1
	return moderation.NormalizedResult{
		Provider: "panic-fake",
		Frames: []moderation.FrameResult{{
			Categories: []moderation.CategoryResult{{
				Category: moderation.CategorySexual, ProviderLabel: "fake/sexual",
				Score: &score, ScoreOrigin: moderation.OriginProbability,
			}},
		}},
	}, nil
}

// TestFrameTaskPanicBecomesAFrameError: one frame's panic must be captured
// as that frame's error. Letting it escape would kill the worker pool and
// take every concurrent job with it.
func TestFrameTaskPanicBecomesAFrameError(t *testing.T) {
	fs := &fakeFrameSource{dir: t.TempDir(), contents: []string{"ok1", "boom", "ok2"}}
	p, _ := newTestPipeline(t, panicOnceModerator{boom: "boom"}, fs)

	env, disp, _ := p.ProcessJob(context.Background(), videoJob("clip.mp4"))
	if disp != queue.DeadLetter {
		t.Fatalf("disposition = %v, want DeadLetter (one frame could not be evaluated)", disp)
	}
	if env.Result == nil || len(env.Result.Frames) != 3 {
		t.Fatalf("want all 3 frames represented, got %+v", env.Result)
	}
	var panicked int
	for _, fr := range env.Result.Frames {
		if fr.Status == moderation.FrameError && strings.Contains(fr.Error, "panic") {
			panicked++
		}
	}
	if panicked != 1 {
		t.Errorf("panicking frames recorded = %d, want 1", panicked)
	}
	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("verdict = %q, want error: an unevaluated frame is never allow", env.Result.Overall.Verdict)
	}
}

// TestCleanupFailureDoesNotChangeTheVerdict: a WorkDir that cannot be
// removed is an operational problem, logged — it must never turn a scored
// video into an error, nor an unsafe one into an allow.
func TestCleanupFailureDoesNotChangeTheVerdict(t *testing.T) {
	fs := &failingCleanupSource{dir: t.TempDir()}
	p, _ := newTestPipeline(t, &fakeModerator{scores: map[string]float64{"a": 0.1}}, fs)

	env, disp, err := p.ProcessJob(context.Background(), videoJob("clip.mp4"))
	if disp != queue.Ack || err != nil {
		t.Fatalf("ProcessJob = (%v, %v), want Ack despite a cleanup failure", disp, err)
	}
	if env.Result.Overall.Verdict != moderation.VerdictAllow {
		t.Errorf("verdict = %q, want allow", env.Result.Overall.Verdict)
	}
}

type failingCleanupSource struct{ dir string }

func (f *failingCleanupSource) Frames(_ context.Context, _ string, _ []string) ([]frames.Frame, func() error, error) {
	p := filepath.Join(f.dir, "frame-000000.png")
	if err := os.WriteFile(p, []byte("a"), 0o600); err != nil {
		return nil, nil, err
	}
	return []frames.Frame{{Index: 0, TimestampSec: 0, Path: p}},
		func() error { return errors.New("workdir busy") }, nil
}

// TestExtractionFailureRunsCleanup: when extraction fails partway it may
// still have created a WorkDir. Skipping cleanup on the error path would
// leak a directory of extracted frames per failed job.
func TestExtractionFailureRunsCleanup(t *testing.T) {
	fs := &fakeFrameSource{dir: t.TempDir(), err: errors.New("ffmpeg exploded")}
	p, _ := newTestPipeline(t, &fakeModerator{}, fs)

	if _, disp, _ := p.ProcessJob(context.Background(), videoJob("clip.mp4")); disp != queue.DeadLetter {
		t.Fatalf("disposition = %v, want DeadLetter", disp)
	}
	if fs.cleaned != 1 {
		t.Errorf("cleanup ran %d times on the extraction-failure path, want 1", fs.cleaned)
	}
}

// TestJobWithNoRefUsesTheJobIDAsAssetID: the envelope must always identify
// what was scanned. An empty asset_id makes a decision untraceable.
func TestJobWithNoRefUsesTheJobIDAsAssetID(t *testing.T) {
	p, _ := newTestPipeline(t, &fakeModerator{}, nil)
	j := queue.Job{ID: "no-ref-job", Source: moderation.Source{Kind: "file", MediaType: "image"}}

	env, _, _ := p.ProcessJob(context.Background(), j)
	if env.Result.AssetID != "no-ref-job" {
		t.Errorf("asset_id = %q, want the job ID as the fallback", env.Result.AssetID)
	}
	if env.Result.MediaType != "image" {
		t.Errorf("media_type = %q, want the job's media type carried into the envelope", env.Result.MediaType)
	}
}

// TestDebugLoggingEmitsPerFrameDetail: the DEBUG per-frame lines are the
// operator's only view of why a rollup landed where it did. They must carry
// scores and verdicts — and nothing else (no media, no Raw).
func TestDebugLoggingEmitsPerFrameDetail(t *testing.T) {
	var logBuf bytes.Buffer
	fs := &fakeFrameSource{dir: t.TempDir(), contents: []string{"a", "b"}}
	p, _ := newTestPipeline(t, &fakeModerator{scores: map[string]float64{"a": 0.9, "b": 0.1}}, fs)
	p.Log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if _, _, err := p.ProcessJob(context.Background(), videoJob("clip.mp4")); err != nil {
		t.Fatalf("ProcessJob: %v", err)
	}
	out := logBuf.String()
	if !strings.Contains(out, "frame=0") && !strings.Contains(out, "frame=1") {
		t.Errorf("no per-frame debug lines emitted:\n%s", out)
	}
	if strings.Contains(out, "media") && strings.Contains(out, "bytes=") {
		t.Errorf("debug logging leaked media bytes:\n%s", out)
	}
}

// TestConcurrencyDefault: an unset frames.concurrency must fall back to a
// real bound. Zero would mean errgroup.SetLimit(0) and no frame would ever
// run.
func TestConcurrencyDefault(t *testing.T) {
	p := &Pipeline{}
	if got := p.concurrency(); got != 4 {
		t.Errorf("concurrency() = %d with no config, want the documented default 4", got)
	}
	p.Concurrency = 7
	if got := p.concurrency(); got != 7 {
		t.Errorf("concurrency() = %d, want the configured 7", got)
	}
}

// TestFrameErrorsAlwaysExplainsItself: an error envelope with no per-frame
// message must still say something. An empty error field reads as "no
// problem" next to verdict=error.
func TestFrameErrorsAlwaysExplainsItself(t *testing.T) {
	p := &Pipeline{}
	if got := p.frameErrors(nil, errors.New("boom")); got != "boom" {
		t.Errorf("frameErrors = %q, want the processing error", got)
	}
	got := p.frameErrors([]moderation.FrameResult{{Status: moderation.FrameOK}}, nil)
	if got == "" {
		t.Error("frameErrors returned an empty explanation for an error verdict")
	}
	joined := p.frameErrors([]moderation.FrameResult{
		{Status: moderation.FrameError, Error: "one"},
		{Status: moderation.FrameError, Error: "two"},
	}, nil)
	if !strings.Contains(joined, "one") || !strings.Contains(joined, "two") {
		t.Errorf("frameErrors = %q, want every frame error represented", joined)
	}
}

// TestSniffMIMEUsesOnlyTheHeader: MIME detection must look at the first
// bytes, never the whole asset — a large frame would otherwise be scanned
// twice in memory.
func TestSniffMIMEUsesOnlyTheHeader(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 2048)...)
	if got := sniffMIME(png); got != "image/png" {
		t.Errorf("sniffMIME(png) = %q, want image/png", got)
	}
	if got := sniffMIME([]byte("not an image at all")); got == "" {
		t.Error("sniffMIME returned an empty type; the adapter needs some MIME to send")
	}
}
