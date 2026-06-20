package cli

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/matthupy/vismod/internal/config"
	"github.com/spf13/cobra"
)

// newHealthcheckCmd is a self-contained liveness probe for the container
// HEALTHCHECK (§I). It GETs /healthz on metrics.addr and exits non-zero on
// failure, so the slim runtime image needs no curl/wget. Honors the same config
// as serve, so a custom metrics.addr is respected.
func newHealthcheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "healthcheck",
		Short:  "Probe /healthz on metrics.addr (for container HEALTHCHECK)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			return probeHealthz(cfg.MetricsAddr)
		},
	}
}

// probeHealthz GETs http://<host>/healthz, normalizing a bare ":9090"-style
// addr to localhost. Returns an error unless the endpoint answers 2xx.
func probeHealthz(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("healthcheck: bad metrics.addr %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("healthcheck: %s returned HTTP %d", url, resp.StatusCode)
	}
	return nil
}
