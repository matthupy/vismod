package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/matthupy/vismod/pkg/moderation"
	"github.com/spf13/viper"
)

// defaults are applied before file/env overlay.
func setDefaults(v *viper.Viper) {
	v.SetDefault("adapter.name", "stub")

	v.SetDefault("thresholds.default.flag_at", 0.5)
	v.SetDefault("thresholds.default.block_at", 0.8)
	// SEXUAL is strictest by default.
	v.SetDefault("thresholds.SEXUAL.flag_at", 0.5)
	v.SetDefault("thresholds.SEXUAL.block_at", 0.667)

	v.SetDefault("queue.driver", "memory")
	v.SetDefault("queue.workers", 4)
	v.SetDefault("queue.buffer", 64)
	v.SetDefault("queue.max_retries", 3)
	v.SetDefault("queue.retry_backoff", "500ms")
	v.SetDefault("queue.drain_timeout", "30s")
	v.SetDefault("queue.job_timeout", "60s")
	v.SetDefault("queue.deadletter_max", 1024)
	v.SetDefault("queue.redis_addr", "localhost:6379")
	v.SetDefault("queue.dedup_ttl", "168h") // 7d; must exceed the redelivery window

	v.SetDefault("frames.workdir", "")
	v.SetDefault("frames.max_frames", 64)
	v.SetDefault("frames.concurrency", 4)
	v.SetDefault("frames.scene", true)
	v.SetDefault("frames.keyframe", true)
	v.SetDefault("frames.temporal", true)
	v.SetDefault("frames.mpdecimate", true)
	// videosift extraction tuning; defaults mirror videosift.DefaultConfig() so an
	// absent key resolves to the upstream default rather than a zero value.
	v.SetDefault("frames.scene_threshold", 0.4)
	v.SetDefault("frames.temporal_interval", 2.0)
	v.SetDefault("frames.mpdecimate_hi", 768)
	v.SetDefault("frames.mpdecimate_lo", 320)
	v.SetDefault("frames.mpdecimate_frac", 0.33)
	v.SetDefault("frames.hash_algo", "phash")
	v.SetDefault("frames.hamming_threshold", 8)
	v.SetDefault("frames.hash_resize_width", 256)
	v.SetDefault("frames.videosift_concurrency", 0) // 0 => videosift uses #enabled strategies
	v.SetDefault("frames.ffmpeg_path", "")
	v.SetDefault("frames.ffprobe_path", "")

	v.SetDefault("log.level", "info")
	v.SetDefault("metrics.addr", ":9090")
	v.SetDefault("audit.path", "")
}

// Load reads config from an optional file plus the VISMOD_ env overlay.
// Secrets are env-only and are never decoded into Config.
//
// When path is empty, the config-file path falls back to the VISMOD_CONFIG env
// var (flag > env > none). This lets the container HEALTHCHECK — which runs
// `vismod healthcheck` with no --config flag — resolve the SAME metrics.addr as
// a `serve` started against a mounted config file, by pointing both at it via
// one env var instead of baking a file into the slim image.
func Load(path string) (Config, error) {
	if path == "" {
		path = os.Getenv("VISMOD_CONFIG")
	}

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
	c.AuditPath = v.GetString("audit.path")
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
		PerCategory: map[moderation.Category]CategoryThreshold{},
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
	return t
}

