package fetch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testFetcher builds a Fetcher pointed at an httptest server. The address
// policy is replaced with a permissive one because httptest binds
// loopback, which the real policy correctly denies. The real policy has
// its own exhaustive test in ippolicy_test.go; this seam keeps the two
// concerns separately testable.
func testFetcher(t *testing.T, srvURL string, mutate func(*Config)) *Fetcher {
	t.Helper()
	host := mustHost(t, srvURL)
	cfg := Config{
		Enabled:           true,
		AllowHosts:        []string{host},
		MaxBytes:          1 << 20,
		Timeout:           5 * time.Second,
		MaxAttempts:       3,
		AllowedMediaTypes: []string{"video/mp4", "image/png"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	f, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.allowScheme = "http" // httptest is http
	f.ipPolicy = func(netip.Addr) error { return nil }
	return f
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	h, _, err := net.SplitHostPort(strings.TrimPrefix(raw, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func serveBytes(t *testing.T, contentType string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchWritesFileAndCleansUp(t *testing.T) {
	want := []byte("fake mp4 bytes")
	srv := serveBytes(t, "video/mp4", want)
	f := testFetcher(t, srv.URL, nil)
	dir := t.TempDir()

	path, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("body: got %q want %q", got, want)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup must remove the file, stat err = %v", err)
	}
}

func TestFetchRejectsRedirect(t *testing.T) {
	final := serveBytes(t, "video/mp4", []byte("x"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/clip.mp4", http.StatusFound)
	}))
	defer srv.Close()

	f := testFetcher(t, srv.URL, nil)
	_, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", t.TempDir())
	defer cleanup()
	if err == nil {
		t.Fatal("redirect must not be followed")
	}
}

func TestFetchEnforcesSizeCap(t *testing.T) {
	srv := serveBytes(t, "video/mp4", make([]byte, 2048))
	f := testFetcher(t, srv.URL, func(c *Config) { c.MaxBytes = 1024 })
	dir := t.TempDir()

	path, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", dir)
	defer cleanup()
	if err == nil {
		t.Fatal("oversize body must be rejected")
	}
	if path != "" {
		if _, statErr := os.Stat(path); statErr == nil {
			t.Error("partial file must not survive an oversize failure")
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("temp dir must be empty after failure, got %v", entries)
	}
}

func TestFetchIgnoresLyingContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "10") // lie: far more follows
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	f := testFetcher(t, srv.URL, func(c *Config) { c.MaxBytes = 1024 })
	_, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", t.TempDir())
	defer cleanup()
	if err == nil {
		t.Fatal("cap must be enforced on bytes read, not on Content-Length")
	}
}

func TestFetchRejectsDisallowedContentType(t *testing.T) {
	srv := serveBytes(t, "text/html", []byte("<html>"))
	f := testFetcher(t, srv.URL, nil)
	_, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", t.TempDir())
	defer cleanup()
	if err == nil {
		t.Fatal("text/html must be rejected")
	}
}

func TestFetchAcceptsContentTypeWithParameters(t *testing.T) {
	srv := serveBytes(t, "video/mp4; charset=binary", []byte("x"))
	f := testFetcher(t, srv.URL, nil)
	_, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", t.TempDir())
	defer cleanup()
	if err != nil {
		t.Errorf("media type parameters must be ignored: %v", err)
	}
}

func TestFetchRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	f := testFetcher(t, srv.URL, nil)
	_, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", t.TempDir())
	defer cleanup()
	if err != nil {
		t.Fatalf("want success after retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("want 2 attempts, got %d", calls.Load())
	}
}

func TestFetchTerminalOn404(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := testFetcher(t, srv.URL, nil)
	_, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", t.TempDir())
	defer cleanup()
	if err == nil {
		t.Fatal("404 must fail")
	}
	if calls.Load() != 1 {
		t.Errorf("404 is terminal: want 1 attempt, got %d", calls.Load())
	}
}

func TestFetchCapsAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	f := testFetcher(t, srv.URL, func(c *Config) { c.MaxAttempts = 2 })
	_, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", t.TempDir())
	defer cleanup()
	if err == nil {
		t.Fatal("want failure after exhausting attempts")
	}
	if calls.Load() != 2 {
		t.Errorf("want 2 attempts, got %d", calls.Load())
	}
}

func TestFetchRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(2 * time.Second) // stall mid-body
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	f := testFetcher(t, srv.URL, nil)
	dir := t.TempDir()
	_, cleanup, err := f.Fetch(ctx, srv.URL+"/clip.mp4", dir)
	defer cleanup()
	if err == nil {
		t.Fatal("cancellation mid-body must fail")
	}
	cleanup()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("partial file must be removed on cancellation, got %v", entries)
	}
}

