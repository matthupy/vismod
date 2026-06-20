package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeHealthzOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %q, want /healthz", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://") // host:port
	if err := probeHealthz(addr); err != nil {
		t.Errorf("probeHealthz(%q) = %v, want nil", addr, err)
	}
}

func TestProbeHealthzNotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := probeHealthz(addr); err == nil {
		t.Error("probeHealthz on 503 returned nil, want error")
	}
}

func TestProbeHealthzUnreachable(t *testing.T) {
	// Closed/unused port: connection refused.
	if err := probeHealthz("127.0.0.1:1"); err == nil {
		t.Error("probeHealthz on unreachable addr returned nil, want error")
	}
}

func TestProbeHealthzBadAddr(t *testing.T) {
	if err := probeHealthz("not-a-host-port"); err == nil {
		t.Error("probeHealthz on malformed addr returned nil, want error")
	}
}
