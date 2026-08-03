// Package pipeline wires the per-job flow:
//
//	frames -> bounded fan-out -> Moderator
//	  -> normalize/thresholds -> rollup -> Sink -> ack/DLQ
//
// Retry policy: transient provider errors (429/5xx/timeouts) are retried
// with bounded backoff inside the adapter's HTTP client; once those are
// exhausted a frame error is final and the job dead-letters with
// Verdict=error (fail safe — never allow). The queue-level Retry
// disposition is reserved for job-level transient infrastructure failures
// (e.g. Sink write errors), where no result has been emitted yet.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/fetch"
	"github.com/vismod/vismod/internal/frames"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

// AuditSink receives one audit record per completed job. Implementations
// must be idempotent per JobID. Nil disables auditing.
type AuditSink interface {
	Record(ctx context.Context, env result.ResultEnvelope) error
}

// EventSink records operational audit events (audit.Log satisfies it).
type EventSink interface {
	AppendEvent(kind string, fields map[string]string) error
}

// errEmptyVideoSkipped signals the gated §F.5 override path: zero frames
// extracted AND the operator enabled allow_empty_video_skip. The job is
// acked with NO verdict emitted (an operational skip, not a Verdict
// value) and a prominent audit event.
var errEmptyVideoSkipped = errors.New("empty video skipped by operator override")

// SourceFetcher resolves a remote source URL to a local file. nil means
// no fetcher was wired, and every url job fails with verdict:"error".
type SourceFetcher interface {
	Fetch(ctx context.Context, rawURL, dir string) (path string, cleanup func(), err error)
}

// Pipeline processes jobs. All collaborators sit behind interfaces and
// swap via config with zero call-site changes.
type Pipeline struct {
	Moderator   moderation.Moderator
	FrameSource frames.FrameSource
	Sink        result.Sink
	Audit       AuditSink
	Thresholds  config.Thresholds
	Concurrency int // frame fan-out bound (frames.concurrency)
	ModelID     result.ModelIdentity
	Log         *slog.Logger
	// Events receives operational audit events (gated override use).
	Events EventSink
	// AllowEmptyVideoSkip enables the audited §F.5 zero-frames override.
	AllowEmptyVideoSkip bool
	// Dedup enables the post-extraction near-duplicate filter; frames
	// within DedupThreshold Hamming distance (dHash) of a kept frame are
	// dropped before the moderation fan-out.
	Dedup          bool
	DedupThreshold int
	// Fetcher resolves kind:"url" sources. nil disables them.
	Fetcher SourceFetcher
	// OnFetch records fetch outcomes for metrics. nil disables. It exists
	// so the pipeline never imports Prometheus; reason comes from
	// fetch.Reason and is always from that bounded label set.
	OnFetch func(d time.Duration, bytes int64, reason string)
	// MaxScanFrames caps frames per video reaching the moderation
	// fan-out, applied AFTER post-processing (dedup) so duplicates are
	// removed before anything is cut for budget. 0 = no cap.
	MaxScanFrames int
}

func (p *Pipeline) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// Handler adapts Process to the queue.Handler signature.
func (p *Pipeline) Handler() queue.Handler {
	return func(ctx context.Context, j queue.Job) (queue.Disposition, error) {
		return p.Process(ctx, j)
	}
}

// Process runs one job end-to-end and maps the outcome to a Disposition.
func (p *Pipeline) Process(ctx context.Context, j queue.Job) (queue.Disposition, error) {
	_, disp, err := p.ProcessJob(ctx, j)
	return disp, err
}

