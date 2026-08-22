package cassette

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/vismod/vismod/pkg/moderation"
)

// loadReplay captures frames, loads the cassette back and returns both the
// cassette (for its hit/miss accounting) and the replay moderator.
func loadReplay(t *testing.T, inner moderation.Moderator, adapter string, frames ...string) (*Cassette, moderation.Moderator) {
	t.Helper()
	path := captureFrames(t, inner, frames...)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	replay, err := NewReplay(c, adapter)
	if err != nil {
		t.Fatalf("NewReplay: %v", err)
	}
	return c, replay
}

// TestReplayReturnsDeepEqualResult: replay is worthless unless a replayed run
// scores identically to the billed run it replaces. Every field, Raw
// included, comes back as it went in.
func TestReplayReturnsDeepEqualResult(t *testing.T) {
	want := scripted("microsoft", "frame-a", ptr(0.62))
	want.Frames = append(want.Frames, moderation.FrameResult{
		TimestampSec: nil,
		Status:       moderation.FrameError,
		Error:        "adapter returned no frame result",
		Categories:   nil,
	})
	want.Overall = moderation.OverallVerdict{
		Verdict:     moderation.VerdictFlag,
		Flagged:     true,
		TopCategory: catPtr(moderation.CategorySexual),
		MaxScore:    ptr(0.62),
		Confidence:  ptr(0.62),
	}
	inner := newFake("microsoft", map[string]moderation.NormalizedResult{"frame-a": want})

	c, replay := loadReplay(t, inner, "microsoft", "frame-a")
	got, err := replay.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-a")})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replayed result is not deep-equal to the captured one:\n got %#v\nwant %#v", got, want)
	}
	// One call total: the billed capture. Anything above that is the replay
	// itself reaching a vendor.
	if inner.callCount() != 1 {
		t.Errorf("the adapter was called %d times, want exactly 1 (the capture); replay must never reach a vendor", inner.callCount())
	}
	if s := c.Stats(); s.Hits != 1 || s.Misses != 0 {
		t.Errorf("stats = %+v, want one hit", s)
	}
}

func catPtr(c moderation.Category) *moderation.Category { return &c }

// TestNilScoreRoundTripsAsNilNotZero is invariant 2 at the cassette boundary.
// nil means could-not-evaluate; 0.0 means confidently safe. A cassette that
// collapses the first into the second would silently turn every unscorable
// frame in the corpus into a clean one and flatter every threshold sweep run
// against it.
func TestNilScoreRoundTripsAsNilNotZero(t *testing.T) {
	want := moderation.NormalizedResult{
		SchemaVersion: moderation.SchemaVersion,
		Provider:      "microsoft",
		MediaType:     "image",
		Frames: []moderation.FrameResult{{
			TimestampSec: nil,
			Status:       moderation.FrameOK,
			Categories: []moderation.CategoryResult{
				{Category: moderation.CategorySexual, ProviderLabel: "ms/sexual", Score: nil, Threshold: nil},
				{Category: moderation.CategoryViolence, ProviderLabel: "ms/violence", Score: ptr(0), Threshold: ptr(0.5)},
			},
		}},
		Overall: moderation.OverallVerdict{Verdict: moderation.VerdictError, MaxScore: nil, Confidence: nil, TopCategory: nil},
	}
	inner := newFake("microsoft", map[string]moderation.NormalizedResult{"frame-a": want})
	path := captureFrames(t, inner, "frame-a")

	// The on-disk form must be JSON null, not an omitted field: a consumer
	// reading the cassette cannot tell "absent" from "unknown" otherwise.
	line := readLines(t, path)[1]
	if !strings.Contains(line, `"score":null`) {
		t.Errorf("a nil score did not serialize as JSON null: %s", line)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	replay, err := NewReplay(c, "microsoft")
	if err != nil {
		t.Fatalf("NewReplay: %v", err)
	}
	got, err := replay.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-a")})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}

	cats := got.Frames[0].Categories
	if cats[0].Score != nil {
		t.Errorf("a nil score replayed as %v (pointer %p); could-not-evaluate became a value", *cats[0].Score, cats[0].Score)
	}
	if cats[0].Threshold != nil {
		t.Errorf("a nil threshold replayed as %v", *cats[0].Threshold)
	}
	if cats[1].Score == nil {
		t.Fatal("a real 0.0 score replayed as nil; confidently-safe became could-not-evaluate")
	}
	if *cats[1].Score != 0 {
		t.Errorf("score = %v, want 0", *cats[1].Score)
	}
	if got.Frames[0].TimestampSec != nil {
		t.Error("a nil timestamp replayed as a value")
	}
	if got.Overall.MaxScore != nil || got.Overall.Confidence != nil || got.Overall.TopCategory != nil {
		t.Errorf("nil rollup fields replayed as values: %+v", got.Overall)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the result:\n got %#v\nwant %#v", got, want)
	}
}

// TestCassetteMissIsHardError: the miss must fail the frame. A fallback to a
// live call is how a "replayed" run quietly bills the vendor and reports
// different numbers than the run it claims to reproduce.
func TestCassetteMissIsHardError(t *testing.T) {
	c, replay := loadReplay(t, newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.1)),
	}), "microsoft", "frame-a")

	got, err := replay.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("never-recorded")})
	if err == nil {
		t.Fatal("a cassette miss returned no error; the run would score a frame nobody scored")
	}
	if !errors.Is(err, ErrMiss) {
		t.Errorf("err = %v, want it to wrap ErrMiss so a caller can count misses", err)
	}
	if !reflect.DeepEqual(got, moderation.NormalizedResult{}) {
		t.Errorf("a miss returned a result as well as an error: %+v", got)
	}
	if moderation.IsRetryable(err) {
		t.Error("a miss is marked retryable; the queue would re-run the whole job (frame extraction included) for something no retry can fix")
	}
	if s := c.Stats(); s.Hits != 0 || s.Misses != 1 {
		t.Errorf("stats = %+v, want one miss", s)
	}
}

