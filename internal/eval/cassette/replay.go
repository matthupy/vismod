package cassette

import (
	"context"
	"errors"
	"fmt"

	"github.com/vismod/vismod/pkg/moderation"
)

// ErrMiss is wrapped by every cassette miss, so a caller can classify one
// without matching on text.
var ErrMiss = errors.New("cassette miss")

// ReplayModerator serves recorded results back in place of a vendor. It
// satisfies moderation.Moderator and never makes a network call of any kind.
//
// It deliberately does NOT satisfy moderation.VideoModerator, even when the
// recorded Capabilities say the adapter supports video: the cassette is keyed
// on frame bytes, so a whole-video call has nothing to serve. A replayed run
// of a video-native adapter therefore takes the frame-extraction path.
type ReplayModerator struct {
	c *Cassette
}

// replayVersioned adds the optional ModelVersion() when — and only when — the
// cassette recorded one. Declaring it unconditionally would report "" and
// make the caller's "unversioned" fallback unreachable.
type replayVersioned struct {
	*ReplayModerator
	version string
}

func (r replayVersioned) ModelVersion() string { return r.version }

// NewReplay returns a Moderator that replays c, refusing a cassette that does
// not answer for the exact configuration this run is using: the adapter it was
// recorded by, the model version that adapter reported, and a recording that
// actually holds frames.
//
// Those refusals are not bookkeeping. Scores are not portable across vendors —
// a Microsoft severity/6, a Google likelihood bucket and a Hive head
// probability are different quantities (MODEL_LIMITATIONS.md) — so replaying
// one vendor's cassette under another's thresholds produces numbers that look
// entirely plausible and mean nothing.
//
// The same argument applies WITHIN a vendor, which is why modelVersion is
// checked and not merely recorded: a shieldgemma policy-set change, a
// microsoft api-version bump or a different google feature list all keep
// Name() identical while changing what the model answers. modelVersion is what
// the CONFIGURED adapter reports (empty when it reports none), and it must
// equal what the recording adapter reported. A caller that does not know its
// own model version cannot establish that the cassette answers for it, so
// there is deliberately no way to skip this check — the parameter is required
// rather than optional for the same reason the adapter name is.
func NewReplay(c *Cassette, adapter, modelVersion string) (moderation.Moderator, error) {
	if c == nil {
		return nil, errors.New("cassette: no cassette to replay")
	}
	if c.adapter != adapter {
		return nil, fmt.Errorf("cassette: recorded by adapter %q, but this run is configured for adapter %q; scores are not portable across vendors, so this cassette cannot answer for it",
			c.adapter, adapter)
	}
	if c.modelVersion != modelVersion {
		return nil, fmt.Errorf("cassette: adapter %q recorded it reporting model version %q, but this run's %q reports %q; the same vendor answers differently across policy sets and api versions, so this cassette does not answer for the configuration being swept",
			c.adapter, orNone(c.modelVersion), adapter, orNone(modelVersion))
	}
	// A header-only cassette is what a capture run that died before its first
	// frame leaves behind. It loads cleanly and then misses on every frame, so
	// the operator would learn the recording never happened only after the
	// whole replay pass had run and every case was verdict:"error".
	if c.Len() == 0 {
		return nil, fmt.Errorf("cassette: recorded by adapter %q but holds no recorded frames; every frame of the corpus would miss, so re-record with a billed pass rather than running a corpus that cannot score", c.adapter)
	}
	r := &ReplayModerator{c: c}
	if c.modelVersion != "" {
		return replayVersioned{ReplayModerator: r, version: c.modelVersion}, nil
	}
	return r, nil
}

// orNone renders an absent model version as something an operator can read.
// An error reading `reports ""` looks like a bug in the message rather than a
// statement that the adapter reports no version at all.
func orNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}

// Cassette is the recording being replayed; its Stats carry the hit and miss
// counts a run record reports.
func (r *ReplayModerator) Cassette() *Cassette { return r.c }

// Name is the adapter that recorded the cassette, so a replayed run stamps
// the same provider on its envelopes, audit records and metrics as the billed
// run it reproduces.
func (r *ReplayModerator) Name() string { return r.c.adapter }

// Capabilities are the recorded adapter's, copied so a caller cannot reach
// back into the cassette through the Categories slice.
func (r *ReplayModerator) Capabilities() moderation.Caps {
	caps := r.c.caps
	if caps.Categories != nil {
		caps.Categories = append([]moderation.Category(nil), caps.Categories...)
	}
	return caps
}

// Close is a no-op: a replay holds no client, no connection and no file.
func (r *ReplayModerator) Close() error { return nil }

// AnalyzeImage serves the recorded result for these exact frame bytes.
//
// A miss is a hard error and never a live call: falling back would bill the
// vendor from a run that claims to be a replay, and would report numbers the
// recorded pass never produced. The error is NOT marked retryable — no retry
// can conjure a recording, and a retryable one would re-run the whole job,
// frame extraction included.
//
// The error names the adapter and the size of the cassette, and deliberately
// never the frame digest: this string lands in FrameResult.Error, which
// reaches the result envelope, the sinks, a webhook body and the logs, and a
// media hash belongs on none of those (invariant 3).
func (r *ReplayModerator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	if err := ctx.Err(); err != nil {
		return moderation.NormalizedResult{}, err
	}
	res, ok := r.c.lookup(Key(img.Bytes))
	if !ok {
		return moderation.NormalizedResult{}, fmt.Errorf("%w: adapter %q recorded %d frames and this one is not among them; a miss is never a live call, so re-record with a billed pass",
			ErrMiss, r.c.adapter, r.c.Len())
	}
	return cloneResult(res), nil
}
