package microsoft

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/internal/moderate/adapters/golden"
	"github.com/vismod/vismod/pkg/moderation"
)

// fixture is the exact documented response shape: top-level
// categoriesAnalysis only, trimmed severity scale 0/2/4/6.
const fixture = `{"categoriesAnalysis":[
	{"category":"Hate","severity":0},
	{"category":"SelfHarm","severity":0},
	{"category":"Sexual","severity":2},
	{"category":"Violence","severity":6}]}`

func secretEnv(key string) string {
	if key == "microsoft.api_key" {
		return "test-key"
	}
	return ""
}

func newTestModerator(t *testing.T, url string) moderation.Moderator {
	t.Helper()
	m, err := New(moderate.AdapterConfig{
		Name: "microsoft",
		Options: map[string]any{
			"endpoint":       url,
			"rate_limit_rps": 1000.0,
			"max_attempts":   3,
		},
		Secret: secretEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAnalyzeImageGolden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/contentsafety/image:analyze" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("api-version"); got != DefaultAPIVersion {
			t.Errorf("api-version = %s", got)
		}
		if r.Header.Get("Ocp-Apim-Subscription-Key") != "test-key" {
			t.Error("missing subscription key header")
		}
		var req analyzeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if req.Image.Content == "" {
			t.Error("inline base64 content missing (v1 is inline-only)")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	m := newTestModerator(t, srv.URL)
	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, "image_analyze", res)

	// severity/6 discipline: Violence severity 6 -> 1.0, Sexual 2 -> 1/3.
	byLabel := map[string]moderation.CategoryResult{}
	for _, c := range res.Frames[0].Categories {
		byLabel[c.ProviderLabel] = c
	}
	if v := byLabel["Violence"]; *v.Score != 1.0 || v.ScoreOrigin != moderation.OriginSeverity {
		t.Errorf("Violence = %+v", v)
	}
	if s := byLabel["Sexual"]; *s.Score < 0.333 || *s.Score > 0.334 {
		t.Errorf("Sexual severity 2 must normalize to 2/6, got %v", *s.Score)
	}
	if h := byLabel["Hate"]; h.Category != moderation.CategoryHate || *h.Score != 0 {
		t.Errorf("Hate = %+v", h)
	}
}

func TestUnknownCategoryFallsBackToOther(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"categoriesAnalysis":[{"category":"FutureCategory","severity":4}]}`))
	}))
	defer srv.Close()
	m := newTestModerator(t, srv.URL)
	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Frames[0].Categories[0]
	if c.Category != moderation.CategoryOther || c.ProviderLabel != "FutureCategory" || *c.Score != 4.0/6.0 {
		t.Errorf("unmapped label must become OTHER carrying its score: %+v", c)
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("x-ms-error-code", "TooManyRequests")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()
	m := newTestModerator(t, srv.URL)
	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")}); err != nil {
		t.Fatalf("429 then 200 should succeed: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

// Every HTTP attempt — including retries — must take a limiter token;
// otherwise a 429 storm has retries stacking on top of fresh requests.
func TestRetriesRespectRateLimiter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	m, err := New(moderate.AdapterConfig{
		Options: map[string]any{"endpoint": srv.URL, "rate_limit_rps": 2.0, "max_attempts": 3},
		Secret:  secretEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")}); err != nil {
		t.Fatal(err)
	}
	// Two attempts at 2 RPS = two tokens 500ms apart, plus the 1s
	// Retry-After: the second attempt cannot fire before ~1s elapsed.
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("retry bypassed pacing: elapsed=%s", elapsed)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

// A 429 with no Retry-After header must back off substantially (the
// provider's rate window), not the generic sub-second backoff.
func TestRetry429WithoutHeaderBacksOff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests) // no Retry-After
			return
		}
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	m := newTestModerator(t, srv.URL)
	start := time.Now()
	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 1900*time.Millisecond {
		t.Errorf("headerless 429 must wait >= ~2s before retrying, elapsed=%s", elapsed)
	}
}

func TestTerminal4xxDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"InvalidRequest"}}`))
	}))
	defer srv.Close()
	m := newTestModerator(t, srv.URL)
	_, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
	if err == nil {
		t.Fatal("want error")
	}
	if moderation.IsRetryable(err) {
		t.Error("4xx must be terminal, not retryable")
	}
	if calls.Load() != 1 {
		t.Errorf("terminal error retried: calls = %d", calls.Load())
	}
}

func TestExhaustedRetriesAreRetryableNeverAllow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	m, err := New(moderate.AdapterConfig{
		Options: map[string]any{"endpoint": srv.URL, "rate_limit_rps": 1000.0, "max_attempts": 2},
		Secret:  secretEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("img")})
	if err == nil {
		t.Fatal("want error after exhausted retries")
	}
	if !moderation.IsRetryable(err) {
		t.Error("exhausted 5xx retries must be marked retryable")
	}
}

func TestOversizeIsTerminal(t *testing.T) {
	m := newTestModerator(t, "http://unused.invalid")
	big := make([]byte, maxImageBytes+1)
	_, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: big})
	if err == nil || moderation.IsRetryable(err) {
		t.Errorf("oversize must be a terminal error, got %v", err)
	}
}

func TestMissingCredentialsFailFast(t *testing.T) {
	_, err := New(moderate.AdapterConfig{
		Options: map[string]any{"endpoint": "https://x.cognitiveservices.azure.com"},
		Secret:  func(string) string { return "" },
	})
	if err == nil || !strings.Contains(err.Error(), "VISMOD_MICROSOFT_API_KEY") {
		t.Errorf("missing key must fail at construction naming the env var: %v", err)
	}
}

func TestMissingEndpointFailFast(t *testing.T) {
	_, err := New(moderate.AdapterConfig{Options: map[string]any{}, Secret: secretEnv})
	if err == nil {
		t.Error("missing endpoint must fail at construction")
	}
}