// TestCassetteMissNamesNoFrameDigest: this error string travels. The pipeline
// puts it in FrameResult.Error, which reaches the result envelope, the sinks,
// a webhook body and the logs — and invariant 3 keeps media hashes off every
// one of those surfaces. The digest stays inside the cassette.
func TestCassetteMissNamesNoFrameDigest(t *testing.T) {
	_, replay := loadReplay(t, newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.1)),
	}), "microsoft", "frame-a")

	frame := []byte("never-recorded")
	_, err := replay.AnalyzeImage(context.Background(), moderation.Image{Bytes: frame})
	if err == nil {
		t.Fatal("expected a miss")
	}
	msg := err.Error()
	if strings.Contains(msg, Key(frame)) {
		t.Errorf("the miss error carries the frame digest, which reaches the envelope and the logs: %q", msg)
	}
	if regexp.MustCompile(`[0-9a-f]{64}`).MatchString(msg) {
		t.Errorf("the miss error carries a sha256-shaped string: %q", msg)
	}
	if !strings.Contains(msg, "microsoft") {
		t.Errorf("the miss error does not name the adapter, so an operator cannot tell which cassette is short: %q", msg)
	}
	// It names the size too: "recorded 0 frames" is a cassette that never
	// loaded, "recorded 1 frames" is a corpus that grew since the billed pass,
	// and an operator fixes those two differently.
	if !regexp.MustCompile(`\b1 frames?\b`).MatchString(msg) {
		t.Errorf("the miss error does not say how many frames the cassette holds: %q", msg)
	}
}

// TestCassetteReportsHitsAndMisses: the run record carries these numbers, and
// "0 billed calls, 41 hits, 0 misses" is the whole claim a replayed run makes.
func TestCassetteReportsHitsAndMisses(t *testing.T) {
	c, replay := loadReplay(t, newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.1)),
		"frame-b": scripted("microsoft", "frame-b", ptr(0.2)),
	}), "microsoft", "frame-a", "frame-b")

	for _, f := range []string{"frame-a", "frame-b", "frame-a"} {
		if _, err := replay.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte(f)}); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	for _, f := range []string{"frame-x", "frame-y"} {
		if _, err := replay.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte(f)}); err == nil {
			t.Fatalf("%s: expected a miss", f)
		}
	}
	if s := c.Stats(); s.Hits != 3 || s.Misses != 2 {
		t.Errorf("stats = %+v, want 3 hits and 2 misses", s)
	}
	if c.Len() != 2 {
		t.Errorf("Len() = %d, want 2 recorded frames (a repeated hit is not a second entry)", c.Len())
	}
}

// TestCassetteRefusesForeignAdapter: scores are not portable across vendors —
// a Microsoft severity/6 and a Hive head probability are different quantities
// (MODEL_LIMITATIONS.md). Replaying one adapter's cassette under another
// would produce a plausible-looking, meaningless threshold sweep.
func TestCassetteRefusesForeignAdapter(t *testing.T) {
	path := captureFrames(t, newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.1)),
	}), "frame-a")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	replay, err := NewReplay(c, "hive")
	if err == nil {
		t.Fatal("a microsoft cassette was accepted for a hive run")
	}
	if replay != nil {
		t.Error("NewReplay returned a moderator alongside its error")
	}
	if !strings.Contains(err.Error(), "microsoft") || !strings.Contains(err.Error(), "hive") {
		t.Errorf("error %q must name both the recorded adapter and the configured one", err)
	}
}