// ProcessJob is Process plus the emitted envelope (used by the one-shot
// CLI for exit codes).
func (p *Pipeline) ProcessJob(ctx context.Context, j queue.Job) (result.ResultEnvelope, queue.Disposition, error) {
	started := time.Now().UTC()

	rs, cleanupSource, resolveErr := p.resolveSource(ctx, j)
	// Lifecycle contract: deferred immediately, so the download is removed
	// on every exit path — error, ctx-cancel, panic — before ack.
	defer cleanupSource()

	// From here on, j carries the LOCAL source (what analysis reads) and
	// rs.env carries what gets recorded.
	j.Source = rs.local

	p.log().Info("job started",
		"job_id", j.ID, "adapter", p.ModelID.Adapter,
		"media_type", rs.env.MediaType, "ref", rs.env.Ref,
		"workflows", workflowsLabel(j.Workflows))

	var res moderation.NormalizedResult
	var procErr error
	switch {
	case resolveErr != nil:
		procErr = resolveErr
	case rs.env.MediaType == "video":
		res, procErr = p.processVideo(ctx, j)
	default:
		res, procErr = p.processImage(ctx, j)
	}
	if errors.Is(procErr, errEmptyVideoSkipped) {
		// Gated §F.5 override: ack with no verdict, prominent audit event.
		p.log().Warn("OPERATOR OVERRIDE: empty video skipped without verdict (failsafe.allow_empty_video_skip)",
			"job_id", j.ID, "asset", rs.env.Ref)
		if p.Events != nil {
			if err := p.Events.AppendEvent("empty_video_skip_override", map[string]string{
				"job_id":      string(j.ID),
				"asset_id":    rs.env.Ref,
				"adapter":     p.ModelID.Adapter,
				"config_hash": p.ModelID.ConfigHash,
			}); err != nil {
				return result.ResultEnvelope{}, queue.DeadLetter, fmt.Errorf("override audit event failed: %w", err)
			}
		}
		return result.ResultEnvelope{JobID: j.ID, Source: rs.env, ModelID: p.ModelID, StartedAt: started, FinishedAt: time.Now().UTC()}, queue.Ack, nil
	}
	if procErr != nil {
		// Could-not-evaluate before any per-frame evidence existed
		// (unreadable input, extraction failure, zero frames). Fail safe:
		// error verdict, dead-letter, never allow.
		res = p.errorResult(j, procErr)
	}

	// Stamp normalizer-owned fields (the adapter leaves them empty).
	res.SchemaVersion = moderation.SchemaVersion
	if res.AssetID = rs.env.Ref; res.AssetID == "" {
		res.AssetID = string(j.ID)
	}
	if res.MediaType == "" {
		res.MediaType = rs.env.MediaType
	}
	res.Overall = Rollup(res.Frames, p.Thresholds)

	env := result.ResultEnvelope{
		JobID:      j.ID,
		Source:     rs.env,
		ModelID:    p.ModelID,
		Result:     &res,
		StartedAt:  started,
		FinishedAt: time.Now().UTC(),
	}
	if res.Overall.Verdict == moderation.VerdictError {
		env.Error = p.frameErrors(res.Frames, procErr)
	}

	if err := p.Sink.Write(ctx, env); err != nil {
		// With MultiSink an error does NOT mean nothing was emitted: every
		// sink is attempted, so some may already have written. Retry is
		// safe because each sink is idempotent per JobID within this
		// process — not because the write did not happen. Note the audit
		// record is NOT written on this path; see the sink gotcha in
		// AGENTS.md before changing this disposition.
		return env, queue.Retry, fmt.Errorf("sink write: %w", err)
	}
	if p.Audit != nil {
		if err := p.Audit.Record(ctx, env); err != nil {
			p.log().Error("audit record failed", "job_id", j.ID, "err", err)
			return env, queue.DeadLetter, fmt.Errorf("audit append: %w", err)
		}
	}

	p.logScanComplete(j, res, started)

	if res.Overall.Verdict == moderation.VerdictError {
		return env, queue.DeadLetter, fmt.Errorf("verdict=error: %s", env.Error)
	}
	return env, queue.Ack, nil
}

// resolved holds the two views of a job's source that must not be
// conflated: what ANALYSIS reads, and what gets RECORDED.
//
// For a url source those differ. Analysis needs the local download;
// the envelope, audit record, and logs must carry the redacted URL,
// because a presigned URL's query string is a credential and a temp path
// is meaningless to whoever reads the verdict later.
type resolved struct {
	local moderation.Source // kind:"file", ref = local path
	env   moderation.Source // what is recorded
}

