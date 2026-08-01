package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/vismod/vismod/internal/frames"
)

var workflowsCmd = &cobra.Command{
	Use:   "workflows",
	Short: "FFmpeg workflow operations",
}

var workflowsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured FFmpeg workflows",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if len(cfg.FFmpeg.Workflows) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no workflows configured (defaults ship in M2)")
			return nil
		}
		for name, wf := range cfg.FFmpeg.Workflows {
			marker := " "
			if name == cfg.FFmpeg.DefaultWorkflow {
				marker = "*"
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\t%s\n", marker, name, wf.Description)
		}
		return nil
	},
}

var workflowsValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate all FFmpeg workflows against the security guardrails",
	Long: `validate is the gate for custom workflows: it confirms ffmpeg/ffprobe
are present, parses every workflow template, rejects forbidden
placeholders and protocols, and dry-renders each workflow to prove its
output stays inside the pipeline-owned WorkDir.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := frames.ValidateBinaries(cfg.FFmpeg); err != nil {
			return err
		}
		if err := frames.ValidateAll(cfg.FFmpeg); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "all %d workflows OK (binaries present, guardrails satisfied)\n", len(cfg.FFmpeg.Workflows))
		return nil
	},
}

func init() {
	workflowsCmd.AddCommand(workflowsListCmd, workflowsValidateCmd)
	rootCmd.AddCommand(workflowsCmd)
}