// TestReplayReportsTheCapturedIdentity: a replayed run has to look like the
// vendor it replays — the provider name reaches the envelope, the audit
// record and the metrics, and Capabilities feeds the pipeline's pre-flight.
func TestReplayReportsTheCapturedIdentity(t *testing.T) {
	type versioner interface{ ModelVersion() string }
	inner := newFake("google", map[string]moderation.NormalizedResult{
		"frame-a": scripted("google", "frame-a", ptr(0.1)),
	})
	inner.caps = moderation.Caps{
		SupportsVideo: true,
		MaxImageBytes: 4 << 20,
		Categories:    []moderation.Category{moderation.CategorySexual, moderation.CategoryViolence},
	}

	t.Run("versioned capture", func(t *testing.T) {
		_, replay := loadReplay(t, fakeVersioned{fakeModerator: inner, version: "safesearch-2026-06"}, "google", "frame-a")
		if replay.Name() != "google" {
			t.Errorf("Name() = %q, want the recorded adapter", replay.Name())
		}
		if !reflect.DeepEqual(replay.Capabilities(), inner.caps) {
			t.Errorf("Capabilities() = %+v, want the recorded %+v", replay.Capabilities(), inner.caps)
		}
		mv, ok := replay.(versioner)
		if !ok {
			t.Fatal("a cassette that recorded a model version must replay it, or a replayed run stamps \"unversioned\"")
		}
		if mv.ModelVersion() != "safesearch-2026-06" {
			t.Errorf("ModelVersion() = %q", mv.ModelVersion())
		}
		if _, ok := replay.(moderation.VideoModerator); ok {
			t.Error("replay must not claim native video analysis: the cassette is keyed on frame bytes, and nothing was recorded for a whole-video call")
		}
		if err := replay.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	t.Run("unversioned capture", func(t *testing.T) {
		_, replay := loadReplay(t, inner, "google", "frame-a")
		if _, ok := replay.(versioner); ok {
			t.Error("nothing recorded a model version, so replay must not declare one")
		}
	})
}

// TestNewReplayRefusesNoCassette: a nil cassette is a wiring mistake, and the
// fail-safe answer to a wiring mistake is refusing to start.
func TestNewReplayRefusesNoCassette(t *testing.T) {
	replay, err := NewReplay(nil, "microsoft")
	if err == nil {
		t.Fatal("NewReplay(nil) succeeded")
	}
	// A constructor that hands back a usable-looking Moderator alongside its
	// error is how a caller that only logs the error ends up running a
	// wrapper around no recording at all.
	if replay != nil {
		t.Errorf("NewReplay(nil) returned %T alongside its error; callers must get nothing to run", replay)
	}
	if !strings.Contains(err.Error(), "cassette") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

// TestReplayExposesItsCassette: the run record reads hits and misses back off
// the moderator it ran with, so the accounting must be reachable from it.
func TestReplayExposesItsCassette(t *testing.T) {
	c, replay := loadReplay(t, newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.1)),
	}), "microsoft", "frame-a")
	r, ok := replay.(*ReplayModerator)
	if !ok {
		t.Fatalf("replay is %T, want *ReplayModerator for an unversioned cassette", replay)
	}
	if r.Cassette() != c {
		t.Error("Cassette() does not return the cassette being replayed")
	}
	// And it is the LIVE cassette, not a snapshot of one: the run record
	// reads the accounting off it AFTER the corpus has run.
	if _, err := replay.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-a")}); err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if got := r.Cassette().Stats(); got != (Stats{Hits: 1}) {
		t.Errorf("Stats() read back through Cassette() = %+v, want one hit", got)
	}
	if got := r.Cassette().Len(); got != 1 {
		t.Errorf("Len() through Cassette() = %d, want 1", got)
	}
	if got := r.Cassette().Adapter(); got != "microsoft" {
		t.Errorf("Adapter() through Cassette() = %q, want %q", got, "microsoft")
	}
}

