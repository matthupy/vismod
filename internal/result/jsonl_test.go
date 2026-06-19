package result

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestJSONLSinkIdempotentPerJobID(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONLSink(&buf)

	env := ResultEnvelope{JobID: "job-1"}
	for i := 0; i < 3; i++ {
		if err := s.Write(context.Background(), env); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// Re-enqueue of the same JobID must not double-write.
	lines := strings.Count(strings.TrimSpace(buf.String()), "\n")
	if lines != 0 { // single line => 0 newlines after trim
		t.Fatalf("want exactly one line for one JobID, got %d extra newlines:\n%s", lines, buf.String())
	}
	if got := strings.Count(buf.String(), "job-1"); got != 1 {
		t.Fatalf("JobID written %d times, want 1", got)
	}
}

func TestJSONLSinkDistinctJobIDs(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONLSink(&buf)
	_ = s.Write(context.Background(), ResultEnvelope{JobID: "a"})
	_ = s.Write(context.Background(), ResultEnvelope{JobID: "b"})
	if got := strings.Count(strings.TrimRight(buf.String(), "\n"), "\n"); got != 1 {
		t.Fatalf("want two lines (one separator), got %d separators", got)
	}
}
