package azure

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matthupy/vismod/internal/moderate"
	"github.com/matthupy/vismod/pkg/moderation"
)

// secretFunc builds a Secret accessor from a fixed map (mirrors VISMOD_<KEY>).
func secretFunc(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestAzureRegistered(t *testing.T) {
	found := false
	for _, n := range moderate.Names() {
		if n == "azure" {
			found = true
		}
	}
	if !found {
		t.Fatal("azure must self-register")
	}
}

func TestNewFailsFastWithoutKey(t *testing.T) {
	_, err := New(moderate.AdapterConfig{
		Name:    "azure",
		Options: map[string]any{"endpoint": "https://x.cognitiveservices.azure.com"},
		Secret:  secretFunc(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "VISMOD_AZURE_KEY") {
		t.Fatalf("want fail-fast on missing key, got %v", err)
	}
}

func TestNewFailsFastWithoutEndpoint(t *testing.T) {
	_, err := New(moderate.AdapterConfig{
		Name:   "azure",
		Secret: secretFunc(map[string]string{"AZURE_KEY": "k"}),
	})
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("want fail-fast on missing endpoint, got %v", err)
	}
}

func TestNewBearerAuthRequiresToken(t *testing.T) {
	_, err := New(moderate.AdapterConfig{
		Name:    "azure",
		Options: map[string]any{"endpoint": "https://x.cognitiveservices.azure.com", "auth_mode": "bearer"},
		Secret:  secretFunc(map[string]string{"AZURE_KEY": "k"}),
	})
	if err == nil || !strings.Contains(err.Error(), "VISMOD_AZURE_TOKEN") {
		t.Fatalf("want bearer to require token, got %v", err)
	}
}

func TestNewEndpointFromSecret(t *testing.T) {
	m, err := New(moderate.AdapterConfig{
		Name: "azure",
		Secret: secretFunc(map[string]string{
			"AZURE_KEY":      "k",
			"AZURE_ENDPOINT": "https://x.cognitiveservices.azure.com",
		}),
	})
	if err != nil {
		t.Fatalf("endpoint-from-secret should construct: %v", err)
	}
	if m.Name() != "azure" {
		t.Errorf("Name = %q", m.Name())
	}
}

func TestCapabilities(t *testing.T) {
	m := mustNew(t, nil)
	caps := m.Capabilities()
	if caps.SupportsVideo {
		t.Error("image:analyze is image-only")
	}
	if caps.MaxImageBytes != maxImageBytes {
		t.Errorf("MaxImageBytes = %d, want %d", caps.MaxImageBytes, maxImageBytes)
	}
	if _, ok := m.(moderation.VideoModerator); ok {
		t.Error("azure must NOT implement VideoModerator")
	}
	if len(caps.Categories) != 4 {
		t.Errorf("want 4 categories, got %d", len(caps.Categories))
	}
}

func TestValidateInput(t *testing.T) {
	cases := []struct {
		name    string
		img     moderation.Image
		wantErr string
	}{
		{"empty", moderation.Image{}, "empty image"},
		{"oversize", moderation.Image{Bytes: make([]byte, maxImageBytes+1), MIME: "image/png"}, "exceeds 4 MB"},
		{"bad-mime", moderation.Image{Bytes: []byte("x"), MIME: "application/pdf"}, "unsupported MIME"},
		{"ok-png", moderation.Image{Bytes: []byte("x"), MIME: "image/png"}, ""},
		{"ok-no-mime", moderation.Image{Bytes: []byte("x")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInput(tc.img)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestAnalyzeImageEndToEnd drives the full adapter against an httptest server
// and asserts the normalized single-frame result + ModelVersion stamping.
func TestAnalyzeImageEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"categoriesAnalysis":[{"category":"Sexual","severity":4}]}`))
	}))
	defer srv.Close()

	m := mustNew(t, map[string]any{"endpoint": srv.URL})
	// Point the real client at the test server.
	az := m.(*azure)
	az.client.httpClient = srv.Client()
	az.client.endpoint = srv.URL

	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img"), MIME: "image/png"})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if res.ModelVersion != defaultAPIVersion {
		t.Errorf("ModelVersion = %q, want %q", res.ModelVersion, defaultAPIVersion)
	}
	if res.Provider != "azure" || res.MediaType != "image" {
		t.Errorf("provider/media = %q/%q", res.Provider, res.MediaType)
	}
	if len(res.Frames) != 1 || res.Frames[0].Status != moderation.FrameStatusOK {
		t.Fatalf("want one ok frame, got %+v", res.Frames)
	}
	got := res.Frames[0].Categories
	if len(got) != 1 || got[0].Category != moderation.CategorySexual {
		t.Fatalf("unexpected categories: %+v", got)
	}
	if got[0].Score == nil || *got[0].Score != 4.0/6.0 {
		t.Errorf("score = %v, want severity/6", got[0].Score)
	}
}

func TestAnalyzeImageReturnsErrorOnProviderFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-error-code", "InvalidRequest")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"InvalidRequest","message":"bad"}}`))
	}))
	defer srv.Close()

	m := mustNew(t, map[string]any{"endpoint": srv.URL})
	az := m.(*azure)
	az.client.httpClient = srv.Client()
	az.client.endpoint = srv.URL

	// Fail-safe: provider failure surfaces an error (pipeline records frame
	// Status=error) — the adapter NEVER fabricates an allow-able result.
	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img"), MIME: "image/png"}); err == nil {
		t.Fatal("want error on provider 400, got nil")
	}
}

// TestAnalyzeImageEmptyResponseIsError proves a 200 with empty categoriesAnalysis
// is treated as could-not-evaluate (fail-safe), not a clean OK frame.
func TestAnalyzeImageEmptyResponseIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"categoriesAnalysis":[]}`))
	}))
	defer srv.Close()

	m := mustNew(t, map[string]any{"endpoint": srv.URL})
	az := m.(*azure)
	az.client.httpClient = srv.Client()
	az.client.endpoint = srv.URL

	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img"), MIME: "image/png"}); err == nil {
		t.Fatal("empty categoriesAnalysis must yield an error, not an OK frame")
	}
}

func mustNew(t *testing.T, opts map[string]any) moderation.Moderator {
	t.Helper()
	if opts == nil {
		opts = map[string]any{"endpoint": "https://x.cognitiveservices.azure.com"}
	}
	m, err := New(moderate.AdapterConfig{
		Name:    "azure",
		Options: opts,
		Secret:  secretFunc(map[string]string{"AZURE_KEY": "k"}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}
