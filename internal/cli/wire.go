package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/vismod/vismod/internal/audit"
	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/fetch"
	"github.com/vismod/vismod/internal/frames"
	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/pipeline"
	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

// buildModerator resolves the single active Moderator from config.
//
// M0 bootstrap only: while the registry is EMPTY (no adapter packages are
// linked yet) and no adapter is configured, a built-in benign fake keeps
// the skeleton runnable with no credentials or network. The moment any
// real adapter registers (M1), this path is unreachable: an unset
// adapter.name then fails fast listing the registered names. There is no
// "stub" adapter in the shipped registry.
func buildModerator(cfg config.Config, log *slog.Logger) (moderation.Moderator, error) {
	if cfg.Adapter.Name == "" && len(moderate.Registered()) == 0 {
		log.Warn("no adapters registered (M0 skeleton); using built-in fake moderator — NOT a real model")
		return devFakeModerator{}, nil
	}
	return moderate.New(cfg.Adapter.Name, moderate.AdapterConfig{
		Options: cfg.Adapter.Options,
		Secret:  config.Secret(),
		// The RAW mode: Load resolves it away for the runtime map, but an
		// adapter whose scores are only meaningful under one mode has to
		// refuse the others at construction.
		ProviderThresholdMode: cfg.ProviderThresholds.Mode,
	})
}

// newFetcher builds the url-source fetcher. Construction IS boot
// validation: a bad allow-list fails here.
func newFetcher(cfg config.Config) (pipeline.SourceFetcher, error) {
	f, err := fetch.New(fetch.Config{
		AllowHosts:        cfg.Source.URL.AllowHosts,
		AllowPrivateHosts: cfg.Source.URL.AllowPrivateHosts,
		MaxBytes:          cfg.Source.URL.MaxBytes,
		Timeout:           cfg.Source.URL.Timeout,
		MaxAttempts:       cfg.Source.URL.MaxAttempts,
		AllowedMediaTypes: cfg.Source.URL.AllowedMediaTypes,
	})
	if err != nil {
		return nil, err
	}
	if f == nil {
		// fetch.New does not currently return a nil Fetcher, but if it ever
		// does again, returning the typed nil *Fetcher would produce a
		// non-nil interface holding a nil pointer, so pipeline's
		// `p.Fetcher == nil` would be false and url jobs would panic
		// instead of erroring. Do not simplify this away.
		return nil, nil
	}
	return f, nil
}

// fetchRecorder adapts the fetch metrics to pipeline.OnFetch, keeping
// Prometheus out of the pipeline package. reason is always a value from
// fetch.Reason's fixed label set, never an error string.
func fetchRecorder(m *observe.Metrics) func(time.Duration, int64, string) {
	if m == nil {
		return nil
	}
	return func(d time.Duration, n int64, reason string) {
		m.FetchSeconds.Observe(d.Seconds())
		if n > 0 {
			m.FetchBytesTotal.Add(float64(n))
		}
		if reason != "" {
			m.FetchFailuresTotal.WithLabelValues(reason).Inc()
		}
	}
}

// buildPipeline assembles the per-job pipeline around the active model.
func buildPipeline(cfg config.Config, mod moderation.Moderator, sink result.Sink, auditLog *audit.Log, f pipeline.SourceFetcher, log *slog.Logger) *pipeline.Pipeline {
	p := &pipeline.Pipeline{
		Moderator:   mod,
		Sink:        sink,
		Fetcher:     f,
		Thresholds:  cfg.Thresholds,
		Concurrency: cfg.Frames.Concurrency,
		ModelID: result.ModelIdentity{
			Adapter:      mod.Name(),
			ModelVersion: modelVersion(mod),
			ConfigHash:   config.ConfigHash(mod.Name(), modelVersion(mod), cfg.Thresholds),
		},
		Log: log,
	}
	if auditLog != nil {
		p.Audit = auditLog
		p.Events = auditLog
	}
	p.AllowEmptyVideoSkip = cfg.Failsafe.AllowEmptyVideoSkip
	p.Dedup = cfg.Frames.Dedup.Enabled
	p.DedupThreshold = cfg.Frames.Dedup.HammingThreshold
	p.MaxScanFrames = cfg.FFmpeg.MaxFrames
	return p
}

// validateDedupThreshold bounds a per-job dedup override against the
// operator's configured ceiling: nil inherits the config; -1 disables;
// 0..ceiling enables at that Hamming distance.
//
// The upper bound is the CONFIGURED threshold, not the dHash width. A
// caller may tighten dedup or switch it off, never loosen it: any pair of
// 64-bit dHashes is within distance 64, so a wide-open override would
// collapse an entire video into its first frame and let that one frame
// decide the verdict. The pipeline re-clamps at execution.
func validateDedupThreshold(v *int, ceiling int) error {
	if v == nil {
		return nil
	}
	if *v < -1 || *v > ceiling {
		return fmt.Errorf("dedup_threshold must be -1 (disable) or 0..%d (frames.dedup.hamming_threshold), got %d", ceiling, *v)
	}
	return nil
}

// validateWorkflowSelection checks a per-job workflow selection against
// the configured (and guardrail-validated) workflow set.
func validateWorkflowSelection(cfg config.Config, names []string) error {
	for _, name := range names {
		if _, ok := cfg.FFmpeg.Workflows[name]; !ok {
			known := make([]string, 0, len(cfg.FFmpeg.Workflows))
			for k := range cfg.FFmpeg.Workflows {
				known = append(known, k)
			}
			sort.Strings(known)
			return fmt.Errorf("unknown workflow %q (configured: %v)", name, known)
		}
	}
	return nil
}

