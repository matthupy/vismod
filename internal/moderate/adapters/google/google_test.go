package google

import (
	"context"
	"testing"

	pb "cloud.google.com/go/vision/v2/apiv1/visionpb"

	"github.com/vismod/vismod/internal/moderate/adapters/golden"
	"github.com/vismod/vismod/pkg/moderation"
)

type fakeAnnotator struct {
	ann *pb.SafeSearchAnnotation
	err error
}

func (f fakeAnnotator) DetectSafeSearch(context.Context, *pb.Image, *pb.ImageContext) (*pb.SafeSearchAnnotation, error) {
	return f.ann, f.err
}
func (fakeAnnotator) Close() error { return nil }

func TestNormalizeGolden(t *testing.T) {
	m := newWith(fakeAnnotator{}, options{})
	res, err := m.Normalize(&pb.SafeSearchAnnotation{
		Adult:    pb.Likelihood_VERY_LIKELY,
		Spoof:    pb.Likelihood_UNLIKELY,
		Medical:  pb.Likelihood_POSSIBLE,
		Violence: pb.Likelihood_LIKELY,
		Racy:     pb.Likelihood_VERY_UNLIKELY,
	})
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, "safesearch", res)

	byCat := map[moderation.Category]moderation.CategoryResult{}
	for _, c := range res.Frames[0].Categories {
		byCat[c.Category] = c
	}
	if c := byCat[moderation.CategorySexual]; *c.Score != 1.0 || c.ScoreOrigin != moderation.OriginLikelihoodEnum {
		t.Errorf("adult VERY_LIKELY = %+v", c)
	}
	if c := byCat[moderation.CategorySuggestiveRacy]; *c.Score != 0.0 {
		t.Errorf("racy VERY_UNLIKELY = %+v", c)
	}
	if c := byCat[moderation.CategoryViolence]; *c.Score != 0.75 {
		t.Errorf("violence LIKELY = %+v", c)
	}
}

func TestUnknownLikelihoodIsNilNeverZero(t *testing.T) {
	m := newWith(fakeAnnotator{}, options{})
	res, err := m.Normalize(&pb.SafeSearchAnnotation{
		Adult: pb.Likelihood_UNKNOWN, // unset
		Racy:  pb.Likelihood_LIKELY,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Frames[0].Categories {
		if c.Category == moderation.CategorySexual && c.Score != nil {
			t.Errorf("UNKNOWN must normalize to nil, got %v", *c.Score)
		}
	}
}

func TestConfigurableLookup(t *testing.T) {
	m := newWith(fakeAnnotator{}, options{LikelihoodScores: map[string]float64{"POSSIBLE": 0.6}})
	res, _ := m.Normalize(&pb.SafeSearchAnnotation{Adult: pb.Likelihood_POSSIBLE})
	for _, c := range res.Frames[0].Categories {
		if c.Category == moderation.CategorySexual && *c.Score != 0.6 {
			t.Errorf("configurable lookup not applied: %v", *c.Score)
		}
	}
}

func TestAnalyzeErrorIsRetryable(t *testing.T) {
	m := newWith(fakeAnnotator{err: context.DeadlineExceeded}, options{RateLimitRPS: 1000})
	_, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x")})
	if err == nil || !moderation.IsRetryable(err) {
		t.Errorf("SDK error must surface as retryable (fail-safe), got %v", err)
	}
}

func TestNilAnnotationIsError(t *testing.T) {
	m := newWith(fakeAnnotator{}, options{})
	if _, err := m.Normalize(nil); err == nil {
		t.Error("nil annotation must be could-not-evaluate")
	}
}
