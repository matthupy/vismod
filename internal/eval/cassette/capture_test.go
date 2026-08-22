package cassette

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/vismod/vismod/pkg/moderation"
)

// TestCapturingModeratorRecordsNormalizedResult is the whole point of the
// billed pass: whatever the adapter returned — Raw included — is on disk,
// keyed by the frame, and the caller still receives it unchanged.
func TestCapturingModeratorRecordsNormalizedResult(t *testing.T) {
	want := scripted("microsoft", "frame-a", ptr(0.62))
	inner := newFake("microsoft", map[string]moderation.NormalizedResult{"frame-a": want})
	path := tempCassette(t)

	rec, err := NewCapturing(inner, path)
	if err != nil {
		t.Fatalf("NewCapturing: %v", err)
	}
	got, err := rec.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-a"), MIME: "image/png"})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the decorator changed the result it passed through:\n got %+v\nwant %+v", got, want)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inner.closed {
		t.Error("Close did not close the wrapped moderator; the adapter's client leaks")
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("cassette has %d lines, want a header plus one entry: %q", len(lines), lines)
	}
	var h header
	if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
		t.Fatalf("header is not json: %v", err)
	}
	if h.Format != Format || h.Version != FormatVersion || h.Adapter != "microsoft" {
		t.Errorf("header = %+v, want format %q version %d adapter %q", h, Format, FormatVersion, "microsoft")
	}
	if h.Caps.MaxImageBytes != inner.caps.MaxImageBytes || !reflect.DeepEqual(h.Caps.Categories, inner.caps.Categories) {
		t.Errorf("header caps = %+v, want the wrapped adapter's %+v", h.Caps, inner.caps)
	}

	var e entry
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil {
		t.Fatalf("entry is not json: %v", err)
	}
	if e.Key != Key([]byte("frame-a")) {
		t.Errorf("entry key = %q, want the frame digest %q", e.Key, Key([]byte("frame-a")))
	}
	if !reflect.DeepEqual(e.Result, want) {
		t.Errorf("recorded result differs from what the adapter returned:\n got %+v\nwant %+v", e.Result, want)
	}
	if len(e.Result.Raw) == 0 {
		t.Error("Raw was dropped; a cassette without Raw cannot answer an audit question later")
	}
}

// TestCassetteKeyIsFrameBytesNotSourceRef is the tempting-wrong-shape guard.
// Two runs over the same source produce different post-dedup frame sets —
// ffmpeg's frame count varies and dHash then drops a different subset — so a
// cassette keyed on the file, or on Source.RefDigest, misses on replay. Every
// frame whose BYTES are identical must hit, whatever else changed around it.
func TestCassetteKeyIsFrameBytesNotSourceRef(t *testing.T) {
	// The billed pass: run 1 over clip.mp4 kept frames a, b and c.
	results := map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.11)),
		"frame-b": scripted("microsoft", "frame-b", ptr(0.22)),
		"frame-c": scripted("microsoft", "frame-c", ptr(0.33)),
	}
	path := captureFrames(t, newFake("microsoft", results), "frame-a", "frame-b", "frame-c")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	replay, err := NewReplay(c, "microsoft", c.ModelVersion())
	if err != nil {
		t.Fatalf("NewReplay: %v", err)
	}

	// Run 2 over the same clip: a different extraction, a different dedup
	// outcome, a different order, a copy of the file at another path, and
	// nothing about the source carried on the image. Bytes are all that
	// survives, and bytes are the key.
	for _, name := range []string{"frame-c", "frame-a"} {
		img := moderation.Image{
			Bytes: []byte(name),
			MIME:  "image/jpeg", // re-extracted as jpeg this time
			Meta:  map[string]string{"source_ref": "/media/copy-of-clip.mp4", "ref_digest": "0000"},
		}
		got, err := replay.AnalyzeImage(context.Background(), img)
		if err != nil {
			t.Fatalf("replaying %s: %v (a byte-identical frame must hit however it was extracted)", name, err)
		}
		if !reflect.DeepEqual(got, results[name]) {
			t.Errorf("%s replayed as %+v, want %+v", name, got, results[name])
		}
	}
	if s := c.Stats(); s.Hits != 2 || s.Misses != 0 {
		t.Errorf("stats = %+v, want 2 hits and 0 misses", s)
	}

	// And the converse: same source, different bytes, is a different key.
	if Key([]byte("frame-a")) == Key([]byte("frame-b")) {
		t.Fatal("two frames from one source share a key")
	}
}

