package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/pkg/moderation"
)

// TestMediaTypeFor decides whether a job goes through frame extraction at
// all. A video classified as an image is scanned as a single still — one
// frame's worth of coverage on an asset that needed many.
func TestMediaTypeFor(t *testing.T) {
	video := []string{
		"a.mp4", "a.mov", "a.mkv", "a.webm", "a.avi",
		"a.m4v", "a.mpg", "a.mpeg", "a.ts",
		"A.MP4", "clip.MoV", // extension case must not decide this
		filepath.Join("some", "dir", "clip.mp4"),
	}
	for _, p := range video {
		if got := mediaTypeFor(p); got != "video" {
			t.Errorf("mediaTypeFor(%q) = %q, want video", p, got)
		}
	}

	image := []string{"a.jpg", "a.jpeg", "a.png", "a.webp", "a.gif", "noext", "", "a.mp4.jpg"}
	for _, p := range image {
		if got := mediaTypeFor(p); got != "image" {
			t.Errorf("mediaTypeFor(%q) = %q, want image", p, got)
		}
	}
}

// TestValidateDedupThreshold bounds the per-job override. A 64-bit dHash
// cannot differ by more than 64 bits, so anything above that would silently
// collapse every frame of a video into one.
func TestValidateDedupThreshold(t *testing.T) {
	ptr := func(v int) *int { return &v }

	for _, v := range []*int{nil, ptr(-1), ptr(0), ptr(1), ptr(32), ptr(64)} {
		if err := validateDedupThreshold(v); err != nil {
			t.Errorf("validateDedupThreshold(%v) = %v, want nil", v, err)
		}
	}
	for _, v := range []*int{ptr(-2), ptr(65), ptr(1000)} {
		if err := validateDedupThreshold(v); err == nil {
			t.Errorf("validateDedupThreshold(%d) accepted an out-of-range override", *v)
		}
	}
}

// TestValidateWorkflowSelection: an unknown workflow name must be refused,
// and the error must list what IS configured — a typo'd workflow otherwise
// reads as "vismod is broken" rather than "you meant keyframes".
func TestValidateWorkflowSelection(t *testing.T) {
	cfg := config.Config{FFmpeg: config.FFmpegConfig{
		Workflows: map[string]config.WorkflowConfig{
			"keyframes": {}, "uniform": {}, "scene_change": {},
		},
	}}

	if err := validateWorkflowSelection(cfg, nil); err != nil {
		t.Errorf("empty selection (= configured default) must be valid: %v", err)
	}
	if err := validateWorkflowSelection(cfg, []string{"keyframes", "uniform"}); err != nil {
		t.Errorf("configured workflows rejected: %v", err)
	}

	err := validateWorkflowSelection(cfg, []string{"keyframes", "keyfrmes"})
	if err == nil {
		t.Fatal("an unknown workflow must be refused at validation")
	}
	if !strings.Contains(err.Error(), "keyfrmes") {
		t.Errorf("error should name the offending workflow: %v", err)
	}
	for _, known := range []string{"keyframes", "scene_change", "uniform"} {
		if !strings.Contains(err.Error(), known) {
			t.Errorf("error should list configured workflow %q: %v", known, err)
		}
	}
}

// TestBuildModeratorRequiresAdapterName: real adapters are registered
// (root.go blank-imports them), so the M0 fake path is unreachable and an
// unset adapter.name must fail fast at boot rather than start a worker that
// cannot score anything.
func TestBuildModeratorRequiresAdapterName(t *testing.T) {
	_, err := buildModerator(config.Config{}, testLogger())
	if err == nil {
		t.Fatal("an unset adapter.name must fail fast once adapters are registered")
	}
	if !strings.Contains(err.Error(), "microsoft") {
		t.Errorf("the error should list the registered adapter names, got: %v", err)
	}
}

func TestBuildModeratorUnknownAdapter(t *testing.T) {
	cfg := config.Config{Adapter: config.AdapterSection{Name: "not-a-vendor"}}
	if _, err := buildModerator(cfg, testLogger()); err == nil {
		t.Fatal("an unregistered adapter name must fail fast")
	}
}

