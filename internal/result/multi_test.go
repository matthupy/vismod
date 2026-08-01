package result

import (
	"context"
	"errors"
	"testing"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

type stubSink struct {
	calls int
	err   error
}

func (s *stubSink) Write(context.Context, ResultEnvelope) error {
	s.calls++
	return s.err
}

func envFixture(id string) ResultEnvelope {
	return ResultEnvelope{
		JobID:  queue.JobID(id),
		Source: moderation.Source{Kind: "file", Ref: "x.png", MediaType: "image"},
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow},
		},
	}
}

func mustMultiSink(t *testing.T, sinks []Sink, names []string, onFail func(string)) *MultiSink {
	t.Helper()
	m, err := NewMultiSink(sinks, names, onFail)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMultiSinkAttemptsAllSinksEvenWhenOneFails(t *testing.T) {
	boom := errors.New("webhook down")
	a, b, c := &stubSink{}, &stubSink{err: boom}, &stubSink{}
	m := mustMultiSink(t, []Sink{a, b, c}, []string{"stdout", "webhook", "file"}, nil)

	err := m.Write(context.Background(), envFixture("job-1"))
	if err == nil {
		t.Fatal("want error from the failing sink, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("want wrapped %v, got %v", boom, err)
	}
	// The point of the design: a webhook outage must not suppress the
	// local record. c is AFTER the failure and must still have been called.
	if a.calls != 1 || b.calls != 1 || c.calls != 1 {
		t.Errorf("all sinks must be attempted: a=%d b=%d c=%d", a.calls, b.calls, c.calls)
	}
}

func TestMultiSinkReturnsFirstError(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	m := mustMultiSink(t,
		[]Sink{&stubSink{err: first}, &stubSink{err: second}},
		[]string{"file", "webhook"}, nil)

	err := m.Write(context.Background(), envFixture("job-1"))
	if !errors.Is(err, first) {
		t.Errorf("want first error %v, got %v", first, err)
	}
	if errors.Is(err, second) {
		t.Errorf("must not report the second error: %v", err)
	}
}

func TestMultiSinkSuccessPathWritesAll(t *testing.T) {
	a, b := &stubSink{}, &stubSink{}
	m := mustMultiSink(t, []Sink{a, b}, []string{"stdout", "file"}, nil)
	if err := m.Write(context.Background(), envFixture("job-1")); err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("a=%d b=%d, want 1 each", a.calls, b.calls)
	}
}

func TestMultiSinkReportsFailingSinkType(t *testing.T) {
	var got []string
	m := mustMultiSink(t,
		[]Sink{&stubSink{}, &stubSink{err: errors.New("x")}},
		[]string{"stdout", "webhook"},
		func(sinkType string) { got = append(got, sinkType) })

	_ = m.Write(context.Background(), envFixture("job-1"))
	if len(got) != 1 || got[0] != "webhook" {
		t.Errorf("onFail must name only the failing sink, got %v", got)
	}
}

// TestNewMultiSinkRejectsNameMismatch enforces the documented contract:
// names must be the same length as sinks. A short names slice used to
// label a failing sink "unknown", which is exactly the label an operator
// cannot act on during an outage.
func TestNewMultiSinkRejectsNameMismatch(t *testing.T) {
	if _, err := NewMultiSink([]Sink{&stubSink{}, &stubSink{}}, []string{"stdout"}, nil); err == nil {
		t.Fatal("want an error when names is shorter than sinks, got nil")
	}
	if _, err := NewMultiSink([]Sink{&stubSink{}}, []string{"stdout", "file"}, nil); err == nil {
		t.Fatal("want an error when names is longer than sinks, got nil")
	}
	if _, err := NewMultiSink([]Sink{&stubSink{}}, []string{"stdout"}, nil); err != nil {
		t.Fatalf("matched lengths must construct: %v", err)
	}
}
