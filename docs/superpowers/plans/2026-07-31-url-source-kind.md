# URL Source Kind Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a job name a remote `https` URL instead of a local file, with the destination bounded by a fail-closed allow-list that survives DNS rebinding.

**Architecture:** A new `internal/fetch` package downloads an allow-listed URL to a job-scoped temp file. The pipeline resolves the source before analysis: everything downstream sees `kind:"file"` pointing at the local path, while the envelope, audit record, and logs carry a **redacted** URL plus a digest. `SECURITY.md` currently disables this as an SSRF vector; this implements the allow-list it names as the precondition.

**Tech Stack:** Go 1.x stdlib (`net`, `net/netip`, `net/http`, `net/url`, `crypto/sha256`), viper, `net/http/httptest`. **No new module dependency.**

## Global Constraints

Copied verbatim from `AGENTS.md` and the spec. Every task's requirements implicitly include these.

- Done gate: `go build ./... && go vet ./... && go test ./...` all exit 0.
- Full suite runs with NO network and NO credentials. Every test uses `httptest` on loopback.
- **Never `allow` on failure.** Every fetch failure yields `verdict:"error"` + dead-letter. Verdict precedence stays `block > error > flag > allow`.
- **Never persist or transmit media downstream of analysis.** The download is transient and deleted on every exit path before ack.
- **Secrets are env-only.** A presigned URL's query string is a credential and must never reach an envelope, log, audit record, or metric label.
- **No shell in the extraction path.** FFmpeg receives the local path. A URL must never appear in an ffmpeg argument — invariant 5's protocol deny-list stays untouched.
- Existing rollup tests must pass UNMODIFIED.
- Feature is OFF by default; every existing config keeps working with no change.
- `deferred cleanup` contract: cleanup is deferred immediately after the call that creates the resource, and runs on every exit path (error, ctx-cancel, panic) before ack.

## File Structure

| Path | Responsibility |
|---|---|
| `internal/fetch/fetch.go` (create) | `Fetcher` — HTTP client construction, the download, retry loop. |
| `internal/fetch/validate.go` (create) | URL parse rules (scheme, userinfo, host allow-list) and `Redact`. Pure functions, no I/O. |
| `internal/fetch/ippolicy.go` (create) | The denied-range policy. Pure function over `netip.Addr`. |
| `internal/fetch/validate_test.go`, `ippolicy_test.go`, `fetch_test.go` (create) | One test file per unit above. |
| `internal/config/config.go` (modify) | `URLSourceConfig`, `SourceConfig`, defaults, validation. |
| `pkg/moderation/types.go` (modify) | `Source.RefDigest`; `SchemaVersion` → `1.2.0`. |
| `internal/pipeline/pipeline.go` (modify) | `SourceFetcher` field + `resolveSource`. |
| `internal/pipeline/source_test.go` (create) | Resolution and fail-safe tests. |
| `internal/cli/serve.go` (modify) | Intake validation of `kind:"url"`; construct the `Fetcher`. |
| `internal/cli/wire.go` (modify) | Pass the fetcher into the pipeline. |
| `internal/observe/observe.go` (modify) | Fetch metrics. |

---

### Task 1: The IP deny-list policy

Start here: it is pure, has no dependencies, and is the control everything else leans on.

**Files:**
- Create: `internal/fetch/ippolicy.go`
- Test: `internal/fetch/ippolicy_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `fetch.DenyPrivate(ip netip.Addr) error`. Task 3 uses it as the default `ipPolicy`.

- [ ] **Step 1: Write the failing test**

```go
package fetch

import (
	"net/netip"
	"testing"
)

func TestDenyPrivate(t *testing.T) {
	for _, tc := range []struct {
		addr    string
		allowed bool
		why     string
	}{
		// Allowed: ordinary public addresses.
		{"8.8.8.8", true, "public v4"},
		{"1.1.1.1", true, "public v4"},
		{"93.184.216.34", true, "public v4"},
		{"2606:4700:4700::1111", true, "public v6"},

		// Loopback.
		{"127.0.0.1", false, "v4 loopback"},
		{"127.1.2.3", false, "v4 loopback range"},
		{"::1", false, "v6 loopback"},

		// RFC 1918.
		{"10.0.0.1", false, "rfc1918 10/8"},
		{"172.16.0.1", false, "rfc1918 172.16/12"},
		{"172.31.255.254", false, "rfc1918 172.16/12 upper"},
		{"192.168.1.1", false, "rfc1918 192.168/16"},

		// Cloud metadata — the range where a miss becomes credential theft.
		{"169.254.169.254", false, "aws/gcp metadata"},
		{"169.254.0.1", false, "v4 link-local"},
		{"fe80::1", false, "v6 link-local"},

		// IPv6 ULA.
		{"fc00::1", false, "v6 unique local"},
		{"fd12:3456::1", false, "v6 unique local"},

		// Unspecified, multicast, CGNAT.
		{"0.0.0.0", false, "unspecified v4"},
		{"::", false, "unspecified v6"},
		{"224.0.0.1", false, "v4 multicast"},
		{"ff02::1", false, "v6 multicast"},
		{"100.64.0.1", false, "cgnat 100.64/10"},
		{"100.127.255.255", false, "cgnat upper"},

		// v4-mapped v6 must not smuggle a private v4 past the check.
		{"::ffff:127.0.0.1", false, "v4-mapped loopback"},
		{"::ffff:10.0.0.1", false, "v4-mapped rfc1918"},
		{"::ffff:169.254.169.254", false, "v4-mapped metadata"},
	} {
		t.Run(tc.addr+" "+tc.why, func(t *testing.T) {
			ip, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("bad test address: %v", err)
			}
			err = DenyPrivate(ip)
			if tc.allowed && err != nil {
				t.Errorf("%s (%s) must be allowed, got %v", tc.addr, tc.why, err)
			}
			if !tc.allowed && err == nil {
				t.Errorf("%s (%s) must be denied, got nil", tc.addr, tc.why)
			}
		})
	}
}