// TestDecoratorForwardsModelVersion: a type assertion sees only the wrapper's
// method set. observe.InstrumentModerator once swallowed ModelVersion() and
// every serve envelope was stamped "unversioned" — nothing errored, the
// capability just disappeared. The capture decorator must not repeat it, and
// must equally NOT declare a method the wrapped moderator does not have, or
// the caller's "unversioned" fallback becomes unreachable and reports "".
func TestDecoratorForwardsModelVersion(t *testing.T) {
	type versioner interface{ ModelVersion() string }
	results := map[string]moderation.NormalizedResult{"frame-a": scripted("microsoft", "frame-a", ptr(0.5))}

	t.Run("versioned inner", func(t *testing.T) {
		inner := fakeVersioned{fakeModerator: newFake("microsoft", results), version: "2026-06-01"}
		rec, err := NewCapturing(inner, tempCassette(t))
		if err != nil {
			t.Fatalf("NewCapturing: %v", err)
		}
		defer func() { _ = rec.Close() }()
		mv, ok := rec.(versioner)
		if !ok {
			t.Fatal("wrapped moderator lost ModelVersion(); every envelope would be stamped \"unversioned\"")
		}
		if mv.ModelVersion() != "2026-06-01" {
			t.Errorf("ModelVersion() = %q, want %q", mv.ModelVersion(), "2026-06-01")
		}
	})

	t.Run("plain inner declares nothing extra", func(t *testing.T) {
		rec, err := NewCapturing(newFake("microsoft", results), tempCassette(t))
		if err != nil {
			t.Fatalf("NewCapturing: %v", err)
		}
		defer func() { _ = rec.Close() }()
		if _, ok := rec.(versioner); ok {
			t.Error("wrapper declares ModelVersion() the inner does not have; the caller's fallback is now unreachable")
		}
		if _, ok := rec.(moderation.VideoModerator); ok {
			t.Error("wrapper declares AnalyzeVideo the inner does not have")
		}
	})

	// AnalyzeVideo is the one optional interface that is deliberately NOT
	// forwarded, so these two subtests assert the DROP. Forwarding it is the
	// bug: the pipeline takes the native path on the type assertion, hands the
	// adapter a Source rather than frames, and there is nothing to key an entry
	// on — a billed corpus and a header-only cassette. See
	// TestCapturingNeverOffersWholeVideoAnalysis for why the assertion, not the
	// caps field, has to be what closes that path.
	t.Run("video inner", func(t *testing.T) {
		inner := fakeVideo{fakeModerator: newFake("microsoft", results)}
		rec, err := NewCapturing(inner, tempCassette(t))
		if err != nil {
			t.Fatalf("NewCapturing: %v", err)
		}
		defer func() { _ = rec.Close() }()
		if _, ok := rec.(moderation.VideoModerator); ok {
			t.Fatal("the wrapper offers AnalyzeVideo; a whole-video pass cannot be recorded, so it must not be reachable through the wrapper")
		}
		if inner.videoCalls != 0 {
			t.Errorf("AnalyzeVideo reached the inner %d times, want 0", inner.videoCalls)
		}
		if _, ok := rec.(versioner); ok {
			t.Error("wrapper declares ModelVersion() the inner does not have")
		}
	})

	// Dropping AnalyzeVideo must not take ModelVersion() down with it: before
	// the drop these two travelled together on one wrapper type, and the whole
	// reason this file asserts capability-by-capability is that
	// observe.InstrumentModerator once swallowed ModelVersion() and stamped
	// "unversioned" on every serve envelope.
	t.Run("video and versioned inner", func(t *testing.T) {
		inner := fakeVideoVersioned{fakeVideo: fakeVideo{fakeModerator: newFake("microsoft", results)}, version: "v9"}
		rec, err := NewCapturing(inner, tempCassette(t))
		if err != nil {
			t.Fatalf("NewCapturing: %v", err)
		}
		defer func() { _ = rec.Close() }()
		mv, ok := rec.(versioner)
		if !ok {
			t.Fatal("both optional interfaces present, ModelVersion() lost")
		}
		if mv.ModelVersion() != "v9" {
			t.Errorf("ModelVersion() = %q, want %q", mv.ModelVersion(), "v9")
		}
		if _, ok := rec.(moderation.VideoModerator); ok {
			t.Fatal("both optional interfaces present, and the wrapper kept AnalyzeVideo; only ModelVersion() may survive")
		}
		if rec.Name() != "microsoft" || rec.Capabilities().MaxImageBytes != 1<<20 {
			t.Error("the wrapper must be transparent for Name and Capabilities too")
		}
	})
}

