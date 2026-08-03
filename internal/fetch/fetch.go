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
	AllowHosts        []string
	AllowPrivateHosts []string
	MaxBytes          int64
	Timeout           time.Duration
	MaxAttempts       int
	AllowedMediaTypes []string
}

// Fetcher downloads media URLs to local files.
type Fetcher struct {
	cfg        Config
	hosts      hostRules
	allowTypes map[string]bool
	client     *http.Client

	// allowScheme is "" in production. Tests set it to "http" so httptest
	// servers are reachable; it is not settable from config.
	allowScheme string

	// ipPolicy runs per-connection against the address actually dialed.
	// This is the DNS-rebinding defense: a name that validated at parse
	// time cannot re-resolve into a denied range without hitting this.
	ipPolicy func(netip.Addr) error

	// privatePolicy replaces ipPolicy for hosts in allow_private_hosts.
	// It still denies the ranges that are never a media server.
	privatePolicy func(netip.Addr) error
}

// terminalErr marks a failure that must not be retried.
type terminalErr struct{ err error }

func (e terminalErr) Error() string { return e.err.Error() }
func (e terminalErr) Unwrap() error { return e.err }

func terminal(format string, a ...any) error {
	return terminalErr{fmt.Errorf(format, a...)}
}

// New builds a Fetcher. url sources are usable with no configuration at
// all: an empty AllowHosts permits any host that survives the per-dial
// address policy, and an operator narrows that in production. What is
// never implicit is non-public address space — only AllowPrivateHosts
// reaches it, and only for the exact hosts named there.
func New(cfg Config) (*Fetcher, error) {
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
		cfg: cfg,
		hosts: hostRules{
			allow:   lowerSet(cfg.AllowHosts),
			private: lowerSet(cfg.AllowPrivateHosts),
		},
		allowTypes:    lowerSet(cfg.AllowedMediaTypes),
		ipPolicy:      DenyPrivate,
		privatePolicy: DenyMetadata,
	}

	f.client = &http.Client{
		Timeout:   cfg.Timeout,
		Transport: &http.Transport{DialContext: f.dial},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A redirect is a destination vismod did not choose.
			return errors.New("fetch: redirects are not followed")
		},
	}
	return f, nil
}

// dial picks the address policy from the HOSTNAME being dialed, then
// enforces it against the address actually connected to.
//
// The policy must be chosen here rather than at construction because it
// varies per request: allow_private_hosts relaxes the address rules for
// the hosts it names and for nothing else. Reading the hostname from the
// pre-resolution addr is what keeps the relaxation from leaking — a
// public host that re-resolves into RFC 1918 still gets DenyPrivate.
func (f *Fetcher) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("fetch: unparseable dial address %q", addr)
	}
	policy := f.ipPolicy
	if f.hosts.private[strings.ToLower(host)] {
		policy = f.privatePolicy
	}

	d := &net.Dialer{Timeout: dialTimeout}
	d.Control = func(_, address string, _ syscall.RawConn) error {
		h, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("fetch: unparseable dial address %q", address)
		}
		ip, err := netip.ParseAddr(h)
		if err != nil {
			return fmt.Errorf("fetch: dial address %q is not an IP", h)
		}
		return policy(ip)
	}
	return d.DialContext(ctx, network, addr)
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
	var mu sync.Mutex
	once := &sync.Once{}
	path := filepath.Join(dir, "source"+extOf(rawURL))
	// cleanup reads `once` under mu because the retry loop replaces it
	// after each discarded attempt, and the caller may hold the closure.
	cleanup := func() {
		mu.Lock()
		o := once
		mu.Unlock()
		o.Do(func() { _ = os.Remove(path) })
	}
	discard := func() {
		cleanup()
		mu.Lock()
		once = &sync.Once{}
		mu.Unlock()
	}

	u, err := validateURL(rawURL, f.hosts, f.allowScheme)
	if err != nil {
		return "", cleanup, terminalErr{err}
	}

	var lastErr error
	for attempt := 1; attempt <= f.cfg.MaxAttempts; attempt++ {
		err := f.attempt(ctx, u.String(), path)
		if err == nil {
			return path, cleanup, nil
		}
		discard() // never leave a partial file between attempts
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
	defer func() { _ = resp.Body.Close() }()

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
	// Belt and braces: the explicit Close below is the one whose error is
	// reported, but every early return still has to release the handle.
	// Closing twice is harmless; the second error is not a new fact.
	defer func() { _ = out.Close() }()

	// Read ONE byte past the cap so an exactly-at-cap body still succeeds
	// while an oversize one is detectable. Content-Length is never trusted.
	n, err := io.Copy(out, io.LimitReader(resp.Body, f.cfg.MaxBytes+1))
	if err != nil {
		return err // transport/context failure: retryable
	}
	if n > f.cfg.MaxBytes {
		return terminal("fetch: body exceeds source.url.max_bytes (%d)", f.cfg.MaxBytes)
	}
	// A failed close can mean the tail of the download never reached disk,
	// which would hand ffmpeg a truncated file. Report it as retryable
	// rather than scanning a partial asset.
	if err := out.Close(); err != nil {
		return fmt.Errorf("fetch: close %s: %w", path, err)
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