// resolveSource materializes a job's source. cleanup is always non-nil
// and must be deferred by the caller on every exit path.
func (p *Pipeline) resolveSource(ctx context.Context, j queue.Job) (resolved, func(), error) {
	if j.Source.Kind != "url" {
		return resolved{local: j.Source, env: j.Source}, func() {}, nil
	}

	// Redact FIRST, so every return path below — including the failures —
	// reports the safe form.
	ref, digest := fetch.Redact(j.Source.Ref)
	envSrc := moderation.Source{
		Kind:      "url",
		Ref:       ref,
		RefDigest: digest,
		MediaType: j.Source.MediaType,
	}

	if p.Fetcher == nil {
		return resolved{local: envSrc, env: envSrc}, func() {},
			fmt.Errorf("no url fetcher is wired into this pipeline")
	}

	dir, err := os.MkdirTemp("", "vismod-fetch-")
	if err != nil {
		return resolved{local: envSrc, env: envSrc}, func() {},
			fmt.Errorf("fetch workdir: %w", err)
	}
	rmDir := func() {
		if err := os.RemoveAll(dir); err != nil {
			p.log().Error("fetch workdir cleanup failed", "job_id", j.ID, "err", err)
		}
	}

	fetchStart := time.Now()
	path, cleanFile, err := p.Fetcher.Fetch(ctx, j.Source.Ref, dir)
	p.recordFetch(fetchStart, path, err)
	cleanup := func() {
		if cleanFile != nil {
			cleanFile()
		}
		rmDir()
	}
	if err != nil {
		return resolved{local: envSrc, env: envSrc}, cleanup, err
	}
	return resolved{
		local: moderation.Source{Kind: "file", Ref: path, MediaType: j.Source.MediaType},
		env:   envSrc,
	}, cleanup, nil
}

// recordFetch reports one fetch outcome. The byte count comes from the
// file on disk, not from Content-Length, which is never trusted.
func (p *Pipeline) recordFetch(start time.Time, path string, err error) {
	if p.OnFetch == nil {
		return
	}
	var n int64
	if err == nil && path != "" {
		if fi, statErr := os.Stat(path); statErr == nil {
			n = fi.Size()
		}
	}
	p.OnFetch(time.Since(start), n, fetch.Reason(err))
}

// processImage handles a still image: one FrameResult, TimestampSec nil.
func (p *Pipeline) processImage(ctx context.Context, j queue.Job) (moderation.NormalizedResult, error) {
	fr, err := p.evaluateFrame(ctx, j.Source.Ref, nil)
	if err != nil {
		return moderation.NormalizedResult{}, err
	}
	return moderation.NormalizedResult{
		Provider:     p.Moderator.Name(),
		ModelVersion: p.ModelID.ModelVersion,
		MediaType:    "image",
		Frames:       []moderation.FrameResult{fr},
	}, nil
}

