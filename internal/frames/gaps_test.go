package frames

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/config"
)

// TestValidateBinariesRejectsEmptyPaths: an empty path is a config mistake
// that would otherwise reach exec.LookPath("") and fail per job instead of
// at boot.
func TestValidateBinariesRejectsEmptyPaths(t *testing.T) {
	for _, cfg := range []config.FFmpegConfig{
		{FFmpegPath: "", FFprobePath: "ffprobe"},
		{FFmpegPath: "ffmpeg", FFprobePath: ""},
	} {
		if err := ValidateBinaries(cfg); err == nil {
			t.Errorf("empty binary path accepted: %+v", cfg)
		}
	}
}

func TestValidateBinariesAcceptsPresentBinaries(t *testing.T) {
	requireFFmpeg(t)
	if err := ValidateBinaries(config.FFmpegConfig{FFmpegPath: "ffmpeg", FFprobePath: "ffprobe"}); err != nil {
		t.Errorf("ValidateBinaries with both binaries present: %v", err)
	}
}

// TestSanitizeDirName: workflow names become directory names under the
// job WorkDir. Anything but [A-Za-z0-9_-] must be flattened, or a crafted
// workflow name could steer the extraction output elsewhere.
func TestSanitizeDirName(t *testing.T) {
	cases := map[string]string{
		"scene-detect": "scene-detect",
		"key_frames99": "key_frames99",
		"../../etc":    "______etc", // letters survive; every separator is flattened
		"a/b\\c":       "a_b_c",
		"drive:name":   "drive_name",
		"sp ace":       "sp_ace",
	}
	for in, want := range cases {
		if got := sanitizeDirName(in); got != want {
			t.Errorf("sanitizeDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTailKeepsTheEnd: ffmpeg's useful diagnostics are at the END of
// stderr. A head-truncating tail would put the banner in the error and
// drop the reason.
func TestTailKeepsTheEnd(t *testing.T) {
	if got := tail("  short  ", 400); got != "short" {
		t.Errorf("tail(short) = %q, want the trimmed string", got)
	}
	long := strings.Repeat("x", 100) + "THE-REASON"
	got := tail(long, 10)
	if !strings.HasSuffix(got, "THE-REASON") {
		t.Errorf("tail dropped the end of the message: %q", got)
	}
	if len([]rune(got)) > 11 { // 10 chars plus the ellipsis marker
		t.Errorf("tail returned %d runes, want at most 11", len([]rune(got)))
	}
}

// TestTimeoutFallback: an unset ffmpeg.timeout must not mean "no timeout".
// A hung ffmpeg would hold a worker forever.
func TestTimeoutFallback(t *testing.T) {
	s := NewFFmpegSource(config.FFmpegConfig{}, nil)
	if got := s.timeout(); got != 2*time.Minute {
		t.Errorf("timeout() = %v with no config, want the 2m default", got)
	}
	s = NewFFmpegSource(config.FFmpegConfig{Timeout: 5 * time.Second}, nil)
	if got := s.timeout(); got != 5*time.Second {
		t.Errorf("timeout() = %v, want the configured 5s", got)
	}
}

// TestCollectPairsFramesWithShowinfoTimestamps: the timestamps come from
// ffmpeg's showinfo log. Losing them silently downgrades every frame to an
// ordinal, which then appears in the envelope as if it were a real
// position in the video.
func TestCollectPairsFramesWithShowinfoTimestamps(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"frame-000000.png", "frame-000001.png", "frame-000002.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("png"), 0o600); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	s := NewFFmpegSource(config.FFmpegConfig{}, nil)

	stderr := "n:0 pts_time:0.5 something\nn:1 pts_time:1.25 something\n"
	got, err := s.collect(dir, scanStderrText(stderr).Timestamps())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("collect returned %d frames, want 3", len(got))
	}
	if got[0].TimestampSec != 0.5 || got[1].TimestampSec != 1.25 {
		t.Errorf("showinfo timestamps not applied: %+v", got[:2])
	}
	// The third frame has no showinfo entry and must fall back to its
	// ordinal rather than reusing another frame's timestamp.
	if got[2].TimestampSec != 2 {
		t.Errorf("frame 2 timestamp = %v, want the ordinal fallback 2", got[2].TimestampSec)
	}
	if got[2].Index != 2 {
		t.Errorf("frame index = %d, want 2", got[2].Index)
	}
}

// TestCollectWithNoShowinfoUsesOrdinals: workflows are allowed to omit the
// showinfo filter. Every frame then carries its ordinal, and none carry a
// fabricated timestamp.
func TestCollectWithNoShowinfoUsesOrdinals(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "frame-000000.png"), []byte("png"), 0o600); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	s := NewFFmpegSource(config.FFmpegConfig{}, nil)
	got, err := s.collect(dir, scanStderrText("no timestamps here").Timestamps())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 1 || got[0].TimestampSec != 0 {
		t.Errorf("collect = %+v, want one frame at the ordinal 0", got)
	}
}

// TestFramesRejectsUnresolvableWorkflowBeforeExtracting: the workflow name
// is re-checked at execution (defense in depth on the intake check).
// Falling back to another workflow would scan a video differently from
// what the submitter asked for, with nothing recording the substitution.
func TestFramesRejectsUnknownDefaultWorkflow(t *testing.T) {
	cfg := intervalCfg()
	cfg.DefaultWorkflow = "not-configured"
	src := NewFFmpegSource(cfg, nil)

	// A real file is required so the check under test is the one that
	// fires, not the earlier stat.
	video := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(video, []byte("not really a video"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, cleanup, err := src.Frames(context.Background(), video, nil)
	if err == nil {
		t.Fatal("an unconfigured default workflow must fail, not fall back")
	}
	if cleanup != nil {
		_ = cleanup()
	}
}

// TestFramesFailsWhenFFmpegItselfFails: a workflow whose ffmpeg invocation
// exits non-zero must surface the stderr tail. Without it an operator
// debugging a custom workflow has only "exit status 1".
func TestFramesFailsWhenFFmpegItselfFails(t *testing.T) {
	requireFFmpeg(t)
	video := makeTestVideo(t, "1")

	cfg := intervalCfg()
	cfg.DefaultWorkflow = "broken"
	cfg.Workflows = map[string]config.WorkflowConfig{
		"broken": {
			Description: "an ffmpeg invocation that cannot succeed",
			// A filter that does not exist: passes the guardrails (no
			// protocols, output confined) but ffmpeg exits non-zero.
			Args: []string{"-hide_banner", "-i", "{{.Input}}", "-vf", "no_such_filter=1", "{{.WorkDir}}/frame-%06d.png"},
		},
	}
	src := NewFFmpegSource(cfg, nil)

	_, cleanup, err := src.Frames(context.Background(), video, nil)
	if cleanup != nil {
		_ = cleanup()
	}
	if err == nil {
		t.Fatal("a failing ffmpeg invocation reported success")
	}
	if !strings.Contains(err.Error(), "stderr tail") {
		t.Errorf("error carries no stderr tail, leaving nothing to debug: %v", err)
	}
}

// TestFramesRejectsAnUnrenderableWorkflow: RenderWorkflow failures are
// caught at boot by `workflows validate`, but the execution path must fail
// too — a config edited after boot must not extract with a half-rendered
// argument list.
func TestFramesRejectsAnUnrenderableWorkflow(t *testing.T) {
	requireFFmpeg(t)
	video := makeTestVideo(t, "1")

	cfg := intervalCfg()
	cfg.DefaultWorkflow = "unrenderable"
	cfg.Workflows = map[string]config.WorkflowConfig{
		"unrenderable": {
			Description: "template that cannot parse",
			Args:        []string{"-i", "{{.Input}}", "{{.WorkDir}}/frame-{{", "x"},
		},
	}
	src := NewFFmpegSource(cfg, nil)

	_, cleanup, err := src.Frames(context.Background(), video, nil)
	if cleanup != nil {
		_ = cleanup()
	}
	if err == nil {
		t.Fatal("an unrenderable workflow extracted frames")
	}
	if !strings.Contains(err.Error(), "render workflow") {
		t.Errorf("error should name the render failure, got: %v", err)
	}
}

// TestRenderWorkflowSurfacesTemplateErrors covers both failure modes of the
// renderer directly: a template that cannot parse and one that cannot
// execute.
func TestRenderWorkflowSurfacesTemplateErrors(t *testing.T) {
	v := TemplateValues{Input: "in.mp4", WorkDir: "/w", MaxFrames: 2, MaxWidth: 320}

	if _, err := RenderWorkflow(config.WorkflowConfig{Args: []string{"{{"}}, v); err == nil {
		t.Error("an unparseable template rendered without error")
	}
	if _, err := RenderWorkflow(config.WorkflowConfig{Args: []string{"{{.Input.Nope}}"}}, v); err == nil {
		t.Error("a template referencing a field that does not exist rendered without error")
	}
}

// TestRenderWorkflowAlwaysEnforcesNostdin: -nostdin is mandatory (invariant
// 5). It must be prepended when absent and not duplicated when present.
func TestRenderWorkflowAlwaysEnforcesNostdin(t *testing.T) {
	v := TemplateValues{Input: "in.mp4", WorkDir: "/w", MaxFrames: 2, MaxWidth: 320}

	got, err := RenderWorkflow(config.WorkflowConfig{Args: []string{"-i", "{{.Input}}"}}, v)
	if err != nil {
		t.Fatalf("RenderWorkflow: %v", err)
	}
	if len(got) == 0 || got[0] != "-nostdin" {
		t.Errorf("rendered args = %v, want -nostdin prepended", got)
	}

	got, err = RenderWorkflow(config.WorkflowConfig{Args: []string{"-nostdin", "-i", "{{.Input}}"}}, v)
	if err != nil {
		t.Fatalf("RenderWorkflow: %v", err)
	}
	var count int
	for _, a := range got {
		if a == "-nostdin" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("-nostdin appears %d times in %v, want exactly 1", count, got)
	}
}

// TestValidateWorkflowRejectsTraversalThatOnlyAppearsAfterRendering: the
// static checks see "{{.WorkDir}}/../out.png" as correctly rooted. Only the
// dry-render exposes the traversal, which is why the render happens at
// validation time rather than first at extraction time.
func TestValidateWorkflowRejectsTraversalThatOnlyAppearsAfterRendering(t *testing.T) {
	cfg := intervalCfg()
	wf := config.WorkflowConfig{
		Description: "escapes via traversal",
		Args:        []string{"-i", "{{.Input}}", "{{.WorkDir}}/../frame-%06d.png"},
	}
	err := ValidateWorkflow("escaper", wf, cfg)
	if err == nil {
		t.Fatal("a workflow whose rendered output traverses out of the WorkDir was accepted")
	}
	if !strings.Contains(err.Error(), "traversal") {
		t.Errorf("error should name the traversal, got: %v", err)
	}
}

// TestFramesEnforcesTheExtractionBudget: {{.MaxFrames}} tells a workflow
// the budget, but a workflow that ignores it must still not blow the disk
// bound. The surplus files are deleted, not merely dropped from the slice.
func TestFramesEnforcesTheExtractionBudget(t *testing.T) {
	requireFFmpeg(t)
	video := makeTestVideo(t, "3")

	cfg := intervalCfg()
	cfg.MaxFrames = 1 // extraction budget = 4 x max_frames
	cfg.DefaultWorkflow = "greedy"
	cfg.Workflows = map[string]config.WorkflowConfig{
		"greedy": {
			Description: "ignores {{.MaxFrames}} entirely",
			Args: []string{"-hide_banner", "-i", "{{.Input}}", "-vf", "fps=10",
				"{{.WorkDir}}/frame-%06d.png"},
		},
	}
	src := NewFFmpegSource(cfg, nil)

	fs, cleanup, err := src.Frames(context.Background(), video, nil)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	defer func() { _ = cleanup() }()

	budget := cfg.ExtractBudget()
	if len(fs) != budget {
		t.Fatalf("returned %d frames, want the extraction budget %d", len(fs), budget)
	}
	// The truncated frames must be removed from disk, not just from the
	// slice — the budget is a disk bound.
	surplus := filepath.Join(filepath.Dir(fs[0].Path), "frame-000099.png")
	if _, err := os.Stat(surplus); err == nil {
		t.Error("a frame beyond the budget is still on disk")
	}
	for i, f := range fs {
		if f.Index != i {
			t.Errorf("frame %d has index %d; indices must be re-numbered across the union", i, f.Index)
		}
	}
}

// TestValidateAllRejectsAnEmptyOrDanglingSet: booting with no workflows,
// or with a default that does not exist, would leave every video job
// failing at extraction time instead of at boot.
func TestValidateAllRejectsAnEmptyOrDanglingSet(t *testing.T) {
	if err := ValidateAll(config.FFmpegConfig{MaxFrames: 4}); err == nil {
		t.Error("an empty workflow set was accepted")
	}
	cfg := intervalCfg()
	cfg.DefaultWorkflow = "no-such-workflow"
	if err := ValidateAll(cfg); err == nil {
		t.Error("a default_workflow that is not defined was accepted")
	}
}

// TestLooksAbsolutePathCoversWindowsDrives: the absolute-path guard runs on
// Windows too, where C:\ and C:/ are the escape shapes rather than a
// leading slash.
func TestLooksAbsolutePathCoversWindowsDrives(t *testing.T) {
	for _, abs := range []string{"/etc/passwd", `C:\Windows`, "D:/data"} {
		if !looksAbsolutePath(abs) {
			t.Errorf("looksAbsolutePath(%q) = false, want true", abs)
		}
	}
	for _, rel := range []string{"frame-%06d.png", "sub/dir/out.png", "C:", ""} {
		if looksAbsolutePath(rel) {
			t.Errorf("looksAbsolutePath(%q) = true, want false", rel)
		}
	}
}
