package cli

import (
	"fmt"

	"github.com/matthupy/vismod/internal/moderate"
	"github.com/spf13/cobra"
)

func newAdaptersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "adapters",
		Short: "List registered moderation adapters and their capabilities",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names := moderate.Names()
			if len(names) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no adapters registered)")
				return nil
			}
			for _, name := range names {
				// Instantiate with an empty config just to read capabilities.
				m, err := moderate.New(name, moderate.AdapterConfig{Name: name, Secret: func(string) string { return "" }})
				if err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "%-16s (init error: %v)\n", name, err)
					continue
				}
				caps := m.Capabilities()
				_ = m.Close()
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s video=%v max_image_bytes=%d categories=%v\n",
					name, caps.SupportsVideo, caps.MaxImageBytes, caps.Categories)
			}
			return nil
		},
	}
}
