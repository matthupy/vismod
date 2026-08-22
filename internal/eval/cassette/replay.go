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

// NewReplay returns a Moderator that replays c, refusing a cassette recorded
// by a different adapter than the one this run is configured for.
//
// That refusal is not bookkeeping. Scores are not portable across vendors — a
// Microsoft severity/6, a Google likelihood bucket and a Hive head
// probability are different quantities (MODEL_LIMITATIONS.md) — so replaying
// one vendor's cassette under another's thresholds produces numbers that look
// entirely plausible and mean nothing.
func NewReplay(c *Cassette, adapter string) (moderation.Moderator, error) {
	if c == nil {
		return nil, errors.New("cassette: no cassette to replay")
	}
	if c.adapter != adapter {
		return nil, fmt.Errorf("cassette: recorded by adapter %q, but this run is configured for adapter %q; scores are not portable across vendors, so this cassette cannot answer for it",
			c.adapter, adapter)
	}
	r := &ReplayModerator{c: c}
	if c.modelVersion != "" {
		return replayVersioned{ReplayModerator: r, version: c.modelVersion}, nil
	}
	return r, nil
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
