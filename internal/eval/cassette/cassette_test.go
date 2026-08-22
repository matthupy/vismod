package cassette

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/vismod/vismod/pkg/moderation"
)

// fakeModerator is the credential-free test double every test drives. It
// never talks to a vendor: a cassette test that needed a live call would be
// testing the thing this package exists to make unnecessary.
type fakeModerator struct {
	mu         sync.Mutex
	name       string
	caps       moderation.Caps
	results    map[string]moderation.NormalizedResult
	err        error
	calls      int
	videoCalls int
	closed     bool
	closeErr   error
}

func newFake(name string, results map[string]moderation.NormalizedResult) *fakeModerator {
	return &fakeModerator{
		name:    name,
		caps:    moderation.Caps{MaxImageBytes: 1 << 20, Categories: []moderation.Category{moderation.CategorySexual}},
		results: results,
	}
}

func (f *fakeModerator) Name() string                  { return f.name }
func (f *fakeModerator) Capabilities() moderation.Caps { return f.caps }

func (f *fakeModerator) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return f.closeErr
}

func (f *fakeModerator) AnalyzeImage(_ context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return moderation.NormalizedResult{}, f.err
	}
	res, ok := f.results[string(img.Bytes)]
	if !ok {
		return moderation.NormalizedResult{}, fmt.Errorf("fake: no scripted result for %q", img.Bytes)
	}
	return res, nil
}

func (f *fakeModerator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeVersioned satisfies the optional ModelVersion() interface; fakeVideo
// satisfies moderation.VideoModerator; fakeVideoVersioned satisfies both.
// Three types rather than three flags, because a method set is what a type
// assertion sees.
type fakeVersioned struct {
	*fakeModerator
	version string
}

func (f fakeVersioned) ModelVersion() string { return f.version }

type fakeVideo struct {
	*fakeModerator
}

func (f fakeVideo) AnalyzeVideo(_ context.Context, _ moderation.Source) (moderation.NormalizedResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.videoCalls++
	return moderation.NormalizedResult{Provider: f.name, MediaType: "video"}, nil
}

type fakeVideoVersioned struct {
	fakeVideo
	version string
}

func (f fakeVideoVersioned) ModelVersion() string { return f.version }

func ptr(f float64) *float64 { return &f }

func tempCassette(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "run"+FileExtension)
}

// scripted builds a NormalizedResult that exercises every nullable field the
// on-disk format has to carry.
func scripted(provider, frame string, score *float64) moderation.NormalizedResult {
	raw, err := json.Marshal(map[string]string{"vendor_frame": frame})
	if err != nil {
		panic(err)
	}
	return moderation.NormalizedResult{
		SchemaVersion: moderation.SchemaVersion,
		Provider:      provider,
		ModelVersion:  "2026-06-01",
		MediaType:     "image",
		AssetID:       "asset-" + frame,
		Frames: []moderation.FrameResult{{
			TimestampSec: ptr(1.5),
			Status:       moderation.FrameOK,
			Categories: []moderation.CategoryResult{{
				Category:      moderation.CategorySexual,
				ProviderLabel: provider + "/sexual",
				Score:         score,
				ScoreOrigin:   moderation.OriginProbability,
			}},
		}},
		Overall: moderation.OverallVerdict{Verdict: moderation.VerdictAllow},
		Raw:     raw,
	}
}

// captureFrames runs frames through a CapturingModerator and returns the
// closed cassette path. It is the "billed pass" every replay test replays.
func captureFrames(t *testing.T, inner moderation.Moderator, frames ...string) string {
	t.Helper()
	path := tempCassette(t)
	rec, err := NewCapturing(inner, path)
	if err != nil {
		t.Fatalf("NewCapturing: %v", err)
	}
	for _, f := range frames {
		if _, err := rec.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte(f), MIME: "image/png"}); err != nil {
			t.Fatalf("AnalyzeImage(%q): %v", f, err)
		}
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cassette: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	return lines
}

// TestKeyIsSHA256OfFrameBytes pins the key derivation itself: the cassette
// key is the hex SHA-256 of exactly the bytes handed to the adapter, so a
// key computed anywhere else (a harness, a future tool) lines up.
func TestKeyIsSHA256OfFrameBytes(t *testing.T) {
	frame := []byte("\x89PNG\r\n\x1a\n frame bytes")
	sum := sha256.Sum256(frame)
	if got, want := Key(frame), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
	if Key(frame) == Key([]byte("other frame")) {
		t.Error("distinct frames must not share a key")
	}
	// The shape is part of the contract in two directions: another tool
	// computes it from the frame, and Load refuses any key that is not this
	// shape. Lowercase hex, 64 characters, nothing else.
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(Key(frame)) {
		t.Errorf("Key = %q, want 64 lowercase hex characters", Key(frame))
	}
	if !isDigest(Key(frame)) {
		t.Error("Load would refuse a key that Key() itself produced")
	}
}

