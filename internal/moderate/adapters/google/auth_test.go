package google

import (
	"net/http"
	"net/url"
	"testing"
)

func secretFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestNewAuthAPIKey: apikey mode reads VISMOD_GOOGLE_API_KEY and applies it as a
// ?key= query param (the Vision REST apikey scheme), NEVER as a header.
func TestNewAuthAPIKey(t *testing.T) {
	auth, err := newAuth("", secretFrom(map[string]string{"GOOGLE_API_KEY": "sekret"}))
	if err != nil {
		t.Fatalf("newAuth apikey: %v", err)
	}
	if auth.describe() != "apikey" {
		t.Errorf("describe = %q, want apikey", auth.describe())
	}
	req, _ := http.NewRequest(http.MethodPost, "https://vision.googleapis.com/v1/images:annotate", nil)
	auth.apply(req)
	if got := req.URL.Query().Get("key"); got != "sekret" {
		t.Errorf("key query = %q, want sekret", got)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("apikey must not set an Authorization header")
	}
}

// TestNewAuthAPIKeyPreservesQuery proves applying the key keeps any pre-existing
// query params on the endpoint.
func TestNewAuthAPIKeyPreservesQuery(t *testing.T) {
	auth, _ := newAuth("apikey", secretFrom(map[string]string{"GOOGLE_API_KEY": "k"}))
	req, _ := http.NewRequest(http.MethodPost, "https://host/v1/images:annotate?alt=json", nil)
	auth.apply(req)
	q, _ := url.ParseQuery(req.URL.RawQuery)
	if q.Get("alt") != "json" || q.Get("key") != "k" {
		t.Errorf("query lost params: %q", req.URL.RawQuery)
	}
}

// TestNewAuthBearer: bearer mode reads VISMOD_GOOGLE_TOKEN and sets the
// Authorization header, plus x-goog-user-project when a project is configured.
func TestNewAuthBearer(t *testing.T) {
	auth, err := newAuth("bearer", secretFrom(map[string]string{
		"GOOGLE_TOKEN":   "tok",
		"GOOGLE_PROJECT": "my-proj",
	}))
	if err != nil {
		t.Fatalf("newAuth bearer: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://host", nil)
	auth.apply(req)
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("x-goog-user-project"); got != "my-proj" {
		t.Errorf("x-goog-user-project = %q", got)
	}
}

// TestNewAuthMissingSecret: each mode fails fast at construction when its secret
// is absent (spec §F.2 — fail at boot, not per-job).
func TestNewAuthMissingSecret(t *testing.T) {
	if _, err := newAuth("apikey", secretFrom(nil)); err == nil {
		t.Error("apikey with no key must error")
	}
	if _, err := newAuth("bearer", secretFrom(nil)); err == nil {
		t.Error("bearer with no token must error")
	}
}

func TestNewAuthUnknownMode(t *testing.T) {
	if _, err := newAuth("magic", secretFrom(map[string]string{"GOOGLE_API_KEY": "k"})); err == nil {
		t.Error("unknown auth_mode must error")
	}
}