// newFrameSource builds the FFmpeg frame source (direct ffmpeg/ffprobe
// via os/exec, workflow-driven).
func newFrameSource(cfg config.Config, log *slog.Logger) frames.FrameSource {
	return frames.NewFFmpegSource(cfg.FFmpeg, log)
}

// validateFrameBoot is the §F.2 boot validation for the extraction path:
// binaries present + every workflow passes the security guardrails.
func validateFrameBoot(cfg config.Config) error {
	if err := frames.ValidateBinaries(cfg.FFmpeg); err != nil {
		return err
	}
	return frames.ValidateAll(cfg.FFmpeg)
}

// providerLabeler lets an adapter declare the provider labels it can emit,
// without widening the public Moderator interface and without reusing
// Caps.Categories (which holds canonical categories, not provider labels).
type providerLabeler interface{ ProviderLabels() []string }

// validateProviderLabelBoot refuses to start when an adapter can emit a
// provider label that has no KEY under provider_thresholds.labels.
//
// Why this exists: in override mode there is no category or default rung
// left in the ResolveFor chain, so a label with no entry has no flag_at and
// no block_at — it can never flag and never block. A single typo, or a
// model taxonomy bump, therefore disarms a hazard with nothing logged and
// nothing failing. config.Load cannot check this: Load runs BEFORE the
// adapter exists (buildModerator builds the Moderator from the loaded
// config), so the check belongs here, after buildModerator, alongside
// validateFrameBoot.
//
// The check is on key presence, NOT value: an entry with both fields nil is
// a valid "deliberately unarmed", which is a decision the operator wrote
// down, and it still lands in the merged map so config_hash changes and the
// decision stays attributable.
func validateProviderLabelBoot(cfg config.Config, mod moderation.Moderator) error {
	labeler, ok := mod.(providerLabeler)
	if !ok {
		return nil
	}
	configured := make(map[string]bool, len(cfg.ProviderThresholds.Labels))
	for k := range cfg.ProviderThresholds.Labels {
		configured[strings.ToLower(k)] = true
	}
	// config.Load folds these into Labels, but read them directly too: this
	// check must not depend on that folding still happening.
	for _, k := range cfg.ProviderThresholds.UnarmedLabels {
		configured[strings.ToLower(strings.TrimSpace(k))] = true
	}
	var missing []string
	for _, label := range labeler.ProviderLabels() {
		if !configured[strings.ToLower(label)] {
			missing = append(missing, label)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("adapter %q can emit provider label(s) %v with no entry under provider_thresholds.labels: in override mode such a label has no flag_at and no block_at, so it can never flag or block. Add a key per label (an entry with no fields set is a valid \"deliberately unarmed\")",
		mod.Name(), missing)
}

// modelVersioner lets adapters expose their pinned API/model version for
// the audit ModelIdentity without widening the public Moderator interface.
type modelVersioner interface{ ModelVersion() string }

func modelVersion(m moderation.Moderator) string {
	if v, ok := m.(modelVersioner); ok {
		return v.ModelVersion()
	}
	return "unversioned"
}

func openAudit(cfg config.Config) (*audit.Log, error) {
	if !cfg.Audit.Enabled {
		return nil, nil
	}
	l, err := audit.Open(cfg.Audit.Path, nil)
	if err != nil {
		return nil, fmt.Errorf("audit log: %w", err)
	}
	return l, nil
}

// devFakeModerator is the M0-only bootstrap model: deterministic, benign,
// image-only. It is NOT registered in the adapter registry and becomes
// unreachable once real adapters exist.
type devFakeModerator struct{}

func (devFakeModerator) Name() string { return "dev-fake" }

func (devFakeModerator) ModelVersion() string { return "m0-skeleton" }

func (devFakeModerator) Capabilities() moderation.Caps {
	return moderation.Caps{
		SupportsVideo: false,
		MaxImageBytes: 32 << 20,
		Categories: []moderation.Category{
			moderation.CategorySexual, moderation.CategoryViolence,
			moderation.CategoryHate, moderation.CategorySelfHarm,
		},
	}
}

func (devFakeModerator) AnalyzeImage(_ context.Context, _ moderation.Image) (moderation.NormalizedResult, error) {
	score := func(v float64) *float64 { return &v }
	return moderation.NormalizedResult{
		Provider: "dev-fake",
		Frames: []moderation.FrameResult{{
			Status: moderation.FrameOK,
			Categories: []moderation.CategoryResult{
				{Category: moderation.CategorySexual, ProviderLabel: "fake/sexual", Score: score(0.01), ScoreOrigin: moderation.OriginProbability},
				{Category: moderation.CategoryViolence, ProviderLabel: "fake/violence", Score: score(0.02), ScoreOrigin: moderation.OriginProbability},
				{Category: moderation.CategoryHate, ProviderLabel: "fake/hate", Score: score(0.0), ScoreOrigin: moderation.OriginProbability},
				{Category: moderation.CategorySelfHarm, ProviderLabel: "fake/self_harm", Score: score(0.0), ScoreOrigin: moderation.OriginProbability},
			},
		}},
	}, nil
}

func (devFakeModerator) Close() error { return nil }

var _ moderation.Moderator = devFakeModerator{}
