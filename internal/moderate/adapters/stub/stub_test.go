package stub

import (
	"context"
	"testing"

	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/pkg/moderation"
)

func newStub(t *testing.T) moderation.Moderator {
	t.Helper()
	m, err := New(moderate.AdapterConfig{Name: adapterName, Secret: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStubRegistered(t *testing.T) {
	found := false
	for _, n := range moderate.Names() {
		if n == "stub" {
			found = true
		}
	}
	if !found {
		t.Fatal("stub must self-register")
	}
}

func TestStubDeterministic(t *testing.T) {
	m := newStub(t)
	img := moderation.Image{Bytes: []byte("same-bytes")}
	a, _ := m.AnalyzeImage(context.Background(), img)
	b, _ := m.AnalyzeImage(context.Background(), img)
	if len(a.Frames) != 1 || len(b.Frames) != 1 {
		t.Fatal("want one frame")
	}
	for i := range a.Frames[0].Categories {
		sa := a.Frames[0].Categories[i].Score
		sb := b.Frames[0].Categories[i].Score
		if (sa == nil) != (sb == nil) || (sa != nil && *sa != *sb) {
			t.Fatalf("non-deterministic score at %d", i)
		}
	}
}

func TestStubEmitsOtherFallback(t *testing.T) {
	m := newStub(t)
	res, _ := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x")})
	var sawOther bool
	for _, c := range res.Frames[0].Categories {
		if c.Category == moderation.CategoryOther {
			sawOther = true
			if c.ProviderLabel == "" {
				t.Error("OTHER row must preserve the raw provider label")
			}
			if c.Score == nil {
				t.Error("OTHER row must carry its score, not drop it")
			}
		}
	}
	if !sawOther {
		t.Fatal("stub must emit an OTHER row to exercise the fallback")
	}
}

func TestStubCaps(t *testing.T) {
	m := newStub(t)
	caps := m.Capabilities()
	if caps.SupportsVideo {
		t.Error("stub is image-only")
	}
	if caps.MaxImageBytes != 4*1024*1024 {
		t.Errorf("MaxImageBytes = %d", caps.MaxImageBytes)
	}
	if _, ok := m.(moderation.VideoModerator); ok {
		t.Error("stub must NOT implement VideoModerator")
	}
}