func TestDenyPrivateRejectsInvalidAddr(t *testing.T) {
	if err := DenyPrivate(netip.Addr{}); err == nil {
		t.Fatal("zero Addr must be denied, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/fetch/ -run TestDenyPrivate -v`
Expected: FAIL — package does not exist / `DenyPrivate` undefined.

- [ ] **Step 3: Implement the policy**

```go
// Package fetch downloads allow-listed remote media to local files.
//
// The destination is chosen by an untrusted job payload, so every control
// here is fail-closed: the feature is off by default, an empty allow-list
// refuses to boot, and the address policy is enforced against the address
// actually dialed rather than the hostname parsed.
package fetch

import (
	"fmt"
	"net/netip"
)

// cgnat is RFC 6598 shared address space. netip has no predicate for it,
// and it routes to carrier infrastructure, so it is denied explicitly.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// DenyPrivate is the default address policy for media-source URLs.
//
// It is deliberately a deny-list of everything non-public rather than an
// allow-list of public ranges: a new special-use range added by IANA
// should fail closed here only if it is also non-routable, and the
// predicates below track the stdlib's view of that.
//
// Addr is unmapped first so a v4-mapped v6 address (::ffff:127.0.0.1)
// cannot smuggle a private v4 past the v4 predicates.
func DenyPrivate(ip netip.Addr) error {
	if !ip.IsValid() {
		return fmt.Errorf("fetch: invalid address")
	}
	ip = ip.Unmap()
	switch {
	case ip.IsLoopback():
		return fmt.Errorf("fetch: %s is loopback", ip)
	case ip.IsPrivate():
		// Covers RFC 1918 and IPv6 ULA (fc00::/7).
		return fmt.Errorf("fetch: %s is a private address", ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10 — the cloud metadata range.
		return fmt.Errorf("fetch: %s is link-local (cloud metadata range)", ip)
	case ip.IsUnspecified():
		return fmt.Errorf("fetch: %s is the unspecified address", ip)
	case ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("fetch: %s is multicast", ip)
	case cgnat.Contains(ip):
		return fmt.Errorf("fetch: %s is CGNAT shared address space", ip)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/fetch/ -run TestDenyPrivate -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/fetch/ippolicy.go internal/fetch/ippolicy_test.go
git commit -m "fetch: add the media-source address deny-list"
```

---

### Task 2: URL validation and redaction

**Files:**
- Create: `internal/fetch/validate.go`
- Test: `internal/fetch/validate_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `fetch.ValidateURL(raw string, allowHosts map[string]bool) (*url.URL, error)`
  - `fetch.Redact(raw string) (ref string, digest string)`

  Task 3 calls `ValidateURL`; Task 5 calls `Redact`.

- [ ] **Step 1: Write the failing test**

```go
package fetch

import (
	"strings"
	"testing"
)

func allowed(hosts ...string) map[string]bool {
	m := map[string]bool{}
	for _, h := range hosts {
		m[h] = true
	}
	return m
}

func TestValidateURLAcceptsAllowListedHTTPS(t *testing.T) {
	u, err := ValidateURL("https://media.example.com/clip.mp4", allowed("media.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "media.example.com" {
		t.Errorf("host: got %q", u.Host)
	}
}

func TestValidateURLHostMatchIsCaseInsensitive(t *testing.T) {
	if _, err := ValidateURL("https://Media.Example.COM/clip.mp4", allowed("media.example.com")); err != nil {
		t.Errorf("host comparison must be case-insensitive: %v", err)
	}
}

func TestValidateURLAllowListIgnoresPort(t *testing.T) {
	if _, err := ValidateURL("https://media.example.com:8443/clip.mp4", allowed("media.example.com")); err != nil {
		t.Errorf("allow-list matches hostname, not host:port: %v", err)
	}
}

func TestValidateURLNegativeCases(t *testing.T) {
	hosts := allowed("media.example.com")
	for name, raw := range map[string]string{
		"http scheme":        "http://media.example.com/clip.mp4",
		"file scheme":        "file:///etc/passwd",
		"ftp scheme":         "ftp://media.example.com/clip.mp4",
		"no scheme":          "media.example.com/clip.mp4",
		"userinfo":           "https://user:pw@media.example.com/clip.mp4",
		"userinfo no pass":   "https://user@media.example.com/clip.mp4",
		"host not allowed":   "https://evil.example.com/clip.mp4",
		"empty host":         "https:///clip.mp4",
		"ip literal denied":  "https://169.254.169.254/clip.mp4",
		"subdomain not list": "https://sub.media.example.com/clip.mp4",
		"unparseable":        "https://exa mple.com/\x7f",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateURL(raw, hosts); err == nil {
				t.Fatalf("%q must be rejected, got nil error", raw)
			}
		})
	}
}

func TestValidateURLEmptyAllowListRejectsEverything(t *testing.T) {
	if _, err := ValidateURL("https://media.example.com/clip.mp4", map[string]bool{}); err == nil {
		t.Fatal("an empty allow-list must reject every host")
	}
}

func TestRedactDropsQueryAndFragmentAndUserinfo(t *testing.T) {
	raw := "https://bucket.s3.amazonaws.com/clip.mp4?X-Amz-Signature=deadbeef&X-Amz-Expires=900#t=10"
	ref, digest := Redact(raw)

	if strings.Contains(ref, "deadbeef") || strings.Contains(ref, "X-Amz") {
		t.Errorf("presigned credential leaked into ref: %q", ref)
	}
	if strings.Contains(ref, "#") {
		t.Errorf("fragment retained: %q", ref)
	}
	if ref != "https://bucket.s3.amazonaws.com/clip.mp4" {
		t.Errorf("ref: got %q", ref)
	}
	if len(digest) != 64 {
		t.Errorf("digest must be hex sha256 (64 chars), got %d: %q", len(digest), digest)
	}
}

func TestRedactDigestCoversTheFullURL(t *testing.T) {
	a, da := Redact("https://h/x.mp4?sig=1")
	b, db := Redact("https://h/x.mp4?sig=2")
	if a != b {
		t.Fatalf("refs should match: %q vs %q", a, b)
	}
	if da == db {
		t.Error("digest must distinguish URLs that differ only in the query")
	}
}

func TestRedactUnparseableStillProducesDigest(t *testing.T) {
	ref, digest := Redact("https://exa mple.com/\x7f")
	if len(digest) != 64 {
		t.Errorf("digest must always be produced, got %q", digest)
	}
	if strings.Contains(ref, " ") {
		t.Errorf("unparseable input must not be echoed back: %q", ref)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/fetch/ -run 'TestValidateURL|TestRedact' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

```go
package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// ValidateURL applies the parse-time rules for a media-source URL.
//
// It deliberately does NOT check the destination address: a hostname can
// re-resolve between this call and the socket connect. Address policy is
// enforced per-connection in the dialer (see Fetcher). This function only
// rejects what is decidable from the text.
func ValidateURL(raw string, allowHosts map[string]bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("fetch: url is not parseable")
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("fetch: url scheme must be https, got %q", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("fetch: url must not contain userinfo — credentials are env-only")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("fetch: url has no host")
	}
	if !allowHosts[host] {
		return nil, fmt.Errorf("fetch: host %q is not in source.url.allow_hosts", host)
	}
	return u, nil
}

// Redact splits a media URL into the part safe to record and a digest of
// the whole.
//
// A presigned URL carries its authorization in the query string, so the
// query, fragment, and userinfo are dropped before the value goes
// anywhere durable. The digest is over the FULL original so a verdict is
// still traceable to the exact request without storing the credential.
//
// An unparseable URL yields an empty ref (never the raw input echoed
// back) and a digest, so a malformed value cannot smuggle itself into a
// log line.
func Redact(raw string) (ref string, digest string) {
	sum := sha256.Sum256([]byte(raw))
	digest = hex.EncodeToString(sum[:])

	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", digest
	}
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	u.User = nil
	return u.String(), digest
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/fetch/ -run 'TestValidateURL|TestRedact' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/fetch/validate.go internal/fetch/validate_test.go
git commit -m "fetch: add url parse rules and presigned-url redaction"
```

---

### Task 3: The Fetcher

**Files:**
- Create: `internal/fetch/fetch.go`
- Test: `internal/fetch/fetch_test.go`
- Modify: `internal/moderate/httpx.go` (export two helpers)

**Interfaces:**
- Consumes: `fetch.DenyPrivate` (Task 1), `fetch.ValidateURL` (Task 2), `moderate.RetryableStatus`, `moderate.RetryAfter`.
- Produces:
  - `fetch.Config{Enabled bool; AllowHosts []string; MaxBytes int64; Timeout time.Duration; MaxAttempts int; AllowedMediaTypes []string}`
  - `fetch.New(cfg Config) (*Fetcher, error)`
  - `(*Fetcher).Fetch(ctx context.Context, rawURL, dir string) (path string, cleanup func(), err error)`

  Task 6 calls `New` and `Fetch`.

**Why not `moderate.DoJSON`:** it buffers the whole response into `[]byte`. A 256 MiB video must stream to disk. This task writes its own attempt loop but reuses `moderate`'s classification so retry policy still has one definition.

- [ ] **Step 1: Export the two classification helpers**

In `internal/moderate/httpx.go`, rename `retryableStatus` → `RetryableStatus` and `retryAfter` → `RetryAfter`, updating their call sites inside `DoJSON`. Add doc comments:

```go
// RetryableStatus reports whether an HTTP status is transient (F.4):
// 429 and 5xx retry, every other 4xx is terminal.
func RetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// RetryAfter returns a usable Retry-After delay, or 0. Values above 120s
// are ignored so a hostile or broken header cannot stall a worker.
func RetryAfter(resp *http.Response) time.Duration {
```

Run: `go build ./... && go test ./internal/moderate/...`
Expected: PASS — pure rename, no behavior change.

- [ ] **Step 2: Write the failing test**

```go
package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
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
	f.allowScheme = "http"                        // httptest is http
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

var _ = filepath.Join // keep import if unused after edits
var _ = errors.Is
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/fetch/ -run TestFetch -v`
Expected: FAIL — `New`, `Config`, `Fetcher` undefined.

- [ ] **Step 4: Implement the Fetcher**

```go
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

const (
	defaultMaxBytes    = 256 << 20 // 256 MiB
	defaultTimeout     = 60 * time.Second
	defaultMaxAttempts = 3
	baseBackoff        = 500 * time.Millisecond
	dialTimeout        = 10 * time.Second
)

// Config is the source.url block.
type Config struct {
	Enabled           bool
	AllowHosts        []string
	MaxBytes          int64
	Timeout           time.Duration
	MaxAttempts       int
	AllowedMediaTypes []string
}

// Fetcher downloads allow-listed media URLs to local files.
type Fetcher struct {
	cfg        Config
	allowHosts map[string]bool
	allowTypes map[string]bool
	client     *http.Client

	// allowScheme is "https" in production. Tests set it to "http" so
	// httptest servers are reachable; it is not settable from config.
	allowScheme string

	// ipPolicy runs per-connection against the address actually dialed.
	// This is the DNS-rebinding defense: a name that validated at parse
	// time cannot re-resolve into a denied range without hitting this.
	ipPolicy func(netip.Addr) error
}

// terminalErr marks a failure that must not be retried.
type terminalErr struct{ err error }

func (e terminalErr) Error() string { return e.err.Error() }
func (e terminalErr) Unwrap() error { return e.err }

func terminal(format string, a ...any) error {
	return terminalErr{fmt.Errorf(format, a...)}
}

// New builds a Fetcher. It refuses an enabled config with no allow-list:
// that combination cannot fetch anything anyway, and accepting it would
// make "enabled" look meaningful when it is not.
func New(cfg Config) (*Fetcher, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if len(cfg.AllowHosts) == 0 {
		return nil, fmt.Errorf("config: source.url.enabled=true requires at least one entry in source.url.allow_hosts")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}

	f := &Fetcher{
		cfg:         cfg,
		allowHosts:  lowerSet(cfg.AllowHosts),
		allowTypes:  lowerSet(cfg.AllowedMediaTypes),
		allowScheme: "https",
		ipPolicy:    DenyPrivate,
	}

	dialer := &net.Dialer{Timeout: dialTimeout}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("fetch: unparseable dial address %q", address)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("fetch: dial address %q is not an IP", host)
		}
		return f.ipPolicy(ip)
	}
	f.client = &http.Client{
		Timeout:   cfg.Timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A redirect is a destination vismod did not choose.
			return errors.New("fetch: redirects are not followed")
		},
	}
	return f, nil
}

func lowerSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		m[strings.ToLower(strings.TrimSpace(s))] = true
	}
	return m
}

// Fetch downloads rawURL into dir.
//
// cleanup is ALWAYS non-nil and is safe to call more than once. Defer it
// immediately, on every exit path, before ack — the same contract as
// FrameSource.Frames.
func (f *Fetcher) Fetch(ctx context.Context, rawURL, dir string) (string, func(), error) {
	var once sync.Once
	path := filepath.Join(dir, "source"+extOf(rawURL))
	cleanup := func() { once.Do(func() { _ = os.Remove(path) }) }

	u, err := ValidateURL(rawURL, f.allowHosts)
	if err != nil {
		return "", cleanup, terminalErr{err}
	}
	if u.Scheme != f.allowScheme {
		return "", cleanup, terminal("fetch: url scheme must be %s, got %q", f.allowScheme, u.Scheme)
	}

	var lastErr error
	for attempt := 1; attempt <= f.cfg.MaxAttempts; attempt++ {
		err := f.attempt(ctx, u.String(), path)
		if err == nil {
			return path, cleanup, nil
		}
		cleanup() // never leave a partial file between attempts
		once = sync.Once{}
		var te terminalErr
		if errors.As(err, &te) {
			return "", cleanup, err
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", cleanup, ctx.Err()
		}
		if attempt < f.cfg.MaxAttempts {
			select {
			case <-ctx.Done():
				return "", cleanup, ctx.Err()
			case <-time.After(baseBackoff * time.Duration(1<<(attempt-1))):
			}
		}
	}
	return "", cleanup, moderation.Retryable(fmt.Errorf("fetch: after %d attempts: %w", f.cfg.MaxAttempts, lastErr))
}

func (f *Fetcher) attempt(ctx context.Context, rawURL, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return terminalErr{err}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		// Dialer.Control rejections and redirect refusals arrive here.
		// Both are terminal: retrying cannot change the destination.
		if strings.Contains(err.Error(), "fetch: ") {
			return terminalErr{err}
		}
		return err // transport error: retryable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if !moderate.RetryableStatus(resp.StatusCode) {
			return terminal("fetch: %s returned %d", redactForError(rawURL), resp.StatusCode)
		}
		if ra := moderate.RetryAfter(resp); ra > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(ra):
			}
		}
		return fmt.Errorf("fetch: %s returned %d", redactForError(rawURL), resp.StatusCode)
	}

	if err := f.checkMediaType(resp.Header.Get("Content-Type")); err != nil {
		return err
	}

	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return terminalErr{fmt.Errorf("fetch: create %s: %w", path, err)}
	}
	defer out.Close()

	// Read ONE byte past the cap so an exactly-at-cap body still succeeds
	// while an oversize one is detectable. Content-Length is never trusted.
	n, err := io.Copy(out, io.LimitReader(resp.Body, f.cfg.MaxBytes+1))
	if err != nil {
		return err // transport/context failure: retryable
	}
	if n > f.cfg.MaxBytes {
		return terminal("fetch: body exceeds source.url.max_bytes (%d)", f.cfg.MaxBytes)
	}
	return nil
}

func (f *Fetcher) checkMediaType(header string) error {
	if len(f.allowTypes) == 0 {
		return nil
	}
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return terminal("fetch: unparseable Content-Type")
	}
	if !f.allowTypes[strings.ToLower(mt)] {
		return terminal("fetch: Content-Type %q is not in source.url.allowed_media_types", mt)
	}
	return nil
}

// redactForError keeps a query string out of an error string, which ends
// up in the envelope's Error field and in logs.
func redactForError(raw string) string {
	ref, _ := Redact(raw)
	return ref
}

// extOf preserves a recognizable extension so ffprobe's container sniffing
// has the usual hint. It never trusts the value for anything else.
func extOf(raw string) string {
	ref, _ := Redact(raw)
	ext := filepath.Ext(ref)
	if len(ext) > 5 || strings.ContainsAny(ext, `/\`) {
		return ""
	}
	return ext
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/fetch/ -v`
Expected: PASS.

- [ ] **Step 6: Run the race detector**

Run: `go test -race ./internal/fetch/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/fetch/fetch.go internal/fetch/fetch_test.go internal/moderate/httpx.go
git commit -m "fetch: add the allow-listed media fetcher with per-dial address policy"
```

---

### Task 4: Config surface

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing (mirrors `fetch.Config` but is the viper-facing type).
- Produces: `config.URLSourceConfig`, `config.SourceConfig`, `Config.Source`. Task 6 maps it to `fetch.Config`.

- [ ] **Step 1: Write the failing test**

```go
func TestSourceURLDefaultsDisabled(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, "ffmpeg:\n  max_frames: 8\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.URL.Enabled {
		t.Error("url sources must be OFF by default")
	}
}

func TestSourceURLParses(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
source:
  url:
    enabled: true
    allow_hosts:
      - media.example.com
    max_bytes: 1048576
    timeout: 30s
    max_attempts: 5
    allowed_media_types:
      - video/mp4
`))
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Source.URL
	if !u.Enabled || len(u.AllowHosts) != 1 || u.MaxBytes != 1048576 ||
		u.Timeout != 30*time.Second || u.MaxAttempts != 5 || len(u.AllowedMediaTypes) != 1 {
		t.Errorf("parsed wrong: %+v", u)
	}
}

func TestSourceURLEnabledWithoutAllowHostsRefusesBoot(t *testing.T) {
	_, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
source:
  url:
    enabled: true
`))
	if err == nil {
		t.Fatal("enabled with no allow_hosts must refuse to boot")
	}
	if !strings.Contains(err.Error(), "allow_hosts") {
		t.Errorf("error must name the offending key, got %v", err)
	}
}

func TestSourceURLNegativeNumbersRefuseBoot(t *testing.T) {
	for name, body := range map[string]string{
		"negative max_bytes": "    max_bytes: -1\n",
		"negative timeout":   "    timeout: -5s\n",
	} {
		t.Run(name, func(t *testing.T) {
			y := "ffmpeg:\n  max_frames: 8\nsource:\n  url:\n    enabled: true\n    allow_hosts: [media.example.com]\n" + body
			if _, err := Load(writeTempYAML(t, y)); err == nil {
				t.Fatal("want boot refusal")
			}
		})
	}
}

func TestConfigHashIgnoresSourceAndOutput(t *testing.T) {
	th := Thresholds{"default": {FlagAt: f64(0.5), BlockAt: f64(0.8)}}
	// ConfigHash takes only adapter, model version, and thresholds — so
	// changing fetch or sink settings CANNOT perturb it. This test exists
	// so a future signature change cannot silently make every previously
	// written envelope incomparable.
	a := ConfigHash("microsoft", "v1", th)
	b := ConfigHash("microsoft", "v1", th)
	if a != b {
		t.Fatal("hash is not stable")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestSourceURL -v`
Expected: FAIL — `cfg.Source` undefined.

- [ ] **Step 3: Add the types and validation**

In `internal/config/config.go`, after `FramesConfig`:

```go
// URLSourceConfig enables jobs whose Source.Kind is "url".
//
// OFF by default. The destination is chosen by an untrusted job payload,
// so this is the one config block where every omission fails closed:
// enabling it without an allow-list refuses to boot, and there is no
// switch that disables the address deny-list.
type URLSourceConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// AllowHosts are exact hostnames (no wildcards, no suffix matching:
	// "example.com" does NOT permit "evil.example.com").
	AllowHosts        []string      `mapstructure:"allow_hosts"`
	MaxBytes          int64         `mapstructure:"max_bytes"`
	Timeout           time.Duration `mapstructure:"timeout"`
	MaxAttempts       int           `mapstructure:"max_attempts"`
	AllowedMediaTypes []string      `mapstructure:"allowed_media_types"`
}

type SourceConfig struct {
	URL URLSourceConfig `mapstructure:"url"`
}
```

Add to `Config`, after `Frames`:

```go
	Source       SourceConfig       `mapstructure:"source"`
```

Add to `Defaults()`:

```go
		Source: SourceConfig{URL: URLSourceConfig{
			Enabled:     false,
			MaxBytes:    256 << 20,
			Timeout:     60 * time.Second,
			MaxAttempts: 3,
			AllowedMediaTypes: []string{
				"video/mp4", "video/webm", "video/quicktime",
				"image/jpeg", "image/png", "image/webp",
			},
		}},
```

Add to `Validate`, before the final `return nil`:

```go
	if err := validateURLSource(cfg.Source.URL); err != nil {
		return err
	}
```

```go
func validateURLSource(u URLSourceConfig) error {
	if !u.Enabled {
		return nil
	}
	if len(u.AllowHosts) == 0 {
		return fmt.Errorf("config: source.url.enabled=true requires at least one entry in source.url.allow_hosts — an empty allow-list can fetch nothing, and accepting it would make \"enabled\" misleading")
	}
	for i, h := range u.AllowHosts {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("config: source.url.allow_hosts[%d] is empty", i)
		}
		if strings.ContainsAny(h, "*/") {
			return fmt.Errorf("config: source.url.allow_hosts[%d] = %q — hostnames are matched exactly; wildcards and paths are not supported", i, h)
		}
	}
	if u.MaxBytes < 0 {
		return fmt.Errorf("config: source.url.max_bytes must be >= 0, got %d", u.MaxBytes)
	}
	if u.Timeout < 0 {
		return fmt.Errorf("config: source.url.timeout must be >= 0, got %s", u.Timeout)
	}
	if u.MaxAttempts < 0 {
		return fmt.Errorf("config: source.url.max_attempts must be >= 0, got %d", u.MaxAttempts)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -run 'TestSourceURL|TestConfigHash' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/config/config.go internal/config/config_test.go
git commit -m "config: add source.url block, off by default and fail-closed"
```

---

### Task 5: Source.RefDigest and the schema bump

**Files:**
- Modify: `pkg/moderation/types.go`
- Test: `internal/result/sink_test.go` (add a case)

**Interfaces:**
- Consumes: nothing.
- Produces: `moderation.Source.RefDigest string`. Task 6 populates it.

- [ ] **Step 1: Write the failing test**

Add to `internal/result/sink_test.go`:

```go
func TestURLSourceSerializesRedactedRefAndDigest(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONLSink(&buf)
	err := s.Write(context.Background(), ResultEnvelope{
		JobID: "job-url",
		Source: moderation.Source{
			Kind:      "url",
			Ref:       "https://bucket.s3.amazonaws.com/clip.mp4",
			RefDigest: "abc123",
			MediaType: "video",
		},
		Result: &moderation.NormalizedResult{
			Overall: moderation.OverallVerdict{Verdict: moderation.VerdictError},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"ref_digest":"abc123"`) {
		t.Errorf("ref_digest missing: %s", out)
	}
	if strings.Contains(out, "X-Amz") {
		t.Errorf("query string leaked: %s", out)
	}
}

