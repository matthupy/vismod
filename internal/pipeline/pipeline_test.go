package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/frames"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

// fakeModerator is the credential-free test double (never a shipped
// adapter). scores maps image content (as string) to a SEXUAL score;
// errOn marks contents that fail.
type fakeModerator struct {
	mu     sync.Mutex
	scores map[string]float64
	errOn  map[string]error
	calls  int
	caps   moderation.Caps
}

func (m *fakeModerator) Name() string { return "fake" }
func (m *fakeModerator) Capabilities() moderation.Caps {
	if m.caps.MaxImageBytes == 0 {
		return moderation.Caps{MaxImageBytes: 1 << 20}
	}
	return m.caps
}
func (m *fakeModerator) Close() error { return nil }

func (m *fakeModerator) AnalyzeImage(_ context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	key := string(img.Bytes)
	if err, ok := m.errOn[key]; ok {
		return moderation.NormalizedResult{}, err
	}
	score := m.scores[key]
	// A distinct, deterministic Raw per image, so tests can assert which
	// response was bound to which frame. Real adapters marshal their own
	// sanitized response here; fakeRawFor recomputes this independently.
	raw, err := json.Marshal(fakeRawBody{Frame: key, Score: score})
	if err != nil {
		return moderation.NormalizedResult{}, err
	}
	return moderation.NormalizedResult{
		Provider: "fake",
		Frames: []moderation.FrameResult{{
			Categories: []moderation.CategoryResult{{
				Category:      moderation.CategorySexual,
				ProviderLabel: "fake/sexual",
				Score:         &score,
				ScoreOrigin:   moderation.OriginProbability,
			}},
		}},
		Raw: raw,
	}, nil
}

// fakeRawBody is the fake adapter's sanitized raw response.
type fakeRawBody struct {
	Frame string  `json:"frame"`
	Score float64 `json:"score"`
}

// fakeFrameSource materializes frame files from given contents.
type fakeFrameSource struct {
	contents []string
	// timestamps, when set, overrides the default TimestampSec of
	// float64(i) — the way to produce frames whose extraction order and
	// timestamp order disagree.
	timestamps []float64
	err        error
	cleaned    int
	dir        string
	zeroClean  bool     // return frames == nil but a cleanup
	gotWFs     []string // workflows requested by the pipeline
}

func (f *fakeFrameSource) Frames(_ context.Context, _ string, workflows []string) ([]frames.Frame, func() error, error) {
	f.gotWFs = workflows
	cleanup := func() error { f.cleaned++; return nil }
	if f.err != nil {
		return nil, cleanup, f.err
	}
	if f.zeroClean {
		return nil, cleanup, nil
	}
	var out []frames.Frame
	for i, c := range f.contents {
		p := filepath.Join(f.dir, fmt.Sprintf("frame-%06d.png", i))
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			return nil, cleanup, err
		}
		ts := float64(i)
		if i < len(f.timestamps) {
			ts = f.timestamps[i]
		}
		out = append(out, frames.Frame{Index: i, TimestampSec: ts, Path: p})
	}
	return out, cleanup, nil
}

func testThresholds() config.Thresholds {
	return config.Thresholds{
		"default": {FlagAt: f(0.5), BlockAt: f(0.8)},
		"SEXUAL":  {FlagAt: f(0.4), BlockAt: f(0.7)},
	}
}

func newTestPipeline(t *testing.T, mod moderation.Moderator, fs frames.FrameSource) (*Pipeline, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return &Pipeline{
		Moderator:   mod,
		FrameSource: fs,
		Sink:        result.NewJSONLSink(&buf),
		Thresholds:  testThresholds(),
		Concurrency: 2,
		ModelID:     result.ModelIdentity{Adapter: "fake", ModelVersion: "t", ConfigHash: "h"},
	}, &buf
}

func writeInput(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input.png")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func imageJob(path string) queue.Job {
	return queue.Job{ID: "j1", Source: moderation.Source{Kind: "file", Ref: path, MediaType: "image"}, SubmittedAt: time.Now()}
}

func videoJob(path string) queue.Job {
	return queue.Job{ID: "v1", Source: moderation.Source{Kind: "file", Ref: path, MediaType: "video"}, SubmittedAt: time.Now()}
}

