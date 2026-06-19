package cli

import (
	"errors"
	"testing"

	"github.com/matthupy/videosift"
	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/frames"
)

// buildFrameSource must translate config into a videosift-backed source,
// carrying the frame knobs (so a video job actually extracts via videosift,
// not the M0 fake).
func TestBuildFrameSourceUsesVideosift(t *testing.T) {
	cfg := config.Config{Frames: config.FramesConfig{
		WorkDir:     t.TempDir(),
		MaxFrames:   16,
		Scene:       true,
		Keyframe:    true,
		Temporal:    true,
		MPDecimate:  true,
		FFmpegPath:  "ffmpeg",
		FFprobePath: "ffprobe",
	}}
	fs := buildFrameSource(cfg)
	if _, ok := fs.(*frames.VideosiftSource); !ok {
		t.Fatalf("buildFrameSource returned %T, want *frames.VideosiftSource", fs)
	}
}

// probeFrameSource must fail fast (boot validation) when ffmpeg/ffprobe are
// absent, wrapping videosift.ErrNoBinaries.
func TestProbeFrameSourceMissingBinaries(t *testing.T) {
	cfg := config.Config{Frames: config.FramesConfig{
		FFmpegPath:  "no-such-ffmpeg-xyz",
		FFprobePath: "no-such-ffprobe-xyz",
	}}
	err := probeFrameSource(cfg)
	if err == nil {
		t.Fatal("probeFrameSource with bogus binaries returned nil, want error")
	}
	if !errors.Is(err, videosift.ErrNoBinaries) {
		t.Errorf("err = %v, want errors.Is ErrNoBinaries", err)
	}
}
