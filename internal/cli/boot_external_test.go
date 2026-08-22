package cli_test

// This file is package cli_test — an EXTERNAL test package — on purpose, and
// that is the whole assertion. Every other test of the seam lives in package
// cli and can therefore reach unexported identifiers, which is exactly how a
// seam can pass its entire suite while remaining unusable by the one caller
// it was built for: cmd/vismod-eval and internal/eval are different packages,
// and an unexported hook is invisible to them.
//
// If Serve, ServerOption or WithModeratorDecorator ever stop being exported,
// this file stops compiling — the failure the in-process eval harness would
// otherwise hit only at integration time.

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/cli"
	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/pkg/moderation"
)

// externalConfig builds a runnable config the way an out-of-package caller
// has to: from the exported config API alone. It cannot reach serveConfig,
// which is the point. The "cli-test" adapter is registered by an in-package
// test init, and in-package and external test files share one test binary.
func externalConfig(t *testing.T) config.Config {
	t.Helper()
	c := config.Defaults()
	c.Adapter = config.AdapterSection{Name: "cli-test"}
	c.Audit = config.AuditConfig{Enabled: false}
	c.LogLevel = "error"
	c.MetricsAddr = "127.0.0.1:0"
	c.IntakeAddr = "127.0.0.1:0"
	c.Queue.Workers = 1
	c.Queue.Buffer = 8
	c.Queue.MaxRetries = 0
	c.Queue.DrainTimeout = 3 * time.Second
	c.Queue.JobTimeout = 10 * time.Second
	c.Output.Sinks = []config.SinkConfig{{Type: "file", Path: filepath.Join(t.TempDir(), "out.jsonl")}}
	return c
}

// externalDecorator is what an out-of-package caller's capture wrapper looks
// like: it forwards ModelVersion() — dropping it is a boot failure — and
// counts the calls it forwards.
type externalDecorator struct {
	inner moderation.Moderator
	calls *atomic.Int64
}

func (e externalDecorator) Name() string                  { return e.inner.Name() }
func (e externalDecorator) Capabilities() moderation.Caps { return e.inner.Capabilities() }
func (e externalDecorator) Close() error                  { return e.inner.Close() }

func (e externalDecorator) ModelVersion() string {
	if v, ok := e.inner.(interface{ ModelVersion() string }); ok {
		return v.ModelVersion()
	}
	return ""
}

func (e externalDecorator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	e.calls.Add(1)
	return e.inner.AnalyzeImage(ctx, img)
}

// TestServeIsUsableFromAnotherPackage: an in-process caller in a DIFFERENT
// package boots the serve stack with its own decorator installed, and the
// hook actually runs. Without the export this test cannot be written at all,
// which was the defect — the seam satisfied every acceptance criterion inside
// package cli while being unreachable from cmd/vismod-eval.
//
// ctx is cancelled up front: this asserts reachability and hook application,
// not the run loop, which serve_run_test.go already covers.
func TestServeIsUsableFromAnotherPackage(t *testing.T) {
	var applied, calls atomic.Int64

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := cli.Serve(ctx, externalConfig(t), cli.WithModeratorDecorator(
		func(m moderation.Moderator) moderation.Moderator {
			applied.Add(1)
			return externalDecorator{inner: m, calls: &calls}
		}))
	if err != nil {
		t.Fatalf("cli.Serve from an external package: %v", err)
	}
	if got := applied.Load(); got != 1 {
		t.Errorf("decorator applied %d times, want exactly 1: an external caller could not install its wrapper", got)
	}
}

// TestServeBootFailureIsDistinguishable: a caller must be able to tell a
// stack that NEVER STARTED from one that started and failed to drain. For the
// eval harness that is the difference between a broken run and a scored one,
// so the boot arm is wrapped "boot: …" and the run arm is not.
func TestServeBootFailureIsDistinguishable(t *testing.T) {
	err := cli.Serve(t.Context(), externalConfig(t), cli.WithModeratorDecorator(
		func(moderation.Moderator) moderation.Moderator { return nil }))
	if err == nil {
		t.Fatal("Serve returned nil with a decorator that hands back no moderator")
	}
	if !strings.HasPrefix(err.Error(), "boot: ") {
		t.Errorf("boot failure is not marked as one: %q; a caller cannot tell it from a drain failure", err)
	}
	if !strings.Contains(err.Error(), "decorator") {
		t.Errorf("boot failure does not name the decorator as the cause: %q", err)
	}
}
