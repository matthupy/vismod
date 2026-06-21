package hive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/pkg/moderation"
)

// cfgWith builds an AdapterConfig whose Secret accessor returns secrets and whose
// Options carry the (test server) endpoint.
func cfgWith(endpoint string, secrets map[string]string) moderate.AdapterConfig {
	return moderate.AdapterConfig{
		Name:    "hive",
		Options: map[string]any{"endpoint": endpoint},
		Secret:  func(k string) string { return secrets[k] },
	}
}

func TestNew_RequiresToken(t *testing.T) {
	_, err := New(cfgWith("https://api.thehive.ai/api/v2/task/sync", map[string]string{}))
	if err == nil {
		t.Fatal("New must fail fast without VISMOD_HIVE_TOKEN")
	}
}

func TestNew_SucceedsWithToken(t *testing.T) {
	m, err := New(cfgWith("", map[string]string{"HIVE_TOKEN": "tok"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.Name() != "hive" {
		t.Errorf("Name = %q, want hive", m.Name())
	}
	_ = m.Close()
}

func TestCapabilities(t *testing.T) {
	m, _ := New(cfgWith("", map[string]string{"HIVE_TOKEN": "tok"}))
	caps := m.Capabilities()
	if caps.SupportsVideo {
		t.Error("SupportsVideo must be false (image-only adapter; video-native is future)")
	}
	// The adapter emits a broad canonical set; spot-check a few + OTHER.
	want := map[moderation.Category]bool{
		moderation.CategorySexual:  false,
		moderation.CategoryWeapons: false,
		moderation.CategoryHate:    false,
		moderation.CategoryOther:   false,
	}
	for _, c := range caps.Categories {
		if _, ok := want[c]; ok {
			want[c] = true
		}
	}
	for c, found := range want {
		if !found {
			t.Errorf("Capabilities missing category %s", c)
		}
	}
}

func TestAnalyzeImage_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, okBody) // general_nsfw 0.91 -> SEXUAL
	}))
	defer srv.Close()

	m, _ := New(cfgWith(srv.URL, map[string]string{"HIVE_TOKEN": "tok"}))
	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("png"), MIME: "image/png"})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if res.Provider != "hive" || res.MediaType != "image" {
		t.Errorf("provider/media = %q/%q", res.Provider, res.MediaType)
	}
	if len(res.Frames) != 1 || res.Frames[0].Status != moderation.FrameStatusOK {
		t.Fatalf("want 1 ok frame, got %+v", res.Frames)
	}
	if res.Frames[0].TimestampSec != nil {
		t.Error("still image frame must have nil TimestampSec")
	}
	cats := res.Frames[0].Categories
	if len(cats) != 1 || cats[0].Category != moderation.CategorySexual {
		t.Fatalf("want single SEXUAL category, got %+v", cats)
	}
	if cats[0].ScoreOrigin != moderation.ScoreOriginProbability || *cats[0].Score != 0.91 {
		t.Errorf("cat = %+v, want probability 0.91", cats[0])
	}
	// Adapter must NOT stamp Threshold/Flagged — that is the pipeline's job.
	if cats[0].Threshold != nil || cats[0].Flagged {
		t.Errorf("adapter must leave Threshold/Flagged unset, got %+v", cats[0])
	}
}

func TestAnalyzeImage_EmptyOutputIsFailSafeError(t *testing.T) {
	// An empty output is an unexpected provider state -> could-not-evaluate, never
	// a clean frame (fail-safe, spec §F.5).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":[{"response":{"output":[]}}]}`)
	}))
	defer srv.Close()

	m, _ := New(cfgWith(srv.URL, map[string]string{"HIVE_TOKEN": "tok"}))
	_, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("png"), MIME: "image/png"})
	if err == nil {
		t.Fatal("empty output must yield an error, never an allow")
	}
}

func TestAnalyzeImage_RejectsUnsupportedMIME(t *testing.T) {
	m, _ := New(cfgWith("https://x", map[string]string{"HIVE_TOKEN": "tok"}))
	_, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x"), MIME: "image/tiff"})
	if err == nil {
		t.Fatal("tiff is not in Hive's image allow-list; want terminal error")
	}
}
