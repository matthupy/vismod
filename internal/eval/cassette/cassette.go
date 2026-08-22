// Package cassette records one billed pass over an evaluation corpus and
// replays it forever at zero cost.
//
// A CapturingModerator decorates the active moderation.Moderator and writes
// every NormalizedResult it returns — Raw included — to a cassette file. A
// ReplayModerator serves those results back, so threshold tuning, scoring and
// re-scoring of the same corpus never touch a vendor again.
//
// # Where the seam is, and why
//
// Capture happens at the moderation.Moderator seam rather than at
// http.RoundTripper. The Moderator seam is uniform across all four adapters;
// the transport is not (google is gRPC, not REST, and shieldgemma issues one
// request per policy per frame, which multiplies keys). It also captures
// exactly the quantity a threshold optimizer needs: per-frame, per-category
// scores BEFORE thresholds are applied.
//
// Stated cost, not a defect: replay does not re-exercise an adapter's
// normalize step. A vendor class-map regression — a renamed provider label, a
// head that stops being emitted — is invisible to a replayed run. That path
// stays covered by the per-adapter golden fixtures in the unit suite
// (internal/moderate/adapters/*). This package measures configuration
// quality, never wire-format correctness.
//
// # The key is the frame bytes
//
// A cassette entry is keyed on SHA-256 of the frame bytes handed to the
// adapter — not the source file, not Source.RefDigest. ffmpeg frame counts
// vary between runs of the same video and dHash dedup then drops a different
// subset, so a file-keyed cassette does not line up on replay. A
// byte-identical frame is a hit no matter which run extracted it.
//
// # A miss is a hard error
//
// Replay never falls back to a live call. A silent fallback would bill the
// vendor from a run that claims to be a replay and would report numbers the
// original pass never produced. A miss fails the frame, which the pipeline
// turns into verdict "error" — fail-safe, and visible.
//
// # Cassettes are strictly local and are never committed
//
// A cassette holds provider output for media the OPERATOR supplied, so it is
// the one place in this repo where moderation.NormalizedResult.Raw is
// deliberately retained at rest, alongside the SHA-256 of media bytes. That
// is a carve-out for a local, operator-owned file and nothing more:
//
//   - cassette paths are gitignored and no cassette is ever committed;
//   - the file is created 0600;
//   - neither Raw nor a frame digest may travel from here into a result
//     envelope, a log line, an audit record, a queue payload, a test fixture
//     or the UI (invariant 3). In particular the miss error names the adapter
//     and never the frame digest, because that string reaches
//     FrameResult.Error and therefore the envelope and the sinks.
//
// # File format
//
// A cassette is JSON Lines. Line 1 is the header (format magic, format
// version, the recording adapter, its model version when it reports one, and
// its capabilities); every later line is one entry, {"key", "result"}. The
// file is an append-only journal of a billed pass: a repeated key is legal
// and the last line for a key wins. A file that cannot be read in full is
// refused in full — a partial load would replay some frames and hard-error on
// the rest, which is indistinguishable from a corpus the vendor scored
// differently.
package cassette

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/vismod/vismod/pkg/moderation"
)

const (
	// Format is the magic string on a cassette's header line.
	Format = "vismod.cassette"
	// FormatVersion is the on-disk format this build reads and writes.
	FormatVersion = 1
	// MinReadableVersion is the oldest on-disk format this build still reads.
	MinReadableVersion = 1
	// FileExtension is the conventional suffix, and what .gitignore matches.
	FileExtension = ".cassette.jsonl"
)

// modelVersioner mirrors the optional interface the cli composition root
// type-asserts to stamp the audit ModelIdentity. It is duplicated here, as it
// is in internal/observe, rather than exported: a decorator's job is to be
// transparent, not to own the contract.
type modelVersioner interface{ ModelVersion() string }

// header is the first line of a cassette file.
type header struct {
	Format       string     `json:"format"`
	Version      int        `json:"version"`
	Adapter      string     `json:"adapter"`
	ModelVersion string     `json:"model_version,omitempty"`
	Caps         capsRecord `json:"caps"`
}

