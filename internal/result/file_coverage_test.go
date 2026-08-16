package result

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/queue"
)

// unmarshalableEnvelope is the one envelope shape that can fail
// json.Marshal without touching production code: Metadata is a
// json.RawMessage passed through verbatim, and encoding/json validates it
// on the way out. Everything else on ResultEnvelope is a plain type that
// always marshals.
func unmarshalableEnvelope(id string) ResultEnvelope {
	env := envFixture(id)
	env.Metadata = json.RawMessage(`{"ticket":`) // truncated: not valid JSON
	return env
}

// TestFileSinkUnrenderableEnvelopeIsAFormatFailure covers the marshal
// branch of FileSink.Write. It must fail — never silently append a
// half-record or, worse, skip the line and report success — and it must
// carry the bounded reason "format" so an operator looks at the payload
// rather than at the disk.
func TestFileSinkUnrenderableEnvelopeIsAFormatFailure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	err = s.Write(context.Background(), unmarshalableEnvelope("job-bad-meta"))
	if err == nil {
		t.Fatal("an envelope that cannot be marshaled must fail, got nil")
	}
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Errorf("want ErrDeliveryFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "("+deliveryReasonFormat+")") {
		t.Errorf("want the bounded reason %q in %v", deliveryReasonFormat, err)
	}
	if strings.Contains(err.Error(), "("+deliveryReasonWrite+")") {
		t.Errorf("a marshal failure must not be reported as a write failure: %v", err)
	}

	b, readErr := os.ReadFile(p)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(b) != 0 {
		t.Errorf("a format failure must append nothing, got %q", b)
	}
}

// TestFileSinkFormatFailureReleasesTheClaim: the claim is taken before the
// marshal, so a failure that keeps it would make the job permanently
// invisible to this sink on redelivery — a silent drop, which is the one
// outcome the fail-safe posture forbids.
func TestFileSinkFormatFailureReleasesTheClaim(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Write(context.Background(), unmarshalableEnvelope("job-retry")); err == nil {
		t.Fatal("want a format failure on the first delivery")
	}
	// Redelivery of the same JobID with a renderable envelope must land.
	if err := s.Write(context.Background(), envFixture("job-retry")); err != nil {
		t.Fatalf("redelivery after a format failure must be retried, got %v", err)
	}
	b, _ := os.ReadFile(p)
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Errorf("want 1 line after the successful redelivery, got %d: %s", got, b)
	}
}

// TestFileSinkWriteFailureIsABoundedWriteReason distinguishes a local
// write failure from an unrenderable payload: the same sentinel, a
// different reason, because one is a disk problem and the other is a bad
// envelope.
func TestFileSinkWriteFailureIsABoundedWriteReason(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // closing underneath makes the append fail
		t.Fatal(err)
	}

	err = s.Write(context.Background(), envFixture("job-closed"))
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("want ErrDeliveryFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "("+deliveryReasonWrite+")") {
		t.Errorf("want the bounded reason %q in %v", deliveryReasonWrite, err)
	}
	// The claim must be released, or a redelivery after the disk recovers
	// would be skipped forever. Claim reports true only for an unheld id.
	if !s.d.Claim(queue.JobID("job-closed")) {
		t.Error("a failed append must release its claim so redelivery can retry")
	}
}

// TestFileSinkWritesAnEmptyEnvelope: an envelope with no job id and no
// result is still a record. Dropping it would turn a pipeline bug into a
// missing audit trail, so the sink writes what it is given.
func TestFileSinkWritesAnEmptyEnvelope(t *testing.T) {
	p := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := NewFileSink(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Write(context.Background(), ResultEnvelope{}); err != nil {
		t.Fatalf("an empty envelope must still be written, got %v", err)
	}
	b, _ := os.ReadFile(p)
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Fatalf("want 1 line, got %d: %s", got, b)
	}
	var back ResultEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &back); err != nil {
		t.Errorf("the written line must be well-formed JSON: %v", err)
	}
	if back.Result != nil {
		t.Errorf("a nil result must stay absent, got %+v", back.Result)
	}
	// The empty JobID is a dedupe key like any other.
	if err := s.Write(context.Background(), ResultEnvelope{}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Errorf("the empty JobID must dedupe like any other: %d lines, want 1", got)
	}
}

// TestNewFileSinkRejectsADirectoryPath: opening happens at construction so
// a misconfigured path is a BOOT error. A directory where a file was
// expected is the likeliest such typo, and it must not be discovered on
// the first verdict.
func TestNewFileSinkRejectsADirectoryPath(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileSink(dir)
	if err == nil {
		s.Close()
		t.Fatal("want a construction error when the path is a directory, got nil")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("the boot error must name the offending path, got %v", err)
	}
}

// TestNewFileSinkRejectsAPathUnderAFile is the other half of the same
// boot check: the parent exists but is not a directory.
func TestNewFileSinkRejectsAPathUnderAFile(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := NewFileSink(filepath.Join(parent, "results.jsonl")); err == nil {
		s.Close()
		t.Fatal("want a construction error when the parent is a file, got nil")
	}
}