func TestFileSourceOmitsRefDigest(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONLSink(&buf)
	_ = s.Write(context.Background(), ResultEnvelope{
		JobID:  "job-file",
		Source: moderation.Source{Kind: "file", Ref: "x.png", MediaType: "image"},
		Result: &moderation.NormalizedResult{},
	})
	if strings.Contains(buf.String(), "ref_digest") {
		t.Errorf("ref_digest must be omitted for file sources: %s", buf.String())
	}
}

func TestSchemaVersionIsBumped(t *testing.T) {
	if moderation.SchemaVersion != "1.2.0" {
		t.Errorf("SchemaVersion = %q, want 1.2.0 after the additive ref_digest field", moderation.SchemaVersion)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/result/ -run 'RefDigest|SchemaVersion' -v`
Expected: FAIL — unknown field `RefDigest`.

- [ ] **Step 3: Add the field and bump the version**

In `pkg/moderation/types.go`, replace the `Source` struct:

```go
// Source identifies an input asset.
//
// Kind is "file" or "url". For a "url" source, Ref carries only
// scheme+host+path: a presigned URL's query string is a CREDENTIAL, and
// Ref reaches the result envelope, the audit record, and structured logs.
// RefDigest is SHA-256 of the FULL original URL, so a verdict stays
// traceable to the exact request without storing that credential.
//
// RefDigest is empty (and omitted) for file sources.
type Source struct {
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	RefDigest string `json:"ref_digest,omitempty"`
	MediaType string `json:"media_type"` // "image" | "video"
}
```

Update the `SchemaVersion` comment block and value:

```go
// 1.2.0: added Source.RefDigest for url-kind sources (additive field
// only; no field or meaning changed). Source is serialized into
// result.ResultEnvelope rather than NormalizedResult, and the envelope
// carries no version of its own, so this constant is the only version
// signal consumers have for that change.
const SchemaVersion = "1.2.0"
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/result/ -v`
Expected: PASS.

- [ ] **Step 5: Check for golden-file churn**

Run: `go test ./internal/moderate/...`
Expected: PASS. If adapter goldens embed `schema_version`, they will fail — regenerate with `go test -update ./internal/moderate/...` and inspect the diff. It must show ONLY the version string changing. Any other change means something unintended moved.

- [ ] **Step 6: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add pkg/moderation/types.go internal/result/sink_test.go internal/moderate/
git commit -m "moderation: add Source.RefDigest and bump SchemaVersion to 1.2.0"
```

---

### Task 6: Pipeline source resolution

**Files:**
- Modify: `internal/pipeline/pipeline.go`
- Create: `internal/pipeline/source_test.go`

**Interfaces:**
- Consumes: `fetch.Fetcher` (Task 3), `fetch.Redact` (Task 2), `moderation.Source.RefDigest` (Task 5).
- Produces: `pipeline.SourceFetcher` interface and `Pipeline.Fetcher` field. Task 7 wires it.

**The key design point:** two different `Source` values exist per job. Analysis sees a `kind:"file"` source pointing at the local download. The envelope, audit record, and logs see the redacted original. Conflating them either leaks the credential or records a meaningless temp path.

- [ ] **Step 1: Write the failing test**

```go
package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

type stubFetcher struct {
	path    string
	err     error
	gotURL  string
	cleaned bool
}

func (s *stubFetcher) Fetch(_ context.Context, rawURL, _ string) (string, func(), error) {
	s.gotURL = rawURL
	return s.path, func() { s.cleaned = true }, s.err
}

func urlJob() queue.Job {
	return queue.Job{
		ID: "job-url",
		Source: moderation.Source{
			Kind:      "url",
			Ref:       "https://media.example.com/clip.mp4?sig=secret",
			MediaType: "image",
		},
	}
}

func TestURLSourceIsFetchedAndRewrittenForAnalysis(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "source.png")
	if err := os.WriteFile(local, pngFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	sf := &stubFetcher{path: local}
	p := newTestPipeline(t)
	p.Fetcher = sf

	env, disp, _ := p.ProcessJob(context.Background(), urlJob())

	if sf.gotURL != "https://media.example.com/clip.mp4?sig=secret" {
		t.Errorf("fetcher must receive the FULL url: %q", sf.gotURL)
	}
	if !sf.cleaned {
		t.Error("cleanup must run before the job finishes")
	}
	if env.Source.Kind != "url" {
		t.Errorf("envelope must record the original kind, got %q", env.Source.Kind)
	}
	if strings.Contains(env.Source.Ref, "secret") {
		t.Errorf("credential leaked into the envelope: %q", env.Source.Ref)
	}
	if env.Source.Ref != "https://media.example.com/clip.mp4" {
		t.Errorf("envelope ref: got %q", env.Source.Ref)
	}
	if len(env.Source.RefDigest) != 64 {
		t.Errorf("ref_digest must be set: %q", env.Source.RefDigest)
	}
	if env.Result.AssetID != "https://media.example.com/clip.mp4" {
		t.Errorf("asset_id must be the redacted url, not the temp path: %q", env.Result.AssetID)
	}
	if disp != queue.Ack {
		t.Errorf("disposition: got %v want Ack", disp)
	}
}

func TestFetchFailureIsErrorVerdictNeverAllow(t *testing.T) {
	sf := &stubFetcher{err: errors.New("host not in allow-list")}
	p := newTestPipeline(t)
	p.Fetcher = sf

	env, disp, _ := p.ProcessJob(context.Background(), urlJob())

	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("verdict = %q, want error — a fetch failure must NEVER allow", env.Result.Overall.Verdict)
	}
	if disp != queue.DeadLetter {
		t.Errorf("disposition = %v, want DeadLetter", disp)
	}
	if !sf.cleaned {
		t.Error("cleanup must run even when the fetch failed")
	}
	if strings.Contains(env.Error, "secret") {
		t.Errorf("credential leaked into the error string: %q", env.Error)
	}
	if strings.Contains(env.Source.Ref, "secret") {
		t.Errorf("credential leaked into the envelope source: %q", env.Source.Ref)
	}
}

func TestURLSourceWithNoFetcherIsErrorVerdict(t *testing.T) {
	p := newTestPipeline(t)
	p.Fetcher = nil // source.url.enabled=false

	env, disp, _ := p.ProcessJob(context.Background(), urlJob())

	if env.Result.Overall.Verdict != moderation.VerdictError {
		t.Errorf("verdict = %q, want error", env.Result.Overall.Verdict)
	}
	if disp != queue.DeadLetter {
		t.Errorf("disposition = %v, want DeadLetter", disp)
	}
	if !strings.Contains(env.Error, "not enabled") {
		t.Errorf("error should explain the feature is off: %q", env.Error)
	}
}

func TestFileSourceIsUnaffected(t *testing.T) {
	sf := &stubFetcher{err: errors.New("must not be called")}
	p := newTestPipeline(t)
	p.Fetcher = sf

	dir := t.TempDir()
	local := filepath.Join(dir, "x.png")
	if err := os.WriteFile(local, pngFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	j := queue.Job{ID: "job-file", Source: moderation.Source{Kind: "file", Ref: local, MediaType: "image"}}

	env, _, _ := p.ProcessJob(context.Background(), j)
	if sf.gotURL != "" {
		t.Error("a file source must not touch the fetcher")
	}
	if env.Source.Ref != local {
		t.Errorf("file ref must pass through unchanged: %q", env.Source.Ref)
	}
	if env.Source.RefDigest != "" {
		t.Error("file sources carry no ref_digest")
	}
}
```

**Implementer note:** `newTestPipeline` and `pngFixture` — reuse whatever the existing `internal/pipeline/pipeline_test.go` already provides (it has a `fakeModerator` and image fixtures). Read that file first and use its helpers rather than adding parallel ones. If the helper names differ, adapt these tests to them; do not duplicate fixtures.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pipeline/ -run 'URLSource|FetchFailure|FileSource' -v`
Expected: FAIL — `p.Fetcher` undefined.

- [ ] **Step 3: Implement resolution**

Add to the imports of `internal/pipeline/pipeline.go`: `"github.com/vismod/vismod/internal/fetch"`.

Add the interface and field:

```go
// SourceFetcher resolves a remote source URL to a local file. nil means
// url sources are disabled (source.url.enabled=false).
type SourceFetcher interface {
	Fetch(ctx context.Context, rawURL, dir string) (path string, cleanup func(), err error)
}
```

Add to the `Pipeline` struct:

```go
	// Fetcher resolves kind:"url" sources. nil disables them.
	Fetcher SourceFetcher
```

Add the resolution type and function:

```go
// resolved holds the two views of a job's source that must not be
// conflated: what ANALYSIS reads, and what gets RECORDED.
//
// For a url source those differ. Analysis needs the local download;
// the envelope, audit record, and logs must carry the redacted URL,
// because a presigned URL's query string is a credential and a temp path
// is meaningless to whoever reads the verdict later.
type resolved struct {
	local moderation.Source // kind:"file", ref = local path
	env   moderation.Source // what is recorded
}

// resolveSource materializes a job's source. cleanup is always non-nil
// and must be deferred by the caller on every exit path.
func (p *Pipeline) resolveSource(ctx context.Context, j queue.Job) (resolved, func(), error) {
	if j.Source.Kind != "url" {
		return resolved{local: j.Source, env: j.Source}, func() {}, nil
	}

	// Redact FIRST, so every return path below — including the failures —
	// reports the safe form.
	ref, digest := fetch.Redact(j.Source.Ref)
	envSrc := moderation.Source{
		Kind:      "url",
		Ref:       ref,
		RefDigest: digest,
		MediaType: j.Source.MediaType,
	}

	if p.Fetcher == nil {
		return resolved{local: envSrc, env: envSrc}, func() {},
			fmt.Errorf("url sources are not enabled (source.url.enabled=false)")
	}

	dir, err := os.MkdirTemp("", "vismod-fetch-")
	if err != nil {
		return resolved{local: envSrc, env: envSrc}, func() {},
			fmt.Errorf("fetch workdir: %w", err)
	}
	rmDir := func() {
		if err := os.RemoveAll(dir); err != nil {
			p.log().Error("fetch workdir cleanup failed", "job_id", j.ID, "err", err)
		}
	}

	path, cleanFile, err := p.Fetcher.Fetch(ctx, j.Source.Ref, dir)
	cleanup := func() {
		if cleanFile != nil {
			cleanFile()
		}
		rmDir()
	}
	if err != nil {
		return resolved{local: envSrc, env: envSrc}, cleanup, err
	}
	return resolved{
		local: moderation.Source{Kind: "file", Ref: path, MediaType: j.Source.MediaType},
		env:   envSrc,
	}, cleanup, nil
}
```

Now modify `ProcessJob`. Replace the opening of the function (currently lines 100–113) with:

```go
	started := time.Now().UTC()

	rs, cleanupSource, resolveErr := p.resolveSource(ctx, j)
	// Lifecycle contract: deferred immediately, so the download is removed
	// on every exit path — error, ctx-cancel, panic — before ack.
	defer cleanupSource()

	// From here on, j carries the LOCAL source (what analysis reads) and
	// rs.env carries what gets recorded.
	j.Source = rs.local

	p.log().Info("job started",
		"job_id", j.ID, "adapter", p.ModelID.Adapter,
		"media_type", rs.env.MediaType, "ref", rs.env.Ref,
		"workflows", workflowsLabel(j.Workflows))

	var res moderation.NormalizedResult
	var procErr error
	switch {
	case resolveErr != nil:
		procErr = resolveErr
	case rs.env.MediaType == "video":
		res, procErr = p.processVideo(ctx, j)
	default:
		res, procErr = p.processImage(ctx, j)
	}
```

Then in the same function, change the two places that record the source. The `AssetID` assignment (currently line 139) becomes:

```go
	if res.AssetID = rs.env.Ref; res.AssetID == "" {
		res.AssetID = string(j.ID)
	}
	if res.MediaType == "" {
		res.MediaType = rs.env.MediaType
	}
```

The envelope literal (currently line 147) becomes:

```go
	env := result.ResultEnvelope{
		JobID:      j.ID,
		Source:     rs.env,
		ModelID:    p.ModelID,
		Result:     &res,
		StartedAt:  started,
		FinishedAt: time.Now().UTC(),
	}
```

And the empty-video-skip early return (currently line 128) becomes:

```go
		return result.ResultEnvelope{JobID: j.ID, Source: rs.env, ModelID: p.ModelID, StartedAt: started, FinishedAt: time.Now().UTC()}, queue.Ack, nil
```

Its audit event's `asset_id` field (line 121) becomes `rs.env.Ref`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/pipeline/ -v`
Expected: PASS, including every pre-existing test unmodified. If an existing test fails, the file-source path changed — fix `resolveSource`, do not edit the test.

- [ ] **Step 5: Verify no URL can reach ffmpeg**

Run: `go test ./internal/frames/ -v`
Expected: PASS. The protocol deny-list in `internal/frames/workflow.go` is untouched by this change; `processVideo` receives `rs.local`, which is always `kind:"file"`. Confirm by reading `processVideo` — it uses `j.Source.Ref`, and `j.Source` was reassigned to `rs.local` above.

- [ ] **Step 6: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/pipeline/pipeline.go internal/pipeline/source_test.go
git commit -m "pipeline: resolve url sources to local files, record redacted refs"
```

---

### Task 7: Wire the fetcher and validate at intake

**Files:**
- Modify: `internal/cli/wire.go`
- Modify: `internal/cli/serve.go` (intake handler ~line 305-340; fetcher construction ~line 80)
- Modify: `internal/cli/scan.go`
- Test: `internal/cli/serve_test.go` (add cases)

**Interfaces:**
- Consumes: `fetch.New` (Task 3), `config.SourceConfig` (Task 4), `Pipeline.Fetcher` (Task 6).
- Produces: nothing later tasks depend on.

**Why validate twice:** intake validation gives the caller a `400` with a reason. Execution validation is what actually protects the fetcher, because a job can arrive directly on the Redis queue from another producer without passing through intake at all. This mirrors the existing rule for per-job workflow and dedup overrides.

- [ ] **Step 1: Write the failing test**

Add to `internal/cli/serve_test.go`:

```go
func TestIntakeRejectsURLKindWhenDisabled(t *testing.T) {
	cfg := config.Defaults()
	cfg.Source.URL.Enabled = false
	body := `{"kind":"url","ref":"https://media.example.com/clip.mp4","media_type":"video"}`

	rec := postJob(t, cfg, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "source.url.enabled") {
		t.Errorf("body should explain the feature is off: %s", rec.Body.String())
	}
}

func TestIntakeAcceptsAllowListedURL(t *testing.T) {
	cfg := config.Defaults()
	cfg.Source.URL.Enabled = true
	cfg.Source.URL.AllowHosts = []string{"media.example.com"}
	body := `{"kind":"url","ref":"https://media.example.com/clip.mp4","media_type":"video"}`

	rec := postJob(t, cfg, body)
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
}

func TestIntakeRejectsBadURLs(t *testing.T) {
	cfg := config.Defaults()
	cfg.Source.URL.Enabled = true
	cfg.Source.URL.AllowHosts = []string{"media.example.com"}

	for name, ref := range map[string]string{
		"not allow-listed": "https://evil.example.com/clip.mp4",
		"http scheme":      "http://media.example.com/clip.mp4",
		"userinfo":         "https://u:p@media.example.com/clip.mp4",
		"metadata ip":      "https://169.254.169.254/clip.mp4",
	} {
		t.Run(name, func(t *testing.T) {
			rec := postJob(t, cfg, `{"kind":"url","ref":"`+ref+`","media_type":"video"}`)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %s", rec.Code, ref)
			}
		})
	}
}

func TestIntakeDoesNotEchoTheQueryString(t *testing.T) {
	cfg := config.Defaults()
	cfg.Source.URL.Enabled = true
	cfg.Source.URL.AllowHosts = []string{"media.example.com"}

	rec := postJob(t, cfg, `{"kind":"url","ref":"https://evil.example.com/c.mp4?sig=secret","media_type":"video"}`)
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("error response echoed the credential: %s", rec.Body.String())
	}
}

