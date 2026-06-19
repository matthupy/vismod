package pipeline

import (
	"io"
	"log/slog"
	"testing"

	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/pkg/moderation"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testPipeline(t *testing.T) *Pipeline {
	t.Helper()
	cfg, err := config.Load("") // defaults: flag_at 0.5, block_at 0.8, SEXUAL block 0.667
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return &Pipeline{Cfg: cfg}
}

func scoreCat(c moderation.Category, score float64) moderation.CategoryResult {
	return moderation.CategoryResult{Category: c, Score: moderation.Ptr(score), ScoreOrigin: moderation.ScoreOriginProbability}
}

func okFrame(cats ...moderation.CategoryResult) moderation.FrameResult {
	return moderation.FrameResult{Status: moderation.FrameStatusOK, Categories: cats}
}

func errFrame() moderation.FrameResult {
	return moderation.FrameResult{Status: moderation.FrameStatusError, Error: "boom"}
}

func rollupOf(t *testing.T, frames ...moderation.FrameResult) moderation.OverallVerdict {
	t.Helper()
	p := testPipeline(t)
	res := moderation.NormalizedResult{Frames: frames}
	p.applyThresholds(&res)
	return p.rollup(res.Frames)
}

func TestRollupAllow(t *testing.T) {
	ov := rollupOf(t, okFrame(scoreCat(moderation.CategoryViolence, 0.1)))
	if ov.Verdict != moderation.VerdictAllow {
		t.Fatalf("want allow, got %s", ov.Verdict)
	}
	if ov.Flagged {
		t.Fatal("allow must not be flagged")
	}
}

func TestRollupFlag(t *testing.T) {
	ov := rollupOf(t, okFrame(scoreCat(moderation.CategoryViolence, 0.6))) // >=0.5 flag, <0.8 block
	if ov.Verdict != moderation.VerdictFlag {
		t.Fatalf("want flag, got %s", ov.Verdict)
	}
	if !ov.Flagged {
		t.Fatal("flag must be flagged")
	}
}

func TestRollupBlock(t *testing.T) {
	ov := rollupOf(t, okFrame(scoreCat(moderation.CategoryViolence, 0.9))) // >=0.8 block
	if ov.Verdict != moderation.VerdictBlock {
		t.Fatalf("want block, got %s", ov.Verdict)
	}
	if ov.MaxScore == nil || *ov.MaxScore != 0.9 {
		t.Fatalf("want max_score 0.9, got %v", ov.MaxScore)
	}
}

func TestRollupSexualStricterBlock(t *testing.T) {
	// SEXUAL block_at is 0.667; 0.7 must block where the default 0.8 would only flag.
	ov := rollupOf(t, okFrame(scoreCat(moderation.CategorySexual, 0.7)))
	if ov.Verdict != moderation.VerdictBlock {
		t.Fatalf("want block (SEXUAL strict), got %s", ov.Verdict)
	}
}

func TestRollupAllNilIsErrorNeverAllow(t *testing.T) {
	nilCat := moderation.CategoryResult{Category: moderation.CategoryOther, Score: nil, ScoreOrigin: moderation.ScoreOriginProbability}
	ov := rollupOf(t, okFrame(nilCat))
	if ov.Verdict != moderation.VerdictError {
		t.Fatalf("all-nil scores must be error (never allow), got %s", ov.Verdict)
	}
	if ov.MaxScore != nil {
		t.Fatalf("max_score must be nil when no non-nil score exists, got %v", *ov.MaxScore)
	}
}

func TestRollupPartialErrorNeverAllow(t *testing.T) {
	// One clean ok frame + one errored frame => error (precedence error > flag/allow).
	ov := rollupOf(t, okFrame(scoreCat(moderation.CategoryViolence, 0.1)), errFrame())
	if ov.Verdict != moderation.VerdictError {
		t.Fatalf("partial error must never allow, got %s", ov.Verdict)
	}
}

func TestRollupBlockBeatsError(t *testing.T) {
	// A genuine block wins even if another frame errored.
	ov := rollupOf(t, okFrame(scoreCat(moderation.CategoryViolence, 0.95)), errFrame())
	if ov.Verdict != moderation.VerdictBlock {
		t.Fatalf("block must beat error, got %s", ov.Verdict)
	}
}

func TestRollupZeroOKFrames(t *testing.T) {
	ov := rollupOf(t, errFrame())
	if ov.Verdict != moderation.VerdictError {
		t.Fatalf("want error, got %s", ov.Verdict)
	}
	if ov.Flagged || ov.MaxScore != nil || ov.TopCategory != nil {
		t.Fatalf("zero-ok-frames must be error/false/nil, got %+v", ov)
	}
}

func TestRollupHashMatchBlocksWithNilScore(t *testing.T) {
	hashCat := moderation.CategoryResult{
		Category:    moderation.CategoryCSAMHashMatch,
		ScoreOrigin: moderation.ScoreOriginListMembership,
		Score:       nil,
		Flagged:     true,
		MatchList:   "ncmec",
		MatchType:   "pdq",
	}
	ov := rollupOf(t, okFrame(hashCat))
	if ov.Verdict != moderation.VerdictBlock {
		t.Fatalf("hash match must block, got %s", ov.Verdict)
	}
	if ov.MaxScore != nil {
		t.Fatalf("hash-match block must have nil max_score, got %v", *ov.MaxScore)
	}
	if !ov.Flagged {
		t.Fatal("hash-match must be flagged")
	}
}
