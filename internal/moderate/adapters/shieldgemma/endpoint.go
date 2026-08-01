package shieldgemma

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// validateEndpoint enforces the SECURITY.md "provider endpoint URLs" rules
// on the operator-supplied inference endpoint. These are deliberately NOT
// the media-source URL rules: a media source is attacker-influenceable job
// input and must never reach a private range, whereas a self-hosted
// inference server is EXPECTED to be loopback or RFC 1918. Conflating the
// two would either break this adapter or quietly license SSRF.
//
// The load-bearing control is that this URL is config-only — it comes from
// adapter.options and is never read from a job, queue payload, or intake
// body. Resolution happens at request time, so a boot-time check cannot
// close DNS rebinding; config-only provenance is what makes that
// acceptable.
func validateEndpoint(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("endpoint is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("endpoint %q is not a valid URL: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("endpoint scheme %q not allowed (http or https only)", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("endpoint must not carry userinfo (credentials in yaml are forbidden; use VISMOD_* env)")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("endpoint %q has no host", raw)
	}

	ip := net.ParseIP(host)
	// 169.254.0.0/16 (and IPv6 link-local) is rejected for BOTH schemes: no
	// legitimate inference server lives there, and it is the one range where
	// a misconfiguration turns into cloud-credential theft.
	if ip != nil && ip.IsLinkLocalUnicast() {
		return fmt.Errorf("endpoint host %s is in the link-local/cloud-metadata range, which is never a valid inference server", host)
	}
	if u.Scheme == "https" {
		return nil
	}
	// Plaintext inward only: http is permitted for loopback and private
	// ranges, a public host must be https. Any credentials are env-only per
	// invariant 4 and must not cross a public network in clear.
	if isLocalHostname(host) {
		return nil
	}
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		return nil
	}
	return fmt.Errorf("endpoint host %s requires https (http is permitted only for loopback and private ranges)", host)
}

// isLocalHostname covers the names that resolve to loopback by definition.
// Any other DNS name is treated as public: vismod cannot know at boot what
// it will resolve to at request time.
func isLocalHostname(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// defaultTimeout is deliberately long. vLLM QUEUES rather than rejecting
// under load, so a short timeout turns backpressure into a retry storm
// (see the amplification note in the package comment).
const defaultTimeout = 120 * time.Second

// newHTTPClient builds the adapter's client. CheckRedirect errors: a
// redirect names a destination vismod did not choose, so following one
// would bypass validateEndpoint entirely.
func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("shieldgemma: refusing redirect to %s (endpoint is config-only)", req.URL.Redacted())
		},
	}
}