func TestIntakeStillRejectsUnknownKinds(t *testing.T) {
	cfg := config.Defaults()
	rec := postJob(t, cfg, `{"kind":"s3","ref":"s3://b/k","media_type":"video"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
```

**Implementer note:** `postJob` is a helper you may need to write, wrapping `serveIntake` with `httptest.NewRecorder`. Check `internal/cli/serve_test.go` first — if an equivalent exists, use it. Otherwise:

```go
func postJob(t *testing.T, cfg config.Config, body string) *httptest.ResponseRecorder {
	t.Helper()
	q := queue.NewMemq(queue.QueueConfig{})
	bp := observe.NewBackpressure(20, 50, time.Minute, 5)
	srv := serveIntake(cfg, q, bp, &intakeSwitch{}, slog.Default())
	t.Cleanup(func() { _ = srv.Close() })

	req := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}
```

Adapt the `queue.NewMemq` call to whatever constructor signature `internal/queue/memq.go` actually exposes.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestIntake -v`
Expected: FAIL — url kind currently rejected unconditionally by the `req.Kind != "file"` check.

- [ ] **Step 3: Widen intake validation**

In `internal/cli/serve.go`, replace the kind check (currently lines 321-324):

```go
		if req.Kind != "file" || req.Ref == "" {
			http.Error(w, `bad request: v1 accepts {"kind":"file","ref":"<abs path>","media_type":"image|video","workflows":["name",...]} (workflows optional)`, http.StatusBadRequest)
			return
		}
```

with:

```go
		if req.Ref == "" {
			http.Error(w, `bad request: ref is required`, http.StatusBadRequest)
			return
		}
		switch req.Kind {
		case "file":
		case "url":
			if err := validateURLIntake(cfg, req.Ref); err != nil {
				// err is built from the REDACTED url — never echo a query
				// string back to the caller or into the access log.
				http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
				return
			}
		default:
			http.Error(w, `bad request: kind must be "file" or "url"`, http.StatusBadRequest)
			return
		}
```

And the `filepath.Abs` block (currently lines 336-340) becomes file-only:

```go
		ref := req.Ref
		if req.Kind == "file" {
			abs, err := filepath.Abs(req.Ref)
			if err != nil {
				http.Error(w, "bad path", http.StatusBadRequest)
				return
			}
			ref = abs
		}
```

with the `Source` literal using `Ref: ref`.

Add the helper to `serve.go`:

```go
// validateURLIntake is the intake-side half of url validation. The
// execution-side half runs in the fetcher, because a job can also arrive
// straight onto the redis queue without passing through here.
func validateURLIntake(cfg config.Config, rawRef string) error {
	if !cfg.Source.URL.Enabled {
		return fmt.Errorf(`kind "url" requires source.url.enabled=true`)
	}
	allow := make(map[string]bool, len(cfg.Source.URL.AllowHosts))
	for _, h := range cfg.Source.URL.AllowHosts {
		allow[strings.ToLower(strings.TrimSpace(h))] = true
	}
	if _, err := fetch.ValidateURL(rawRef, allow); err != nil {
		return err
	}
	return nil
}
```

`fetch.ValidateURL`'s messages never include the raw URL except the host, which carries no credential — verify this when reading the Task 2 implementation.

- [ ] **Step 4: Construct the fetcher and pass it to the pipeline**

In `internal/cli/wire.go`, add a parameter to `buildPipeline`:

```go
func buildPipeline(cfg config.Config, mod moderation.Moderator, sink result.Sink, auditLog *audit.Log, f pipeline.SourceFetcher, log *slog.Logger) *pipeline.Pipeline {
	p := &pipeline.Pipeline{
		Moderator: mod,
		Sink:      sink,
		Fetcher:   f,
		// … existing fields unchanged
```

Add a constructor next to the other `newX` helpers in `wire.go`:

```go
// newFetcher builds the url-source fetcher, or nil when the feature is
// off. Construction IS boot validation: a bad allow-list fails here.
func newFetcher(cfg config.Config) (pipeline.SourceFetcher, error) {
	f, err := fetch.New(fetch.Config{
		Enabled:           cfg.Source.URL.Enabled,
		AllowHosts:        cfg.Source.URL.AllowHosts,
		MaxBytes:          cfg.Source.URL.MaxBytes,
		Timeout:           cfg.Source.URL.Timeout,
		MaxAttempts:       cfg.Source.URL.MaxAttempts,
		AllowedMediaTypes: cfg.Source.URL.AllowedMediaTypes,
	})
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, nil // disabled: typed-nil must not be returned
	}
	return f, nil
}
```

**Implementer warning:** `fetch.New` returns `(*Fetcher, error)` and returns a nil `*Fetcher` when disabled. Returning that directly as a `pipeline.SourceFetcher` produces a non-nil interface holding a nil pointer, and `p.Fetcher == nil` in `resolveSource` would be FALSE — url jobs would then panic instead of erroring. The explicit `if f == nil { return nil, nil }` above is what prevents that. Do not simplify it away.

In `serve.go` and `scan.go`, call it before `buildPipeline` and pass the result:

```go
	fetcher, err := newFetcher(cfg)
	if err != nil {
		return err
	}
	p := buildPipeline(cfg, mod, sink, auditLog, fetcher, log)
```

Add a log line in `serve.go` when the feature is on:

```go
	if cfg.Source.URL.Enabled {
		log.Warn("url media sources are ENABLED; jobs may cause outbound fetches",
			"allow_hosts", strings.Join(cfg.Source.URL.AllowHosts, ","))
	}
```

- [ ] **Step 5: Add the typed-nil regression test**

```go
func TestDisabledFetcherIsAnUntypedNil(t *testing.T) {
	cfg := config.Defaults()
	cfg.Source.URL.Enabled = false
	f, err := newFetcher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		t.Fatal("a disabled fetcher must be an untyped nil, or pipeline's nil check silently fails and url jobs panic")
	}
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/cli/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/cli/serve.go internal/cli/scan.go internal/cli/wire.go internal/cli/serve_test.go
git commit -m "cli: accept kind:url at intake and wire the fetcher into the pipeline"
```

---

### Task 8: Fetch metrics

**Files:**
- Modify: `internal/observe/observe.go`
- Modify: `internal/pipeline/pipeline.go` (record around the fetch)

- [ ] **Step 1: Add the metrics**

To the `Metrics` struct:

```go
	FetchSeconds       prometheus.Histogram
	FetchBytesTotal    prometheus.Counter
	FetchFailuresTotal *prometheus.CounterVec
```

To `NewMetrics()`:

```go
		FetchSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "vismod_fetch_duration_seconds",
			Help:    "Remote media fetch latency.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60},
		}),
		FetchBytesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vismod_fetch_bytes_total", Help: "Bytes downloaded for url sources.",
		}),
		FetchFailuresTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			// reason is a BOUNDED set — never an error string, which would
			// be both a cardinality bomb and a way for a url to reach a
			// metric label.
			Name: "vismod_fetch_failures_total", Help: "Remote fetch failures, by reason.",
		}, []string{"reason"}),
