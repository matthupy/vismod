// Package cli wires the cobra command tree and the composition root. Adapters
// are pulled in here by blank import so they self-register; the registry never
// imports adapter packages.
package cli

import (
	"github.com/spf13/cobra"

	// Blank imports: each adapter self-registers via init(). This is the ONLY
	// place that knows the concrete adapter set.
	_ "github.com/matthupy/vismod/internal/moderate/adapters/azure"
	_ "github.com/matthupy/vismod/internal/moderate/adapters/stub"
)

// Version is overridden at build time via -ldflags.
var Version = "0.0.0-dev"

var configPath string

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "vismod",
		Short: "Open-source visual content moderation pipeline",
		Long: "vismod scans images and video for harmful content using a pluggable " +
			"visual-moderation model and normalizes provider outputs into one schema.",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "path to config file (yaml)")

	root.AddCommand(newScanCmd(), newServeCmd(), newAdaptersCmd(), newAuditCmd(), newVersionCmd())
	return root
}

// Execute runs the root command.
func Execute() error {
	return newRootCmd().Execute()
}