func decodeEnvelope(t *testing.T, buf *bytes.Buffer) result.ResultEnvelope {
	t.Helper()
	var env result.ResultEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &env); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, buf.String())
	}
	return env
}

func TestImageAllow(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, buf := newTestPipeline(t, mod, nil)
	path := writeInput(t, "benign")

	env, disp, err := p.ProcessJob(context.Background(), imageJob(path))
	if err != nil || disp != queue.Ack {
		t.Fatalf("disp=%v err=%v", disp, err)
	}
	if env.Result.Overall.Verdict != moderation.VerdictAllow {
		t.Errorf("verdict = %s, want allow", env.Result.Overall.Verdict)
	}
	if env.Result.SchemaVersion != moderation.SchemaVersion {
		t.Errorf("schema_version not stamped")
	}
	if env.Result.AssetID != path {
		t.Errorf("AssetID = %q, want source ref", env.Result.AssetID)
	}
	out := decodeEnvelope(t, buf)
	if out.Result.Frames[0].TimestampSec != nil {
		t.Error("still image TimestampSec must be nil")
	}
	// Nullable scalars serialize as JSON null, never omitted.
	if !strings.Contains(buf.String(), `"timestamp_sec":null`) {
		t.Errorf("timestamp_sec must serialize as null: %s", buf.String())
	}
}

func TestImageProviderErrorFailsSafe(t *testing.T) {
	mod := &fakeModerator{errOn: map[string]error{"bad": errors.New("provider 500")}}
	p, buf := newTestPipeline(t, mod, nil)
	path := writeInput(t, "bad")

	env, disp, _ := p.ProcessJob(context.Background(), imageJob(path))
	if disp != queue.DeadLetter {
		t.Errorf("provider failure must dead-letter, got %v", disp)
	}
	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("verdict = %s, want error (never allow)", env.Result.Overall.Verdict)
	}
	if env.Error == "" {
		t.Error("envelope must carry the error")
	}
	if !strings.Contains(buf.String(), `"max_score":null`) {
		t.Errorf("max_score must be null (not 0) on error: %s", buf.String())
	}
}

func TestOversizeImagePreflight(t *testing.T) {
	mod := &fakeModerator{caps: moderation.Caps{MaxImageBytes: 3}}
	p, _ := newTestPipeline(t, mod, nil)
	path := writeInput(t, "way too big")

	env, disp, _ := p.ProcessJob(context.Background(), imageJob(path))
	if disp != queue.DeadLetter || env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("oversize must be terminal error: disp=%v verdict=%v", disp, env.Result.Overall.Verdict)
	}
	if mod.calls != 0 {
		t.Errorf("adapter must not be called for oversize image, calls=%d", mod.calls)
	}
}

func TestVideoFanOutPartialErrorNeverAllows(t *testing.T) {
	mod := &fakeModerator{
		scores: map[string]float64{"f0": 0.1, "f2": 0.2},
		errOn:  map[string]error{"f1": errors.New("frame provider error")},
	}
	fs := &fakeFrameSource{contents: []string{"f0", "f1", "f2"}, dir: t.TempDir()}
	p, _ := newTestPipeline(t, mod, fs)
	path := writeInput(t, "video-bytes")

	env, disp, _ := p.ProcessJob(context.Background(), videoJob(path))
	if disp != queue.DeadLetter {
		t.Errorf("partial video error must dead-letter, got %v", disp)
	}
	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("partial video verdict = %s, want error", env.Result.Overall.Verdict)
	}
	// Independent evidence: siblings still evaluated despite f1 failing.
	if mod.calls != 3 {
		t.Errorf("all frames must be attempted, calls=%d", mod.calls)
	}
	if len(env.Result.Frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(env.Result.Frames))
	}
	if fs.cleaned == 0 {
		t.Error("cleanup must run on every exit path")
	}
}

func TestVideoBlockedFrameBlocksAsset(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"f0": 0.1, "f1": 0.95}}
	fs := &fakeFrameSource{contents: []string{"f0", "f1"}, dir: t.TempDir()}
	p, _ := newTestPipeline(t, mod, fs)
	path := writeInput(t, "video-bytes")

	env, disp, err := p.ProcessJob(context.Background(), videoJob(path))
	if err != nil || disp != queue.Ack {
		t.Fatalf("disp=%v err=%v", disp, err)
	}
	if env.Result.Overall.Verdict != moderation.VerdictBlock {
		t.Errorf("verdict = %s, want block (any-frame rollup)", env.Result.Overall.Verdict)
	}
	if env.Result.MediaType != "video" {
		t.Errorf("media_type = %q", env.Result.MediaType)
	}
}