```

Add all three to `reg.MustRegister(...)`.

- [ ] **Step 2: Add the bounded reason classifier**

In `internal/fetch/fetch.go`:

```go
// Reason maps a fetch error to one of a FIXED set of metric labels.
// Never derive a label from an error string: it would be unbounded
// cardinality and could carry a URL into Prometheus.
func Reason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "allow_hosts"), strings.Contains(s, "scheme"),
		strings.Contains(s, "userinfo"), strings.Contains(s, "no host"),
		strings.Contains(s, "not parseable"):
		return "rejected_url"
	case strings.Contains(s, "loopback"), strings.Contains(s, "private"),
		strings.Contains(s, "link-local"), strings.Contains(s, "multicast"),
		strings.Contains(s, "CGNAT"), strings.Contains(s, "unspecified"):
		return "denied_address"
	case strings.Contains(s, "redirect"):
		return "redirect"
	case strings.Contains(s, "max_bytes"):
		return "oversize"
	case strings.Contains(s, "Content-Type"):
		return "media_type"
	case strings.Contains(s, "returned "):
		return "http_status"
	}
	return "other"
}
```

Add a test asserting every branch returns a value from the fixed set, and that no returned label ever contains a `/` or `:` (which a URL would):

```go
func TestReasonLabelsAreBounded(t *testing.T) {
	allowed := map[string]bool{
		"": true, "timeout": true, "rejected_url": true, "denied_address": true,
		"redirect": true, "oversize": true, "media_type": true,
		"http_status": true, "other": true,
	}
	for _, err := range []error{
		nil,
		context.DeadlineExceeded,
		errors.New("fetch: host \"x\" is not in source.url.allow_hosts"),
		errors.New("fetch: 10.0.0.1 is a private address"),
		errors.New("fetch: redirects are not followed"),
		errors.New("fetch: body exceeds source.url.max_bytes (1024)"),
		errors.New("fetch: Content-Type \"text/html\" is not in source.url.allowed_media_types"),
		errors.New("fetch: https://h/x returned 500"),
		errors.New("something entirely unexpected"),
	} {
		got := Reason(err)
		if !allowed[got] {
			t.Errorf("Reason(%v) = %q, outside the fixed label set", err, got)
		}
		if strings.ContainsAny(got, "/:?") {
			t.Errorf("label %q looks like it carries a url", got)
		}
	}
}
```

- [ ] **Step 3: Record from the pipeline**

Add an optional hook to `Pipeline` rather than importing Prometheus into the pipeline:

```go
	// OnFetch records fetch outcomes for metrics. nil disables.
	OnFetch func(d time.Duration, bytes int64, reason string)