func TestFetchDeniedAddressIsRefusedAtDial(t *testing.T) {
	srv := serveBytes(t, "video/mp4", []byte("x"))
	f := testFetcher(t, srv.URL, nil)
	// Restore the REAL policy: httptest is on loopback, which must be
	// denied even though the host is allow-listed. This is the test that
	// proves the two checks are independent.
	f.ipPolicy = DenyPrivate

	_, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", t.TempDir())
	defer cleanup()
	if err == nil {
		t.Fatal("an allow-listed host resolving to loopback must still be refused")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("want the address policy to be the stated cause, got %v", err)
	}
}

func TestFetchDNSRebinding(t *testing.T) {
	srv := serveBytes(t, "video/mp4", []byte("x"))
	f := testFetcher(t, srv.URL, nil)

	// Simulate rebinding: the name validated fine, but the address the
	// dialer is handed at connect time is the metadata service. The
	// policy runs per-connection, so it catches this.
	var seen atomic.Int32
	f.ipPolicy = func(ip netip.Addr) error {
		if seen.Add(1) == 1 {
			return nil // first lookup: benign
		}
		return DenyPrivate(netip.MustParseAddr("169.254.169.254"))
	}
	// Force a second connection by disabling keep-alive reuse.
	f.client.Transport.(*http.Transport).DisableKeepAlives = true

	if _, cleanup, err := f.Fetch(context.Background(), srv.URL+"/a.mp4", t.TempDir()); err != nil {
		cleanup()
		t.Fatalf("first fetch should succeed: %v", err)
	}
	_, cleanup, err := f.Fetch(context.Background(), srv.URL+"/b.mp4", t.TempDir())
	defer cleanup()
	if err == nil {
		t.Fatal("re-resolution to the metadata range must be refused at dial")
	}
	// The plan flags this test as vacuous if the connection is reused:
	// assert the policy actually ran a second time.
	if seen.Load() < 2 {
		t.Fatalf("ipPolicy ran %d time(s); the second connection was reused, so this test proved nothing", seen.Load())
	}
}

func TestFetchRejectsNonAllowListedHostBeforeDialing(t *testing.T) {
	f := testFetcher(t, "http://127.0.0.1:1/", nil)
	f.ipPolicy = func(netip.Addr) error {
		t.Fatal("dialer must not be reached for a non-allow-listed host")
		return nil
	}
	if _, cleanup, err := f.Fetch(context.Background(), "http://evil.example.com/x.mp4", t.TempDir()); err == nil {
		cleanup()
		t.Fatal("non-allow-listed host must be rejected")
	} else {
		cleanup()
	}
}

func TestNewRefusesEmptyAllowList(t *testing.T) {
	if _, err := New(Config{Enabled: true, AllowHosts: nil}); err == nil {
		t.Fatal("enabled with an empty allow-list must refuse construction")
	}
}

func TestFetchCleanupIsSafeToCallTwice(t *testing.T) {
	srv := serveBytes(t, "video/mp4", []byte("x"))
	f := testFetcher(t, srv.URL, nil)
	_, cleanup, err := f.Fetch(context.Background(), srv.URL+"/clip.mp4", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	cleanup() // must not panic
}

func TestFetchNilErrorNeverLeavesNilCleanup(t *testing.T) {
	f := testFetcher(t, "http://127.0.0.1:1/", nil)
	_, cleanup, _ := f.Fetch(context.Background(), "http://evil.example.com/x.mp4", t.TempDir())
	if cleanup == nil {
		t.Fatal("cleanup must be non-nil even on the earliest failure path")
	}
	cleanup()
}
