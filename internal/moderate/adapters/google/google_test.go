package google

import (
	"context"
	"encoding/base64"
	"errors"
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
// five SafeSearch categories, the derived raw-image cap (so the pre-flight gate
// matches Vision's 10 MB inline-request limit), and no video support.
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
	if caps.MaxImageBytes != maxRawImageBytes {
		t.Errorf("MaxImageBytes = %d, want %d", caps.MaxImageBytes, maxRawImageBytes)
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

// codeOf extracts the CodedError code, failing the test if err does not
// implement moderation.CodedError (every terminal validateInput failure must,
// so it lands in vismod_adapter_errors_total{code} with a label).
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var ce moderation.CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("error %T does not implement moderation.CodedError: %v", err, err)
	}
	return ce.ErrorCode()
}

func TestValidateInput(t *testing.T) {
	if got := codeOf(t, validateInput(moderation.Image{})); got != "input_empty" {
		t.Errorf("empty bytes code = %q, want input_empty", got)
	}
	// Raw bytes over the derived raw ceiling -> input_oversize.
	if got := codeOf(t, validateInput(moderation.Image{Bytes: make([]byte, maxRawImageBytes+1), MIME: "image/png"})); got != "input_oversize" {
		t.Errorf("oversize code = %q, want input_oversize", got)
	}
	if got := codeOf(t, validateInput(moderation.Image{Bytes: []byte("x"), MIME: "application/zip"})); got != "input_mime" {
		t.Errorf("unsupported MIME code = %q, want input_mime", got)
	}
	if err := validateInput(moderation.Image{Bytes: []byte("x"), MIME: "image/png"}); err != nil {
		t.Errorf("valid png rejected: %v", err)
	}
}

// TestValidateInputBase64Inflation proves issue 1: an image whose RAW size sat
// under the old 20 MB image cap but whose base64-ENCODED form exceeds Vision's
// 10 MB JSON request limit is now rejected. 8 MB raw inflates to ~10.67 MB
// encoded (> 10 MB), so it must be a terminal input_oversize, not slip through
// to a Vision 400.
func TestValidateInputBase64Inflation(t *testing.T) {
	raw := make([]byte, 8*1024*1024) // 8 MB: under the old 20 MB image cap...
	if base64.StdEncoding.EncodedLen(len(raw)) <= maxRequestBytes {
		t.Fatalf("test premise broken: 8 MB encodes to %d, want > %d", base64.StdEncoding.EncodedLen(len(raw)), maxRequestBytes)
	}
	if got := codeOf(t, validateInput(moderation.Image{Bytes: raw, MIME: "image/png"})); got != "input_oversize" {
		t.Errorf("base64-inflated image code = %q, want input_oversize", got)
	}
}

// TestRegistered: the adapter self-registers under "google" via init().
func TestRegistered(t *testing.T) {
	names := strings.Join(moderate.Names(), ",")
	if !strings.Contains(names, "google") {
		t.Errorf("google not registered; names = %s", names)
	}
}
