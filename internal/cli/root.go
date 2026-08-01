// Package cli is the cobra composition root. Adapters self-register via
// init(); the blank imports below are the ONLY place adapter packages are
// pulled in (the registry itself never imports them).
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/vismod/vismod/internal/config"
	// Adapter self-registration: blank imports pull each adapter's init()
	// into the binary. This is the ONLY coupling point.
	_ "github.com/vismod/vismod/internal/moderate/adapters/google"
	_ "github.com/vismod/vismod/internal/moderate/adapters/hive"
	_ "github.com/vismod/vismod/internal/moderate/adapters/microsoft"
	_ "github.com/vismod/vismod/internal/moderate/adapters/shieldgemma"
)

var (
	cfgPath string
	cfg     config.Config
)

var rootCmd = &cobra.Command{
	Use:   "vismod",
	Short: "vismod — open-source visual content moderation pipeline",
	Long: `vismod scans images and video for harmful content using a pluggable
visual-moderation model, normalizes provider outputs into one common
scoring schema, and runs as a one-shot CLI or a long-running worker.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		// version/healthcheck don't need config.
		switch cmd.Name() {
		case "version", "healthcheck":
			return nil
		}
		var err error
		cfg, err = config.Load(cfgPath)
		if err != nil {
			return err
		}
		return nil
	},
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgPath, "config", "c", "", "path to config yaml (env VISMOD_* overlays it)")
}

// Execute runs the CLI.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}