```

Call it in `resolveSource` around the `p.Fetcher.Fetch` call, using `os.Stat(path)` for the byte count on success. Wire it in `wire.go` where the metrics registry is available.

- [ ] **Step 4: Run tests and commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/observe/observe.go internal/fetch/ internal/pipeline/ internal/cli/wire.go
git commit -m "observe: add fetch metrics with a bounded failure-reason label set"
```

---

### Task 9: Documentation

**Files:**
- Modify: `SECURITY.md`, `config.example.yaml`, `README.md`, `CLAUDE.md`, `AGENTS.md`, `docs/agent/{STATUS,TASKS,UNVERIFIED}.md`

- [ ] **Step 1: Rewrite SECURITY.md's SSRF section**

The current text (lines ~36-49) says media-source URLs are *disabled*. Replace with three classes:

```markdown
### SSRF / egress posture

Three kinds of URL exist in this system and they take **different** rules.
Do not apply one rule to another.

**1. Media source URLs — job-supplied, untrusted, deny private ranges.**

Enabled by `source.url.enabled` (OFF by default). The destination comes
from an intake request body, so an attacker who can post a job chooses
it. Controls, all fail-closed:

- `https` only. No `http`, no exceptions.
- Exact hostname allow-list (`source.url.allow_hosts`), no wildcards and
  no suffix matching. Enabling the feature with an empty list refuses to
  boot.
- No userinfo in the URL; credentials are env-only.
- Loopback, RFC 1918, 169.254.0.0/16, IPv6 link-local, ULA fc00::/7, ::1,
  unspecified, multicast, and CGNAT 100.64.0.0/10 are denied
  unconditionally. There is no flag that disables this.
- The address policy runs in the dialer against the address ACTUALLY
  DIALED, not the hostname parsed, so DNS rebinding is refused at socket
  level.
- Redirects are not followed.
- Size cap (`source.url.max_bytes`) enforced on bytes read; Content-Length
  is never trusted. Response Content-Type must be allow-listed.
- The download is transient: deleted on every exit path before ack.
- Frame extraction still rejects any input path containing a protocol
  scheme. The fetcher hands ffmpeg a local path; a URL never reaches an
  ffmpeg argument.
- A presigned URL's query string is a CREDENTIAL. `Source.Ref` records
  scheme+host+path only, and `Source.RefDigest` carries SHA-256 of the
  full URL for correlation. The full URL is never logged, audited,
  enveloped, or used as a metric label.

**2. Provider endpoint URLs — operator config, private ranges expected.**

[existing text unchanged]

**3. Result webhook URLs — operator config, private ranges expected.**

`output.sinks[].url` is operator configuration, never job input, so it is
the same trust class as a provider endpoint: internal and private
addresses are expected and permitted. Rule 1's deny-list must NOT be
applied to it. It is still validated for scheme, absence of userinfo, and
the 169.254.0.0/16 metadata range.
```