// TestCapturingModeratorDoesNotRecordFailures: a provider outage is not a
// vendor opinion. Baking a 503 into a cassette would replay that outage
// forever, and the retryable mark the queue reads must survive the wrapper.
func TestCapturingModeratorDoesNotRecordFailures(t *testing.T) {
	boom := errors.New("provider returned 503")
	inner := newFake("microsoft", nil)
	inner.err = moderation.Retryable(boom)
	path := tempCassette(t)

	rec, err := NewCapturing(inner, path)
	if err != nil {
		t.Fatalf("NewCapturing: %v", err)
	}
	_, err = rec.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-a")})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the adapter's error unchanged", err)
	}
	if !moderation.IsRetryable(err) {
		t.Error("the retryable mark was lost; a transient 429 would dead-letter instead of backing off")
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Len() != 0 {
		t.Errorf("cassette holds %d entries after a failed call, want 0", c.Len())
	}
}

// TestCapturingRefusesToOverwriteAnExistingCassette: the file being written is
// the record of money already spent. Opening it O_EXCL means a re-run with a
// stale --cassette path fails at startup instead of erasing the billed pass.
func TestCapturingRefusesToOverwriteAnExistingCassette(t *testing.T) {
	path := captureFrames(t, newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.1)),
	}), "frame-a")

	rec, err := NewCapturing(newFake("microsoft", nil), path)
	if err == nil {
		_ = rec.Close()
		t.Fatal("NewCapturing overwrote an existing cassette")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the cassette path", err)
	}
	if rec != nil {
		t.Error("NewCapturing returned a moderator alongside its error")
	}
	if c, lerr := Load(path); lerr != nil || c.Len() != 1 {
		t.Errorf("the existing cassette was damaged: Load = %v, entries = %v", lerr, c)
	}
}

// TestCapturingRefusesAnUnwritablePath: failing at construction is the whole
// point — discovering the cassette cannot be written AFTER a billed pass has
// run is discovering it too late.
func TestCapturingRefusesAnUnwritablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "run"+FileExtension)
	if _, err := NewCapturing(newFake("microsoft", nil), path); err == nil {
		t.Fatal("NewCapturing succeeded on an unwritable path")
	}
}

