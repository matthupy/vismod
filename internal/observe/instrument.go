package observe

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

// modelVersioner mirrors the optional interface the cli composition root
// type-asserts to stamp the audit ModelIdentity. It is duplicated here rather
// than exported because the wrapper's job is to be transparent, not to own
// the contract.
type modelVersioner interface{ ModelVersion() string }

// InstrumentModerator wraps the active Moderator with request-latency and
// error metrics.
//
// The wrapper must forward every optional interface the underlying Moderator
// satisfies, because a type assertion sees only the wrapper's method set and
// a swallowed capability fails silently — nothing errors, the capability just
// disappears:
//
//   - VideoModerator: the pipeline asserts it to choose native video analysis
//     over frame extraction.
//   - ModelVersion(): buildPipeline asserts it to stamp ModelIdentity. Under
//     the serve wiring order instrumentation happens BEFORE buildPipeline, so
//     losing it stamped model_version "unversioned" and computed config_hash
//     over that string, while scan (which does not instrument) stamped the
//     real one — the same audit question answered two ways per command.
//
// Forwarding by composition costs one type per combination, which is the
// price of keeping the assertions honest: a wrapper that always declares a
// method would make the caller's "unversioned" fallback unreachable and
// report an empty version instead.
func InstrumentModerator(m moderation.Moderator, metrics *Metrics) moderation.Moderator {
	base := instrumented{inner: m, metrics: metrics}
	vm, isVideo := m.(moderation.VideoModerator)
	mv, isVersioned := m.(modelVersioner)
	switch {
	case isVideo && isVersioned:
		return instrumentedVideoVersioned{
			instrumentedVideo: instrumentedVideo{instrumented: base, video: vm},
			versioner:         mv,
		}
	case isVideo:
		return instrumentedVideo{instrumented: base, video: vm}
	case isVersioned:
		return instrumentedVersioned{instrumented: base, versioner: mv}
	default:
		return base
	}
}

type instrumented struct {
	inner   moderation.Moderator
	metrics *Metrics
}

func (i instrumented) Name() string                     { return i.inner.Name() }
func (i instrumented) Capabilities() moderation.Caps    { return i.inner.Capabilities() }
func (i instrumented) Close() error                     { return i.inner.Close() }

func (i instrumented) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	start := time.Now()
	res, err := i.inner.AnalyzeImage(ctx, img)
	i.metrics.AdapterRequestSeconds.WithLabelValues(i.inner.Name()).Observe(time.Since(start).Seconds())
	if err != nil {
		i.metrics.AdapterErrorsTotal.WithLabelValues(i.inner.Name(), errCode(err)).Inc()
	}
	return res, err
}

type instrumentedVideo struct {
	instrumented
	video moderation.VideoModerator
}

func (i instrumentedVideo) AnalyzeVideo(ctx context.Context, v moderation.Source) (moderation.NormalizedResult, error) {
	start := time.Now()
	res, err := i.video.AnalyzeVideo(ctx, v)
	i.metrics.AdapterRequestSeconds.WithLabelValues(i.inner.Name()).Observe(time.Since(start).Seconds())
	if err != nil {
		i.metrics.AdapterErrorsTotal.WithLabelValues(i.inner.Name(), errCode(err)).Inc()
	}
	return res, err
}

// instrumentedVersioned forwards the optional ModelVersion() of an
// image-only moderator.
type instrumentedVersioned struct {
	instrumented
	versioner modelVersioner
}

func (i instrumentedVersioned) ModelVersion() string { return i.versioner.ModelVersion() }

// instrumentedVideoVersioned forwards both optional interfaces at once.
type instrumentedVideoVersioned struct {
	instrumentedVideo
	versioner modelVersioner
}

func (i instrumentedVideoVersioned) ModelVersion() string { return i.versioner.ModelVersion() }

// errCode labels provider errors: the HTTP status when known, otherwise a
// retryable/terminal class. Bounded cardinality by construction.
func errCode(err error) string {
	var herr *moderate.HTTPError
	if errors.As(err, &herr) {
		return strconv.Itoa(herr.Status)
	}
	if moderation.IsRetryable(err) {
		return "retryable"
	}
	return "terminal"
}