// processVideo prefers a video-native adapter; otherwise extracts frames
// and moderates each as an image.
func (p *Pipeline) processVideo(ctx context.Context, j queue.Job) (moderation.NormalizedResult, error) {
	if vm, ok := p.Moderator.(moderation.VideoModerator); ok && p.Moderator.Capabilities().SupportsVideo {
		res, err := vm.AnalyzeVideo(ctx, j.Source)
		if err != nil {
			return moderation.NormalizedResult{}, fmt.Errorf("video-native analyze: %w", err)
		}
		for i := range res.Frames {
			res.Frames[i].Categories = ApplyThresholds(res.Frames[i].Categories, p.Thresholds)
		}
		return res, nil
	}

	if p.FrameSource == nil {
		return moderation.NormalizedResult{}, fmt.Errorf("no frame source configured for video input")
	}
	fs, cleanup, err := p.FrameSource.Frames(ctx, j.Source.Ref, j.Workflows)
	if err != nil {
		if cleanup != nil {
			p.runCleanup(j.ID, cleanup)
		}
		return moderation.NormalizedResult{}, fmt.Errorf("frame extraction: %w", err)
	}
	// Lifecycle contract: cleanup is deferred BEFORE any fan-out so the
	// WorkDir is deleted on every exit path (error, ctx-cancel, panic).
	defer p.runCleanup(j.ID, cleanup)

	if len(fs) == 0 {
		// Zero frames is could-not-evaluate, never clean: a static or
		// looping harmful video must not pass by producing no frames.
		// Only the gated, audited operator override downgrades this to an
		// operational skip (job acked, NO verdict emitted).
		if p.AllowEmptyVideoSkip {
			return moderation.NormalizedResult{}, errEmptyVideoSkipped
		}
		return moderation.NormalizedResult{}, fmt.Errorf("frame extraction produced zero frames")
	}

	// Optional post-processing: drop visually near-duplicate frames
	// (Hamming distance over dHash) before spending moderation calls.
	// The first occurrence always survives, so this can never produce an
	// empty set out of a non-empty one. The job may override the config:
	// nil inherits; 0..64 enables at that threshold; negative disables.
	enabled, threshold := p.Dedup, p.DedupThreshold
	if j.DedupThreshold != nil {
		enabled = *j.DedupThreshold >= 0
		threshold = *j.DedupThreshold
	}
	if enabled {
		var removed int
		fs, removed = frames.Dedup(fs, threshold)
		if removed > 0 {
			p.log().Info("near-duplicate frames removed before moderation",
				"job_id", j.ID, "removed", removed, "kept", len(fs),
				"hamming_threshold", threshold)
		}
	}

	// Scan cap (max_frames), applied AFTER post-processing so dedup got
	// to collapse duplicates first. Truncation keeps the earliest frames
	// in workflow order — the tail of the video goes unscanned, hence
	// the WARN: this is a cost backstop, not a tuning lever.
	if p.MaxScanFrames > 0 && len(fs) > p.MaxScanFrames {
		p.log().Warn("post-processed frames exceed max_frames; truncating (video tail unscanned)",
			"job_id", j.ID, "post_processed", len(fs), "max_frames", p.MaxScanFrames)
		fs = fs[:p.MaxScanFrames]
	}

	results := make([]moderation.FrameResult, len(fs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.concurrency())
	for i, f := range fs {
		g.Go(func() error {
			// Each frame is an independent evidence sample: capture the
			// outcome and return nil so one frame's failure never cancels
			// siblings. Lazy decode: bytes are read inside the task, so at
			// most Concurrency frames are resident at once.
			defer func() {
				if r := recover(); r != nil {
					ts := f.TimestampSec
					results[i] = moderation.FrameResult{
						TimestampSec: &ts,
						Status:       moderation.FrameError,
						Error:        fmt.Sprintf("frame task panic: %v", r),
						Categories:   []moderation.CategoryResult{},
					}
				}
			}()
			ts := f.TimestampSec
			fr, err := p.evaluateFrame(gctx, f.Path, &ts)
			if err != nil {
				fr = moderation.FrameResult{
					TimestampSec: &ts,
					Status:       moderation.FrameError,
					Error:        err.Error(),
					Categories:   []moderation.CategoryResult{},
				}
			}
			results[i] = fr
			return nil
		})
	}
	_ = g.Wait() // tasks always return nil; outcomes are in results

	sort.SliceStable(results, func(a, b int) bool {
		ta, tb := results[a].TimestampSec, results[b].TimestampSec
		if ta == nil || tb == nil {
			return ta != nil
		}
		return *ta < *tb
	})

	return moderation.NormalizedResult{
		Provider:     p.Moderator.Name(),
		ModelVersion: p.ModelID.ModelVersion,
		MediaType:    "video",
		Frames:       results,
	}, nil
}

// evaluateFrame runs one image (a still or one extracted frame) through
// pre-flight, the Moderator, and threshold application.
func (p *Pipeline) evaluateFrame(ctx context.Context, path string, ts *float64) (moderation.FrameResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return moderation.FrameResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	img := moderation.Image{Bytes: raw, MIME: sniffMIME(raw)}

	// Pre-flight oversize images before the adapter (and before its rate
	// limiter spends a token).
	caps := p.Moderator.Capabilities()
	if caps.MaxImageBytes > 0 && int64(len(raw)) > caps.MaxImageBytes {
		return moderation.FrameResult{}, fmt.Errorf("image is %d bytes, adapter %s max is %d (terminal)",
			len(raw), p.Moderator.Name(), caps.MaxImageBytes)
	}

	res, err := p.Moderator.AnalyzeImage(ctx, img)
	if err != nil {
		return moderation.FrameResult{}, fmt.Errorf("analyze: %w", err)
	}
	if len(res.Frames) == 0 {
		return moderation.FrameResult{}, fmt.Errorf("adapter %s returned no frame result", p.Moderator.Name())
	}
	fr := res.Frames[0]
	fr.TimestampSec = ts
	fr.Status = moderation.FrameOK
	fr.Categories = ApplyThresholds(fr.Categories, p.Thresholds)
	return fr, nil
}

// logScanComplete emits the INFO summary (overall rollup) and, at DEBUG,
// one line per frame with its per-category scores. Fields carry scores,
// verdicts, and refs only — never media bytes, Raw, or free text.
func (p *Pipeline) logScanComplete(j queue.Job, res moderation.NormalizedResult, started time.Time) {
	log := p.log()
	log.Info("scan complete",
		"job_id", j.ID, "adapter", p.ModelID.Adapter,
		"media_type", res.MediaType, "verdict", res.Overall.Verdict,
		"flagged", res.Overall.Flagged,
		"top_category", derefCategory(res.Overall.TopCategory),
		"max_score", derefScore(res.Overall.MaxScore),
		"confidence", derefScore(res.Overall.Confidence),
		"frames", len(res.Frames), "latency", time.Since(started))

	if !log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	for i, fr := range res.Frames {
		attrs := []any{
			"job_id", j.ID, "frame", i,
			"timestamp_sec", derefScore(fr.TimestampSec),
			"status", fr.Status,
		}
		if fr.Error != "" {
			attrs = append(attrs, "error", fr.Error)
		}
		var cats strings.Builder
		for ci, c := range fr.Categories {
			if ci > 0 {
				cats.WriteByte(' ')
			}
			cats.WriteString(string(c.Category))
			cats.WriteByte('=')
			if c.Score == nil {
				cats.WriteString("null")
			} else {
				fmt.Fprintf(&cats, "%.3f", *c.Score)
			}
			if c.Flagged {
				cats.WriteString("(flagged)")
			}
		}
		attrs = append(attrs, "categories", cats.String())
		log.Debug("frame result", attrs...)
	}
}

func derefScore(f *float64) any {
	if f == nil {
		return "null"
	}
	return *f
}

func derefCategory(c *moderation.Category) any {
	if c == nil {
		return "null"
	}
	return string(*c)
}

func workflowsLabel(w []string) string {
	if len(w) == 0 {
		return "(default)"
	}
	return strings.Join(w, ",")
}

func (p *Pipeline) runCleanup(id queue.JobID, cleanup func() error) {
	if cleanup == nil {
		return
	}
	// Cleanup errors are logged but never change the verdict.
	if err := cleanup(); err != nil {
		p.log().Error("frame workdir cleanup failed", "job_id", id, "err", err)
	}
}

// errorResult builds the fail-safe could-not-evaluate result.
func (p *Pipeline) errorResult(j queue.Job, cause error) moderation.NormalizedResult {
	name := ""
	if p.Moderator != nil {
		name = p.Moderator.Name()
	}
	return moderation.NormalizedResult{
		Provider:     name,
		ModelVersion: p.ModelID.ModelVersion,
		MediaType:    j.Source.MediaType,
		Frames: []moderation.FrameResult{{
			TimestampSec: nil,
			Status:       moderation.FrameError,
			Error:        cause.Error(),
			Categories:   []moderation.CategoryResult{},
		}},
	}
}

func (p *Pipeline) frameErrors(frs []moderation.FrameResult, procErr error) string {
	if procErr != nil {
		return procErr.Error()
	}
	var msgs []string
	for _, f := range frs {
		if f.Status == moderation.FrameError && f.Error != "" {
			msgs = append(msgs, f.Error)
		}
	}
	if len(msgs) == 0 {
		return "could not evaluate (no scorable signal)"
	}
	return strings.Join(msgs, "; ")
}

func (p *Pipeline) concurrency() int {
	if p.Concurrency > 0 {
		return p.Concurrency
	}
	return 4
}

func sniffMIME(b []byte) string {
	if len(b) > 512 {
		b = b[:512]
	}
	return http.DetectContentType(b)
}
