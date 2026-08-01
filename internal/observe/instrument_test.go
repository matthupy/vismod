package observe

import (
	"context"
	"testing"

	"github.com/vismod/vismod/pkg/moderation"
)

// versionedFake is an image-only moderator that pins its model version, the
// way every real adapter does.
type versionedFake struct{}

func (versionedFake) Name() string                  { return "versioned-fake" }
func (versionedFake) ModelVersion() string          { return "v1.2.3" }
func (versionedFake) Capabilities() moderation.Caps { return moderation.Caps{} }
func (versionedFake) Close() error                  { return nil }
func (versionedFake) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	return moderation.NormalizedResult{}, nil
}

// unversionedFake declares no version: wrapping must not make it look like
// it does, or the caller's "unversioned" fallback turns into an empty string
// in the audit trail. ModelVersion is deliberately absent, so this cannot
// embed versionedFake.
type unversionedFake struct{}

func (unversionedFake) Name() string                  { return "unversioned-fake" }
func (unversionedFake) Capabilities() moderation.Caps { return moderation.Caps{} }
func (unversionedFake) Close() error                  { return nil }
func (unversionedFake) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	return moderation.NormalizedResult{}, nil
}

type videoVersionedFake struct{ versionedFake }

func (videoVersionedFake) Name() string { return "video-versioned-fake" }
func (videoVersionedFake) AnalyzeVideo(context.Context, moderation.Source) (moderation.NormalizedResult, error) {
	return moderation.NormalizedResult{}, nil
}

// These assert against modelVersioner, the same optional interface the cli
// composition root type-asserts to stamp ModelIdentity. Under the serve
// wiring order the Moderator is instrumented BEFORE the pipeline is built, so
// a wrapper that swallows this reports "unversioned" and computes config_hash
// over that string, making serve envelopes incomparable with scan envelopes
// for the same model.
func TestInstrumentModeratorForwardsModelVersion(t *testing.T) {
	wrapped := InstrumentModerator(versionedFake{}, NewMetrics())

	mv, ok := wrapped.(modelVersioner)
	if !ok {
		t.Fatal("instrumented moderator dropped ModelVersion(): audit ModelIdentity would read \"unversioned\"")
	}
	if got := mv.ModelVersion(); got != "v1.2.3" {
		t.Errorf("ModelVersion() = %q, want %q", got, "v1.2.3")
	}
}

func TestInstrumentModeratorDoesNotInventModelVersion(t *testing.T) {
	wrapped := InstrumentModerator(unversionedFake{}, NewMetrics())

	if _, ok := wrapped.(modelVersioner); ok {
		t.Fatal("wrapper claimed ModelVersion() for a moderator that has none; the caller's \"unversioned\" fallback becomes unreachable")
	}
}

func TestInstrumentModeratorKeepsVideoAndModelVersion(t *testing.T) {
	wrapped := InstrumentModerator(videoVersionedFake{}, NewMetrics())

	if _, ok := wrapped.(moderation.VideoModerator); !ok {
		t.Error("wrapper dropped VideoModerator; the pipeline would fall back to frame extraction")
	}
	mv, ok := wrapped.(modelVersioner)
	if !ok {
		t.Fatal("wrapper dropped ModelVersion() on a video-native moderator")
	}
	if got := mv.ModelVersion(); got != "v1.2.3" {
		t.Errorf("ModelVersion() = %q, want %q", got, "v1.2.3")
	}
}
