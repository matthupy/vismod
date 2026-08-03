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
	u, err := ValidateURL("https://media.example.com/clip.mp4", allowed("media.example.com"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "media.example.com" {
		t.Errorf("host: got %q", u.Host)
	}
}

func TestValidateURLHostMatchIsCaseInsensitive(t *testing.T) {
	if _, err := ValidateURL("https://Media.Example.COM/clip.mp4", allowed("media.example.com"), nil); err != nil {
		t.Errorf("host comparison must be case-insensitive: %v", err)
	}
}

func TestValidateURLAllowListIgnoresPort(t *testing.T) {
	if _, err := ValidateURL("https://media.example.com:8443/clip.mp4", allowed("media.example.com"), nil); err != nil {
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
			if _, err := ValidateURL(raw, hosts, nil); err == nil {
				t.Fatalf("%q must be rejected, got nil error", raw)
			}
		})
	}
}

// An empty allow_hosts means "any host": url sources work out of the box
// and an operator narrows them in production. The address policy, not the
// host list, is what keeps a job off non-public infrastructure.
func TestValidateURLEmptyAllowListPermitsAnyHost(t *testing.T) {
	if _, err := ValidateURL("https://anything.example.com/clip.mp4", map[string]bool{}, nil); err != nil {
		t.Fatalf("an empty allow-list must permit any host, got %v", err)
	}
}

func TestValidateURLEmptyAllowListStillRejectsBadURLs(t *testing.T) {
	for name, raw := range map[string]string{
		"http scheme": "http://media.example.com/clip.mp4",
		"file scheme": "file:///etc/passwd",
		"userinfo":    "https://user:pw@media.example.com/clip.mp4",
		"empty host":  "https:///clip.mp4",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateURL(raw, nil, nil); err == nil {
				t.Fatalf("%q must be rejected even with no allow-list", raw)
			}
		})
	}
}

// A private host is permitted without appearing in allow_hosts, and only
// there is plain http accepted.
func TestValidateURLPrivateHosts(t *testing.T) {
	private := allowed("host.docker.internal")

	if _, err := ValidateURL("http://host.docker.internal:8000/clip.mp4",
		allowed("media.example.com"), private); err != nil {
		t.Errorf("a private host must be permitted over http: %v", err)
	}
	if _, err := ValidateURL("https://host.docker.internal/clip.mp4",
		allowed("media.example.com"), private); err != nil {
		t.Errorf("a private host must still accept https: %v", err)
	}
	if _, err := ValidateURL("http://media.example.com/clip.mp4",
		allowed("media.example.com"), private); err == nil {
		t.Error("http must stay rejected for a host that is not in allow_private_hosts")
	}
	if _, err := ValidateURL("http://other.example.com/clip.mp4",
		nil, private); err == nil {
		t.Error("an empty allow_hosts must not extend the http exemption to every host")
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