// TestCapturingRecordsEveryFrameOfAConcurrentFanOut: the pipeline scans a
// video's frames through one Moderator concurrently (errgroup.SetLimit), so
// the decorator is shared. Every frame must land exactly once, with no
// interleaved half-line. (-race is CI-only on this box; see
// docs/agent/UNVERIFIED.md.)
func TestCapturingRecordsEveryFrameOfAConcurrentFanOut(t *testing.T) {
	const frames = 16
	results := map[string]moderation.NormalizedResult{}
	names := make([]string, 0, frames)
	for i := 0; i < frames; i++ {
		name := "frame-" + string(rune('a'+i))
		names = append(names, name)
		results[name] = scripted("microsoft", name, ptr(float64(i)/frames))
	}
	inner := newFake("microsoft", results)
	path := tempCassette(t)
	rec, err := NewCapturing(inner, path)
	if err != nil {
		t.Fatalf("NewCapturing: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, frames)
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			_, errs[i] = rec.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte(name)})
		}(i, name)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v (a torn line means the writes were not serialized)", err)
	}
	if c.Len() != frames {
		t.Errorf("cassette holds %d entries, want %d", c.Len(), frames)
	}
	if inner.callCount() != frames {
		t.Errorf("inner saw %d calls, want %d", inner.callCount(), frames)
	}
}

// TestCapturingCloseReportsBothFailures: closing is where a buffered write
// would surface, and the adapter's own Close matters too. Neither error may
// swallow the other.
func TestCapturingCloseReportsBothFailures(t *testing.T) {
	inner := newFake("microsoft", nil)
	inner.closeErr = errors.New("adapter client close failed")
	rec, err := NewCapturing(inner, tempCassette(t))
	if err != nil {
		t.Fatalf("NewCapturing: %v", err)
	}
	if err := rec.Close(); !errors.Is(err, inner.closeErr) {
		t.Errorf("Close error = %v, want the wrapped adapter's close error", err)
	}
}

// TestCaptureWriteFailureFailsTheFrame: if the cassette cannot be written,
// the run must stop. Carrying on would keep spending vendor money on frames
// whose record is being dropped, and the shortfall would only surface on
// replay, as misses on exactly the frames the disk was full for.
func TestCaptureWriteFailureFailsTheFrame(t *testing.T) {
	inner := newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.1)),
	})
	rec, err := NewCapturing(inner, tempCassette(t))
	if err != nil {
		t.Fatalf("NewCapturing: %v", err)
	}
	c, ok := rec.(*CapturingModerator)
	if !ok {
		t.Fatalf("rec is %T, want *CapturingModerator", rec)
	}
	if err := c.f.Close(); err != nil { // the disk-full stand-in
		t.Fatalf("close: %v", err)
	}

	got, err := rec.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-a")})
	if err == nil {
		t.Fatal("a frame whose record could not be written was reported as a success")
	}
	if !strings.Contains(err.Error(), "cassette") {
		t.Errorf("error %q does not say the cassette is what failed", err)
	}
	if !reflect.DeepEqual(got, moderation.NormalizedResult{}) {
		t.Errorf("a failed write returned a result as well as an error: %+v", got)
	}
	// Closing a file that is already closed fails, and that failure must
	// reach the caller too: a cassette whose close failed may be short of
	// its last writes.
	if err := rec.Close(); err == nil || !strings.Contains(err.Error(), c.path) {
		t.Errorf("Close error = %v, want one naming the cassette", err)
	}
}

// TestCaptureRefusesAnUnencodableResult: an adapter that hands back malformed
// Raw cannot be recorded, and pretending otherwise would write a line that
// makes the whole cassette unloadable later — one bad frame costing an entire
// billed pass.
func TestCaptureRefusesAnUnencodableResult(t *testing.T) {
	bad := scripted("microsoft", "frame-a", ptr(0.1))
	bad.Raw = json.RawMessage(`{"truncated":`)
	rec, err := NewCapturing(newFake("microsoft", map[string]moderation.NormalizedResult{"frame-a": bad}), tempCassette(t))
	if err != nil {
		t.Fatalf("NewCapturing: %v", err)
	}
	defer func() { _ = rec.Close() }()

	if _, err := rec.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-a")}); err == nil {
		t.Fatal("a result that cannot be encoded was recorded as a success")
	} else if !strings.Contains(err.Error(), "encode") {
		t.Errorf("error %q does not say the record could not be encoded", err)
	}
}

