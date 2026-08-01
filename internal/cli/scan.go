package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".mkv": true, ".webm": true,
	".avi": true, ".m4v": true, ".mpg": true, ".mpeg": true, ".ts": true,
}

func mediaTypeFor(path string) string {
	if videoExts[strings.ToLower(filepath.Ext(path))] {
		return "video"
	}
	return "image"
}

var (
	scanWorkflows      []string
	scanDedupThreshold int
)

var scanCmd = &cobra.Command{
	Use:   "scan <file>...",
	Short: "Moderate one or more local image/video files and print JSONL envelopes",
	Long: `scan runs each file through the active moderation model and writes one
JSON-lines result envelope per file to stdout.

--workflow selects the FFmpeg extraction workflow(s) for video inputs
(repeatable, or comma-separated); frames are the union across the
selected workflows. Omitted = the configured default workflow.

--dedup-threshold overrides frames.dedup for this scan: 0..64 enables
near-duplicate removal at that Hamming distance, -1 disables it.
Omitted = the configured behavior.

Exit codes: 0 = all allow; 1 = at least one flag/block; 2 = at least one
error verdict or processing failure (fail safe: an error is never allow).`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log := observe.NewLogger(cfg.LogLevel)

		// The extraction path is only validated when a video is actually
		// being scanned; image-only scans don't require ffmpeg.
		for _, path := range args {
			if mediaTypeFor(path) == "video" {
				if err := validateFrameBoot(cfg); err != nil {
					return fmt.Errorf("boot validation: %w", err)
				}
				break
			}
		}
		if err := validateWorkflowSelection(cfg, scanWorkflows); err != nil {
			return err
		}
		var dedupOverride *int
		if cmd.Flags().Changed("dedup-threshold") {
			if err := validateDedupThreshold(&scanDedupThreshold); err != nil {
				return err
			}
			dedupOverride = &scanDedupThreshold
		}

		mod, err := buildModerator(cfg, log)
		if err != nil {
			return err
		}
		defer mod.Close()
		// After buildModerator: the label declaration lives on the adapter,
		// so this check cannot run in config.Load.
		if err := validateProviderLabelBoot(cfg, mod); err != nil {
			return fmt.Errorf("boot validation: %w", err)
		}

		auditLog, err := openAudit(cfg)
		if err != nil {
			return err
		}
		if auditLog != nil {
			defer auditLog.Close()
		}

		sink, closeSinks, err := buildSinks(cfg, cmd.OutOrStdout(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = closeSinks() }()
		p := buildPipeline(cfg, mod, sink, auditLog, log)
		p.FrameSource = newFrameSource(cfg, log)

		exit := 0
		for i, path := range args {
			abs, err := filepath.Abs(path)
			if err != nil {
				return fmt.Errorf("resolve %s: %w", path, err)
			}
			if _, err := os.Stat(abs); err != nil {
				return fmt.Errorf("input %s: %w", path, err)
			}
			j := queue.Job{
				ID: queue.JobID(fmt.Sprintf("scan-%d-%d", time.Now().UnixNano(), i)),
				Source: moderation.Source{
					Kind: "file", Ref: abs, MediaType: mediaTypeFor(abs),
				},
				Workflows:      scanWorkflows,
				DedupThreshold: dedupOverride,
				SubmittedAt:    time.Now().UTC(),
			}
			env, disp, perr := p.ProcessJob(cmd.Context(), j)
			if disp != queue.Ack {
				exit = 2
				log.Error("scan job failed", "input", path, "err", perr)
				continue
			}
			if env.Result != nil && env.Result.Overall.Verdict != moderation.VerdictAllow && exit == 0 {
				exit = 1
			}
		}
		if exit != 0 {
			os.Exit(exit)
		}
		return nil
	},
}

func init() {
	scanCmd.Flags().StringSliceVar(&scanWorkflows, "workflow", nil,
		"FFmpeg workflow(s) for video inputs (repeatable or comma-separated); default: configured default_workflow")
	scanCmd.Flags().IntVar(&scanDedupThreshold, "dedup-threshold", 0,
		"near-duplicate removal override: 0..64 = Hamming threshold, -1 = disable; default: configured frames.dedup")
	rootCmd.AddCommand(scanCmd)
}
