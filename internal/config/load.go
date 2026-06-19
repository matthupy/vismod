package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/spf13/viper"
	"sort"
	"strings"
)

// defaults are applied before file/env overlay.
func setDefaults(v *viper.Viper) {
	v.SetDefault("adapter.name", "stub")

	v.SetDefault("thresholds.default.flag_at", 0.5)
	v.SetDefault("thresholds.default.block_at", 0.8)
	// SEXUAL is strictest by default.
	v.SetDefault("thresholds.SEXUAL.flag_at", 0.5)
	v.SetDefault("thresholds.SEXUAL.block_at", 0.667)
	v.SetDefault("thresholds.SEXUAL.potential_csam", 0.667)

	v.SetDefault("queue.driver", "memory")
	v.SetDefault("queue.workers", 4)
	v.SetDefault("queue.buffer", 64)
	v.SetDefault("queue.max_retries", 3)
	v.SetDefault("queue.retry_backoff", "500ms")
	v.SetDefault("queue.drain_timeout", "30s")
	v.SetDefault("queue.job_timeout", "60s")
	v.SetDefault("queue.deadletter_max", 1024)
	v.SetDefault("queue.redis_addr", "localhost:6379")

	v.SetDefault("frames.workdir", "")
	v.SetDefault("frames.max_frames", 64)
	v.SetDefault("frames.concurrency", 4)
	v.SetDefault("frames.scene", true)
	v.SetDefault("frames.keyframe", true)
	v.SetDefault("frames.temporal", true)
	v.SetDefault("frames.mpdecimate", true)

	v.SetDefault("log.level", "info")
	v.SetDefault("metrics.addr", ":9090")
}

// Load reads config from an optional file plus the VISMOD_ env overlay.
// Secrets are env-only and are never decoded into Config.
func Load(path string) (Config, error) {
	v := viper.New()
	setDefaults(v)

	v.SetEnvPrefix("VISMOD")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return Config{}, fmt.Errorf("config: read %q: %w", path, err)
		}
	}

	var c Config
	// viper's default Unmarshal already applies StringToTimeDurationHookFunc,
	// so "500ms"/"30s" decode into time.Duration fields.
	if err := v.Unmarshal(&c); err != nil {
		return Config{}, fmt.Errorf("config: unmarshal: %w", err)
	}

	// Fields viper's struct-unmarshal can't reach cleanly.
	c.LogLevel = v.GetString("log.level")
	c.MetricsAddr = v.GetString("metrics.addr")
	c.Thresholds = resolveThresholds(v)

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// resolveThresholds builds the per-category map from the thresholds sub-tree,
// which has arbitrary category keys alongside "default".
func resolveThresholds(v *viper.Viper) Thresholds {
	t := Thresholds{
		Default: CategoryThreshold{
			FlagAt:  v.GetFloat64("thresholds.default.flag_at"),
			BlockAt: v.GetFloat64("thresholds.default.block_at"),
		},
		PerCategory:         map[moderation.Category]CategoryThreshold{},
		SexualPotentialCSAM: v.GetFloat64("thresholds.SEXUAL.potential_csam"),
	}
	sub := v.Sub("thresholds")
	if sub == nil {
		return t
	}
	for key := range v.GetStringMap("thresholds") {
		if strings.EqualFold(key, "default") {
			continue
		}
		ct := CategoryThreshold{
			FlagAt:  sub.GetFloat64(key + ".flag_at"),
			BlockAt: sub.GetFloat64(key + ".block_at"),
		}
		t.PerCategory[moderation.Category(strings.ToUpper(key))] = ct
	}
	if t.SexualPotentialCSAM == 0 {
		t.SexualPotentialCSAM = 0.667
	}
	return t
}

func (c Config) validate() error {
	if c.Adapter.Name == "" {
		return fmt.Errorf("config: adapter.name is required")
	}
	switch c.Queue.Driver {
	case "memory", "redis":
	default:
		return fmt.Errorf("config: queue.driver must be memory|redis, got %q", c.Queue.Driver)
	}
	if c.Frames.MaxFrames <= 0 {
		return fmt.Errorf("config: frames.max_frames must be > 0 (bounds per-video cost and disk)")
	}
	return nil
}

// ConfigHash is a SHA-256 over the canonicalized verdict-affecting config: the
// adapter name + modelVersion + the resolved per-category threshold map.
// Secrets, log level and addresses are excluded. Stamped into ModelIdentity so
// an audit record's decision is reproducible.
func (c Config) ConfigHash(modelVersion string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "adapter=%s\n", c.Adapter.Name)
	fmt.Fprintf(&b, "model_version=%s\n", modelVersion)
	fmt.Fprintf(&b, "default=flag:%g,block:%g\n", c.Thresholds.Default.FlagAt, c.Thresholds.Default.BlockAt)
	fmt.Fprintf(&b, "sexual_potential_csam=%g\n", c.Thresholds.SexualPotentialCSAM)

	cats := make([]string, 0, len(c.Thresholds.PerCategory))
	for cat := range c.Thresholds.PerCategory {
		cats = append(cats, string(cat))
	}
	sort.Strings(cats)
	for _, cat := range cats {
		ct := c.Thresholds.PerCategory[moderation.Category(cat)]
		fmt.Fprintf(&b, "cat=%s,flag:%g,block:%g\n", cat, ct.FlagAt, ct.BlockAt)
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