- [ ] **Step 2: Document the config surface**

Append to `config.example.yaml`:

```yaml
# Remote media sources. OFF by default.
#
# SECURITY: the URL comes from the job payload, so whoever can post a job
# chooses where vismod connects. Read SECURITY.md before enabling this.
# There is deliberately no switch that permits private/internal addresses.
source:
  url:
    enabled: false
    # Exact hostnames. No wildcards, no suffix matching:
    # "example.com" does NOT permit "cdn.example.com".
    allow_hosts: []
    max_bytes: 268435456   # 256 MiB, enforced on bytes read
    timeout: 60s
    max_attempts: 3
    allowed_media_types:
      - video/mp4
      - video/webm
      - video/quicktime
      - image/jpeg
      - image/png
      - image/webp
```

- [ ] **Step 3: Update README intake docs**

Wherever `POST /jobs` is documented, add the `url` kind, noting it is off by default and that `ref` must be an allow-listed `https` URL.

- [ ] **Step 4: Update the architecture maps and gotchas**

`CLAUDE.md` "Shape of the code" and `AGENTS.md` "Architecture map" both gain:

```
internal/fetch/   allow-listed remote media fetch (url source kind)
```

New `AGENTS.md` gotchas:

```markdown
- **Two Source values exist per url job.** `pipeline.resolveSource`
  returns `resolved{local, env}`: analysis reads `local` (kind:"file",
  the temp download), everything RECORDED uses `env` (kind:"url", the
  redacted ref + digest). Conflating them either leaks a presigned URL's
  credential into the audit log or records a meaningless temp path.
- **`fetch.New` returns a nil `*Fetcher` when disabled.** Assigning that
  directly to the `pipeline.SourceFetcher` interface makes a non-nil
  interface holding a nil pointer, and `p.Fetcher == nil` becomes false —
  url jobs panic instead of erroring. `cli.newFetcher` converts it to an
  untyped nil for exactly this reason.
- **The url host allow-list and the address deny-list are separate
  checks.** The allow-list is hostnames, checked at parse. The deny-list
  is addresses, checked per-connection in `net.Dialer.Control`. Only the
  second one catches DNS rebinding; collapsing them into one parse-time
  check silently removes that defense.
```