func (c Config) validate() error {
	if c.Adapter.Name == "" {
		return fmt.Errorf("config: adapter.name is required")
	}
	switch c.Queue.Driver {
	case "memory":
	case "redis":
		if c.Queue.RedisAddr == "" {
			return fmt.Errorf("config: queue.redis_addr is required when queue.driver=redis")
		}
	default:
		return fmt.Errorf("config: queue.driver must be memory|redis, got %q", c.Queue.Driver)
	}
	if c.Frames.MaxFrames <= 0 {
		return fmt.Errorf("config: frames.max_frames must be > 0 (bounds per-video cost and disk)")
	}
	// hash_algo is cast to videosift.HashAlgo at the seam, where an unknown value
	// silently falls back to phash. Reject typos at Load so a misconfigured algo
	// fails fast instead of quietly degrading. Empty is fine — it keeps
	// videosift.DefaultConfig()'s algorithm (see internal/frames/videosift.go).
	switch c.Frames.HashAlgo {
	case "", "phash", "dhash":
	default:
		return fmt.Errorf("config: frames.hash_algo must be phash|dhash (or empty), got %q", c.Frames.HashAlgo)
	}
	// Lower bound is EXCLUSIVE: videosift re-defaults an explicit 0 (scene_threshold
	// 0→0.4, mpdecimate_frac 0→0.33), so 0 silently becomes the default instead of
	// what the user typed. Reject it. An ABSENT key still passes — setDefaults seeds
	// both above, and validate runs on the resolved (post-default) config.
	if c.Frames.SceneThreshold <= 0 || c.Frames.SceneThreshold > 1 {
		return fmt.Errorf("config: frames.scene_threshold must be in (0, 1], got %v", c.Frames.SceneThreshold)
	}
	if c.Frames.MPDecimateFrac <= 0 || c.Frames.MPDecimateFrac > 1 {
		return fmt.Errorf("config: frames.mpdecimate_frac must be in (0, 1], got %v", c.Frames.MPDecimateFrac)
	}
	// temporal_interval / mpdecimate_hi / mpdecimate_lo get the SAME reject-0 as
	// scene_threshold / mpdecimate_frac: videosift re-defaults an explicit 0 (2.0,
	// 768, 320 — see videosift extract.go:81-95), so a user's typed 0 is silently
	// swapped for the default. Reject <= 0 so a degenerate value fails fast.
	if c.Frames.TemporalInterval <= 0 {
		return fmt.Errorf("config: frames.temporal_interval must be > 0, got %v", c.Frames.TemporalInterval)
	}
	if c.Frames.MPDecimateHi <= 0 {
		return fmt.Errorf("config: frames.mpdecimate_hi must be > 0, got %d", c.Frames.MPDecimateHi)
	}
	if c.Frames.MPDecimateLo <= 0 {
		return fmt.Errorf("config: frames.mpdecimate_lo must be > 0, got %d", c.Frames.MPDecimateLo)
	}
	// hamming_threshold / hash_resize_width DIFFER: videosift honors their 0 as a
	// "disable" signal (0 disables hash dedup / rescale — see videosift extract.go:99-102),
	// so 0 is a legitimate value that must pass. Only a negative is meaningless; reject < 0.
	if c.Frames.HammingThreshold < 0 {
		return fmt.Errorf("config: frames.hamming_threshold must be >= 0 (0 disables dedup), got %d", c.Frames.HammingThreshold)
	}
	if c.Frames.HashResizeWidth < 0 {
		return fmt.Errorf("config: frames.hash_resize_width must be >= 0 (0 disables rescale), got %d", c.Frames.HashResizeWidth)
	}
	// Sanity-check the decision policy: the default and every per-category override.
	if err := validateCategoryThreshold("default", c.Thresholds.Default); err != nil {
		return err
	}
	for cat, ct := range c.Thresholds.PerCategory {
		if err := validateCategoryThreshold(string(cat), ct); err != nil {
			return err
		}
	}
	return nil
}

