package microsoft

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

func noSecrets(string) string { return "" }

// TestNewFailsFastOnBadConfig: construction IS boot validation. Every one of
// these must fail at startup Ã¢â‚¬â€ discovering a missing endpoint or credential
// per job means the failure lands as verdict=error on real traffic instead
// of a refused boot the operator sees immediately.
func TestNewFailsFastOnBadConfig(t *testing.T) {
	cases := []struct {
		name    string
		options map[string]any
		secret  func(string) string
		wantIn  string
	}{
		{
			name:    "missing endpoint",
			options: map[string]any{},
			secret:  secretEnv,
			wantIn:  "endpoint",
		},
		{
			name:    "key auth without a key",
			options: map[string]any{"endpoint": "https://x.cognitiveservices.azure.com"},
			secret:  noSecrets,
			wantIn:  "VISMOD_MICROSOFT_API_KEY",
		},
		{
			name:    "unknown auth mode",
			options: map[string]any{"endpoint": "https://x.cognitiveservices.azure.com", "auth_mode": "oauth"},
			secret:  secretEnv,
			wantIn:  "auth_mode",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(moderate.AdapterConfig{Name: "microsoft", Options: tc.options, Secret: tc.secret})
			if err == nil {
				t.Fatal("want a boot error")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q should mention %q", err, tc.wantIn)
			}
		})
	}
}