- [ ] **Step 5: Update the agent docs**

- `STATUS.md`: what landed.
- `TASKS.md`: remove completed entries.
- `UNVERIFIED.md`: **no fetch has ever run against a real remote host** — every test uses `httptest` on loopback with the address policy replaced. State that proving it needs a live allow-listed host, and that the DNS-rebinding defense has been verified only against a simulated policy, not a real rebinding resolver.

- [ ] **Step 6: Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add SECURITY.md config.example.yaml README.md CLAUDE.md AGENTS.md docs/agent/
git commit -m "docs: document the url source kind and the three URL trust classes"
```

---

## Self-review notes

Checked against the spec, Part 1:

| Spec requirement | Task |
|---|---|
| Off by default; intake 400 / pipeline error when disabled | 4, 6, 7 ✓ |
| Empty allow-list refuses boot | 3, 4 ✓ |
| `https` only | 2 ✓ |
| No userinfo | 2 ✓ |
| Denied ranges incl. CGNAT, ULA, v4-mapped | 1 ✓ |
| DNS rebinding via `Dialer.Control` | 3 ✓ |
| Redirects error | 3 ✓ |
| Size cap, Content-Length untrusted | 3 ✓ |
| Independent timeout | 3, 4 ✓ |
| Media-type allow-list | 3 ✓ |
| URL never reaches ffmpeg | 6 (Step 5) ✓ |
| No `allow_private_hosts`; `ipPolicy` seam instead | 3 ✓ |
| `Source` rewritten before `AnalyzeVideo` | 6 ✓ |
| Presigned redaction + `RefDigest` + SchemaVersion 1.2.0 | 2, 5, 6 ✓ |
| Failure matrix, terminal vs retryable | 3, 6 ✓ |
| Validation at intake AND execution | 7 ✓ |
| Metrics with bounded reason label | 8 ✓ |
| SECURITY.md three classes | 9 ✓ |

**Deviations from the spec, deliberate:**

1. **`max_attempts` added to `source.url`.** The spec described retries but named no knob. Defaulted to 3, matching `DoJSON`.
2. **`Fetch` does not use `moderate.DoJSON`.** `DoJSON` buffers the whole body into memory; a 256 MiB video must stream to disk. The retry *classification* is still shared via the two helpers exported in Task 3, so the policy has one definition even though the loop does not.
3. **`allowScheme` field on `Fetcher`.** Not in the spec. It exists solely so tests can reach `httptest` (which is `http`) without a config flag that would weaken production. It is unexported and not settable from config.

**Type consistency check:** `fetch.Config` (Task 3) ↔ `config.URLSourceConfig` (Task 4) ↔ `newFetcher` mapping (Task 7) — field names and types match across all three. `SourceFetcher.Fetch` (Task 6) matches `(*Fetcher).Fetch` (Task 3): `(ctx, rawURL, dir) (string, func(), error)`. `Redact` returns `(ref, digest)` in Tasks 2, 6 consistently.

**Known-unverifiable at plan time:** Task 3's `TestFetchDNSRebinding` forces a second connection with `DisableKeepAlives`. If `httptest` reuses the connection anyway, the test passes vacuously. The implementer must confirm `ipPolicy` was called twice — add a counter assertion if the behavior is unclear when running it.
