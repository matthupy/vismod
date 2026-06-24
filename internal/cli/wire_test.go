package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/matthupy/videosift"
	"github.com/matthupy/vismod/internal/config"
	"github.com/matthupy/vismod/internal/frames"
	"github.com/matthupy/vismod/internal/observe"
	"github.com/matthupy/vismod/internal/queue"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// buildFrameSource must translate config into a videosift-backed source,
// carrying the frame knobs (so a video job actually extracts via videosift,
// not the M0 fake).
func TestBuildFrameSourceUsesVideosift(t *testing.T) {
	cfg := config.Config{Frames: config.FramesConfig{
		WorkDir:     t.TempDir(),
		MaxFrames:   16,
		Scene:       true,
		Keyframe:    true,
		Temporal:    true,
		MPDecimate:  true,
		FFmpegPath:  "ffmpeg",
		FFprobePath: "ffprobe",
	}}
	fs := buildFrameSource(cfg)
	if _, ok := fs.(*frames.VideosiftSource); !ok {
		t.Fatalf("buildFrameSource returned %T, want *frames.VideosiftSource", fs)
	}
}

// buildPipeline with a non-nil Metrics must instrument the adapter (wrap it)
// and record jobs_total{verdict} after processing. With nil Metrics the
// pipeline stays unmetered (one-shot scan path).
func TestBuildPipelineWiresMetrics(t *testing.T) {
	cfg := config.Config{
		Adapter: config.AdapterConfig{Name: "stub"},
		Frames:  config.FramesConfig{MaxFrames: 8, Concurrency: 1},
	}

	// Nil metrics: unmetered, raw moderator.
	plain, modPlain, err := buildPipeline(cfg, result.NewJSONLSink(&strings.Builder{}), observe.NewLogger("error"), nil)
	if err != nil {
		t.Fatalf("buildPipeline(nil metrics): %v", err)
	}
	defer modPlain.Close()
	if plain.Metrics != nil {
		t.Error("nil metrics should leave Pipeline.Metrics nil")
	}
	if plain.Moderator != modPlain {
		t.Error("nil metrics should not wrap the moderator")
	}

	// With metrics: instrumented moderator + recorder set.
	m := observe.NewMetrics()
	p, mod, err := buildPipeline(cfg, result.NewJSONLSink(&strings.Builder{}), observe.NewLogger("error"), m)
	if err != nil {
		t.Fatalf("buildPipeline(metrics): %v", err)
	}
	defer mod.Close()
	if p.Metrics == nil {
		t.Error("metrics should set Pipeline.Metrics")
	}
	if p.Moderator == mod {
		t.Error("metrics should wrap the moderator (instrumented != raw)")
	}

	// Process a still image through the metered pipeline → one job recorded.
	img := filepath.Join(t.TempDir(), "x.jpg")
	if err := os.WriteFile(img, []byte("not-a-real-jpeg-but-stub-ignores-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(context.Background(), result.JobID("t1"), moderation.Source{Kind: "file", Ref: img}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := testutil.CollectAndCount(m.Registry(), "vismod_jobs_total"); got == 0 {
		t.Error("expected vismod_jobs_total to record the processed job")
	}
}

// probeFrameSource must fail fast (boot validation) when ffmpeg/ffprobe are
// absent, wrapping videosift.ErrNoBinaries.
func TestProbeFrameSourceMissingBinaries(t *testing.T) {
	cfg := config.Config{Frames: config.FramesConfig{
		FFmpegPath:  "no-such-ffmpeg-xyz",
		FFprobePath: "no-such-ffprobe-xyz",
	}}
	err := probeFrameSource(cfg)
	if err == nil {
		t.Fatal("probeFrameSource with bogus binaries returned nil, want error")
	}
	if !errors.Is(err, videosift.ErrNoBinaries) {
		t.Errorf("err = %v, want errors.Is ErrNoBinaries", err)
	}
}

// buildQueue must select the redis driver, returning a Pinger-capable
// DepthReporter (so serve can boot-validate + register the readiness probe) and
// NO memq durability warning.
func TestBuildQueueRedisDriver(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := config.Config{Queue: config.QueueConfig{
		Driver:    "redis",
		Workers:   2,
		RedisAddr: mr.Addr(),
	}}
	q, warnings, err := buildQueue(cfg, result.NewJSONLSink(&strings.Builder{}), observe.NewMetrics(), observe.NewLogger("error"))
	if err != nil {
		t.Fatalf("buildQueue(redis): %v", err)
	}
	defer q.Close(context.Background())

	if len(warnings) != 0 {
		t.Errorf("redis driver warnings = %v, want none (durability is the memq concern)", warnings)
	}
	pinger, ok := q.(queue.Pinger)
	if !ok {
		t.Fatal("redis driver must implement queue.Pinger for boot/readiness validation")
	}
	if err := pinger.Ping(context.Background()); err != nil {
		t.Fatalf("Ping against live miniredis: %v", err)
	}
}

// buildDeduper for the redis driver pings its own client at boot (fail-closed)
// and returns a usable Deduper against live miniredis. A healthy TTL above the
// retry budget must NOT warn.
func TestBuildDeduperRedisPingsAndBuilds(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := config.Config{Queue: config.QueueConfig{
		Driver:       "redis",
		RedisAddr:    mr.Addr(),
		DedupTTL:     168 * time.Hour,
		MaxRetries:   5,
		RetryBackoff: time.Second,
	}}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	d, closer, err := buildDeduper(cfg, log)
	if err != nil {
		t.Fatalf("buildDeduper(redis): %v", err)
	}
	defer closer()
	if d == nil {
		t.Fatal("redis driver must return a non-nil Deduper")
	}
	if err := d.Commit(context.Background(), "j1"); err != nil {
		t.Fatalf("Commit against live miniredis: %v", err)
	}
	if done, _ := d.Done(context.Background(), "j1"); !done {
		t.Error("Done should report a committed job")
	}
	if strings.Contains(buf.String(), "dedup_ttl") {
		t.Errorf("healthy TTL must not warn; got %q", buf.String())
	}
}

// A dedup_ttl at or below the retry budget must emit a loud boot warning so the
// silent double-write failure mode is visible before prod.
func TestBuildDeduperWarnsOnShortTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := config.Config{Queue: config.QueueConfig{
		Driver:       "redis",
		RedisAddr:    mr.Addr(),
		DedupTTL:     2 * time.Second,
		MaxRetries:   5,
		RetryBackoff: time.Second, // budget 5s > ttl 2s
	}}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	_, closer, err := buildDeduper(cfg, log)
	if err != nil {
		t.Fatalf("buildDeduper: %v", err)
	}
	defer closer()
	if !strings.Contains(buf.String(), "dedup_ttl") {
		t.Errorf("short TTL must warn; log = %q", buf.String())
	}
}

