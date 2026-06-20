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
		Reason:       "sexual score >= potential_csam",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "deadbeef") {
		t.Fatalf("divert must log the frame hash, got: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "potential") && !strings.Contains(strings.ToLower(out), "csam") {
		t.Fatalf("divert log must signal potential-CSAM, got: %s", out)
	}
}
