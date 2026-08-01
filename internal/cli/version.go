package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Version and Commit are stamped via -ldflags at build time.
var (
	Version = "dev"
	Commit  = "unknown"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "vismod %s (%s) %s/%s\n", Version, Commit, runtime.GOOS, runtime.GOARCH)
	},
}

func init() { rootCmd.AddCommand(versionCmd) }
