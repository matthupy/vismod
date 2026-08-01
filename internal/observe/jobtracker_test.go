package observe

import (
	"fmt"
	"testing"
	"time"
)

func rec(id, verdict string) JobRecord {
	return JobRecord{ID: id, Ref: "/data/" + id, MediaType: "image",
		Verdict: verdict, FinishedAt: time.Now().UTC()}
}

func TestJobTrackerRates(t *testing.T) {
	tr := NewJobTracker(10)
	for _, v := range []string{"allow", "allow", "allow", "flag", "block", "error", "skip", "allow", "flag", "allow"} {
		tr.Record(rec("j", v))
	}
	_, s := tr.Snapshot()
	if s.Total != 10 {
		t.Fatalf("total = %d", s.Total)
	}
	if s.Counts["allow"] != 5 || s.Counts["flag"] != 2 || s.Counts["block"] != 1 ||
		s.Counts["error"] != 1 || s.Counts["skip"] != 1 {
		t.Errorf("counts = %v", s.Counts)
	}
	// flag/block are over EVALUATED jobs (5 allow + 2 flag + 1 block = 8);
	// errored/skipped jobs never dilute them.
	if s.Evaluated != 8 {
		t.Errorf("evaluated = %d, want 8", s.Evaluated)
	}
	if s.FlagRate != 3.0/8.0 {
		t.Errorf("flag_rate = %v, want 0.375", s.FlagRate)
	}
	if s.BlockRate != 1.0/8.0 {
		t.Errorf("block_rate = %v, want 0.125", s.BlockRate)
	}
	// error rate stays over all finished jobs.
	if s.ErrorRate != 0.1 {
		t.Errorf("error_rate = %v, want 0.1", s.ErrorRate)
	}
}

// The worked example from the spec of this behavior: 1 block + 1 allow +
// 1 error → flag & block rates 50% (of the 2 evaluated), error rate 33.3%.
func TestJobTrackerRatesExcludeErrorsFromEvaluated(t *testing.T) {
	tr := NewJobTracker(10)
	for _, v := range []string{"block", "allow", "error"} {
		tr.Record(rec("j", v))
	}
	_, s := tr.Snapshot()
	if s.Evaluated != 2 {
		t.Fatalf("evaluated = %d, want 2", s.Evaluated)
	}
	if s.FlagRate != 0.5 || s.BlockRate != 0.5 {
		t.Errorf("flag=%v block=%v, want 0.5 each", s.FlagRate, s.BlockRate)
	}
	if s.ErrorRate != 1.0/3.0 {
		t.Errorf("error_rate = %v, want 1/3", s.ErrorRate)
	}
}

// All jobs errored: no evaluated jobs → flag/block rates stay 0 (the UI
// renders a dash), never NaN.
func TestJobTrackerAllErrors(t *testing.T) {
	tr := NewJobTracker(10)
	tr.Record(rec("j", "error"))
	tr.Record(rec("j", "error"))
	_, s := tr.Snapshot()
	if s.Evaluated != 0 || s.FlagRate != 0 || s.BlockRate != 0 {
		t.Errorf("all-error stats = %+v", s)
	}
	if s.ErrorRate != 1.0 {
		t.Errorf("error_rate = %v, want 1", s.ErrorRate)
	}
}

func TestJobTrackerEmptySnapshot(t *testing.T) {
	tr := NewJobTracker(10)
	recent, s := tr.Snapshot()
	if len(recent) != 0 || s.Total != 0 || s.FlagRate != 0 {
		t.Errorf("empty tracker: recent=%d stats=%+v", len(recent), s)
	}
}

func TestFrameStats(t *testing.T) {
	tr := NewJobTracker(20)
	vid := func(verdict string, scanned, flagged int) JobRecord {
		r := rec("v", verdict)
		r.MediaType = "video"
		r.FramesScanned = scanned
		r.FramesFlagged = flagged
		return r
	}
	img := func(verdict string) JobRecord {
		r := rec("i", verdict)
		r.FramesScanned = 1
		return r
	}

	tr.Record(img("allow"))        // 1 frame
	tr.Record(vid("allow", 10, 0)) // clean video
	tr.Record(vid("block", 20, 5)) // solid detection margin
	tr.Record(vid("flag", 30, 1))  // single-frame flag: recall floor
	tr.Record(vid("error", 4, 0))  // errored video: counted in volume only

	f := tr.FrameSnapshot()
	if f.TotalFrames != 65 {
		t.Errorf("total_frames = %d, want 65", f.TotalFrames)
	}
	if f.VideoJobs != 4 || f.VideoFrames != 64 {
		t.Errorf("video jobs/frames = %d/%d, want 4/64", f.VideoJobs, f.VideoFrames)
	}
	if f.AvgFramesPerVideo != 16.0 {
		t.Errorf("avg_frames_per_video = %v, want 16", f.AvgFramesPerVideo)
	}
	if f.FlaggedVideos != 2 {
		t.Errorf("flagged_videos = %d, want 2 (flag+block only)", f.FlaggedVideos)
	}
	if f.AvgFlaggedFramesPerFlagged != 3.0 { // (5+1)/2
		t.Errorf("avg_flagged_frames = %v, want 3", f.AvgFlaggedFramesPerFlagged)
	}
	if f.SingleFrameFlagVideos != 1 {
		t.Errorf("single_frame_flag_videos = %d, want 1", f.SingleFrameFlagVideos)
	}
}

func TestFrameStatsEmpty(t *testing.T) {
	f := NewJobTracker(5).FrameSnapshot()
	if f.AvgFramesPerVideo != 0 || f.AvgFlaggedFramesPerFlagged != 0 || f.TotalFrames != 0 {
		t.Errorf("empty tracker frame stats = %+v", f)
	}
}

func TestJobTrackerRingBoundAndOrder(t *testing.T) {
	tr := NewJobTracker(3)
	for i := range 5 {
		tr.Record(rec(fmt.Sprintf("j%d", i), "allow"))
	}
	recent, s := tr.Snapshot()
	if len(recent) != 3 {
		t.Fatalf("ring = %d, want 3", len(recent))
	}
	if recent[0].ID != "j4" || recent[2].ID != "j2" {
		t.Errorf("newest-first order violated: %v %v", recent[0].ID, recent[2].ID)
	}
	if s.Total != 5 {
		t.Errorf("lifetime total must survive ring eviction: %d", s.Total)
	}
}