// TestMalformedCassetteIsLoadError: a cassette that cannot be read in full is
// refused in full. A partial load would replay the entries that survived and
// hard-error on the rest, which reads exactly like a corpus the vendor scored
// differently — the failure mode this package exists to prevent.
func TestMalformedCassetteIsLoadError(t *testing.T) {
	good := captureFrames(t, newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.1)),
		"frame-b": scripted("microsoft", "frame-b", ptr(0.2)),
	}), "frame-a", "frame-b")
	goodBytes, err := os.ReadFile(good)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"empty file", "", "header"},
		{"header only whitespace", "\n", "header"},
		{"header is not json", "not json at all\n", "header"},
		{"foreign format", `{"format":"someone.elses","version":1,"adapter":"microsoft"}` + "\n", "format"},
		{"unsupported version", `{"format":"` + Format + `","version":99,"adapter":"microsoft"}` + "\n", "version"},
		{"header names no adapter", `{"format":"` + Format + `","version":1,"adapter":""}` + "\n", "adapter"},
		{"header never finished being written", `{"format":"` + Format + `","version":1,"adapter":"microsoft"}`, "truncated"},
		{"truncated final entry", string(goodBytes[:len(goodBytes)-20]), "line 3"},
		{"entry is not json", string(goodBytes) + "{oops\n", "line 4"},
		{"entry key is not a digest", string(goodBytes) + `{"key":"zz","result":{}}` + "\n", "key"},
		// Right length, wrong alphabet. Key() can never produce this, so a
		// cassette carrying it holds a record no frame can ever hit: dead
		// weight inflating Len(), which is what the miss error and the run
		// record report as "the cassette recorded N frames".
		{"entry key is 64 characters but not hex", string(goodBytes) + `{"key":"` + strings.Repeat("zq", 32) + `","result":{}}` + "\n", "digest"},
	}
	// One directory for every case, with indexed file names: a per-subtest
	// t.TempDir() embeds the subtest name in the path, and the path is in
	// every error message — so "unsupported version" would satisfy its own
	// assertion no matter what Load reported.
	dir := t.TempDir()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("broken-%d%s", i, FileExtension))
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			c, err := Load(path)
			if err == nil {
				t.Fatalf("Load succeeded on a %s cassette (%d entries loaded); a partial replay is worse than none", tc.name, c.Len())
			}
			if c != nil {
				t.Error("Load returned a cassette alongside its error; callers must get nothing to replay")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q, so an operator cannot tell what to fix", err, tc.want)
			}
		})
	}
}

// TestLoadMissingFileIsAnError: pointing at a cassette that is not there must
// fail loudly at load, not degrade into a run where every frame misses.
func TestLoadMissingFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent"+FileExtension)
	c, err := Load(path)
	if err == nil {
		t.Fatal("Load of a missing cassette succeeded")
	}
	if c != nil {
		t.Error("Load returned a cassette for a missing file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path it looked at, so an operator cannot see the typo", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error %v does not wrap os.ErrNotExist; a caller cannot tell a missing cassette from an unreadable one", err)
	}
}

// TestLoadedCassetteReportsItsIdentity: the header carries the identity a run
// record needs, and a repeated key collapses to one entry (the file is an
// append-only journal of a billed pass). WHICH of the two entries survives is
// TestALaterEntryForAKeySupersedesTheEarlierOne's job — this fixture records
// the same result twice, so it cannot tell first-wins from last-wins.
func TestLoadedCassetteReportsItsIdentity(t *testing.T) {
	inner := newFake("hive", map[string]moderation.NormalizedResult{
		"frame-a": scripted("hive", "frame-a", ptr(0.4)),
	})
	path := captureFrames(t, fakeVersioned{fakeModerator: inner, version: "hive-v3"}, "frame-a", "frame-a")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Adapter() != "hive" {
		t.Errorf("Adapter() = %q, want %q", c.Adapter(), "hive")
	}
	if c.ModelVersion() != "hive-v3" {
		t.Errorf("ModelVersion() = %q, want %q", c.ModelVersion(), "hive-v3")
	}
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1: a repeated key is one entry, last write winning", c.Len())
	}
	if got := c.Stats(); got != (Stats{}) {
		t.Errorf("Stats() = %+v on a freshly loaded cassette, want zero", got)
	}
}

// TestALaterEntryForAKeySupersedesTheEarlierOne pins "last write wins", which
// the package doc and Load both promise and which nothing else proves: a
// fixture that records one result twice is green under first-wins too.
//
// It matters because a cassette is an append-only journal of a BILLED pass. A
// frame scanned twice — a retry after a partial failure, a corpus that lists
// the same asset twice — has two lines, and the one the vendor answered LAST
// is the one the run actually ended on. Serving the earlier line would replay
// a result the recorded pass had already superseded.
func TestALaterEntryForAKeySupersedesTheEarlierOne(t *testing.T) {
	inner := newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.10)),
	})
	path := tempCassette(t)
	rec, err := NewCapturing(inner, path)
	if err != nil {
		t.Fatalf("NewCapturing: %v", err)
	}
	img := moderation.Image{Bytes: []byte("frame-a"), MIME: "image/png"}
	if _, err := rec.AnalyzeImage(context.Background(), img); err != nil {
		t.Fatalf("first AnalyzeImage: %v", err)
	}
	// The same frame, scored again in the same pass, and the vendor answered
	// differently the second time.
	inner.results["frame-a"] = scripted("microsoft", "frame-a", ptr(0.90))
	if _, err := rec.AnalyzeImage(context.Background(), img); err != nil {
		t.Fatalf("second AnalyzeImage: %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Both lines are on disk: the journal appends, it does not update.
	if lines := readLines(t, path); len(lines) != 3 {
		t.Fatalf("cassette has %d lines, want a header plus two entries: %q", len(lines), lines)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1: two lines for one key are one entry", c.Len())
	}
	replay, err := NewReplay(c, "microsoft", c.ModelVersion())
	if err != nil {
		t.Fatalf("NewReplay: %v", err)
	}
	got, err := replay.AnalyzeImage(context.Background(), img)
	if err != nil {
		t.Fatalf("replay AnalyzeImage: %v", err)
	}
	score := got.Frames[0].Categories[0].Score
	if score == nil {
		t.Fatal("the replayed result carries no score")
	}
	if *score != 0.90 {
		t.Errorf("replayed score = %v, want 0.9 (the LAST line for the key); %v is the superseded first line", *score, *score)
	}
}