// capsRecord is moderation.Caps with explicit JSON names. The on-disk format
// is a contract; it does not inherit field names from a struct that is free
// to rename them.
type capsRecord struct {
	SupportsVideo bool                  `json:"supports_video"`
	MaxImageBytes int64                 `json:"max_image_bytes"`
	Categories    []moderation.Category `json:"categories"`
}

func capsOf(c moderation.Caps) capsRecord {
	return capsRecord{SupportsVideo: c.SupportsVideo, MaxImageBytes: c.MaxImageBytes, Categories: c.Categories}
}

func (c capsRecord) caps() moderation.Caps {
	return moderation.Caps{SupportsVideo: c.SupportsVideo, MaxImageBytes: c.MaxImageBytes, Categories: c.Categories}
}

// entry is one recorded frame: the frame digest and what the adapter said
// about it.
type entry struct {
	Key    string                      `json:"key"`
	Result moderation.NormalizedResult `json:"result"`
}

// Key derives a cassette key from the bytes handed to the adapter. It is the
// hex SHA-256 of exactly those bytes, so any tool that has the frame can
// compute the same key.
func Key(frame []byte) string {
	sum := sha256.Sum256(frame)
	return hex.EncodeToString(sum[:])
}

// Stats is the hit/miss accounting a run record carries. A replayed run
// claims "0 billed calls"; these two numbers are what makes that checkable.
type Stats struct {
	Hits   int `json:"hits"`
	Misses int `json:"misses"`
}

// Cassette is a loaded recording. It is safe for concurrent use: the pipeline
// scans a video's frames through one Moderator concurrently.
type Cassette struct {
	adapter      string
	modelVersion string
	caps         moderation.Caps
	entries      map[string]moderation.NormalizedResult

	mu     sync.Mutex
	hits   int
	misses int
}

// Adapter is the adapter that recorded this cassette.
func (c *Cassette) Adapter() string { return c.adapter }

// ModelVersion is the model version the recording adapter reported, or "" if
// it reported none.
func (c *Cassette) ModelVersion() string { return c.modelVersion }

// Len is the number of distinct frames recorded.
func (c *Cassette) Len() int { return len(c.entries) }

// Stats returns the hits and misses accumulated by replay so far.
func (c *Cassette) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{Hits: c.hits, Misses: c.misses}
}

// lookup finds a recorded result and accounts for the hit or the miss.
func (c *Cassette) lookup(key string) (moderation.NormalizedResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, ok := c.entries[key]
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	return res, ok
}

// Load reads a cassette from disk. A cassette that cannot be read in full —
// truncated, malformed, foreign format, unsupported version — is an error and
// no Cassette, never a partial replay.
//
// The file is read whole rather than streamed: a cassette is a local,
// operator-sized artifact, and reading it in one shot is what makes "the last
// line is torn" detectable instead of merely likely.
func Load(path string) (*Cassette, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cassette: read %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("cassette: %s is empty: line 1 must be a %s header", path, Format)
	}

	lines := strings.Split(string(data), "\n")
	// A well-formed cassette ends with a newline, so the final split element
	// is empty. Anything else is a line that was never finished being
	// written — a torn write, or a truncated copy.
	lastComplete := lines[len(lines)-1] == ""
	if lastComplete {
		lines = lines[:len(lines)-1]
	}

	h, err := parseHeader(path, lines[0], lastComplete || len(lines) > 1)
	if err != nil {
		return nil, err
	}

	c := &Cassette{
		adapter:      h.Adapter,
		modelVersion: h.ModelVersion,
		caps:         h.Caps.caps(),
		entries:      make(map[string]moderation.NormalizedResult, len(lines)-1),
	}
	for i, line := range lines[1:] {
		n := i + 2 // 1-based line number
		if !lastComplete && n == len(lines) {
			return nil, fmt.Errorf("cassette: %s: line %d is truncated (no trailing newline): the recording was cut short and replaying part of it would score a corpus nobody scored", path, n)
		}
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("cassette: %s: line %d is not a readable entry: %w", path, n, err)
		}
		if !isDigest(e.Key) {
			return nil, fmt.Errorf("cassette: %s: line %d has key %q, which is not a sha-256 frame digest", path, n, e.Key)
		}
		// Last write wins: the file is an append-only journal, so a frame
		// scanned twice in one pass appears twice.
		c.entries[e.Key] = e.Result
	}
	return c, nil
}

