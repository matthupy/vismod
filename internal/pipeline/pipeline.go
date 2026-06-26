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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matthupy/vismod/internal/audit"
	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/internal/review"
	"github.com/matthupy/vismod/pkg/moderation"
	"golang.org/x/sync/errgroup"
)

// JobRecorder counts finished jobs by overall verdict for observability
// (§F.6 vismod_jobs_total{verdict}). Optional — a nil Metrics is a no-op, so
// the one-shot scan path needs no metrics server. observe.Metrics satisfies it.
type JobRecorder interface {
	RecordJob(verdict moderation.Verdict)
}

// Deduper provides durable cross-process once-only job recording (§L, issue #9).
// Optional: a nil Deduper falls back to the in-memory Sink/audit guards — the
// single-process scan/memq path. The redis-backed impl (internal/dedup) makes
// dedup survive a restart and span replicas under the at-least-once redis queue.
//
// ORDERING: Process checks Done BEFORE the writes and calls Commit only AFTER
// Sink+audit succeed (write-then-commit) — fail-safe: a crash before Commit
// redelivers and redoes the job, never a silent loss.
type Deduper interface {
	Done(ctx context.Context, jobID string) (bool, error)
	Commit(ctx context.Context, jobID string) error
}

// DivertFailureRecorder counts flagged-frame diverts that failed to reach the
// review channel (§G.8 vismod_divert_failures_total). It is an OPTIONAL
// capability discovered by type-asserting the wired Metrics — a recorder that
// does not implement it (or a nil Metrics) makes the bump a no-op, so the divert
// stays fail-safe. observe.Metrics satisfies it.
type DivertFailureRecorder interface {
	RecordDivertFailure()
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
	Metrics   JobRecorder     // optional; nil = no metrics
	Audit     *audit.Log      // optional; nil = no audit trail (CLI scan default)
	Diverter  review.Diverter // optional; nil = no flagged-frame divert (§G.8)
	Dedup     Deduper         // optional; nil = in-memory guards only (scan/memq)
}

// Process handles one job: it builds and writes the ResultEnvelope to the Sink.
// The returned error is non-nil only on an infrastructure failure that should
// be retried/dead-lettered by the caller; a "could not evaluate" decision is a
// successful write of an error-verdict envelope (fail-safe).
func (p *Pipeline) Process(ctx context.Context, jobID result.JobID, src moderation.Source) error {
	started := time.Now().UTC()

	// Cross-process dedup gate (§L, issue #9): skip a job already durably
	// recorded so a redelivery to a fresh process/replica does not re-analyze or
	// double-write the Sink/audit. A Done error is an infra failure, not a
	// verdict — return it so the job is retried/dead-lettered (never auto-allow,
	// never silently skip).
	if p.Dedup != nil {
		done, err := p.Dedup.Done(ctx, string(jobID))
		if err != nil {
			return fmt.Errorf("dedup done check: %w", err)
		}
		if done {
			return nil
		}
	}

	mediaType := detectMediaType(src)
	meta := jobMeta{ID: string(jobID), AssetID: assetID(jobID, src)}
	res, procErr := p.analyze(ctx, mediaType, src, meta)

	// Stamp normalizer-owned provenance fields.
	res.SchemaVersion = moderation.SchemaVersion
	res.Provider = p.Moderator.Name()
	res.MediaType = mediaType
	res.AssetID = assetID(jobID, src)

	// Apply thresholds and roll up the asset verdict.
	p.applyThresholds(&res)
	res.Overall = p.rollup(res.Frames)

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

	// Tamper-evident audit (§G.5): bind the decision to its inputs BY HASH after
	// the Sink commits. Idempotent per JobID within one process (in-memory `seen`),
	// so in-process retry never double-appends. Stores SHA-256(Raw) + ModelIdentity
	// + verdict — NEVER Raw itself (§G.2). An audit failure is an infra error: the
	// job is retried (Sink and audit are both idempotent per JobID, so retry is
	// safe). SEQUENTIAL cross-process redelivery (a fresh process/replica picking
	// up the job AFTER the first worker died) is gated earlier by p.Dedup so it
	// never reaches this append twice (§L, issue #9). KNOWN RESIDUAL: the gate
	// orders writes vs. the durable claim but provides NO mutual exclusion, so it
	// does not stop a genuinely CONCURRENT second worker (asynq lease-recovery can
	// re-queue a job past JobTimeout while the first goroutine is still draining
	// after ctx-cancel — both see Done=false). That overlap, like a crash strictly
	// between the writes and Commit, is an accepted v1 residual; a SETNX claim/lease
	// (Deduper godoc, design doc) is the future hardening seam if it must close.
	// SEPARATE CONCERN: the dedup gate only covers SAME-job redelivery. It does
	// NOT cover DIFFERENT-job concurrent writers across replicas appending to ONE
	// shared audit chain file — audit.Log idempotency/ordering is a per-process
	// `mu` only (see audit.Log godoc). Deployment invariant: each replica owns its
	// own chain file (or audit writers are serialized) before sharing one.
	if p.Audit != nil {
		pl := audit.Payload{
			JobID:        string(jobID),
			Verdict:      string(res.Overall.Verdict),
			RawSHA256:    audit.RawSHA256(res.Raw),
			Adapter:      env.ModelID.Adapter,
			ModelVersion: env.ModelID.ModelVersion,
			ConfigHash:   env.ModelID.ConfigHash,
		}
		if _, _, err := p.Audit.Append(pl, env.FinishedAt); err != nil {
			return fmt.Errorf("audit append: %w", err)
		}
	}

	// Commit the cross-process dedup claim only AFTER Sink+audit succeed
	// (write-then-commit, §L). A Commit failure is an infra error: return it so
	// the job retries; the next attempt re-writes (Sink/audit are idempotent per
	// JobID within a process) and re-commits. Fail-safe: never auto-allow.
	if p.Dedup != nil {
		if err := p.Dedup.Commit(ctx, string(jobID)); err != nil {
			return fmt.Errorf("dedup commit: %w", err)
		}
	}

	// Count only AFTER a successful emit. Counting earlier would (a) inflate
	// jobs_total for a job whose envelope never reached the sink and (b) double
	// count on queue retry, since a sink-write failure re-runs Process. Recording
	// here makes vismod_jobs_total = jobs successfully emitted, once each.
	if p.Metrics != nil {
		p.Metrics.RecordJob(res.Overall.Verdict)
	}
	return nil
}