// Queue backoff is LINEAR: retryDelay returns (n+1)*RetryBackoff, so the true
// span across all attempts is the triangular sum RetryBackoff*M*(M+1)/2, not
// MaxRetries*RetryBackoff. A dedup_ttl above the old (wrong) floor but below the
// real span must now WARN. M=5, RetryBackoff=1s => true span 15s; ttl 10s clears
// the old 5s floor but sits inside the real 15s redelivery window.
func TestBuildDeduperWarnsBelowTriangularSpan(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := config.Config{Queue: config.QueueConfig{
		Driver:       "redis",
		RedisAddr:    mr.Addr(),
		DedupTTL:     10 * time.Second, // > old floor 5s, < true span 15s
		MaxRetries:   5,
		RetryBackoff: time.Second,
	}}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	_, closer, err := buildDeduper(cfg, log)
	if err != nil {
		t.Fatalf("buildDeduper: %v", err)
	}
	defer closer()
	if !strings.Contains(buf.String(), "dedup_ttl") {
		t.Errorf("ttl below the triangular redelivery span must warn; log = %q", buf.String())
	}
}

// buildDeduper fails closed when its own redis connection is unreachable: a
// broken dedup path must surface at boot, not on job #1.
func TestBuildDeduperPingFailsClosed(t *testing.T) {
	cfg := config.Config{Queue: config.QueueConfig{
		Driver:    "redis",
		RedisAddr: "127.0.0.1:1", // nothing listening
		DedupTTL:  168 * time.Hour,
	}}
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if _, _, err := buildDeduper(cfg, log); err == nil {
		t.Fatal("buildDeduper with an unreachable redis must return a boot error")
	}
}

// The memory driver is single-process: buildDeduper returns a nil Deduper (the
// in-memory guards suffice) and a no-op closer.
func TestBuildDeduperMemoryIsNil(t *testing.T) {
	cfg := config.Config{Queue: config.QueueConfig{Driver: "memory"}}
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	d, closer, err := buildDeduper(cfg, log)
	if err != nil {
		t.Fatalf("buildDeduper(memory): %v", err)
	}
	if d != nil {
		t.Error("memory driver must return a nil Deduper (single-process)")
	}
	if err := closer(); err != nil {
		t.Errorf("memory closer should be a no-op, got %v", err)
	}
}

// buildQueue memory driver carries the durability warning.
func TestBuildQueueMemoryDriverWarns(t *testing.T) {
	cfg := config.Config{Queue: config.QueueConfig{Driver: "memory", Workers: 1}}
	q, warnings, err := buildQueue(cfg, result.NewJSONLSink(&strings.Builder{}), observe.NewMetrics(), observe.NewLogger("error"))
	if err != nil {
		t.Fatalf("buildQueue(memory): %v", err)
	}
	defer q.Close(context.Background())
	if len(warnings) == 0 {
		t.Error("memory driver must surface a durability warning")
	}
	if _, ok := q.(queue.Pinger); ok {
		t.Error("memory driver must NOT implement Pinger (it is in-process)")
	}
}