func TestVideoZeroFramesIsError(t *testing.T) {
	fs := &fakeFrameSource{zeroClean: true, dir: t.TempDir()}
	p, _ := newTestPipeline(t, &fakeModerator{}, fs)
	path := writeInput(t, "video-bytes")

	env, disp, _ := p.ProcessJob(context.Background(), videoJob(path))
	if disp != queue.DeadLetter || env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("zero frames must be error + dead-letter (never clean): disp=%v verdict=%v",
			disp, env.Result.Overall.Verdict)
	}
	if fs.cleaned == 0 {
		t.Error("cleanup must run even on zero frames")
	}
}

func TestVideoExtractionErrorIsError(t *testing.T) {
	fs := &fakeFrameSource{err: errors.New("ffmpeg exploded")}
	p, _ := newTestPipeline(t, &fakeModerator{}, fs)
	path := writeInput(t, "video-bytes")

	env, disp, _ := p.ProcessJob(context.Background(), videoJob(path))
	if disp != queue.DeadLetter || env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("extraction failure must be error + dead-letter: disp=%v verdict=%v", disp, env.Result.Overall.Verdict)
	}
	if fs.cleaned == 0 {
		t.Error("cleanup must run when extraction fails")
	}
}

// Per-job workflow selection flows from the job through to the frame
// source untouched.
func TestJobWorkflowSelectionReachesFrameSource(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"f0": 0.1}}
	fs := &fakeFrameSource{contents: []string{"f0"}, dir: t.TempDir()}
	p, _ := newTestPipeline(t, mod, fs)
	path := writeInput(t, "video-bytes")

	j := videoJob(path)
	j.Workflows = []string{"keyframe", "interval"}
	if _, disp, err := p.ProcessJob(context.Background(), j); err != nil || disp != queue.Ack {
		t.Fatalf("disp=%v err=%v", disp, err)
	}
	if len(fs.gotWFs) != 2 || fs.gotWFs[0] != "keyframe" || fs.gotWFs[1] != "interval" {
		t.Errorf("workflows not passed through: %v", fs.gotWFs)
	}
}

// Dedup drops identical frames before moderation (fewer adapter calls)
// and the rollup still evaluates the survivors.
func TestDedupRemovesDuplicateFramesBeforeModeration(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{}}
	// fakeFrameSource writes literal contents; identical PNG bytes hash
	// identically. Patterned (not flat) images so the dHashes differ.
	gradient, checker := testPNGGradient(), testPNGChecker()
	fs := &fakeFrameSource{contents: []string{gradient, gradient, checker}, dir: t.TempDir()}
	mod.scores[gradient] = 0.1
	mod.scores[checker] = 0.2
	p, _ := newTestPipeline(t, mod, fs)
	p.Dedup = true
	p.DedupThreshold = 0
	path := writeInput(t, "video-bytes")

	env, disp, err := p.ProcessJob(context.Background(), videoJob(path))
	if err != nil || disp != queue.Ack {
		t.Fatalf("disp=%v err=%v", disp, err)
	}
	if mod.calls != 2 {
		t.Errorf("duplicate frame should be skipped: adapter calls = %d, want 2", mod.calls)
	}
	if len(env.Result.Frames) != 2 {
		t.Errorf("frames in result = %d, want 2", len(env.Result.Frames))
	}
}