// analyze produces an un-thresholded NormalizedResult (frames + raw categories).
// It never returns an error that maps to "allow": on failure it returns a
// result whose frames carry Status=error.
func (p *Pipeline) analyze(ctx context.Context, mediaType string, src moderation.Source, meta jobMeta) (moderation.NormalizedResult, error) {
	if mediaType == "video" {
		// Prefer a video-native adapter when one is present.
		if vm, ok := p.Moderator.(moderation.VideoModerator); ok && p.Moderator.Capabilities().SupportsVideo {
			res, err := vm.AnalyzeVideo(ctx, src)
			if err != nil {
				return errorResult(nil, err), err
			}
			return res, nil
		}
		return p.analyzeVideoByFrames(ctx, src, meta)
	}
	return p.analyzeImage(ctx, src, meta)
}

// analyzeImage moderates a single still image (one frame, TimestampSec nil).
func (p *Pipeline) analyzeImage(ctx context.Context, src moderation.Source, meta jobMeta) (moderation.NormalizedResult, error) {
	img, err := loadImage(src.Ref)
	if err != nil {
		return errorResult(nil, err), err
	}
	fr := p.moderateFrame(ctx, img, nil, meta)
	res := moderation.NormalizedResult{Frames: []moderation.FrameResult{fr}}
	if fr.Status == moderation.FrameStatusError {
		return res, fmt.Errorf("%s", fr.Error)
	}
	return res, nil
}

// analyzeVideoByFrames extracts frames, then moderates each as an independent
// image. One frame's error never cancels its siblings.
func (p *Pipeline) analyzeVideoByFrames(ctx context.Context, src moderation.Source, meta jobMeta) (moderation.NormalizedResult, error) {
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
			res := p.moderateFrame(gctx, img, &ts, meta)
			results[i] = res
			return nil
		})
	}
	_ = g.Wait() // no task returns a non-nil error; errors live in FrameResults

	return moderation.NormalizedResult{Frames: results}, nil
}

