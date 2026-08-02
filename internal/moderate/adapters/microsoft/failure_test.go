package microsoft

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

// TestNewRejectsMalformedOptions: an options typo must fail at boot naming
// the option, before the credential check reports a misleading "key
// required".
func TestNewRejectsMalformedOptions(t *testing.T) {
	_, err := New(moderate.AdapterConfig{
		Options: map[string]any{"rate_limit_rps": "four"},
		Secret:  secretEnv,
	})
	if err == nil {
		t.Fatal("a non-numeric rate_limit_rps was accepted")
	}
	if !strings.Contains(err.Error(), "options") {
		t.Errorf("error = %v, want it to name the bad option", err)
	}
}

// TestUndecodableResponseIsAnError: a 200 that is not the documented
// envelope is could-not-evaluate, never an empty (clean) analysis.
func TestUndecodableResponseIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"categoriesAnalysis": "not-an-array"}`))
	}))
	defer srv.Close()

	m := newTestModerator(t, srv.URL)
	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x")}); err == nil {
		t.Fatal("an undecodable response was treated as a successful analysis")
	}
}

// TestCancelledContextNeverReachesTheVendor: the limiter wait lives in the
// per-attempt request builder, so a cancelled job must stop before the
// billed call goes out.
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

	if _, err := m.AnalyzeImage(ctx, moderation.Image{Bytes: []byte("x")}); err == nil {
		t.Fatal("a cancelled analysis reported success")
	}
	if hits != 0 {
		t.Errorf("the vendor was called %d times for a cancelled job", hits)
	}
}

// erroringTokens is an Entra token source whose credential has gone away.
type erroringTokens struct{}

func (erroringTokens) Token(context.Context) (string, error) {
	return "", errors.New("managed identity unavailable")
}

// TestEntraTokenFailureIsAnErrorNotAnUnauthenticatedCall: without a token
// the request must not be sent at all. Sending it unauthenticated would
// draw a 401 that reads as a provider problem rather than a credential one.
func TestEntraTokenFailureIsAnErrorNotAnUnauthenticatedCall(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, err := New(moderate.AdapterConfig{
		Options: map[string]any{
			"endpoint": srv.URL, "rate_limit_rps": 1000.0,
			"max_attempts": 1, "auth_mode": "entra",
		},
		Secret: func(key string) string {
			if key == "microsoft.access_token" {
				return "seed" // a static source, swapped out below
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mm, ok := m.(*Moderator)
	if !ok {
		t.Fatalf("New returned %T", m)
	}
	mm.tokens = erroringTokens{}

	if _, err := mm.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x")}); err == nil {
		t.Fatal("an unobtainable Entra token was treated as a successful analysis")
	}
	if hits != 0 {
		t.Errorf("the vendor was called %d times without a bearer token", hits)
	}
}

// TestEntraStaticTokenIsSentAsBearer: the entra path must actually
// authorize. A missing header would be a 401 on every job.
func TestEntraStaticTokenIsSentAsBearer(t *testing.T) {
	var gotAuth, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("Ocp-Apim-Subscription-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"categoriesAnalysis":[{"category":"Hate","severity":0}]}`))
	}))
	defer srv.Close()

	m, err := New(moderate.AdapterConfig{
		Options: map[string]any{
			"endpoint": srv.URL, "rate_limit_rps": 1000.0,
			"max_attempts": 1, "auth_mode": "entra",
		},
		Secret: func(key string) string {
			if key == "microsoft.access_token" {
				return "static-token"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x")}); err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}
	if gotAuth != "Bearer static-token" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
	if gotKey != "" {
		t.Error("the key header was also sent under auth_mode=entra; only one credential belongs on the wire")
	}
}
