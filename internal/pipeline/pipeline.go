// Package pipeline orchestrates one job end-to-end:
//
//	HashMatcher pre-stage -> frames -> per-frame fan-out -> Moderator ->
//	normalize (thresholds) -> asset rollup -> Sink.
//
// It is fail-safe: any provider/frame/extraction failure yields Verdict=error,
// never allow. Per-frame errors never cancel sibling frames (each frame is an
// independent evidence sample).
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
	"golang.org/x/sync/errgroup"
)

// JobRecorder counts finished jobs by overall verdict for observability
// (§F.6 vismod_jobs_total{verdict}). Optional — a nil Metrics is a no-op, so
// the one-shot scan path needs no metrics server. observe.Metrics satisfies it.
type JobRecorder interface {
	RecordJob(verdict moderation.Verdict)
}

// Pipeline holds the wired dependencies for processing jobs. One active
// Moderator per process; its shared rate limiter (M1) gates all fan-out.
type Pipeline struct {
	Moderator moderation.Moderator
	Frames    frames.FrameSource
	Matcher   moderation.HashMatcher
	Sink      result.Sink
	Cfg       config.Config
	Log       *slog.Logger
	Metrics   JobRecorder // optional; nil = no metrics
}

// Process handles one job: it builds and writes the ResultEnvelope to the Sink.
// The returned error is non-nil only on an infrastructure failure that should
// be retried/dead-lettered by the caller; a "could not evaluate" decision is a
// successful write of an error-verdict envelope (fail-safe).
func (p *Pipeline) Process(ctx context.Context, jobID result.JobID, src moderation.Source) error {
	started := time.Now().UTC()

	mediaType := detectMediaType(src)
	res, procErr := p.analyze(ctx, mediaType, src)

	// Stamp normalizer-owned provenance fields.
	res.SchemaVersion = moderation.SchemaVersion
	res.Provider = p.Moderator.Name()
	res.MediaType = mediaType
	res.AssetID = assetID(jobID, src)

	// Apply thresholds and roll up the asset verdict.
	p.applyThresholds(&res)
	res.Overall = p.rollup(res.Frames)

	if p.Metrics != nil {
		p.Metrics.RecordJob(res.Overall.Verdict)
	}

	env := result.ResultEnvelope{
		JobID:  jobID,
		Source: src,
		ModelID: result.ModelIdentity{
			Adapter:      p.Moderator.Name(),
			ModelVersion: res.ModelVersion,
			ConfigHash:   p.Cfg.ConfigHash(res.ModelVersion),
		},
		Result:     &res,
		StartedAt:  started.Format(time.RFC3339),
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if procErr != nil {
		env.Error = procErr.Error()
	}

	if err := p.Sink.Write(ctx, env); err != nil {
		return fmt.Errorf("sink write: %w", err)
	}
	return nil
}

// analyze produces an un-thresholded NormalizedResult (frames + raw categories).
// It never returns an error that maps to "allow": on failure it returns a
// result whose frames carry Status=error.
func (p *Pipeline) analyze(ctx context.Context, mediaType string, src moderation.Source) (moderation.NormalizedResult, error) {
	if mediaType == "video" {
		// Prefer a video-native adapter when one is present.
		if vm, ok := p.Moderator.(moderation.VideoModerator); ok && p.Moderator.Capabilities().SupportsVideo {
			res, err := vm.AnalyzeVideo(ctx, src)
			if err != nil {
				return errorResult(nil, err), err
			}
			return res, nil
		}
		return p.analyzeVideoByFrames(ctx, src)
	}
	return p.analyzeImage(ctx, src)
}

// analyzeImage moderates a single still image (one frame, TimestampSec nil).
func (p *Pipeline) analyzeImage(ctx context.Context, src moderation.Source) (moderation.NormalizedResult, error) {
	img, err := loadImage(src.Ref)
	if err != nil {
		return errorResult(nil, err), err
	}
	fr := p.moderateFrame(ctx, img, nil)
	res := moderation.NormalizedResult{Frames: []moderation.FrameResult{fr}}
	if fr.Status == moderation.FrameStatusError {
		return res, fmt.Errorf("%s", fr.Error)
	}
	return res, nil
}

// analyzeVideoByFrames extracts frames, then moderates each as an independent
// image. One frame's error never cancels its siblings.
func (p *Pipeline) analyzeVideoByFrames(ctx context.Context, src moderation.Source) (moderation.NormalizedResult, error) {
	fr, cleanup, err := p.Frames.Frames(ctx, src.Ref)
	// LIFECYCLE: delete the WorkDir on every exit path (error, cancel, panic).
	defer func() {
		if cerr := cleanup(); cerr != nil {
			p.Log.Warn("frame cleanup failed", "err", cerr)
		}
	}()
	if err != nil {
		// Extraction failure (incl. ErrNoFrames) is could-not-evaluate -> error,
		// never allow.
		return errorResult(nil, fmt.Errorf("frame extraction: %w", err)),
			fmt.Errorf("frame extraction: %w", err)
	}
	if len(fr) == 0 {
		return errorResult(nil, fmt.Errorf("no frames extracted")),
			fmt.Errorf("no frames extracted")
	}

	results := make([]moderation.FrameResult, len(fr))
	g, gctx := errgroup.WithContext(ctx)
	conc := p.Cfg.Frames.Concurrency
	if conc <= 0 {
		conc = 4
	}
	g.SetLimit(conc)

	for i, f := range fr {
		i, f := i, f
		g.Go(func() error {
			// Lazy decode INSIDE the task so at most `conc` images are resident.
			img, derr := loadImage(f.Path)
			ts := f.TimestampSec
			if derr != nil {
				results[i] = moderation.FrameResult{
					TimestampSec: &ts,
					Status:       moderation.FrameStatusError,
					Error:        derr.Error(),
				}
				return nil // never cancel siblings
			}
			res := p.moderateFrame(gctx, img, &ts)
			results[i] = res
			return nil
		})
	}
	_ = g.Wait() // no task returns a non-nil error; errors live in FrameResults

	return moderation.NormalizedResult{Frames: results}, nil
}

// moderateFrame runs the hash-match pre-stage then (if no match) the classifier
// for one decoded image. Pre-flight oversize rejection is terminal per frame.
func (p *Pipeline) moderateFrame(ctx context.Context, img moderation.Image, ts *float64) moderation.FrameResult {
	// Pre-stage: CSAM hash match short-circuits the classifier.
	if m, err := p.Matcher.Match(ctx, img); err == nil && m.Matched {
		return moderation.FrameResult{
			TimestampSec: ts,
			Status:       moderation.FrameStatusOK,
			Categories: []moderation.CategoryResult{{
				Category:    moderation.CategoryCSAMHashMatch,
				ScoreOrigin: moderation.ScoreOriginListMembership,
				Score:       nil,
				Threshold:   nil,
				Flagged:     true,
				MatchType:   m.Algo,
				MatchList:   m.ListName,
			}},
		}
	}

	// Pre-flight: reject oversize images before calling the classifier.
	if maxBytes := p.Moderator.Capabilities().MaxImageBytes; maxBytes > 0 && int64(len(img.Bytes)) > maxBytes {
		return moderation.FrameResult{
			TimestampSec: ts,
			Status:       moderation.FrameStatusError,
			Error:        fmt.Sprintf("image %d bytes exceeds adapter limit %d", len(img.Bytes), maxBytes),
		}
	}

	res, err := p.Moderator.AnalyzeImage(ctx, img)
	if err != nil {
		return moderation.FrameResult{TimestampSec: ts, Status: moderation.FrameStatusError, Error: err.Error()}
	}
	var cats []moderation.CategoryResult
	if len(res.Frames) > 0 {
		cats = res.Frames[0].Categories
	}
	return moderation.FrameResult{TimestampSec: ts, Status: moderation.FrameStatusOK, Categories: cats}
}

// errorResult builds a single-frame error result (could-not-evaluate).
func errorResult(ts *float64, err error) moderation.NormalizedResult {
	return moderation.NormalizedResult{
		Frames: []moderation.FrameResult{{
			TimestampSec: ts,
			Status:       moderation.FrameStatusError,
			Error:        err.Error(),
		}},
	}
}

// DetectMediaType resolves a Source to "image" or "video" (honoring an explicit
// MediaType, else by extension). Exported so commands can pre-flight the video
// path (e.g. boot-probe ffmpeg only when a video is involved).
func DetectMediaType(src moderation.Source) string { return detectMediaType(src) }

func detectMediaType(src moderation.Source) string {
	if src.MediaType == "image" || src.MediaType == "video" {
		return src.MediaType
	}
	switch strings.ToLower(filepath.Ext(src.Ref)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".mpeg", ".mpg":
		return "video"
	default:
		return "image"
	}
}

func assetID(jobID result.JobID, src moderation.Source) string {
	if src.Ref != "" {
		return src.Ref
	}
	return string(jobID)
}

func loadImage(path string) (moderation.Image, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return moderation.Image{}, fmt.Errorf("read image %q: %w", path, err)
	}
	return moderation.Image{Bytes: b, MIME: mimeFromExt(path)}, nil
}

func mimeFromExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	case ".webp":
		return "image/webp"
	case ".tif", ".tiff":
		return "image/tiff"
	default:
		return "application/octet-stream"
	}
}
