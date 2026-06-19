package cli

import (
	"github.com/matthupy/vismod/internal/pipeline"
	"github.com/matthupy/vismod/internal/result"
	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/spf13/cobra"
)

func newScanCmd() *cobra.Command {
	var mediaType string
	cmd := &cobra.Command{
		Use:   "scan <path>",
		Short: "Moderate a single image or video file (one-shot)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, log, err := loadConfigAndLogger()
			if err != nil {
				return err
			}

			src := moderation.Source{Kind: "file", Ref: args[0], MediaType: mediaType}

			// Boot-probe ffmpeg/ffprobe only when a video is involved, so a
			// missing binary fails fast with a clear error instead of an
			// error-verdict envelope (image scans need no ffmpeg).
			if pipeline.DetectMediaType(src) == "video" {
				if err := probeFrameSource(cfg); err != nil {
					return err
				}
			}

			sink := result.NewJSONLSink(cmd.OutOrStdout())
			p, mod, err := buildPipeline(cfg, sink, log)
			if err != nil {
				return err
			}
			defer mod.Close()

			return p.Process(cmd.Context(), result.JobID("scan-1"), src)
		},
	}
	cmd.Flags().StringVar(&mediaType, "media-type", "", "force media type: image|video (default: auto-detect)")
	return cmd
}