// TestOpenAuditDisabled: audit is opt-in, and a disabled audit returns a nil
// log rather than an error — buildPipeline treats nil as "no audit".
func TestOpenAuditDisabled(t *testing.T) {
	log, err := openAudit(config.Config{Audit: config.AuditConfig{Enabled: false, Path: "ignored"}})
	if err != nil {
		t.Fatalf("openAudit: %v", err)
	}
	if log != nil {
		t.Error("audit.enabled=false must not open a log")
	}
}

func TestOpenAuditEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	log, err := openAudit(config.Config{Audit: config.AuditConfig{Enabled: true, Path: path}})
	if err != nil {
		t.Fatalf("openAudit: %v", err)
	}
	if log == nil {
		t.Fatal("audit.enabled=true returned no log")
	}
	if err := log.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestOpenAuditUnwritablePathFails: an audit log that cannot be opened is a
// boot failure. Running without one would produce decisions no one can
// later account for.
func TestOpenAuditUnwritablePathFails(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{Audit: config.AuditConfig{Enabled: true, Path: dir}} // a directory, not a file
	if _, err := openAudit(cfg); err == nil {
		t.Error("an unopenable audit path must fail boot, not run unaudited")
	}
}

// TestModelVersionFallback: adapters that pin a version expose it for the
// audit ModelIdentity; ones that do not must read "unversioned" rather than
// an empty string, so the gap is visible in the trail.
func TestModelVersionFallback(t *testing.T) {
	if got := modelVersion(devFakeModerator{}); got != "m0-skeleton" {
		t.Errorf("modelVersion = %q, want the adapter's pinned version", got)
	}
	if got := modelVersion(labelingModerator{}); got != "unversioned" {
		t.Errorf("modelVersion = %q, want %q for an adapter with no version", got, "unversioned")
	}
}

func TestNewFrameSourceIsBuilt(t *testing.T) {
	cfg := config.Config{FFmpeg: config.FFmpegConfig{MaxFrames: 4, MaxWidth: 640}}
	if src := newFrameSource(cfg, testLogger()); src == nil {
		t.Error("newFrameSource returned nil; video jobs would panic instead of failing safe")
	}
}

// TestValidateFrameBootRejectsMissingBinary: boot validation is what keeps a
// missing ffmpeg from becoming a per-job error on every video hours later.
func TestValidateFrameBootRejectsMissingBinary(t *testing.T) {
	cfg := config.Config{FFmpeg: config.FFmpegConfig{
		FFmpegPath:  filepath.Join(t.TempDir(), "no-such-ffmpeg"),
		FFprobePath: filepath.Join(t.TempDir(), "no-such-ffprobe"),
		MaxFrames:   4,
	}}
	if err := validateFrameBoot(cfg); err == nil {
		t.Error("a missing ffmpeg binary must fail boot validation")
	}
}

// TestDevFakeModeratorIsBenignAndImageOnly: the M0 bootstrap model is the
// one Moderator that ships with no vendor behind it. It must stay benign,
// fully scored (no nil scores that would read as could-not-evaluate), and
// image-only — a fake that claimed video support would silently replace
// frame extraction.
func TestDevFakeModeratorIsBenignAndImageOnly(t *testing.T) {
	m := devFakeModerator{}
	if m.Name() != "dev-fake" {
		t.Errorf("Name = %q", m.Name())
	}
	caps := m.Capabilities()
	if caps.SupportsVideo {
		t.Error("the dev fake must not claim video support")
	}
	if caps.MaxImageBytes <= 0 {
		t.Error("MaxImageBytes must be set so the pipeline can pre-flight oversize images")
	}

	res, err := m.AnalyzeImage(context.Background(), moderation.Image{})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if len(res.Frames) != 1 || res.Frames[0].Status != moderation.FrameOK {
		t.Fatalf("want one ok frame, got %+v", res.Frames)
	}
	for _, c := range res.Frames[0].Categories {
		if c.Score == nil {
			t.Errorf("%s has a nil score; the bootstrap model must be scorable, not could-not-evaluate", c.Category)
			continue
		}
		if *c.Score < 0 || *c.Score > 0.1 {
			t.Errorf("%s score %v is not benign", c.Category, *c.Score)
		}
		if c.ScoreOrigin != moderation.OriginProbability {
			t.Errorf("%s score_origin = %q, want probability", c.Category, c.ScoreOrigin)
		}
	}
	if _, ok := any(m).(moderation.VideoModerator); ok {
		t.Error("the dev fake must not satisfy VideoModerator")
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