// TestCassetteFileIsOwnerOnly: a cassette holds provider Raw for media the
// operator supplied. It is the one place this repo deliberately keeps Raw at
// rest, so it is not world-readable. (Unix modes only; Windows reports 0666.)
func TestCassetteFileIsOwnerOnly(t *testing.T) {
	path := captureFrames(t, newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.1)),
	}), "frame-a")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows reports 0666 for every file regardless of the mode passed to
	// OpenFile, so the assertion is skipped there rather than widened: a
	// "perm != 0o666" escape hatch would also wave through a genuinely
	// world-readable 0666 cassette on Linux, which is the exact regression
	// this test exists to catch.
	if runtime.GOOS == "windows" {
		t.Skip("windows reports 0666 for every file; the mode carries no information here")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("cassette mode = %v, want owner-only", perm)
	}
}

// --- capture_test.go's own fixtures -------------------------------------
//
// capFake is this file's moderator double, deliberately independent of the
// shared fakes: the tests below assert on refusals at CONSTRUCTION, and a
// fixture whose capabilities another file tunes could turn one of them into a
// test of nothing.
type capFake struct {
	mu         sync.Mutex
	caps       moderation.Caps
	result     moderation.NormalizedResult
	err        error
	calls      int
	videoCalls int
	closes     int
	closeErr   error
}

func newCapFake() *capFake {
	return &capFake{
		caps:   moderation.Caps{MaxImageBytes: 1 << 20, Categories: []moderation.Category{moderation.CategorySexual}},
		result: capResult(),
	}
}

func capResult() moderation.NormalizedResult {
	return moderation.NormalizedResult{
		SchemaVersion: moderation.SchemaVersion,
		Provider:      "microsoft",
		MediaType:     "image",
		Overall:       moderation.OverallVerdict{Verdict: moderation.VerdictAllow},
		Raw:           json.RawMessage(`{"vendor":"said something"}`),
	}
}

func (f *capFake) Name() string                  { return "microsoft" }
func (f *capFake) Capabilities() moderation.Caps { return f.caps }

func (f *capFake) AnalyzeImage(_ context.Context, _ moderation.Image) (moderation.NormalizedResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return moderation.NormalizedResult{}, f.err
	}
	return f.result, nil
}

func (f *capFake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return f.closeErr
}

func (f *capFake) counts() (calls, videoCalls, closes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.videoCalls, f.closes
}

// capVideoFake is video-native: it satisfies moderation.VideoModerator, and
// (when its caps say so) reports SupportsVideo. That PAIR is what
// internal/pipeline gates the whole-video path on.
type capVideoFake struct{ *capFake }

func (f *capVideoFake) AnalyzeVideo(_ context.Context, _ moderation.Source) (moderation.NormalizedResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.videoCalls++
	return moderation.NormalizedResult{Provider: "microsoft", MediaType: "video"}, nil
}

// scrubPath removes the cassette path from an error before a test asserts on
// its wording. t.TempDir() embeds the TEST NAME in the path, so a naive
// strings.Contains(err.Error(), "newer") passes on the directory name alone
// and asserts nothing.
func scrubPath(err error, path string) string {
	return strings.ReplaceAll(err.Error(), path, "<cassette>")
}

func capPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "run"+FileExtension)
}