// A per-job override may TIGHTEN dedup or disable it, never loosen it past
// the operator's configured ceiling.
//
// This is a fail-open guard, not a tuning nicety. Every pair of 64-bit
// dHashes is within Hamming distance 64, so an honored override of 64 makes
// frame 1..N all "near-duplicates" of frame 0: one frame reaches the vendor
// and it decides the verdict for the whole asset. Put a benign frame first
// and an abusive clip returns allow. Intake clamps this too — the pipeline
// re-clamps because a job can be pushed straight onto Redis without ever
// passing intake (see SECURITY.md).
func TestJobDedupThresholdCannotLoosenPastConfig(t *testing.T) {
	gradient, checker := testPNGGradient(), testPNGChecker()
	mod := &fakeModerator{scores: map[string]float64{gradient: 0.1, checker: 0.2}}
	fs := &fakeFrameSource{contents: []string{gradient, gradient, checker}, dir: t.TempDir()}
	p, _ := newTestPipeline(t, mod, fs)
	p.Dedup = true
	p.DedupThreshold = 0 // operator ceiling: exact duplicates only

	j := videoJob(writeInput(t, "video-bytes"))
	wideOpen := 64
	j.DedupThreshold = &wideOpen

	env, disp, err := p.ProcessJob(context.Background(), j)
	if err != nil || disp != queue.Ack {
		t.Fatalf("disp=%v err=%v", disp, err)
	}
	// Clamped to 0: the duplicate gradient collapses, the checker survives.
	// Unclamped, the checker collapses too and calls would be 1.
	if mod.calls != 2 {
		t.Errorf("override loosened dedup past the config ceiling: adapter calls = %d, want 2", mod.calls)
	}
	if len(env.Result.Frames) != 2 {
		t.Errorf("frames in result = %d, want 2 (a visually distinct frame was dropped)", len(env.Result.Frames))
	}
}

// Per-job dedup override: enables dedup when the config has it off, and
// disables it when the config has it on.
func TestJobDedupThresholdOverride(t *testing.T) {
	gradient, checker := testPNGGradient(), testPNGChecker()

	run := func(t *testing.T, cfgOn bool, override *int) (calls int) {
		t.Helper()
		mod := &fakeModerator{scores: map[string]float64{gradient: 0.1, checker: 0.2}}
		fs := &fakeFrameSource{contents: []string{gradient, gradient, checker}, dir: t.TempDir()}
		p, _ := newTestPipeline(t, mod, fs)
		p.Dedup = cfgOn
		p.DedupThreshold = 0
		j := videoJob(writeInput(t, "video-bytes"))
		j.DedupThreshold = override
		if _, disp, err := p.ProcessJob(context.Background(), j); err != nil || disp != queue.Ack {
			t.Fatalf("disp=%v err=%v", disp, err)
		}
		return mod.calls
	}

	zero, minusOne := 0, -1
	if calls := run(t, false, &zero); calls != 2 {
		t.Errorf("override must ENABLE dedup when config is off: calls=%d, want 2", calls)
	}
	if calls := run(t, true, &minusOne); calls != 3 {
		t.Errorf("negative override must DISABLE dedup when config is on: calls=%d, want 3", calls)
	}
	if calls := run(t, true, nil); calls != 2 {
		t.Errorf("nil override must inherit config: calls=%d, want 2", calls)
	}
}

// The scan cap applies AFTER dedup: duplicates are collapsed first, then
// the survivors are capped. Cap-before-dedup would waste the budget on
// duplicates ([A,A,B,C] capped at 2 -> [A,A] -> dedup -> just A).
func TestScanCapAppliesAfterDedup(t *testing.T) {
	gradient, checker := testPNGGradient(), testPNGChecker()
	third := testPNGThirdPattern()
	mod := &fakeModerator{scores: map[string]float64{gradient: 0.1, checker: 0.2, third: 0.3}}
	fs := &fakeFrameSource{contents: []string{gradient, gradient, checker, third}, dir: t.TempDir()}
	p, _ := newTestPipeline(t, mod, fs)
	p.Dedup = true
	p.DedupThreshold = 0
	p.MaxScanFrames = 2
	path := writeInput(t, "video-bytes")

	env, disp, err := p.ProcessJob(context.Background(), videoJob(path))
	if err != nil || disp != queue.Ack {
		t.Fatalf("disp=%v err=%v", disp, err)
	}
	// dedup: [g,g,c,t] -> [g,c,t]; cap 2 -> [g,c]. Two DISTINCT frames
	// moderated, not one.
	if mod.calls != 2 {
		t.Errorf("adapter calls = %d, want 2 (dedup before cap)", mod.calls)
	}
	if len(env.Result.Frames) != 2 {
		t.Errorf("result frames = %d, want 2", len(env.Result.Frames))
	}
}

