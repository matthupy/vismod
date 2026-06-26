// Package config loads and validates the typed application configuration.
// Secrets are env-only (VISMOD_ prefix, dots -> underscores) and never read
// from yaml. Boot fails fast on missing required values.
package config

import (
	"time"

	"github.com/matthupy/vismod/pkg/moderation"
)

// CategoryThreshold holds the two decision boundaries for one category.
// Scores are within-provider comparable only — thresholds are per-adapter and
// NOT portable across providers.
type CategoryThreshold struct {
	FlagAt  float64 `mapstructure:"flag_at"`
	BlockAt float64 `mapstructure:"block_at"`
}

// Thresholds is the resolved decision policy.
type Thresholds struct {
	Default     CategoryThreshold                         `mapstructure:"default"`
	PerCategory map[moderation.Category]CategoryThreshold `mapstructure:"-"`
}

// For returns the threshold for category c, falling back to Default.
func (t Thresholds) For(c moderation.Category) CategoryThreshold {
	if ct, ok := t.PerCategory[c]; ok {
		return ct
	}
	return t.Default
}

// AdapterConfig is the configured adapter selection.
type AdapterConfig struct {
	Name    string         `mapstructure:"name"`
	Options map[string]any `mapstructure:"options"`
}

// QueueConfig mirrors queue.QueueConfig's scalar knobs (driver-agnostic).
type QueueConfig struct {
	Driver        string        `mapstructure:"driver"` // "memory" | "redis"
	Workers       int           `mapstructure:"workers"`
	Buffer        int           `mapstructure:"buffer"`
	MaxRetries    int           `mapstructure:"max_retries"`
	RetryBackoff  time.Duration `mapstructure:"retry_backoff"`
	DrainTimeout  time.Duration `mapstructure:"drain_timeout"`
	JobTimeout    time.Duration `mapstructure:"job_timeout"`
	DeadLetterMax int           `mapstructure:"deadletter_max"`
	RedisAddr     string        `mapstructure:"redis_addr"`
	// DedupTTL bounds the lifetime of a cross-process dedup claim (redis driver,
	// §L issue #9). MUST exceed the maximum redelivery window (asynq retention +
	// retry backoff). Ignored by the memory driver (single-process).
	DedupTTL time.Duration `mapstructure:"dedup_ttl"`
}

// FramesConfig tunes frame extraction and per-job fan-out.
type FramesConfig struct {
	WorkDir     string `mapstructure:"workdir"`
	MaxFrames   int    `mapstructure:"max_frames"`
	Concurrency int    `mapstructure:"concurrency"`
	Scene       bool   `mapstructure:"scene"`
	Keyframe    bool   `mapstructure:"keyframe"`
	Temporal    bool   `mapstructure:"temporal"`
	MPDecimate  bool   `mapstructure:"mpdecimate"`
	// videosift extraction tuning. These are NOT verdict-affecting (consistent
	// with the scene/keyframe/temporal/mpdecimate toggles) and are intentionally
	// excluded from ConfigHash/ModelFingerprint. Defaults mirror
	// videosift.DefaultConfig(); an absent key resolves to that default via
	// setDefaults so it never zeroes a meaningful value.
	// SceneThreshold is the scene-change score in (0, 1]; LOWER is more sensitive.
	// 0 is NOT max-sensitive — videosift re-defaults 0 -> 0.4, so a true 0 is
	// unreachable. Validated to (0, 1] at Load (explicit 0 is rejected).
	//
	// TemporalInterval/MPDecimateHi/MPDecimateLo are validated > 0 at Load for the
	// same reason: videosift re-defaults their explicit 0 (2.0 / 768 / 320), so a
	// typed 0 would be silently swapped. HammingThreshold/HashResizeWidth are the
	// exception — videosift honors 0 as a "disable" signal (dedup / rescale off),
	// so they are validated >= 0 (0 is a valid value, only negatives are rejected).
	SceneThreshold   float64 `mapstructure:"scene_threshold"`
	TemporalInterval float64 `mapstructure:"temporal_interval"`
	MPDecimateHi     int     `mapstructure:"mpdecimate_hi"`
	MPDecimateLo     int     `mapstructure:"mpdecimate_lo"`
	// MPDecimateFrac is the changed-block fraction in (0, 1]; 0 is re-defaulted to
	// 0.33 upstream, so it is not a usable value. Validated to (0, 1] at Load.
	MPDecimateFrac       float64 `mapstructure:"mpdecimate_frac"`
	HashAlgo             string  `mapstructure:"hash_algo"` // "phash" | "dhash"
	HammingThreshold     int     `mapstructure:"hamming_threshold"`
	HashResizeWidth      int     `mapstructure:"hash_resize_width"`
	VideosiftConcurrency int     `mapstructure:"videosift_concurrency"`
	// Binary overrides for the videosift extractor; empty => discovered on PATH.
	FFmpegPath  string `mapstructure:"ffmpeg_path"`
	FFprobePath string `mapstructure:"ffprobe_path"`
}

// Config is the full typed configuration.
type Config struct {
	Adapter     AdapterConfig `mapstructure:"adapter"`
	Thresholds  Thresholds    `mapstructure:"thresholds"`
	Queue       QueueConfig   `mapstructure:"queue"`
	Frames      FramesConfig  `mapstructure:"frames"`
	LogLevel    string        `mapstructure:"-"`
	MetricsAddr string        `mapstructure:"-"`
	// AuditPath is the file path for the tamper-evident decision log (§G.5).
	// Empty disables the audit trail (acceptable for one-shot CLI scans; a
	// production `serve` should set it).
	AuditPath string `mapstructure:"-"`
}
