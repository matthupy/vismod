package shieldgemma

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestValidateEndpoint covers the provider-endpoint URL rules one at a
// time. These are NOT the media-source URL rules: a self-hosted inference
// server is EXPECTED to be loopback or RFC 1918, so those ranges are
// permitted here and plaintext is permitted only for them.
func TestValidateEndpoint(t *testing.T) {
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		// Scheme allow-list.
		{"https public host", "https://gpu.example.com/v1/chat/completions", true},
		{"ftp rejected", "ftp://gpu.example.com/v1", false},
		{"file rejected", "file:///etc/passwd", false},
		{"gopher rejected", "gopher://gpu.example.com/1", false},
		{"scheme missing", "gpu.example.com/v1", false},
		{"empty", "", false},
		{"unparseable", "http://[::1", false},
		{"no host", "https:///v1/chat", false},

		// Plaintext inward only.
		{"http loopback ipv4", "http://127.0.0.1:8000/v1/chat/completions", true},
		{"http loopback name", "http://localhost:8000/v1/chat/completions", true},
		{"http loopback ipv6", "http://[::1]:8000/v1/chat/completions", true},
		{"http rfc1918 10/8", "http://10.0.0.7:8000/v1", true},
		{"http rfc1918 172.16/12", "http://172.16.4.4:8000/v1", true},
		{"http rfc1918 172.31 edge", "http://172.31.255.255:8000/v1", true},
		{"http 172.32 is public", "http://172.32.0.1:8000/v1", false},
		{"http rfc1918 192.168/16", "http://192.168.1.10:8000/v1", true},
		{"http public ip rejected", "http://93.184.216.34:8000/v1", false},
		{"http public name rejected", "http://gpu.example.com/v1", false},
		{"https private host allowed", "https://10.0.0.7:8443/v1", true},

		// Cloud metadata range, unconditionally.
		{"http link-local metadata", "http://169.254.169.254/latest/meta-data/", false},
		{"https link-local metadata", "https://169.254.169.254/v1", false},
		{"http link-local any address", "http://169.254.0.1/v1", false},

		// Userinfo is a secret in yaml (invariant 4).
		{"userinfo rejected", "https://user:pass@gpu.example.com/v1", false},
		{"username only rejected", "http://user@127.0.0.1:8000/v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEndpoint(tc.url)
			if tc.ok && err != nil {
				t.Fatalf("validateEndpoint(%q) = %v, want accepted", tc.url, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("validateEndpoint(%q) accepted, want rejected", tc.url)
			}
		})
	}
}

// TestClientDoesNotFollowRedirects proves the CheckRedirect hook errors: a
// redirect is a destination vismod did not choose, and following one would
// reopen the SSRF hole the validator closes.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var reachedTarget bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedTarget = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	resp, err := newHTTPClient(0).Get(redirector.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("redirect was followed or tolerated; want an error from CheckRedirect")
	}
	if reachedTarget {
		t.Error("client reached the redirect target")
	}
}