// TestReplayHonorsContextCancellation: replay is fast, not instant — a corpus
// run that is shutting down must stop, and returning a recorded result for a
// cancelled scan would put a verdict on a job nobody is waiting for.
func TestReplayHonorsContextCancellation(t *testing.T) {
	c, replay := loadReplay(t, newFake("microsoft", map[string]moderation.NormalizedResult{
		"frame-a": scripted("microsoft", "frame-a", ptr(0.1)),
	}), "microsoft", "frame-a")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := replay.AnalyzeImage(ctx, moderation.Image{Bytes: []byte("frame-a")})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	// A recorded frame WOULD have hit. Returning it anyway alongside the
	// error is how a cancelled scan still produces a verdict.
	if !reflect.DeepEqual(got, moderation.NormalizedResult{}) {
		t.Errorf("a cancelled scan returned a result as well as an error: %+v", got)
	}
	if s := c.Stats(); s != (Stats{}) {
		t.Errorf("a cancelled scan was counted as %+v; it was neither a hit nor a miss", s)
	}
}

// TestReplayHandsOutIndependentCopies: the pipeline mutates what a Moderator
// returns — ApplyThresholds stamps Threshold and Flagged onto the categories
// it is given. If replay handed back the cassette's own slices, the second
// scan of a repeated frame would see the first scan's thresholds, and a
// threshold sweep is nothing but repeated scans of the same frames.
func TestReplayHandsOutIndependentCopies(t *testing.T) {
	// Every pointer-valued field the format carries is non-nil here, and the
	// mutations below write THROUGH every one of them. A fixture that leaves
	// the rollup pointers nil cannot catch a clone that forgets them: the
	// caller has nothing to write through, so sharing them is invisible until
	// a real sweep rewrites the recording under itself.
	want := scripted("microsoft", "frame-a", ptr(0.62))
	want.Overall = moderation.OverallVerdict{
		Verdict:     moderation.VerdictFlag,
		Flagged:     true,
		TopCategory: catPtr(moderation.CategorySexual),
		MaxScore:    ptr(0.62),
		Confidence:  ptr(0.62),
	}
	want.Frames[0].Categories[0].Threshold = ptr(0.5)
	_, replay := loadReplay(t, newFake("microsoft", map[string]moderation.NormalizedResult{"frame-a": want}), "microsoft", "frame-a")

	first, err := replay.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-a")})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("the first replay is already wrong, so nothing below means anything:\n got %#v\nwant %#v", first, want)
	}
	// What ApplyThresholds and the rollup do to a result they are handed.
	first.Frames[0].Categories[0].Flagged = true
	*first.Frames[0].Categories[0].Score = 0.99
	*first.Frames[0].Categories[0].Threshold = 0.4
	*first.Frames[0].TimestampSec = 99
	first.Frames[0].Status = moderation.FrameError
	*first.Overall.MaxScore = 0.99
	*first.Overall.Confidence = 0.99
	*first.Overall.TopCategory = moderation.CategoryViolence
	first.Overall.Verdict = moderation.VerdictBlock
	first.Raw[0] = 'X'

	second, err := replay.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("frame-a")})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if !reflect.DeepEqual(second, want) {
		t.Errorf("the second replay saw the first caller's mutations:\n got %#v\nwant %#v", second, want)
	}
	if !json.Valid(second.Raw) {
		t.Error("Raw was mutated through a shared backing array")
	}

	// DeepEqual compares what the pointers point AT. Two handouts sharing a
	// pointer that neither caller has written through yet are still deep-equal
	// and still a corruption waiting for the next sweep, so check the
	// addresses too.
	shared := []struct {
		field string
		a, b  any
	}{
		{"Frames[0].Categories[0].Score", first.Frames[0].Categories[0].Score, second.Frames[0].Categories[0].Score},
		{"Frames[0].Categories[0].Threshold", first.Frames[0].Categories[0].Threshold, second.Frames[0].Categories[0].Threshold},
		{"Frames[0].TimestampSec", first.Frames[0].TimestampSec, second.Frames[0].TimestampSec},
		{"Overall.MaxScore", first.Overall.MaxScore, second.Overall.MaxScore},
		{"Overall.Confidence", first.Overall.Confidence, second.Overall.Confidence},
		{"Overall.TopCategory", first.Overall.TopCategory, second.Overall.TopCategory},
	}
	for _, s := range shared {
		if s.a == s.b {
			t.Errorf("two replays of the same frame share the pointer for %s; the next writer through it edits the recording", s.field)
		}
	}
	if len(first.Frames) > 0 && len(second.Frames) > 0 && &first.Frames[0] == &second.Frames[0] {
		t.Error("two replays share the Frames backing array")
	}
	if &first.Frames[0].Categories[0] == &second.Frames[0].Categories[0] {
		t.Error("two replays share the Categories backing array")
	}
	if &first.Raw[0] == &second.Raw[0] {
		t.Error("two replays share the Raw backing array")
	}
}
