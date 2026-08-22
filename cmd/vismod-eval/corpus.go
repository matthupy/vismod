package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/vismod/vismod/internal/eval/corpus"
)

// newRootCmd builds the command tree. out is where a subcommand writes its
// product when no output file is named; errors and diagnostics go to
// stderr, so `corpus convert list.csv > corpus.yaml` is safe to redirect.
func newRootCmd(out io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "vismod-eval",
		Short: "Measure a configured vismod against a corpus you supply",
		// A failed conversion is a bad input file, not a bad invocation;
		// dumping usage after it buries the reason.
		SilenceUsage: true,
	}
	root.AddCommand(newCorpusCmd(out))
	return root
}

func newCorpusCmd(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "corpus",
		Short: "Work with evaluation corpus manifests",
	}
	cmd.AddCommand(newCorpusConvertCmd(out))
	return cmd
}

func newCorpusConvertCmd(out io.Writer) *cobra.Command {
	var outPath string
	cmd := &cobra.Command{
		Use:   "convert <list.csv>",
		Short: "Desugar a flat ref,expected_flagged CSV into a v1 corpus manifest",
		Long: `convert turns a flat CSV corpus into the v1 corpus.yaml manifest it
stands for, so the CSV is an on-ramp with a growth path rather than a
parallel input format.

  ref,expected_flagged
  https://media.example.com/a.jpg,true
  /mnt/corpus/b.png,false
  ./local/c.mp4,

ref is required. An https:// ref is kind url and anything else is a path,
resolved against the CSV's own directory and written out resolved. The
media type is inferred from the extension. Ids are assigned case-0001... in
file order, and label_provenance is human for every row — a CSV cannot
express derived labels, and an operator who has them writes a manifest.

An EMPTY expected_flagged means not asserted: that case is scanned and
reported, never scored. It is not the same as expecting allow.

The emitted manifest is what a run loads, validates and digests. Edit it
and it stays a corpus; its SHA-256 is the corpus identity.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			manifest, err := corpus.Convert(args[0])
			if err != nil {
				return err
			}
			if outPath == "" {
				_, err := out.Write(manifest)
				return err
			}
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				return fmt.Errorf("corpus convert: create output directory: %w", err)
			}
			if err := os.WriteFile(outPath, manifest, 0o600); err != nil {
				return fmt.Errorf("corpus convert: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", outPath)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outPath, "out", "o", "", "write the manifest here (default: stdout)")
	return cmd
}
