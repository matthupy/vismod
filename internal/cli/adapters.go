package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/moderate"
)

var adaptersCmd = &cobra.Command{
	Use:   "adapters",
	Short: "List registered moderation adapters and their capabilities",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		names := moderate.Registered()
		if len(names) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no adapters registered (M0 skeleton)")
			return nil
		}
		for _, name := range names {
			fmt.Fprintf(cmd.OutOrStdout(), "%s", name)
			// Capabilities need an instance; construction may fail without
			// credentials — report why instead of hiding the adapter.
			m, err := moderate.New(name, moderate.AdapterConfig{
				Options: cfg.Adapter.Options,
				Secret:  config.Secret(),
			})
			if err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "\t(capabilities unavailable: %v)\n", err)
				continue
			}
			caps := m.Capabilities()
			_ = m.Close()
			fmt.Fprintf(cmd.OutOrStdout(), "\tvideo=%v max_image_bytes=%d categories=%v\n",
				caps.SupportsVideo, caps.MaxImageBytes, caps.Categories)
		}
		return nil
	},
}

func init() { rootCmd.AddCommand(adaptersCmd) }
