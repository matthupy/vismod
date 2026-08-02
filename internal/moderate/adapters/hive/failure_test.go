package hive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

// TestNewRejectsMalformedOptions: an options typo must fail at boot with a
// message about the option, not surface later as a per-job failure.
func TestNewRejectsMalformedOptions(t *testing.T) {
	_, err := New(moderate.AdapterConfig{
		Options: map[string]any{"rate_limit_rps": "ten"},
		Secret:  func(string) string { return "token" },
	})
	if err == nil {
		t.Fatal("a non-numeric rate_limit_rps was accepted")
	}
	if !strings.Contains(err.Error(), "options") {
		t.Errorf("error = %v, want it to name the bad option", err)
	}
}

// TestIdentityIsPinned: the adapter name is what selects it from config and
// what appears in every envelope and audit record; the model version is
// what makes a verdict attributable to a model generation.
func TestIdentityIsPinned(t *testing.T) {
	m, err := New(moderate.AdapterConfig{Secret: func(string) string { return "token" }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.Name() != "hive" {
		t.Errorf("Name = %q, want hive", m.Name())
	}
	versioned, ok := m.(interface{ ModelVersion() string })
	if !ok {
		t.Fatal("the adapter no longer reports a ModelVersion; envelopes would read \"unversioned\"")
	}
	if versioned.ModelVersion() == "" {
		t.Error("empty ModelVersion")
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestMediaFilenameFollowsTheMIMEType: Hive keys off the upload's
// extension. Sending a PNG named .jpg risks the vendor mis-decoding a
// frame, which fails the job rather than scoring it.
func TestMediaFilenameFollowsTheMIMEType(t *testing.T) {
	cases := map[string]string{
		"image/png":                "frame.png",
		"image/webp":               "frame.webp",
		"image/gif":                "frame.gif",
		"IMAGE/PNG":                "frame.png", // vendors are inconsistent about case
		"image/jpeg":               "frame.jpg",
		"":                         "frame.jpg", // ffmpeg's output is the documented default
		"application/octet-stream": "frame.jpg",
	}
	for mime, want := range cases {
		if got := mediaFilename(mime); got != want {
			t.Errorf("mediaFilename(%q) = %q, want %q", mime, got, want)
		}
	}
}

// TestUndecodableResponseIsAnError: a 200 carrying something that is not
// the documented envelope is could-not-evaluate. Treating it as an empty
// result would score the frame as clean.
func TestUndecodableResponseIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status": "not-an-array"}`))
	}))
	defer srv.Close()

	m := newTestModerator(t, srv.URL)
	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x"), MIME: "image/png"}); err == nil {
		t.Fatal("an undecodable response was treated as a successful analysis")
	}
}

// TestCancelledContextNeverReachesTheVendor: the rate limiter sits in the
// per-attempt builder. A cancelled job must stop there — sending anyway
// spends a billed call on work nobody is waiting for.
func TestCancelledContextNeverReachesTheVendor(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newTestModerator(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.AnalyzeImage(ctx, moderation.Image{Bytes: []byte("x"), MIME: "image/png"}); err == nil {
		t.Fatal("a cancelled analysis reported success")
	}
	if hits != 0 {
		t.Errorf("the vendor was called %d times for a cancelled job", hits)
	}
}
