package result

import (
	"context"
	"errors"
	"testing"

	"github.com/matthupy/vismod/pkg/moderation"
)

// fakeSink records every Write call and optionally returns a configured error.
type fakeSink struct {
	calls []ResultEnvelope
	ctxs  []context.Context
	err   error
}

func (f *fakeSink) Write(ctx context.Context, env ResultEnvelope) error {
	f.calls = append(f.calls, env)
	f.ctxs = append(f.ctxs, ctx)
	return f.err
}

func makeEnv(jobID JobID) ResultEnvelope {
	return ResultEnvelope{
		JobID:  jobID,
		Source: moderation.Source{Ref: "gs://bucket/image.jpg"},
	}
}

// TestMultiSink_FanOut verifies that a single Write reaches all wrapped sinks.
func TestMultiSink_FanOut(t *testing.T) {
	a, b, c := &fakeSink{}, &fakeSink{}, &fakeSink{}
	ms := NewMultiSink(a, b, c)
	env := makeEnv("job-1")

	if err := ms.Write(context.Background(), env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i, s := range []*fakeSink{a, b, c} {
		if len(s.calls) != 1 {
			t.Errorf("sink %d: got %d calls, want 1", i, len(s.calls))
			continue
		}
		if s.calls[0].JobID != env.JobID {
			t.Errorf("sink %d: got JobID %q, want %q", i, s.calls[0].JobID, env.JobID)
		}
		if s.calls[0].Source.Ref != env.Source.Ref {
			t.Errorf("sink %d: got Source.Ref %q, want %q", i, s.calls[0].Source.Ref, env.Source.Ref)
		}
	}
}

// TestMultiSink_FirstErrorSemantics verifies that the first error is returned
// but ALL sinks are still called even after an error occurs.
func TestMultiSink_FirstErrorSemantics(t *testing.T) {
	errA := errors.New("sink A error")
	errB := errors.New("sink B error")

	ok := &fakeSink{}
	sA := &fakeSink{err: errA}
	sB := &fakeSink{err: errB}
	ms := NewMultiSink(ok, sA, sB)
	env := makeEnv("job-2")

	err := ms.Write(context.Background(), env)
	if !errors.Is(err, errA) {
		t.Errorf("got error %v, want errA (%v)", err, errA)
	}

	// All three sinks must have been called despite the errors.
	for i, s := range []*fakeSink{ok, sA, sB} {
		if len(s.calls) != 1 {
			t.Errorf("sink %d: got %d calls, want 1 (remaining sinks must still be attempted)", i, len(s.calls))
		}
	}
}

// TestMultiSink_AllOK verifies nil is returned when all sinks succeed.
func TestMultiSink_AllOK(t *testing.T) {
	ms := NewMultiSink(&fakeSink{}, &fakeSink{})
	if err := ms.Write(context.Background(), makeEnv("job-3")); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// TestMultiSink_ZeroSinks verifies that an empty MultiSink returns nil and does
// not panic.
func TestMultiSink_ZeroSinks(t *testing.T) {
	ms := NewMultiSink()
	if err := ms.Write(context.Background(), makeEnv("job-4")); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// TestMultiSink_ContextThreaded verifies the same context is forwarded to every
// sink rather than a new one being created.
func TestMultiSink_ContextThreaded(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "sentinel")

	a, b := &fakeSink{}, &fakeSink{}
	ms := NewMultiSink(a, b)

	if err := ms.Write(ctx, makeEnv("job-5")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i, s := range []*fakeSink{a, b} {
		if len(s.ctxs) == 0 {
			t.Errorf("sink %d: no context recorded", i)
			continue
		}
		if got := s.ctxs[0].Value(ctxKey{}); got != "sentinel" {
			t.Errorf("sink %d: context value = %v, want sentinel", i, got)
		}
	}
}
