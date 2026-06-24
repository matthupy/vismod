package google

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/pkg/moderation"
)

// cfgWith builds an AdapterConfig with the given options and secret map.
func cfgWith(opts map[string]any, secrets map[string]string) moderate.AdapterConfig {
	return moderate.AdapterConfig{
		Name:    "google",
		Options: opts,
		Secret:  func(k string) string { return secrets[k] },
	}
}

// TestNewRequiresSecret: the factory fails fast (spec §F.2) when the apikey
// secret is missing.
func TestNewRequiresSecret(t *testing.T) {
	if _, err := New(cfgWith(nil, nil)); err == nil {
		t.Fatal("New must error when VISMOD_GOOGLE_API_KEY is absent")
	}
}

// TestNewAndCapabilities: a valid apikey config builds, and Caps advertises the
// five SafeSearch categories, the 20 MB image cap, and no video support.
func TestNewAndCapabilities(t *testing.T) {
	m, err := New(cfgWith(nil, map[string]string{"GOOGLE_API_KEY": "k"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.Name() != "google" {
		t.Errorf("Name = %q", m.Name())
	}
	caps := m.Capabilities()
	if caps.SupportsVideo {
		t.Error("SafeSearch is image-only")
	}
	if caps.MaxImageBytes != maxImageBytes {
		t.Errorf("MaxImageBytes = %d, want %d", caps.MaxImageBytes, maxImageBytes)
	}
	want := map[moderation.Category]bool{
		moderation.CategorySexual: true, moderation.CategorySuggestiveRacy: true,
		moderation.CategoryViolence: true, moderation.CategoryMedical: true,
		moderation.CategorySpoof: true,
	}
	if len(caps.Categories) != len(want) {
		t.Fatalf("Categories = %v", caps.Categories)
	}
	for _, c := range caps.Categories {
		if !want[c] {
			t.Errorf("unexpected category %s", c)
		}
	}
}

// newTestAdapter builds an adapter pointed at srv with apikey auth.
func newTestAdapter(t *testing.T, srv *httptest.Server) moderation.Moderator {
	t.Helper()
	m, err := New(cfgWith(
		map[string]any{"endpoint": srv.URL, "max_retries": 0},
		map[string]string{"GOOGLE_API_KEY": "k"},
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// TestAnalyzeImageSuccess: a 200 annotation normalizes into a single OK frame
// with the five categories, provider+model stamped.
func TestAnalyzeImageSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"responses":[{"safeSearchAnnotation":{"adult":"VERY_LIKELY","spoof":"VERY_UNLIKELY","medical":"UNLIKELY","violence":"POSSIBLE","racy":"LIKELY"}}]}`)
	}))
	defer srv.Close()

	res, err := newTestAdapter(t, srv).AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x"), MIME: "image/png"})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if res.Provider != "google" {
		t.Errorf("Provider = %q", res.Provider)
	}
	if res.ModelVersion != "v1" {
		t.Errorf("ModelVersion = %q, want v1", res.ModelVersion)
	}
	if len(res.Frames) != 1 || res.Frames[0].Status != moderation.FrameStatusOK {
		t.Fatalf("frames = %+v", res.Frames)
	}
	if len(res.Frames[0].Categories) != 5 {
		t.Errorf("categories = %d, want 5", len(res.Frames[0].Categories))
	}
}

// TestAnalyzeImagePerResponseError: an HTTP-200 with responses[].error is
// could-not-evaluate -> error (fail-safe), never a clean frame.
func TestAnalyzeImagePerResponseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"responses":[{"error":{"code":7,"message":"permission denied"}}]}`)
	}))
	defer srv.Close()

	_, err := newTestAdapter(t, srv).AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x"), MIME: "image/png"})
	if err == nil {
		t.Fatal("per-response error must surface as error (fail-safe)")
	}
}

// TestAnalyzeImageMissingAnnotation: a 200 with no safeSearchAnnotation is
// could-not-evaluate -> error, not an empty clean frame.
func TestAnalyzeImageMissingAnnotation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"responses":[{}]}`)
	}))
	defer srv.Close()

	if _, err := newTestAdapter(t, srv).AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x"), MIME: "image/png"}); err == nil {
		t.Fatal("missing annotation must surface as error")
	}
}

func TestValidateInput(t *testing.T) {
	if err := validateInput(moderation.Image{}); err == nil {
		t.Error("empty bytes must error")
	}
	if err := validateInput(moderation.Image{Bytes: make([]byte, maxImageBytes+1), MIME: "image/png"}); err == nil {
		t.Error("oversize must error")
	}
	if err := validateInput(moderation.Image{Bytes: []byte("x"), MIME: "application/zip"}); err == nil {
		t.Error("unsupported MIME must error")
	}
	if err := validateInput(moderation.Image{Bytes: []byte("x"), MIME: "image/png"}); err != nil {
		t.Errorf("valid png rejected: %v", err)
	}
}

// TestRegistered: the adapter self-registers under "google" via init().
func TestRegistered(t *testing.T) {
	names := strings.Join(moderate.Names(), ",")
	if !strings.Contains(names, "google") {
		t.Errorf("google not registered; names = %s", names)
	}
}
