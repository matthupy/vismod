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
	// SexualPotentialCSAM is the SEXUAL score at/above which a frame is treated
	// as potential-CSAM and diverted (default 0.667 = Azure severity 4).
	SexualPotentialCSAM float64 `mapstructure:"-"`
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
}
