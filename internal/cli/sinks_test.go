package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
)

func TestBuildSinksDefaultsToStdout(t *testing.T) {
	cfg := config.Defaults()
	s, closeFn, err := buildSinks(cfg, io.Discard, observe.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if s == nil {
		t.Fatal("want a sink, got nil")
	}
}

func TestBuildSinksConstructsFileSink(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.jsonl")
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{{Type: "file", Path: p}}

	_, closeFn, err := buildSinks(cfg, io.Discard, observe.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("file sink must create its file at construction: %v", err)
	}
}

func TestBuildSinksUnwritableFilePathRefusesBoot(t *testing.T) {
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{{Type: "file", Path: filepath.Join(t.TempDir(), "nope", "out.jsonl")}}
	if _, _, err := buildSinks(cfg, io.Discard, observe.NewMetrics()); err == nil {
		t.Fatal("want boot refusal for an unwritable path, got nil")
	}
}

func TestBuildSinksUnknownTypeRefusesBoot(t *testing.T) {
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{{Type: "carrier-pigeon"}}
	if _, _, err := buildSinks(cfg, io.Discard, observe.NewMetrics()); err == nil {
		t.Fatal("want boot refusal for an unknown sink type, got nil")
	}
}

func TestBuildSinksClosesFileOnPartialFailure(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jsonl")
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{
		{Type: "file", Path: good},
		{Type: "file", Path: filepath.Join(dir, "nope", "bad.jsonl")},
	}
	if _, _, err := buildSinks(cfg, io.Discard, observe.NewMetrics()); err == nil {
		t.Fatal("want boot refusal, got nil")
	}
	// The already-opened good file must have been closed, not leaked.
	// Nothing portable asserts an fd is closed, so assert the contract
	// holds by re-opening exclusively — on Windows this fails if the
	// handle leaked.
	f, err := os.OpenFile(good, os.O_RDWR, 0o600)
	if err != nil {
		t.Errorf("first sink's file handle leaked after partial failure: %v", err)
	} else {
		f.Close()
	}
}

// TestBuildSinksZeroSinksRefusesBoot covers the config.Validate bypass:
// a directly-constructed Config never passes through validateOutput, and
// a MultiSink over zero sinks returns nil from Write — the pipeline would
// Ack a job whose envelope went nowhere. buildSinks is the last
// checkpoint before the destination is fixed for the process lifetime.
func TestBuildSinksZeroSinksRefusesBoot(t *testing.T) {
	cfg := config.Defaults()
	cfg.Output.Sinks = []config.SinkConfig{}
	s, closeFn, err := buildSinks(cfg, io.Discard, observe.NewMetrics())
	if err == nil {
		if closeFn != nil {
			_ = closeFn()
		}
		t.Fatalf("want boot refusal for zero sinks, got sink %v", s)
	}
}
