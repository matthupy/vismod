package google

import (
	"context"
	"errors"
	"testing"

	pb "cloud.google.com/go/vision/v2/apiv1/visionpb"

	"github.com/vismod/vismod/pkg/moderation"
)

// TestAdapterIdentityAndCapabilities: Name and ModelVersion are stamped into
// every envelope's ModelIdentity and folded into ConfigHash, so they must be
// stable and non-empty — an unversioned adapter makes verdicts untraceable
// to the model that produced them.
func TestAdapterIdentityAndCapabilities(t *testing.T) {
	m := newWith(fakeAnnotator{}, options{})

	if m.Name() != "google" {
		t.Errorf("Name = %q", m.Name())
	}
	if m.ModelVersion() == "" {
		t.Error("ModelVersion is empty; the audit trail could not say which model scored an asset")
	}

	caps := m.Capabilities()
	if caps.SupportsVideo {
		t.Error("SafeSearch is image-only; claiming video would skip frame extraction")
	}
	if caps.MaxImageBytes <= 0 {
		t.Error("MaxImageBytes must bound requests before a billed call")
	}
	// MEDICAL and SPOOF are provenance carriers, not harm signals, but they
	// are still declared: dropping them would silently stop carrying the
	// signal at all.
	want := []moderation.Category{
		moderation.CategorySexual, moderation.CategorySuggestiveRacy,
		moderation.CategoryViolence, moderation.CategoryMedical,
		moderation.CategorySpoof,
	}
	if len(caps.Categories) != len(want) {
		t.Fatalf("categories = %v, want %v", caps.Categories, want)
	}
	for i, c := range caps.Categories {
		if c != want[i] {
			t.Errorf("categories[%d] = %q, want %q", i, c, want[i])
		}
	}
	if _, ok := any(m).(moderation.VideoModerator); ok {
		t.Error("the adapter must not satisfy VideoModerator")
	}
}

func TestCloseClosesTheClient(t *testing.T) {
	if err := newWith(fakeAnnotator{}, options{}).Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestAnalyzeImageMarksProviderFailuresRetryable: a surviving gRPC error
// (the SDK already retried) must reach the pipeline marked retryable so the
// fail-safe path applies. An unmarked error is classified terminal and the
// frame's could-not-evaluate reason is misreported.
func TestAnalyzeImageMarksProviderFailuresRetryable(t *testing.T) {
	sentinel := errors.New("rpc error: code = Unavailable")
	m := newWith(fakeAnnotator{err: sentinel}, options{})

	_, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
	if err == nil {
		t.Fatal("a provider failure must not be swallowed")
	}
	if !moderation.IsRetryable(err) {
		t.Errorf("err = %v, want it marked retryable", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the underlying cause preserved", err)
	}
}

// TestAnalyzeImageStopsOnCancelledContext: the limiter sits in front of the
// call, so a cancelled job must not spend a billed request.
func TestAnalyzeImageStopsOnCancelledContext(t *testing.T) {
	calls := 0
	m := newWith(countingAnnotator{calls: &calls}, options{RateLimitRPS: 1})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.AnalyzeImage(ctx, moderation.Image{Bytes: []byte("img")}); err == nil {
		t.Fatal("a cancelled context must fail before the provider call")
	}
	if calls != 0 {
		t.Errorf("provider called %d times on a cancelled job, want 0", calls)
	}
}

type countingAnnotator struct{ calls *int }

func (c countingAnnotator) DetectSafeSearch(context.Context, *pb.Image, *pb.ImageContext) (*pb.SafeSearchAnnotation, error) {
	*c.calls++
	return &pb.SafeSearchAnnotation{}, nil
}
func (countingAnnotator) Close() error { return nil }