// TestNewAppliesDefaults: the rate limit defaults BELOW the Azure F0 quota
// (5 RPS) on purpose, and the API version is pinned so envelopes stay
// attributable to a known model revision.
func TestNewAppliesDefaults(t *testing.T) {
	m, err := New(moderate.AdapterConfig{
		Name:    "microsoft",
		Options: map[string]any{"endpoint": "https://x.cognitiveservices.azure.com/"},
		Secret:  secretEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	mod := m.(*Moderator)

	if mod.opts.RateLimitRPS >= 5 {
		t.Errorf("default rate limit %v is not below the 5 RPS F0 quota", mod.opts.RateLimitRPS)
	}
	if mod.opts.APIVersion != DefaultAPIVersion {
		t.Errorf("api version = %q, want the pinned default", mod.opts.APIVersion)
	}
	if strings.HasSuffix(mod.opts.Endpoint, "/") {
		t.Errorf("endpoint %q keeps its trailing slash; request URLs would double up", mod.opts.Endpoint)
	}
	if m.Name() != "microsoft" {
		t.Errorf("Name = %q", m.Name())
	}
	if mv, ok := m.(interface{ ModelVersion() string }); !ok || mv.ModelVersion() != DefaultAPIVersion {
		t.Error("adapter must expose its pinned API version for the audit ModelIdentity")
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestCapabilitiesAreImageOnlyAndBounded: the pipeline pre-flights oversize
// images against MaxImageBytes and extracts frames because SupportsVideo is
// false. Both are load-bearing, not documentation.
func TestCapabilitiesAreImageOnlyAndBounded(t *testing.T) {
	m, err := New(moderate.AdapterConfig{
		Name:    "microsoft",
		Options: map[string]any{"endpoint": "https://x.cognitiveservices.azure.com"},
		Secret:  secretEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	caps := m.Capabilities()
	if caps.SupportsVideo {
		t.Error("Azure Content Safety is per-image; claiming video would skip frame extraction")
	}
	if caps.MaxImageBytes <= 0 {
		t.Error("MaxImageBytes must be set so oversize images fail before a billed call")
	}
	want := map[moderation.Category]bool{
		moderation.CategoryHate: true, moderation.CategorySelfHarm: true,
		moderation.CategorySexual: true, moderation.CategoryViolence: true,
	}
	if len(caps.Categories) != len(want) {
		t.Fatalf("categories = %v, want the four documented ones", caps.Categories)
	}
	for _, c := range caps.Categories {
		if !want[c] {
			t.Errorf("unexpected declared category %q", c)
		}
	}
	if _, ok := m.(moderation.VideoModerator); ok {
		t.Error("the adapter must not satisfy VideoModerator")
	}
}

// TestEntraAuthPrefersStaticToken: when an external process already handles
// acquisition and rotation, the adapter must use that token rather than
// reaching for IMDS (which only answers on Azure compute and would hang
// elsewhere).
func TestEntraAuthPrefersStaticToken(t *testing.T) {
	m, err := New(moderate.AdapterConfig{
		Name:    "microsoft",
		Options: map[string]any{"endpoint": "https://x.cognitiveservices.azure.com", "auth_mode": "entra"},
		Secret: func(k string) string {
			if k == "microsoft.access_token" {
				return "static-token"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mod := m.(*Moderator)
	if _, ok := mod.tokens.(staticToken); !ok {
		t.Fatalf("token source = %T, want staticToken", mod.tokens)
	}
	tok, err := mod.tokens.Token(context.Background())
	if err != nil || tok != "static-token" {
		t.Errorf("Token = (%q, %v)", tok, err)
	}
	if mod.key != "" {
		t.Error("entra mode must not also carry an API key")
	}
}

func TestEntraAuthFallsBackToIMDS(t *testing.T) {
	m, err := New(moderate.AdapterConfig{
		Name:    "microsoft",
		Options: map[string]any{"endpoint": "https://x.cognitiveservices.azure.com", "auth_mode": "entra"},
		Secret:  noSecrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(*Moderator).tokens.(*imdsTokenSource); !ok {
		t.Errorf("token source = %T, want *imdsTokenSource", m.(*Moderator).tokens)
	}
}

// imdsRedirect routes the hard-coded IMDS address to a test server.
type imdsRedirect struct {
	target *url.URL
	seen   *int
}

func (r imdsRedirect) RoundTrip(req *http.Request) (*http.Response, error) {
	*r.seen++
	clone := req.Clone(req.Context())
	clone.URL.Scheme = r.target.Scheme
	clone.URL.Host = r.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func imdsClient(t *testing.T, handler http.HandlerFunc) (*http.Client, *int) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	return &http.Client{Transport: imdsRedirect{target: u, seen: calls}}, calls
}

// TestIMDSTokenIsCachedUntilExpiry: every moderation request needs a bearer
// token. Re-fetching per request would add an IMDS round trip to each frame
// of every video.
func TestIMDSTokenIsCachedUntilExpiry(t *testing.T) {
	client, calls := imdsClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" {
			t.Error("IMDS requires the Metadata: true header; without it the request is rejected")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": "imds-token",
			"expires_on":   strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		})
	})

	src := newIMDSTokenSource(client)
	for range 3 {
		tok, err := src.Token(context.Background())
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if tok != "imds-token" {
			t.Fatalf("token = %q", tok)
		}
	}
	if *calls != 1 {
		t.Errorf("IMDS called %d times, want 1 (token not cached)", *calls)
	}
}

// TestIMDSRefetchesNearExpiry: the cache is only valid up to two minutes
// before expiry. Serving a token past that window means requests start
// failing with 401 mid-job.
func TestIMDSRefetchesNearExpiry(t *testing.T) {
	client, calls := imdsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": "short-lived",
			"expires_on":   strconv.FormatInt(time.Now().Add(30*time.Second).Unix(), 10),
		})
	})

	src := newIMDSTokenSource(client)
	for range 2 {
		if _, err := src.Token(context.Background()); err != nil {
			t.Fatalf("Token: %v", err)
		}
	}
	if *calls != 2 {
		t.Errorf("IMDS called %d times, want 2 (a near-expiry token must be refreshed)", *calls)
	}
}

// TestIMDSFailuresAreErrors: an empty token, a non-200, or unparseable JSON
// must surface as an error. Returning "" would send unauthenticated requests
// and turn an auth misconfiguration into a stream of provider 401s.
func TestIMDSFailuresAreErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"non-200", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no identity", http.StatusBadRequest)
		}},
		{"empty token", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"","expires_on":"0"}`))
		}},
		{"unparseable body", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not json`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := imdsClient(t, tc.handler)
			tok, err := newIMDSTokenSource(client).Token(context.Background())
			if err == nil {
				t.Fatal("want an error")
			}
			if tok != "" {
				t.Errorf("token = %q, want empty on failure", tok)
			}
		})
	}
}

// TestIMDSUnparseableExpiryStillCaches: a missing/garbled expires_on must
// not disable caching outright Ã¢â‚¬â€ it falls back to a conservative window.
func TestIMDSUnparseableExpiryStillCaches(t *testing.T) {
	client, calls := imdsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_on":"whenever"}`))
	})

	src := newIMDSTokenSource(client)
	for range 2 {
		if _, err := src.Token(context.Background()); err != nil {
			t.Fatalf("Token: %v", err)
		}
	}
	if *calls != 1 {
		t.Errorf("IMDS called %d times, want 1 (fallback expiry should still cache)", *calls)
	}
}