// validateCategoryThreshold sanity-checks one category's decision boundaries.
// flag_at must lie in the [0, 1] score range, and block_at must sit at or above
// flag_at (a flag boundary past the block boundary would let a score block
// without ever flagging). block_at is INTENTIONALLY allowed to exceed 1.0 — that
// is the documented "never auto-block" lever: scores are [0,1] and the rollup
// uses >=, so block_at > 1.0 routes every above-flag hit to manual review
// instead of auto-removing. There is deliberately NO upper bound on block_at.
func validateCategoryThreshold(name string, ct CategoryThreshold) error {
	if ct.FlagAt < 0 || ct.FlagAt > 1 {
		return fmt.Errorf("config: thresholds.%s.flag_at must be in [0, 1], got %v", name, ct.FlagAt)
	}
	if ct.BlockAt < ct.FlagAt {
		return fmt.Errorf("config: thresholds.%s.block_at must be >= flag_at (%v), got %v", name, ct.FlagAt, ct.BlockAt)
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

// The adapter-option registry is the SINGLE SOURCE OF TRUTH that classifies
// every known adapter.options key into exactly one of two partitions:
// verdict-affecting (hashed into ModelFingerprint) or operational-only (never
// hashed). The keys mirror what the adapters actually decode today (see
// internal/moderate/adapters/{azure,hive}/options.go): azure reads endpoint,
// auth_mode, api_version, rps, max_retries, retry_backoff; hive reads endpoint,
// rps, max_retries, retry_backoff. model/model_id are forward-looking verdict
// selectors no adapter sets yet; timeout is a documented operational knob.
//
// 🔴 MAINTENANCE — FAIL-OPEN WARNING: ModelFingerprint hashes ONLY
// verdictAffectingOptionKeys. A new adapter.options key that is absent from this
// registry is SILENTLY un-guarded — a rolling deploy could swap the model via
// that key WITHOUT dead-lettering (§L). So any NEW key an adapter decodes MUST be
// added to one partition below, classified by the single question "does this
// change the score a model returns?" — yes → verdictAffectingOptionKeys, no →
// operationalOptionKeys. This is not advisory: TestKnownOptionKeysPinned pins the
// exact union, so an unclassified key fails CI instead of going unguarded.

// verdictAffectingOptionKeys is the WHITELIST of adapter.options keys that
// participate in ModelFingerprint. Each selects the model / API contract whose
// scores the verdict is computed from, so changing one means "a different model"
// and MUST trip the §L deploy guard:
//
//   - api_version — pins the provider's model/behavior version (azure §D contract)
//   - auth_mode   — auth path can route to a different model deployment
//   - endpoint    — which resource/region/host (and thus which deployed model) answers
//   - model       — explicit model selector (forward-looking; no adapter sets it yet)
//   - model_id    — explicit model-deployment id (forward-looking)
var verdictAffectingOptionKeys = []string{
	"api_version",
	"auth_mode",
	"endpoint",
	"model",
	"model_id",
}

// operationalOptionKeys are DELIBERATELY EXCLUDED from ModelFingerprint — they
// tune throughput and resilience, NOT the verdict. Folding them in made a rolling
// deploy that merely retuned rps/max_retries compute a different fingerprint and
// spuriously dead-letter in-flight jobs (§L). They never change what score a model
// returns, so they never belong in the model identity. Listed here (not just
// "everything not whitelisted") so a new operational key is still a DELIBERATE,
// test-pinned classification — see the 🔴 MAINTENANCE note above.
var operationalOptionKeys = []string{
	"max_retries",
	"retry_backoff",
	"rps",
	"timeout",
}

// knownOptionKeys returns every classified adapter.options key (both partitions).
// It exists so a test can pin the FULL known set: a new key that an adapter reads
// but no one classified will not appear here, failing the pin and forcing a
// deliberate verdict-vs-operational decision before the key can ship unguarded.
func knownOptionKeys() []string {
	keys := make([]string, 0, len(verdictAffectingOptionKeys)+len(operationalOptionKeys))
	keys = append(keys, verdictAffectingOptionKeys...)
	keys = append(keys, operationalOptionKeys...)
	return keys
}

// ModelFingerprint is a SHA-256 over the canonicalized DEPLOY-affecting config:
// the adapter name + the VERDICT-AFFECTING adapter.options keys + the resolved
// per-category threshold map. It is the boot-knowable identity of the loaded
// model (§L), stamped on every enqueued job so a worker running a different model
// can dead-letter (not silently process) a job that requires another model under
// a rolling deploy.
//
// Distinct from ConfigHash — do NOT merge the two:
//   - ConfigHash: per-job audit provenance; folds in the adapter's RUNTIME-reported
//     ModelVersion; excludes adapter.options.
//   - ModelFingerprint: boot-time deploy guard; folds in the WHITELISTED
//     adapter.options keys (api_version/model/model_id/endpoint/auth_mode); no
//     runtime model version.
//
// SCOPING: only verdictAffectingOptionKeys are hashed, NOT the whole options map.
// Operational knobs (rps/max_retries/timeout/retry_backoff) have no verdict impact,
// so tuning them in a rolling deploy must not change the fingerprint — otherwise it
// spuriously trips the dead-letter guard.
//
// CANONICALIZATION (the #1 correctness landmine): adapter.options is a
// map[string]any from viper. Naive fmt formatting over a Go map yields random key
// order => a non-deterministic hash => replicas with identical config compute
// different fingerprints and dead-letter each other. We iterate the whitelist in a
// FIXED (sorted) order and json.Marshal each present value individually; json.Marshal
// sorts map keys lexicographically AND recursively (Go stdlib guarantee), so nested
// values stay canonical and map-order-invariant — no custom encoder, no JCS dep.
func (c Config) ModelFingerprint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "adapter=%s\n", c.Adapter.Name)

	// The deploy surface, scoped to verdict-affecting keys in a fixed order. Each
	// present value is marshaled individually so nested maps stay key-sorted and
	// order-invariant. An absent key contributes nothing (so adding an unrelated
	// operational key never moves the hash). The error path is unreachable for
	// viper-sourced scalar values; fall back to a stable sentinel rather than a
	// nondeterministic Sprintf.
	keys := append([]string(nil), verdictAffectingOptionKeys...)
	sort.Strings(keys)
	for _, k := range keys {
		val, ok := c.Adapter.Options[k]
		if !ok {
			continue
		}
		valJSON, err := json.Marshal(val)
		if err != nil {
			valJSON = []byte("null")
		}
		fmt.Fprintf(&b, "opt.%s=%s\n", k, valJSON)
	}

	fmt.Fprintf(&b, "default=flag:%g,block:%g\n", c.Thresholds.Default.FlagAt, c.Thresholds.Default.BlockAt)

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
