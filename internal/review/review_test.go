package review

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLogDiverterRecordsHashNotBytes(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	d := NewLogDiverter(slog.New(h))

	ts := 2.5
	score := 0.9
	err := d.Divert(context.Background(), Item{
		JobID:        "job-1",
		FrameSHA256:  "deadbeef",
		TimestampSec: &ts,
		Category:     "SEXUAL",
		Score:        &score,
		Reason:       "score >= flag_at",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "deadbeef") {
		t.Fatalf("divert must log the frame hash, got: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "flagged") && !strings.Contains(out, "review_divert") {
		t.Fatalf("divert log must signal a flagged-frame review divert, got: %s", out)
	}
	// Doc promises the score is recorded; assert it is actually emitted.
	if !strings.Contains(out, "score=0.9") {
		t.Fatalf("divert must log the score, got: %s", out)
	}
}

// A nil Score must not panic and must simply omit the score field.
func TestLogDiverterOmitsNilScore(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	d := NewLogDiverter(slog.New(h))

	if err := d.Divert(context.Background(), Item{JobID: "job-1", FrameSHA256: "deadbeef"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "score=") {
		t.Fatalf("nil score must be omitted, got: %s", buf.String())
	}
}