// moderateFrame runs the hash-match pre-stage then (if no match) the classifier
// for one decoded image. Pre-flight oversize rejection is terminal per frame.
func (p *Pipeline) moderateFrame(ctx context.Context, img moderation.Image, ts *float64, meta jobMeta) moderation.FrameResult {
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
	// §G.8: a frame whose category score lands in the flag band ([flag_at,
	// block_at)) is flagged for manual review. Divert it to human review BEFORE
	// Sink.Write, carrying only SHA-256(frame) — never the frame bytes or Raw
	// (§G.2). Scores >= block_at auto-block and are NOT diverted.
	//
	// DELIVERY CONTRACT — AT-LEAST-ONCE (unlike Sink.Write + Audit.Append, which
	// are idempotent per JobID). Divert fires here inside analyze, which re-runs
	// in full whenever Process is retried (a transient sink/audit failure
	// dead-letters and redelivers the whole job). The pipeline cannot dedup across
	// that retry: redelivery may land on a different worker, so any in-process
	// seen-set is lost. The (JobID, FrameSHA256, Category) triple on review.Item is
	// the stable dedup key — the downstream Diverter (durable review queue) MUST
	// collapse repeats on it. A frame flagged in N categories emits N Items that
	// share (JobID, FrameSHA256) but differ in Category, so all N survive while a
	// retry's identical re-emission still collapses. v1 LogDiverter is dedup-exempt
	// (a repeated WARN is harmless).
	p.divertFlagged(ctx, meta, img, ts, cats)
	return moderation.FrameResult{TimestampSec: ts, Status: moderation.FrameStatusOK, Categories: cats}
}

// jobMeta carries per-job identity into the frame fan-out so a divert can be
// attributed without re-threading the whole Source/JobID.
type jobMeta struct {
	ID      string
	AssetID string
}

// divertFlagged routes a frame to human review when any category score lands in
// the flag band [flag_at, block_at) for that category (§G.8). Scores >= block_at
// auto-block and are NOT diverted; scores < flag_at allow. It is a no-op when no
// Diverter is configured.
//
// Flagged is not yet stamped on the CategoryResult here (applyThresholds runs
// later in Process), so the band is computed inline from the per-category config.
func (p *Pipeline) divertFlagged(ctx context.Context, meta jobMeta, img moderation.Image, ts *float64, cats []moderation.CategoryResult) {
	if p.Diverter == nil {
		return
	}
	// SHA-256 over the frame bytes is identical for every in-band category, and
	// frames can be MB — so hash at most ONCE per frame, lazily on the first
	// match, and reuse the hex digest for every emitted Item.
	var frameSHA string
	for _, c := range cats {
		if c.Score == nil {
			continue
		}
		ct := p.Cfg.Thresholds.For(c.Category)
		if *c.Score < ct.FlagAt || *c.Score >= ct.BlockAt {
			continue
		}
		if frameSHA == "" {
			sum := sha256.Sum256(img.Bytes)
			frameSHA = hex.EncodeToString(sum[:])
		}
		score := *c.Score
		it := review.Item{
			JobID:        meta.ID,
			AssetID:      meta.AssetID,
			FrameSHA256:  frameSHA,
			TimestampSec: ts,
			Category:     string(c.Category),
			Score:        &score,
			Reason:       "flagged: flag_at <= score < block_at",
		}
		if err := p.Diverter.Divert(ctx, it); err != nil {
			// FAIL-SAFE: a dropped divert must NOT block the job (the §G.8 seam is
			// best-effort by contract). But a silently-lost flagged frame is the one
			// thing §G.8 must not hide, so make the drop observable via a counter an
			// operator can alert on — log alone is not alertable.
			p.Log.Warn("flagged frame divert failed", "job_id", meta.ID, "err", err)
			// TODO(v1.1): p.Metrics is nil on the scan path, so a real erroring
			// Diverter's failure goes uncounted here (only logged). Harmless today
			// (LogDiverter never errors); wire a Metrics sink on the scan path
			// before shipping a Diverter that can fail.
			if r, ok := p.Metrics.(DivertFailureRecorder); ok {
				r.RecordDivertFailure()
			}
		}
		// No early return: one Item per in-band category. The dedup key
		// (JobID, FrameSHA256, Category) keeps these distinct downstream.
	}
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