func testPNGThirdPattern() string {
	img := image.NewGray(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			img.SetGray(x, y, color.Gray{Y: uint8(255 - x*8)}) // reversed ramp
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.String()
}

// testPNGGradient renders a horizontal luminance ramp; testPNGChecker a
// checkerboard — visually distinct dHashes.
func testPNGGradient() string {
	img := image.NewGray(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			img.SetGray(x, y, color.Gray{Y: uint8(x * 8)})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.String()
}

func testPNGChecker() string {
	img := image.NewGray(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			if (x/4+y/4)%2 == 0 {
				img.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.String()
}

func TestSinkFailureRetries(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, _ := newTestPipeline(t, mod, nil)
	p.Sink = failingSink{}
	path := writeInput(t, "benign")

	_, disp, err := p.ProcessJob(context.Background(), imageJob(path))
	if disp != queue.Retry || err == nil {
		t.Errorf("sink failure should Retry: disp=%v err=%v", disp, err)
	}
}

type failingSink struct{}

func (failingSink) Write(context.Context, result.ResultEnvelope) error {
	return errors.New("sink down")
}

type capturingEvents struct {
	events []string
	fields []map[string]string
}

func (c *capturingEvents) AppendEvent(kind string, f map[string]string) error {
	c.events = append(c.events, kind)
	c.fields = append(c.fields, f)
	return nil
}

// The gated §F.5 override: zero frames + operator flag = operational
// skip (Ack, NO verdict written) + prominent audit event. Without the
// flag the default fail-safe (error + dead-letter) is proven by
// TestVideoZeroFramesIsError.
func TestEmptyVideoSkipOverrideIsAuditedAck(t *testing.T) {
	fs := &fakeFrameSource{zeroClean: true, dir: t.TempDir()}
	p, buf := newTestPipeline(t, &fakeModerator{}, fs)
	events := &capturingEvents{}
	p.AllowEmptyVideoSkip = true
	p.Events = events
	path := writeInput(t, "video-bytes")

	_, disp, err := p.ProcessJob(context.Background(), videoJob(path))
	if err != nil || disp != queue.Ack {
		t.Fatalf("override must ack: disp=%v err=%v", disp, err)
	}
	if buf.Len() != 0 {
		t.Errorf("operational skip must not emit a verdict envelope, got: %s", buf.String())
	}
	if len(events.events) != 1 || events.events[0] != "empty_video_skip_override" {
		t.Fatalf("override must emit a prominent audit event, got %v", events.events)
	}
	if events.fields[0]["job_id"] == "" {
		t.Error("audit event must carry the job id")
	}
}

// Logging contract: "job started" and "scan complete" (with the overall
// rollup) at INFO; per-frame detail only at DEBUG.
func TestScanLoggingLevels(t *testing.T) {
	run := func(t *testing.T, level slog.Level) string {
		t.Helper()
		var buf bytes.Buffer
		mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
		p, _ := newTestPipeline(t, mod, nil)
		p.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
		path := writeInput(t, "benign")
		if _, disp, err := p.ProcessJob(context.Background(), imageJob(path)); err != nil || disp != queue.Ack {
			t.Fatalf("disp=%v err=%v", disp, err)
		}
		return buf.String()
	}

	info := run(t, slog.LevelInfo)
	for _, want := range []string{"job started", "scan complete", "verdict=allow", "max_score=0.05", "media_type=image"} {
		if !strings.Contains(info, want) {
			t.Errorf("INFO log missing %q:\n%s", want, info)
		}
	}
	if strings.Contains(info, "frame result") {
		t.Error("frame-by-frame detail must NOT appear at INFO")
	}

	debug := run(t, slog.LevelDebug)
	for _, want := range []string{"frame result", "categories=", "SEXUAL=0.050"} {
		if !strings.Contains(debug, want) {
			t.Errorf("DEBUG log missing %q:\n%s", want, debug)
		}
	}
}

func TestUnreadableInputFailsSafe(t *testing.T) {
	p, _ := newTestPipeline(t, &fakeModerator{}, nil)
	env, disp, _ := p.ProcessJob(context.Background(), imageJob(filepath.Join(t.TempDir(), "missing.png")))
	if disp != queue.DeadLetter || env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("unreadable input must be error + dead-letter: disp=%v", disp)
	}
}
