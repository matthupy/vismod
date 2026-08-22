package cassette

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/vismod/vismod/pkg/moderation"
)

// CapturingModerator decorates a moderation.Moderator and records every
// result it returns to a cassette file, keyed by the frame bytes that
// produced it. It is otherwise transparent: the caller gets back exactly what
// the adapter returned.
//
// It is a decorator, not a second Moderator: exactly one Moderator is active
// per process (invariant 8), and this wraps that one.
type CapturingModerator struct {
	inner moderation.Moderator
	path  string

	mu sync.Mutex
	f  *os.File
	// w is where records go. It is f for every production cassette; tests
	// substitute a writer that tears a record the way a full disk does.
	w io.Writer
	// werr latches the first failed write. A torn record poisons the file
	// for the whole run — see writeLine.
	werr   error
	closed bool
}

// NewCapturing wraps inner and begins a cassette at path.
//
// The returned Moderator forwards ModelVersion() when inner reports one, and
// declares it otherwise not. A type assertion sees only the wrapper's method
// set, so a swallowed capability fails silently: observe.InstrumentModerator
// once dropped ModelVersion() and stamped "unversioned" on every serve
// envelope. Declaring a method inner lacks is the mirror-image bug, since it
// makes the caller's fallback unreachable.
//
// moderation.VideoModerator is the one capability deliberately NOT forwarded,
// for the same reason ReplayModerator does not declare it: a cassette entry is
// keyed on frame bytes, and a whole-video call hands the adapter a Source
// rather than frames, so there is nothing to record and nothing to replay.
// internal/pipeline takes the native video path only when the moderator both
// satisfies moderation.VideoModerator AND reports Capabilities().SupportsVideo
// — so not declaring AnalyzeVideo is what keeps that path unreachable even if
// an adapter's capabilities change after construction.
//
// Two refusals, both before the first vendor call:
//
//   - a video-native inner (VideoModerator + SupportsVideo). Wrapping it would
//     bill an entire corpus and leave a header-only cassette, discovered as
//     100% misses on replay, after the money was spent;
//   - a path not ending in FileExtension. A cassette holds provider Raw and a
//     SHA-256 of every frame, and the repo's .gitignore keeps it out of git by
//     that suffix alone.
//
// An existing file at path is refused rather than truncated. That file is the
// record of money already spent; a re-run with a stale path must fail at
// startup, not erase it.
func NewCapturing(inner moderation.Moderator, path string) (moderation.Moderator, error) {
	return newCapturing(inner, path, nil)
}

// newCapturing is NewCapturing with a seam on the write path: w replaces the
// file as the destination of every record, which is how a test produces the
// short write a filling disk produces. w is nil in every production call, and
// the cassette file is created, fsynced, closed and removed exactly the same
// way either way.
func newCapturing(inner moderation.Moderator, path string, w io.Writer) (moderation.Moderator, error) {
	if !strings.HasSuffix(path, FileExtension) {
		return nil, fmt.Errorf("cassette: %s does not end in %s, and it must: a cassette keeps provider Raw and a sha-256 of every frame at rest, and the only thing keeping it out of git is the .gitignore entry that matches that suffix — under any other name the next `git add` commits it", path, FileExtension)
	}
	if _, isVideo := inner.(moderation.VideoModerator); isVideo && inner.Capabilities().SupportsVideo {
		return nil, fmt.Errorf("cassette: adapter %q analyzes whole videos natively (supports_video), and a cassette records at the frame seam: a native pass is handed a source rather than frames, so there is nothing to key an entry on. Capturing against it would bill the vendor for the whole corpus and record nothing", inner.Name())
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cassette: create %s: %w (a cassette records a billed pass and is never overwritten)", path, err)
	}
	c := &CapturingModerator{inner: inner, path: path, f: f, w: f}
	if w != nil {
		c.w = w
	}

	h := header{
		Format:  Format,
		Version: FormatVersion,
		Adapter: inner.Name(),
		Caps:    capsOf(inner.Capabilities()),
	}
	mv, versioned := inner.(modelVersioner)
	if versioned {
		h.ModelVersion = mv.ModelVersion()
	}
	if err := c.writeLine(h); err != nil {
		_ = f.Close()
		// Nothing billed can exist yet, and the empty file the O_EXCL open
		// just created would block every later run at this path until an
		// operator deleted it by hand.
		_ = os.Remove(path)
		return nil, err
	}

	if versioned {
		return capturingVersioned{CapturingModerator: c, versioner: mv}, nil
	}
	return c, nil
}

