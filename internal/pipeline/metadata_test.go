package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// The happy path: opaque caller JSON reaches the sink envelope byte-for-byte.
func TestMetadataReachesEnvelope(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, buf := newTestPipeline(t, mod, nil)
	j := imageJob(writeInput(t, "benign"))
	j.Metadata = json.RawMessage(`{"ticket":"T-1","tenant":"acme"}`)

	if _, disp, err := p.ProcessJob(context.Background(), j); err != nil || disp != queue.Ack {
		t.Fatalf("disp=%v err=%v", disp, err)
	}
	env := decodeEnvelope(t, buf)
	if string(env.Metadata) != `{"ticket":"T-1","tenant":"acme"}` {
		t.Errorf("metadata must pass through untouched, got %s", env.Metadata)
	}
}

// The gated §F.5 override returns an envelope from a SECOND construction
// site. Missing it there drops the correlation ID on exactly the jobs an
// operator most needs to reconcile.
func TestMetadataSurvivesEmptyVideoSkipOverride(t *testing.T) {
	fs := &fakeFrameSource{zeroClean: true, dir: t.TempDir()}
	p, _ := newTestPipeline(t, &fakeModerator{}, fs)
	p.AllowEmptyVideoSkip = true
	p.Events = &capturingEvents{}
	j := videoJob(writeInput(t, "video-bytes"))
	j.Metadata = json.RawMessage(`{"ticket":"T-2"}`)

	env, disp, err := p.ProcessJob(context.Background(), j)
	if err != nil || disp != queue.Ack {
		t.Fatalf("override must ack: disp=%v err=%v", disp, err)
	}
	if string(env.Metadata) != `{"ticket":"T-2"}` {
		t.Errorf("override envelope must carry metadata, got %s", env.Metadata)
	}
}

// A job can reach redisq without passing through POST /jobs, so the
// pipeline validates too. Fail safe: error verdict + dead-letter, and
// the rejected bytes never reach the envelope.
func TestInvalidMetadataAtExecutionIsErrorVerdict(t *testing.T) {
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, buf := newTestPipeline(t, mod, nil)
	j := imageJob(writeInput(t, "benign"))
	j.Metadata = json.RawMessage(`["not","an","object"]`)

	env, disp, err := p.ProcessJob(context.Background(), j)
	if disp != queue.DeadLetter {
		t.Fatalf("invalid metadata must dead-letter, got disp=%v err=%v", disp, err)
	}
	if env.Result == nil || env.Result.Overall.Verdict != moderation.VerdictError {
		t.Fatalf("invalid metadata must never allow, got %+v", env.Result)
	}
	if env.Metadata != nil {
		t.Errorf("rejected metadata must not reach the envelope, got %s", env.Metadata)
	}
	if !strings.Contains(decodeEnvelope(t, buf).Error, "metadata") {
		t.Error("the envelope error must name metadata as the cause")
	}
}

// Metadata is caller free text: it is carried, never logged.
func TestMetadataNeverReachesLogs(t *testing.T) {
	var logBuf bytes.Buffer
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, _ := newTestPipeline(t, mod, nil)
	p.Log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	j := imageJob(writeInput(t, "benign"))
	j.Metadata = json.RawMessage(`{"secret_marker":"do-not-log-me"}`)

	if _, _, err := p.ProcessJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logBuf.String(), "do-not-log-me") {
		t.Errorf("metadata must never appear in a log line:\n%s", logBuf.String())
	}
}

// Metadata must never reach logs on the INVALID-metadata path either.
// procErr -> env.Error -> the dead-letter reason -> a queue driver's
// "job dead-lettered" log.Warn call is the exact chain that would carry
// caller bytes into logs if ValidateMetadata's wrapped error ever echoed
// the input. The marker is multi-character and deliberately placed
// inside a syntactically-invalid (truncated) payload: a wrapped
// json.SyntaxError may legitimately name a single offending character,
// which is not a leak, so the assertion is on the multi-character marker
// rather than on the absence of every caller byte.
func TestInvalidMetadataNeverReachesLogs(t *testing.T) {
	var logBuf bytes.Buffer
	mod := &fakeModerator{scores: map[string]float64{"benign": 0.05}}
	p, _ := newTestPipeline(t, mod, nil)
	p.Log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	j := imageJob(writeInput(t, "benign"))
	// Truncated JSON: invalid (unexpected end of input), but still
	// carries the marker string verbatim in the raw bytes.
	j.Metadata = json.RawMessage(`{"secret_marker":"do-not-log-me"`)

	env, disp, err := p.ProcessJob(context.Background(), j)
	if disp != queue.DeadLetter {
		t.Fatalf("invalid metadata must dead-letter, got disp=%v err=%v", disp, err)
	}
	if !strings.Contains(env.Error, "metadata") {
		t.Fatalf("the envelope error must name metadata as the cause, got %q", env.Error)
	}
	if strings.Contains(logBuf.String(), "do-not-log-me") {
		t.Errorf("invalid metadata must never appear in a log line:\n%s", logBuf.String())
	}
}