// TestCapturingRefusesAVideoNativeAdapter: internal/pipeline takes the native
// whole-video path when the moderator both satisfies moderation.VideoModerator
// and reports Capabilities().SupportsVideo. A decorator that forwarded both
// would therefore be handed a Source, not frames — it would bill the entire
// corpus and leave a header-only cassette, discovered as 100% replay misses
// after the money was spent. The cassette records at the frame seam, so the
// only honest answer is to refuse before the first call.
func TestCapturingRefusesAVideoNativeAdapter(t *testing.T) {
	inner := &capVideoFake{capFake: newCapFake()}
	inner.caps.SupportsVideo = true
	path := capPath(t)

	rec, err := NewCapturing(inner, path)
	if err == nil {
		_ = rec.Close()
		t.Fatal("NewCapturing accepted a video-native adapter; a capture run against it bills the corpus and records nothing")
	}
	if rec != nil {
		t.Error("NewCapturing returned a moderator alongside its refusal")
	}
	for _, want := range []string{"frame", "bill"} {
		if !strings.Contains(scrubPath(err, path), want) {
			t.Errorf("refusal %q does not mention %q; the operator has to be told why the run cannot be recorded", err, want)
		}
	}
	if _, serr := os.Stat(path); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("the refusal left a file at %s, which O_EXCL then blocks on the next run", path)
	}
	if calls, videoCalls, _ := inner.counts(); calls != 0 || videoCalls != 0 {
		t.Errorf("the refused adapter was called %d/%d times; nothing may be billed", calls, videoCalls)
	}
}

// TestCapturingNeverOffersWholeVideoAnalysis: an adapter may satisfy
// moderation.VideoModerator while reporting SupportsVideo false (the pipeline
// then uses frames anyway). Capture accepts that adapter, but the wrapper
// still declares no AnalyzeVideo — the same posture as ReplayModerator. The
// type assertion, not the caps field, is then what makes the unrecordable
// path unreachable, so a caps value that changes after construction cannot
// reopen it.
func TestCapturingNeverOffersWholeVideoAnalysis(t *testing.T) {
	inner := &capVideoFake{capFake: newCapFake()} // SupportsVideo stays false
	rec, err := NewCapturing(inner, capPath(t))
	if err != nil {
		t.Fatalf("NewCapturing: %v (an adapter that does not report SupportsVideo is recordable)", err)
	}
	defer func() { _ = rec.Close() }()
	if _, ok := rec.(moderation.VideoModerator); ok {
		t.Error("the wrapper offers AnalyzeVideo; a cassette cannot record a whole-video pass, so it must never be reachable through the wrapper")
	}
}

// TestCapturingRefusesAPathThatIsNotACassette: the ONE guard that keeps a file
// holding provider Raw and a SHA-256 of every frame out of git is the
// .gitignore suffix. A constructor that accepted any path would let
// --cassette ./corpus-run.jsonl produce an untracked file that the next
// `git add -A` commits.
func TestCapturingRefusesAPathThatIsNotACassette(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus-run.jsonl")
	rec, err := NewCapturing(newCapFake(), path)
	if err == nil {
		_ = rec.Close()
		t.Fatal("NewCapturing accepted a path git does not ignore; the cassette would be committable")
	}
	if rec != nil {
		t.Error("NewCapturing returned a moderator alongside its refusal")
	}
	if !strings.Contains(err.Error(), FileExtension) || !strings.Contains(scrubPath(err, path), "gitignore") {
		t.Errorf("refusal %q must name %s and say that .gitignore is what depends on it", err, FileExtension)
	}
	if _, serr := os.Stat(path); !errors.Is(serr, os.ErrNotExist) {
		t.Error("the refused path was created anyway")
	}
}

// TestRepoGitignoreExcludesEveryCassette closes the loop the constructor check
// opens: the suffix is only a defence while .gitignore actually matches it.
// Renaming FileExtension without editing .gitignore must fail here rather than
// in a commit that publishes provider Raw.
func TestRepoGitignoreExcludesEveryCassette(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", ".gitignore"))
	if err != nil {
		t.Fatalf("read the repo .gitignore: %v", err)
	}
	pattern := "*" + FileExtension
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == pattern {
			return
		}
	}
	t.Errorf(".gitignore has no %q line, so every cassette this build writes is committable", pattern)
}

