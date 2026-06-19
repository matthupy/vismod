package cli

import (
	"fmt"

	"github.com/matthupy/vismod/internal/audit"
	"github.com/spf13/cobra"
)

func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect the tamper-evident decision audit log",
	}
	cmd.AddCommand(newAuditVerifyCmd())
	return cmd
}

func newAuditVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <log-path>",
		Short: "Recompute the audit hash chain and report the first broken link",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			broken, err := audit.Verify(args[0])
			if err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "FAIL: chain broken at seq %d: %v\n", broken, err)
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "OK: audit chain intact")
			return nil
		},
	}
}
