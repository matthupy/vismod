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
	return validateURL(raw, allowHosts, "https")
}

// validateURL is ValidateURL with the required scheme as a parameter.
// Production always passes "https"; the Fetcher passes its allowScheme so
// tests can reach an httptest server without a config flag that would
// weaken production. Nothing outside this package can vary it.
func validateURL(raw string, allowHosts map[string]bool, scheme string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("fetch: url is not parseable")
	}
	if u.Scheme != scheme {
		return nil, fmt.Errorf("fetch: url scheme must be %s, got %q", scheme, u.Scheme)
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