// Name and Capabilities are the wrapped adapter's, unchanged: the recording
// must look exactly like the run it is recording.
func (c *CapturingModerator) Name() string                  { return c.inner.Name() }
func (c *CapturingModerator) Capabilities() moderation.Caps { return c.inner.Capabilities() }

// AnalyzeImage calls the wrapped adapter and records the result.
//
// A failed call records NOTHING. A provider outage is not a vendor opinion,
// and baking a 503 into a cassette would replay that outage forever; the
// error (retryable mark included) passes through untouched.
//
// A failed WRITE fails the frame, and every frame after it — the check comes
// before the vendor call, so a cassette that can no longer be appended to
// stops the spending immediately. The alternative is to keep billing a run
// whose record is being lost, and to discover on replay that the cassette is
// short of exactly the frames the disk was full for.
func (c *CapturingModerator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	if err := c.writeErr(); err != nil {
		return moderation.NormalizedResult{}, err
	}
	res, err := c.inner.AnalyzeImage(ctx, img)
	if err != nil {
		return moderation.NormalizedResult{}, err
	}
	if err := c.writeLine(entry{Key: Key(img.Bytes), Result: res}); err != nil {
		return moderation.NormalizedResult{}, err
	}
	return res, nil
}

// Close fsyncs and closes the cassette file and closes the wrapped adapter,
// reporting both failures. Neither may hide the other: an unclosed adapter
// client leaks, and an unclosed cassette can lose its final writes.
//
// The fsync is not ceremony. An eight-hour billed pass that loses power at
// hour seven loses whatever the OS had not flushed, and because the surviving
// tail is then a half-written record, Load refuses the ENTIRE file rather than
// replaying part of it.
//
// A second Close is a no-op returning nil: Close is reached both from a defer
// and from the caller's own shutdown path, and closing the adapter's HTTP
// client twice — or reporting the os.ErrClosed this decorator caused itself —
// is noise an operator cannot act on.
func (c *CapturingModerator) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	ferr := errors.Join(c.f.Sync(), c.f.Close())
	c.mu.Unlock()
	if ferr != nil {
		ferr = fmt.Errorf("cassette: close %s: %w", c.path, ferr)
	}
	return errors.Join(ferr, c.inner.Close())
}

// writeErr reports the latched write failure, if there has been one.
func (c *CapturingModerator) writeErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.poisonedLocked()
}

func (c *CapturingModerator) poisonedLocked() error {
	if c.werr == nil {
		return nil
	}
	return fmt.Errorf("cassette: %s stopped being written when an earlier record failed; nothing more may be appended after a half-written line: %w", c.path, c.werr)
}

// writeLine appends one JSON line under the lock. The pipeline fans video
// frames out concurrently through one Moderator, so unsynchronized writes
// would interleave into a torn line and lose the whole billed pass at load.
//
// A failed write POISONS the cassette for the rest of the run. A short write
// commits n bytes and then fails, so what is on disk is half a record; if only
// that one frame failed, the fan-out goroutines still inside AnalyzeImage
// would take the mutex and append complete lines directly after the fragment,
// and Load refuses a file it cannot read in full. One full disk would cost
// every frame of an already-billed pass, not the frames it actually hit.
func (c *CapturingModerator) writeLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("cassette: encode record for %s: %w", c.path, err)
	}
	b = append(b, '\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.poisonedLocked(); err != nil {
		return err
	}
	n, err := c.w.Write(b)
	if err == nil && n != len(b) {
		// io.Writer permits a short count with a nil error; os.File does
		// not, but the record on disk is torn either way.
		err = io.ErrShortWrite
	}
	if err != nil {
		c.werr = err
		return fmt.Errorf("cassette: write %s: %w", c.path, err)
	}
	return nil
}

// capturingVersioned forwards the optional ModelVersion() of a wrapped
// adapter that reports one.
type capturingVersioned struct {
	*CapturingModerator
	versioner modelVersioner
}

func (c capturingVersioned) ModelVersion() string { return c.versioner.ModelVersion() }
