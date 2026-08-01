package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/vismod/vismod/internal/audit"
)

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Audit-log operations",
}

var auditVerifyCmd = &cobra.Command{
	Use:   "verify [path]",
	Short: "Recompute the audit hash chain and report the first broken link",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := cfg.Audit.Path
		if len(args) == 1 {
			path = args[0]
		}
		valid, err := audit.Verify(path)
		if err != nil {
			return fmt.Errorf("%w (%d records verified before the break)", err, valid)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "audit chain OK: %d records verified (%s)\n", valid, path)
		return nil
	},
}

func init() {
	auditCmd.AddCommand(auditVerifyCmd)
	rootCmd.AddCommand(auditCmd)
}
