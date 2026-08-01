package observe

import (
	"sync"
	"time"
)

// JobRecord is one finished job's outcome as shown on operator surfaces.
// It carries the job's opaque ref and verdict metadata ONLY — never media
// bytes, Raw payloads, OCR text, or scores' underlying content.
type JobRecord struct {
	ID          string `json:"id"`
	Ref         string `json:"ref"` // opaque source ref (file path)
	MediaType   string `json:"media_type"`
	Verdict     string `json:"verdict"` // allow|flag|block|error|skip
	TopCategory string `json:"top_category,omitempty"`
	// MaxScore/Confidence mirror the envelope's overall rollup: nil when
	// no non-nil score existed (never collapsed to 0).
	MaxScore   *float64 `json:"max_score"`
	Confidence *float64 `json:"confidence"`
	// FramesScanned is the number of frames evaluated for this job (1
	// for still images; post-dedup count for videos). FramesFlagged is
	// how many of them carried at least one flagged category — a
	// flagged video with only 1 flagged frame has no sampling margin,
	// so reducing extraction density risks recall on assets like it.
	FramesScanned int       `json:"frames_scanned"`
	FramesFlagged int       `json:"frames_flagged"`
	FinishedAt    time.Time `json:"finished_at"`
	DurationMS    int64     `json:"duration_ms"`
}

// JobTracker keeps lifetime verdict counters plus a bounded ring of the
// most recent job outcomes for the UI. Retries are not final outcomes and
// must not be recorded.
type JobTracker struct {
	mu     sync.Mutex
	ring   []JobRecord // newest last; bounded by cap
	max    int
	counts map[string]int
	total  int

	// Frame-extraction counters (lifetime), for tuning FFmpeg workflow
	// density against moderation quality.
	totalFrames        int
	videoJobs          int
	videoFrames        int
	flaggedVideos      int // video jobs with verdict flag|block
	flaggedVideoFrames int // flagged frames within those jobs
	singleFrameFlags   int // flagged videos where EXACTLY 1 frame flagged
}

// NewJobTracker keeps the last max records (counters are lifetime).
func NewJobTracker(max int) *JobTracker {
	if max <= 0 {
		max = 200
	}
	return &JobTracker{max: max, counts: map[string]int{}}
}

func (t *JobTracker) Record(r JobRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total++
	t.counts[r.Verdict]++
	t.totalFrames += r.FramesScanned
	if r.MediaType == "video" {
		t.videoJobs++
		t.videoFrames += r.FramesScanned
		if r.Verdict == "flag" || r.Verdict == "block" {
			t.flaggedVideos++
			t.flaggedVideoFrames += r.FramesFlagged
			if r.FramesFlagged == 1 {
				t.singleFrameFlags++
			}
		}
	}
	t.ring = append(t.ring, r)
	if len(t.ring) > t.max {
		t.ring = t.ring[len(t.ring)-t.max:]
	}
}

// VerdictStats is the aggregate snapshot.
//
// FlagRate and BlockRate are fractions of successfully EVALUATED jobs
// (allow+flag+block): a job that errored (or was skipped) produced no
// verdict and must not dilute them. ErrorRate is a fraction of ALL
// finished jobs. Example: 1 block + 1 allow + 1 error → flag_rate 0.5,
// block_rate 0.5, error_rate 0.333.
type VerdictStats struct {
	Total     int            `json:"total"`     // all finished jobs
	Evaluated int            `json:"evaluated"` // allow+flag+block
	Counts    map[string]int `json:"counts"`
	FlagRate  float64        `json:"flag_rate"`  // (flag+block)/evaluated
	BlockRate float64        `json:"block_rate"` // block/evaluated
	ErrorRate float64        `json:"error_rate"` // error/total
}

// FrameStats aggregates frame-extraction volume so operators can tune
// FFmpeg workflow density (fewer frames = lower vendor cost) against
// moderation quality:
//
//   - Precision pressure: more frames per video = more chances for a
//     borderline false-positive frame to flag a clean video. If
//     AvgFlaggedFramesPerFlagged is high, extraction can usually be
//     thinned without losing detections.
//   - Recall pressure: SingleFrameFlagVideos counts flagged videos where
//     EXACTLY ONE frame carried the detection — those are the assets a
//     sparser workflow would have missed. A rising share of
//     single-frame flags means the current density is already near the
//     recall floor; do not thin further.
type FrameStats struct {
	TotalFrames       int     `json:"total_frames"`         // all jobs, images count 1
	VideoJobs         int     `json:"video_jobs"`
	VideoFrames       int     `json:"video_frames"`
	AvgFramesPerVideo float64 `json:"avg_frames_per_video"` // 0 until a video finishes
	FlaggedVideos     int     `json:"flagged_videos"`       // verdict flag|block
	// AvgFlaggedFramesPerFlagged is the mean count of flagged frames
	// within flagged videos (detection margin).
	AvgFlaggedFramesPerFlagged float64 `json:"avg_flagged_frames_per_flagged"`
	SingleFrameFlagVideos      int     `json:"single_frame_flag_videos"`
}

// Snapshot returns the recent jobs (newest first) and lifetime stats.
func (t *JobTracker) Snapshot() ([]JobRecord, VerdictStats) {
	t.mu.Lock()
	defer t.mu.Unlock()
	recent := make([]JobRecord, len(t.ring))
	for i, r := range t.ring {
		recent[len(t.ring)-1-i] = r
	}
	counts := make(map[string]int, len(t.counts))
	for k, v := range t.counts {
		counts[k] = v
	}
	s := VerdictStats{
		Total:     t.total,
		Evaluated: counts["allow"] + counts["flag"] + counts["block"],
		Counts:    counts,
	}
	if s.Evaluated > 0 {
		n := float64(s.Evaluated)
		s.FlagRate = float64(counts["flag"]+counts["block"]) / n
		s.BlockRate = float64(counts["block"]) / n
	}
	if t.total > 0 {
		s.ErrorRate = float64(counts["error"]) / float64(t.total)
	}
	return recent, s
}

// FrameSnapshot returns the lifetime frame-extraction aggregates.
func (t *JobTracker) FrameSnapshot() FrameStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := FrameStats{
		TotalFrames:           t.totalFrames,
		VideoJobs:             t.videoJobs,
		VideoFrames:           t.videoFrames,
		FlaggedVideos:         t.flaggedVideos,
		SingleFrameFlagVideos: t.singleFrameFlags,
	}
	if t.videoJobs > 0 {
		f.AvgFramesPerVideo = float64(t.videoFrames) / float64(t.videoJobs)
	}
	if t.flaggedVideos > 0 {
		f.AvgFlaggedFramesPerFlagged = float64(t.flaggedVideoFrames) / float64(t.flaggedVideos)
	}
	return f
}