// TestCapturingCloseIsIdempotent: Close runs from a defer and from the CLI's
// own shutdown path. A second call must not re-close the adapter's HTTP
// client, and must not report the os.ErrClosed it caused itself.
func TestCapturingCloseIsIdempotent(t *testing.T) {
	inner := newCapFake()
	rec, err := NewCapturing(inner, capPath(t))
	if err != nil {
		t.Fatalf("NewCapturing: %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Errorf("second Close = %v, want nil: closing twice is not an error the operator can act on", err)
	}
	if _, _, closes := inner.counts(); closes != 1 {
		t.Errorf("the wrapped adapter was closed %d times, want 1", closes)
	}
}

// writeRawCassette writes a cassette byte-for-byte, for the malformed files no
// CapturingModerator would ever produce.
func writeRawCassette(t *testing.T, lines ...string) string {
	t.Helper()
	path := capPath(t)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write cassette: %v", err)
	}
	return path
}

func rawHeader(t *testing.T, version int) string {
	t.Helper()
	b, err := json.Marshal(header{Format: Format, Version: version, Adapter: "microsoft"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	return string(b)
}

// TestLoadVersionRangeBoundaries: the version check is a RANGE, not an
// equality. A cassette is the artifact whose entire value is that the vendor
// was already paid for it, so the first format bump must be able to widen the
// floor deliberately instead of orphaning every cassette in existence. Newer
// than this build is still refused — this build cannot know what a later
// version means.
func TestLoadVersionRangeBoundaries(t *testing.T) {
	if MinReadableVersion > FormatVersion {
		t.Fatalf("MinReadableVersion %d is above FormatVersion %d: this build cannot read what it writes", MinReadableVersion, FormatVersion)
	}
	b, err := json.Marshal(entry{Key: Key([]byte("frame-a")), Result: capResult()})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	entryLine := string(b)

	t.Run("newer than this build is refused", func(t *testing.T) {
		path := writeRawCassette(t, rawHeader(t, FormatVersion+1), entryLine)
		_, err := Load(path)
		if err == nil {
			t.Fatal("a cassette from a newer vismod loaded; this build cannot know what its lines mean")
		}
		if !strings.Contains(scrubPath(err, path), "newer") {
			t.Errorf("error %q does not say the file was written by a newer vismod", err)
		}
	})

	t.Run("older than the floor is refused", func(t *testing.T) {
		path := writeRawCassette(t, rawHeader(t, MinReadableVersion-1), entryLine)
		_, err := Load(path)
		if err == nil {
			t.Fatal("a cassette below MinReadableVersion loaded")
		}
		if !strings.Contains(scrubPath(err, path), "old") {
			t.Errorf("error %q does not say the file is too old to read", err)
		}
	})

	t.Run("the floor itself is readable", func(t *testing.T) {
		path := writeRawCassette(t, rawHeader(t, MinReadableVersion), entryLine)
		c, err := Load(path)
		if err != nil {
			t.Fatalf("Load at MinReadableVersion: %v", err)
		}
		if c.Len() != 1 {
			t.Errorf("loaded %d entries, want 1", c.Len())
		}
	})
}

// TestLoadRefusesUppercaseHexKeys: Key emits lowercase hex and only lowercase
// hex, but hex.DecodeString accepts either case. An uppercased cassette — a jq
// round-trip, another tool's rewrite — would otherwise load cleanly and then
// miss on every single frame, which is precisely the partial replay this
// package refuses to perform.
func TestLoadRefusesUppercaseHexKeys(t *testing.T) {
	b, err := json.Marshal(entry{Key: strings.ToUpper(Key([]byte("frame-a"))), Result: capResult()})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	path := writeRawCassette(t, rawHeader(t, FormatVersion), string(b))
	if _, err := Load(path); err == nil {
		t.Fatal("an uppercased cassette loaded; every frame would then miss, and the run would report a corpus nobody scored")
	}
}

// partialWriter is the disk-filling-mid-record stand-in: it commits part of
// the bytes it was given and reports fewer than it was handed. os.File.Write
// does exactly this — n bytes ALREADY on disk, and io.ErrShortWrite — and a
// writer that reports a short count with a nil error is equally legal under
// io.Writer, so both are exercised.
type partialWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	writes    int
	tearOn    int // 1-based index of the write that tears
	keep      int // how many of its bytes land
	reportErr bool
	afterTear int // writes accepted in full after the tear
	torn      bool
}

func (w *partialWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.writes == w.tearOn {
		w.torn = true
		n := w.keep
		if n > len(p) {
			n = len(p)
		}
		w.buf.Write(p[:n])
		if w.reportErr {
			return n, io.ErrShortWrite
		}
		return n, nil // a short count and NO error: still a torn record
	}
	if w.torn {
		w.afterTear++
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *partialWriter) snapshot() (data string, appendedAfterTear int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String(), w.afterTear
}

// TestCaptureShortWritePoisonsTheCassette: a short write leaves a partial
// record on disk. If the decorator merely failed that one frame, the other
// fan-out goroutines would append complete lines directly after the fragment,
// and Load — which refuses a file it cannot read in full — would then throw
// away an entire billed pass. Once any write has failed, every later frame
// must fail fast, before the vendor is called again.
func TestCaptureShortWritePoisonsTheCassette(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reportErr bool
	}{
		{"short write with io.ErrShortWrite", true},
		{"short write reported as success", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := newCapFake()
			w := &partialWriter{tearOn: 2, keep: 10, reportErr: tc.reportErr} // write 1 is the header
			rec, err := newCapturing(inner, capPath(t), w)
			if err != nil {
				t.Fatalf("newCapturing: %v", err)
			}
			// The O_EXCL handle is real even though the records go to w, and
			// on Windows an open handle makes t.TempDir()'s RemoveAll fail.
			defer func() { _ = rec.Close() }()

			if _, err := rec.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-a")}); err == nil {
				t.Fatal("a frame whose record was torn in half was reported as a success")
			} else if !strings.Contains(err.Error(), "cassette") {
				t.Errorf("error %q does not say the cassette is what failed", err)
			}

			before, _ := w.snapshot()
			if _, err := rec.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-b")}); err == nil {
				t.Fatal("the cassette kept accepting frames after a torn write; the next complete line would sit right after the fragment and cost the whole pass at load")
			}
			after, appended := w.snapshot()
			if appended != 0 {
				t.Errorf("%d line(s) were appended after the torn record", appended)
			}
			if after != before {
				t.Errorf("bytes were written after the cassette was poisoned:\n before %q\n  after %q", before, after)
			}
			if calls, _, _ := inner.counts(); calls != 1 {
				t.Errorf("the adapter was called %d times, want 1: a frame that cannot be recorded must not be billed", calls)
			}
		})
	}
}

// TestCaptureHeaderWriteFailureLeavesNoFile: no billed data can exist when the
// HEADER write fails, but the empty file the O_EXCL open just created would
// block every later run at the same path until an operator deletes it by hand.
func TestCaptureHeaderWriteFailureLeavesNoFile(t *testing.T) {
	path := capPath(t)
	w := &partialWriter{tearOn: 1, keep: 0, reportErr: true}
	rec, err := newCapturing(newCapFake(), path, w)
	if err == nil {
		_ = rec.Close()
		t.Fatal("newCapturing succeeded although the header could not be written")
	}
	if rec != nil {
		t.Error("newCapturing returned a moderator alongside its error")
	}
	if _, serr := os.Stat(path); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("a zero-length file was left at %s; O_EXCL blocks the re-run until it is deleted by hand", path)
	}

	// The point of removing it: re-running at the same path just works.
	rec, err = NewCapturing(newCapFake(), path)
	if err != nil {
		t.Fatalf("re-run at the same path: %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
