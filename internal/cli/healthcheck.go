package cli

import (
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

var healthURL string

var healthcheckCmd = &cobra.Command{
	Use:   "healthcheck",
	Short: "Probe a running vismod serve instance (used by Docker HEALTHCHECK)",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(healthURL)
		if err != nil {
			return fmt.Errorf("healthcheck: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("healthcheck: %s returned %d", healthURL, resp.StatusCode)
		}
		return nil
	},
}

func init() {
	healthcheckCmd.Flags().StringVar(&healthURL, "url", "http://127.0.0.1:9090/healthz", "health endpoint to probe")
	rootCmd.AddCommand(healthcheckCmd)
}