// parseHeader validates line 1. complete is false when the header is the only
// line and it was never terminated.
func parseHeader(path, line string, complete bool) (header, error) {
	if strings.TrimSpace(line) == "" {
		return header{}, fmt.Errorf("cassette: %s: line 1 is blank, and it must be a %s header", path, Format)
	}
	if !complete {
		return header{}, fmt.Errorf("cassette: %s: the header line is truncated (no trailing newline)", path)
	}
	var h header
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		return header{}, fmt.Errorf("cassette: %s: line 1 is not a readable header: %w", path, err)
	}
	if h.Format != Format {
		return header{}, fmt.Errorf("cassette: %s: format is %q, want %q — this is not a vismod cassette", path, h.Format, Format)
	}
	// The two directions are different problems and an operator acts on them
	// differently, so they do not share a message. A cassette is the record of
	// money already spent: the day FormatVersion moves, every file written
	// before it is still worth reading, and MinReadableVersion is what says how
	// far back that goes.
	if h.Version > FormatVersion {
		return header{}, fmt.Errorf("cassette: %s: format version %d was written by a newer vismod than this build, which reads up to version %d — upgrade rather than re-billing the corpus", path, h.Version, FormatVersion)
	}
	if h.Version < MinReadableVersion {
		return header{}, fmt.Errorf("cassette: %s: format version %d is too old to read; this build reads version %d and above, so this cassette must be re-recorded", path, h.Version, MinReadableVersion)
	}
	if h.Adapter == "" {
		return header{}, fmt.Errorf("cassette: %s: the header names no adapter, so nothing can check that it replays the adapter that recorded it", path)
	}
	return h, nil
}

// isDigest reports whether s is the exact form Key emits: lowercase hex.
//
// hex.DecodeString accepts either case, and accepting both here would be a
// silent partial replay rather than a refusal. An uppercased cassette — a jq
// round-trip, another tool's rewrite — would load cleanly, report its full
// Len(), and then miss on every frame, pointing the operator at their corpus
// instead of at the file.
func isDigest(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	if strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// cloneResult deep-copies a recorded result before handing it out.
//
// The pipeline MUTATES what a Moderator returns: ApplyThresholds stamps
// Threshold and Flagged onto the categories it is given. Sharing the
// cassette's own slices and *float64s would let the first scan of a frame
// change what every later scan of that frame sees, and a threshold sweep is
// nothing but repeated scans of the same frames. Nil stays nil throughout:
// could-not-evaluate is not a value (invariant 2).
func cloneResult(r moderation.NormalizedResult) moderation.NormalizedResult {
	out := r
	if r.Raw != nil {
		out.Raw = make(json.RawMessage, len(r.Raw))
		copy(out.Raw, r.Raw)
	}
	if r.Frames != nil {
		out.Frames = make([]moderation.FrameResult, len(r.Frames))
		for i, f := range r.Frames {
			cf := f
			cf.TimestampSec = cloneFloat(f.TimestampSec)
			if f.Categories != nil {
				cf.Categories = make([]moderation.CategoryResult, len(f.Categories))
				for j, cat := range f.Categories {
					cc := cat
					cc.Score = cloneFloat(cat.Score)
					cc.Threshold = cloneFloat(cat.Threshold)
					cf.Categories[j] = cc
				}
			}
			out.Frames[i] = cf
		}
	}
	out.Overall.MaxScore = cloneFloat(r.Overall.MaxScore)
	out.Overall.Confidence = cloneFloat(r.Overall.Confidence)
	if r.Overall.TopCategory != nil {
		cat := *r.Overall.TopCategory
		out.Overall.TopCategory = &cat
	}
	return out
}

func cloneFloat(f *float64) *float64 {
	if f == nil {
		return nil
	}
	v := *f
	return &v
}
