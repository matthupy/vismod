// Package config loads and validates vismod configuration (viper: yaml
// file + VISMOD_* env overlay). Secrets are env-only — never in yaml.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/vismod/vismod/pkg/moderation"
)

// EnvPrefix is the prefix for all vismod environment variables; config
// keys map to env by upper-casing and replacing "." with "_"
// (queue.redis.addr -> VISMOD_QUEUE_REDIS_ADDR).
const EnvPrefix = "VISMOD"

// CategoryThreshold holds the per-category decision boundaries. Pointers
// distinguish "not set" (inherit default) from an explicit value.
// Thresholds are per-adapter and NOT portable across providers.
type CategoryThreshold struct {
	FlagAt  *float64 `mapstructure:"flag_at" json:"flag_at"`
	BlockAt *float64 `mapstructure:"block_at" json:"block_at"`
}

// Thresholds maps a lookup key to its boundaries. Three key namespaces
// share the map and cannot collide:
//   - "default"          — the field-by-field fallback.
//   - "SEXUAL", "HATE"…  — canonical categories, always UPPERCASE.
//   - "label:yes_gambling" — a provider label, always lowercase after the
//     prefix (see ProviderLabelKey).
type Thresholds map[string]CategoryThreshold

// Provider-label threshold modes. The mode is resolved away at config
// load: it decides which entries end up in the Thresholds map, so nothing
// downstream has to branch on it.
const (
	// ProviderModeOff ignores provider_thresholds entirely. Category
	// thresholds alone decide. This is the default.
	ProviderModeOff = "off"
	// ProviderModeHybrid keeps category thresholds as the fallback and
	// lets a provider label override them where one is configured.
	ProviderModeHybrid = "hybrid"
	// ProviderModeOverride drops the category and default thresholds
	// completely: ONLY configured provider labels carry boundaries. A
	// label with no entry has no flag_at and no block_at, so it can never
	// flag and never block. See the fail-safe note on ProviderThresholds.
	ProviderModeOverride = "override"
)

const providerLabelPrefix = "label:"

// ProviderLabelKey builds the Thresholds key for a provider label. Labels
// are matched case-insensitively: viper lowercases yaml map keys, and
// vendors are inconsistent (hive emits `yes_gambling`, microsoft `Sexual`).
func ProviderLabelKey(label string) string {
	return providerLabelPrefix + strings.ToLower(label)
}

// ProviderThresholds is the "advanced" threshold surface: boundaries set
// on a specific vendor label rather than on a canonical category.
//
// FAIL-SAFE NOTE: in "override" mode a label with no configured entry has
// no boundaries at all and therefore cannot flag or block. That is the
// requested semantic, but it means a typo in a label name silently
// disarms that signal. Load rejects "override" with an empty Labels map
// for this reason, and the config_hash stamped on every envelope changes
// with these values so a tuning is always attributable.
type ProviderThresholds struct {
	Mode   string     `mapstructure:"mode"`
	Labels Thresholds `mapstructure:"labels"`
	// UnarmedLabels names labels that are DELIBERATELY left with no
	// boundaries. It exists because an adapter that declares its own label
	// set (see internal/cli.validateProviderLabelBoot) refuses to boot when
	// a declared label has no key here — and "keyed but unarmed" cannot be
	// written under Labels at all: viper drops a yaml key whose value has no
	// scalar leaf, so `some_label: {}`, `some_label:` and
	// `some_label: {flag_at: null}` all vanish before decoding (verified
	// 2026-07-29). A list of names survives, so the decision can be written
	// down. Load folds each name into Labels as a boundary-less entry, which
	// keeps it in the merged map and therefore inside ConfigHash.
	UnarmedLabels []string `mapstructure:"unarmed_labels"`
}

// withUnarmed returns Labels with every unarmed_labels name present as a
// boundary-less entry. That is what makes "keyed but deliberately unarmed"
// expressible at all: the boot-time completeness check looks for a KEY, and
// viper cannot carry a yaml key with no scalar leaf.
func (p ProviderThresholds) withUnarmed() Thresholds {
	out := make(Thresholds, len(p.Labels)+len(p.UnarmedLabels))
	for k, v := range p.Labels {
		out[k] = v
	}
	for _, name := range p.UnarmedLabels {
		out[strings.ToLower(strings.TrimSpace(name))] = CategoryThreshold{}
	}
	return out
}

// normalized upper-cases category keys (viper lowercases map keys) and
// keeps the "default" key lowercase.
func (t Thresholds) normalized() Thresholds {
	out := make(Thresholds, len(t))
	for k, v := range t {
		if strings.EqualFold(k, "default") {
			out["default"] = v
			continue
		}
		out[strings.ToUpper(k)] = v
	}
	return out
}

// Merge folds provider-label thresholds into the category map according to
// the mode, producing the single map the pipeline resolves against. Load
// calls this once; after it, the mode no longer exists as a runtime
// concept.
func (t Thresholds) Merge(p ProviderThresholds) Thresholds {
	mode := strings.ToLower(strings.TrimSpace(p.Mode))
	if mode == "" {
		mode = ProviderModeOff
	}
	out := make(Thresholds, len(t)+len(p.Labels))
	if mode != ProviderModeOverride {
		for k, v := range t {
			out[k] = v
		}
	}
	if mode != ProviderModeOff {
		for label, v := range p.Labels {
			out[ProviderLabelKey(label)] = v
		}
		// Unarmed labels land as boundary-less entries: they can never flag
		// or block (that is the request), but they are present, so
		// ConfigHash covers the decision and it stays attributable.
		for _, label := range p.UnarmedLabels {
			out[ProviderLabelKey(label)] = CategoryThreshold{}
		}
	}
	return out
}

// Resolve returns the effective threshold for a category with no provider
// label in hand. It delegates to ResolveFor so there is exactly one
// fallback chain in the codebase.
func (t Thresholds) Resolve(cat moderation.Category) CategoryThreshold {
	return t.ResolveFor(cat, "")
}

// ResolveFor is the ONE threshold resolution path. Both the flagging pass
// (pipeline.ApplyThresholds) and the block check (pipeline.Rollup) call
// it, because a provider label that flags but does not block — or the
// reverse — is a bug that a second copy of this logic would inevitably
// produce.
//
// Precedence is provider label > category > "default", applied
// field-by-field: an override that sets only flag_at still inherits
// block_at from the category, and from default under that.
func (t Thresholds) ResolveFor(cat moderation.Category, providerLabel string) CategoryThreshold {
	chain := make([]CategoryThreshold, 0, 3)
	if providerLabel != "" {
		if v, ok := t[ProviderLabelKey(providerLabel)]; ok {
			chain = append(chain, v)
		}
	}
	if v, ok := t[string(cat)]; ok {
		chain = append(chain, v)
	}
	chain = append(chain, t["default"])

	var out CategoryThreshold
	for _, c := range chain {
		if out.FlagAt == nil {
			out.FlagAt = c.FlagAt
		}
		if out.BlockAt == nil {
			out.BlockAt = c.BlockAt
		}
	}
	return out
}

// WorkflowConfig is one named FFmpeg argument-list template (§B.2). Args
// are rendered via text/template with typed substitution — never a shell
// string.
type WorkflowConfig struct {
	Description string   `mapstructure:"description" json:"description"`
	Args        []string `mapstructure:"args" json:"args"`
}

type FFmpegConfig struct {
	FFmpegPath      string `mapstructure:"ffmpeg_path"`
	FFprobePath     string `mapstructure:"ffprobe_path"`
	DefaultWorkflow string `mapstructure:"default_workflow"`
	// MaxFrames is the SCAN cap: the maximum frames per video that reach
	// the moderation fan-out, applied AFTER all post-processing (dedup).
	// REQUIRED > 0.
	MaxFrames int `mapstructure:"max_frames"`
	// MaxExtractFrames bounds how many frames extraction may MATERIALIZE
	// on disk (the {{.MaxFrames}} template value and the union cap)
	// before post-processing trims toward MaxFrames. 0 derives
	// 4 × MaxFrames. Must be >= MaxFrames when set.
	MaxExtractFrames int                       `mapstructure:"max_extract_frames"`
	MaxWidth         int                       `mapstructure:"max_width"`
	Timeout          time.Duration             `mapstructure:"timeout"`
	Workflows        map[string]WorkflowConfig `mapstructure:"workflows"`
}

// ExtractBudget resolves the effective extraction (disk) bound.
func (f FFmpegConfig) ExtractBudget() int {
	if f.MaxExtractFrames > 0 {
		return f.MaxExtractFrames
	}
	return f.MaxFrames * 4
}

// DedupConfig is the optional post-extraction near-duplicate filter:
// frames whose dHash Hamming distance to an already-kept frame is <=
// hamming_threshold are dropped before moderation.
type DedupConfig struct {
	Enabled          bool `mapstructure:"enabled"`
	HammingThreshold int  `mapstructure:"hamming_threshold"`
}

type FramesConfig struct {
	Concurrency int         `mapstructure:"concurrency"`
	Dedup       DedupConfig `mapstructure:"dedup"`
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"-"` // env-only: VISMOD_QUEUE_REDIS_PASSWORD
	DB       int    `mapstructure:"db"`
}

type QueueConfig struct {
	Driver        string        `mapstructure:"driver"` // "memory" | "redis"
	Workers       int           `mapstructure:"workers"`
	Buffer        int           `mapstructure:"buffer"`
	MaxRetries    int           `mapstructure:"max_retries"`
	RetryBackoff  time.Duration `mapstructure:"retry_backoff"`
	DrainTimeout  time.Duration `mapstructure:"drain_timeout"`
	JobTimeout    time.Duration `mapstructure:"job_timeout"`
	DeadLetterMax int           `mapstructure:"deadletter_max"`
	Redis         RedisConfig   `mapstructure:"redis"`
}

type AdapterSection struct {
	Name    string         `mapstructure:"name"`
	Options map[string]any `mapstructure:"options"`
}

type AuditConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
}

type UIConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Addr    string `mapstructure:"addr"`
	Auth    string `mapstructure:"auth"` // "basic" (credentials env-only) | "none" (loopback only)
}

// SinkConfig is one entry in output.sinks. Fields not relevant to the
// chosen Type are ignored; Validate rejects a Type whose required field
// is missing rather than silently emitting nowhere.
type SinkConfig struct {
	Type string `mapstructure:"type"` // "stdout" | "file" | "webhook"
	Path string `mapstructure:"path"` // file
	// URL is OPERATOR configuration, never job input. It is the same
	// trust class as a provider endpoint (SECURITY.md): private ranges
	// are expected and allowed. Do NOT apply the media-source deny-list.
	URL         string        `mapstructure:"url"`          // webhook
	Timeout     time.Duration `mapstructure:"timeout"`      // webhook
	MaxAttempts int           `mapstructure:"max_attempts"` // webhook
}

// OutputConfig selects where result envelopes go. An absent block means
// stdout. A present block with an empty list refuses to boot: emitting
// nothing silently is the failure mode this project exists to prevent.
type OutputConfig struct {
	Sinks []SinkConfig `mapstructure:"sinks"`
}

type FailsafeConfig struct {
	// AllowEmptyVideoSkip is the gated §F.5 override: when true, a video
	// that extracts ZERO frames is acked as an operational SKIP (no
	// verdict emitted, prominent audit event) instead of the default
	// Verdict=error + dead-letter. Non-default; enabling it accepts that
	// a static/looping harmful video can pass unevaluated.
	AllowEmptyVideoSkip bool `mapstructure:"allow_empty_video_skip"`
}

type BackpressureConfig struct {
	ConsecutiveErrors int           `mapstructure:"consecutive_errors"` // N
	ErrorRatePct      float64       `mapstructure:"error_rate_pct"`     // X
	Window            time.Duration `mapstructure:"window"`             // W
	RecoverySuccesses int           `mapstructure:"recovery_successes"` // M (hysteresis)
}

type Config struct {
	Adapter            AdapterSection     `mapstructure:"adapter"`
	Thresholds         Thresholds         `mapstructure:"thresholds"`
	ProviderThresholds ProviderThresholds `mapstructure:"provider_thresholds"`
	FFmpeg             FFmpegConfig       `mapstructure:"ffmpeg"`
	Frames             FramesConfig       `mapstructure:"frames"`
	Queue              QueueConfig        `mapstructure:"queue"`
	Audit              AuditConfig        `mapstructure:"audit"`
	UI                 UIConfig           `mapstructure:"ui"`
	Output             OutputConfig       `mapstructure:"output"`
	Backpressure       BackpressureConfig `mapstructure:"backpressure"`
	Failsafe           FailsafeConfig     `mapstructure:"failsafe"`
	LogLevel           string             `mapstructure:"log_level"`
	MetricsAddr        string             `mapstructure:"metrics_addr"`
	IntakeAddr         string             `mapstructure:"intake_addr"`
}

func f64(v float64) *float64 { return &v }

// DefaultWorkflows are the three standard extraction workflows. They are
// ordinary configuration — part of the built-in defaults, listed in
// config.example.yaml, overridable or replaceable per name in yaml, and
// validated by the same guardrail gate as user-authored workflows.
// showinfo in the filter graph lets frame timestamps (pts_time) be
// recovered from ffmpeg's log.
func DefaultWorkflows() map[string]WorkflowConfig {
	return map[string]WorkflowConfig{
		"scene-detect": {
			Description: "Extract one frame per detected scene change (select='gt(scene,0.4)').",
			Args: []string{
				"-hide_banner", "-nostdin", "-y", "-i", "{{.Input}}",
				"-vf", "select='gt(scene,0.4)',scale={{.MaxWidth}}:-1,showinfo",
				"-vsync", "vfr", "-frames:v", "{{.MaxFrames}}",
				"{{.WorkDir}}/frame-%06d.png",
			},
		},
		"keyframe": {
			Description: "Extract keyframes (I-frames) only.",
			Args: []string{
				"-hide_banner", "-nostdin", "-y", "-skip_frame", "nokey", "-i", "{{.Input}}",
				"-vf", "showinfo", "-vsync", "vfr", "-frames:v", "{{.MaxFrames}}",
				"{{.WorkDir}}/frame-%06d.png",
			},
		},
		"interval": {
			Description: "Extract one frame every 2 seconds.",
			Args: []string{
				"-hide_banner", "-nostdin", "-y", "-i", "{{.Input}}",
				"-vf", "fps=1/2,scale={{.MaxWidth}}:-1,showinfo",
				"-frames:v", "{{.MaxFrames}}",
				"{{.WorkDir}}/frame-%06d.png",
			},
		},
	}
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		// Keys are lowercase here because viper lowercases yaml map keys;
		// Load normalizes everything to the canonical uppercase form. A
		// yaml override for "sexual" then merges into this same entry
		// instead of colliding with an uppercase duplicate.
		Thresholds: Thresholds{
			"default": {FlagAt: f64(0.5), BlockAt: f64(0.8)},
			"sexual":  {FlagAt: f64(0.4), BlockAt: f64(0.7)},
		},
		FFmpeg: FFmpegConfig{
			FFmpegPath:      "ffmpeg",
			FFprobePath:     "ffprobe",
			DefaultWorkflow: "scene-detect",
			MaxFrames:       64,
			MaxWidth:        1280,
			Timeout:         120 * time.Second,
			Workflows:       DefaultWorkflows(),
		},
		Frames: FramesConfig{Concurrency: 4, Dedup: DedupConfig{Enabled: false, HammingThreshold: 8}},
		Queue: QueueConfig{
			Driver:        "memory",
			Workers:       4,
			Buffer:        1024,
			MaxRetries:    3,
			RetryBackoff:  2 * time.Second,
			DrainTimeout:  30 * time.Second,
			JobTimeout:    5 * time.Minute,
			DeadLetterMax: 1000,
			Redis:         RedisConfig{Addr: "localhost:6379"},
		},
		Audit:        AuditConfig{Enabled: true, Path: "audit.log"},
		UI:           UIConfig{Enabled: false, Addr: "127.0.0.1:8081", Auth: "basic"},
		Output:       OutputConfig{Sinks: []SinkConfig{{Type: "stdout"}}},
		Backpressure: BackpressureConfig{ConsecutiveErrors: 20, ErrorRatePct: 50, Window: 60 * time.Second, RecoverySuccesses: 5},
		LogLevel:     "info",
		MetricsAddr:  ":9090",
		IntakeAddr:   "127.0.0.1:8080",
	}
}

// Load reads config from an optional file path plus the VISMOD_* env
// overlay, over the built-in defaults.
func Load(path string) (Config, error) {
	v := viper.New()
	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	cfg := Defaults()
	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
	}
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse: %w", err)
	}
	cfg.Thresholds = cfg.Thresholds.normalized()
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	// Fold provider-label thresholds in AFTER validation, so validation
	// reports errors against the keys the operator actually wrote. From
	// here on the mode is resolved away and cfg.Thresholds is the single
	// map every verdict is decided against — including ConfigHash.
	cfg.ProviderThresholds.Labels = cfg.ProviderThresholds.withUnarmed()
	cfg.Thresholds = cfg.Thresholds.Merge(cfg.ProviderThresholds)
	return cfg, nil
}

// Validate enforces boot-time invariants that don't need external probes
// (ffmpeg/adapter/Redis probes are separate boot validation steps).
func Validate(cfg Config) error {
	if cfg.FFmpeg.MaxFrames <= 0 {
		return fmt.Errorf("config: ffmpeg.max_frames is required and must be > 0 (hard scan cap)")
	}
	if cfg.FFmpeg.MaxExtractFrames != 0 && cfg.FFmpeg.MaxExtractFrames < cfg.FFmpeg.MaxFrames {
		return fmt.Errorf("config: ffmpeg.max_extract_frames (%d) must be >= max_frames (%d)",
			cfg.FFmpeg.MaxExtractFrames, cfg.FFmpeg.MaxFrames)
	}
	if cfg.Frames.Concurrency <= 0 {
		return fmt.Errorf("config: frames.concurrency must be > 0")
	}
	if cfg.Frames.Dedup.Enabled && (cfg.Frames.Dedup.HammingThreshold < 0 || cfg.Frames.Dedup.HammingThreshold > 64) {
		return fmt.Errorf("config: frames.dedup.hamming_threshold must be in [0,64], got %d", cfg.Frames.Dedup.HammingThreshold)
	}
	switch cfg.Queue.Driver {
	case "memory", "redis":
	default:
		return fmt.Errorf("config: queue.driver must be \"memory\" or \"redis\", got %q", cfg.Queue.Driver)
	}
	for name, th := range cfg.Thresholds {
		for label, p := range map[string]*float64{"flag_at": th.FlagAt, "block_at": th.BlockAt} {
			if p != nil && (*p < 0 || *p > 1) {
				return fmt.Errorf("config: thresholds.%s.%s must be in [0,1], got %v", name, label, *p)
			}
		}
	}
	if err := validateProviderThresholds(cfg.ProviderThresholds); err != nil {
		return err
	}
	if err := validateOutput(cfg.Output); err != nil {
		return err
	}
	return nil
}

func validateProviderThresholds(p ProviderThresholds) error {
	mode := strings.ToLower(strings.TrimSpace(p.Mode))
	switch mode {
	case "", ProviderModeOff, ProviderModeHybrid, ProviderModeOverride:
	default:
		return fmt.Errorf("config: provider_thresholds.mode must be %q, %q or %q, got %q",
			ProviderModeOff, ProviderModeHybrid, ProviderModeOverride, p.Mode)
	}
	if mode == ProviderModeOverride && len(p.Labels) == 0 {
		// Every label would resolve to no boundaries at all: nothing could
		// ever flag or block. Refuse to boot into a config that silently
		// disarms the classifier. unarmed_labels does NOT satisfy this: an
		// all-unarmed override is exactly the disarmed state.
		return fmt.Errorf("config: provider_thresholds.mode=%q requires at least one entry under provider_thresholds.labels, otherwise nothing can ever flag or block (provider_thresholds.unarmed_labels does not count — an unarmed label cannot flag or block by definition)", ProviderModeOverride)
	}
	if mode == "" || mode == ProviderModeOff {
		if len(p.Labels) > 0 || len(p.UnarmedLabels) > 0 {
			return fmt.Errorf("config: provider_thresholds.labels/unarmed_labels is set but mode is %q — set mode to %q or %q, or remove them",
				ProviderModeOff, ProviderModeHybrid, ProviderModeOverride)
		}
		return nil
	}
	for _, name := range p.UnarmedLabels {
		if _, dup := p.Labels[strings.ToLower(name)]; dup {
			return fmt.Errorf("config: %q appears in both provider_thresholds.labels and provider_thresholds.unarmed_labels — a label is either armed or deliberately unarmed, not both", name)
		}
	}
	for name, th := range p.Labels {
		for field, v := range map[string]*float64{"flag_at": th.FlagAt, "block_at": th.BlockAt} {
			if v != nil && (*v < 0 || *v > 1) {
				return fmt.Errorf("config: provider_thresholds.labels.%s.%s must be in [0,1], got %v", name, field, *v)
			}
		}
	}
	return nil
}

// validateOutput fails closed on every ambiguous sink definition. A sink
// that cannot be built is a boot error, never a silently dropped output.
func validateOutput(o OutputConfig) error {
	if len(o.Sinks) == 0 {
		return fmt.Errorf("config: output.sinks is present but empty — vismod would emit no results anywhere; remove the output block to use stdout, or list at least one sink")
	}
	for i, s := range o.Sinks {
		switch strings.ToLower(strings.TrimSpace(s.Type)) {
		case "stdout":
		case "file":
			if strings.TrimSpace(s.Path) == "" {
				return fmt.Errorf("config: output.sinks[%d] type=file requires a path", i)
			}
		case "webhook":
			if err := validateWebhookURL(s.URL); err != nil {
				return fmt.Errorf("config: output.sinks[%d]: %w", i, err)
			}
			if s.MaxAttempts < 0 {
				return fmt.Errorf("config: output.sinks[%d].max_attempts must be >= 0, got %d", i, s.MaxAttempts)
			}
			if s.Timeout < 0 {
				return fmt.Errorf("config: output.sinks[%d].timeout must be >= 0, got %s", i, s.Timeout)
			}
		default:
			return fmt.Errorf("config: output.sinks[%d].type must be \"stdout\", \"file\" or \"webhook\", got %q", i, s.Type)
		}
	}
	return nil
}

// validateWebhookURL applies the operator-endpoint rules (SECURITY.md
// class 2): http or https, no userinfo, the metadata range refused
// unconditionally because no legitimate receiver lives there and a
// misconfiguration there turns into cloud-credential exposure, and
// plaintext http permitted only inward (loopback and RFC 1918) so
// envelopes never cross a public network in clear.
//
// This is deliberately the same rule set, in the same order, as
// validateEndpoint in internal/moderate/adapters/shieldgemma — both
// govern an operator-supplied URL that is config-only, and they must
// stay recognizably one rule set. The redirect half of that rule set
// lives on the client (result.NewWebhookSink's CheckRedirect).
func validateWebhookURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("type=webhook requires a url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhook url is not parseable: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhook url scheme must be http or https, got %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("webhook url must not contain userinfo — credentials are env-only")
	}
	if u.Host == "" {
		return fmt.Errorf("webhook url has no host")
	}
	host := u.Hostname()
	ip, ipErr := netip.ParseAddr(host)
	isIP := ipErr == nil
	if isIP && ip.IsLinkLocalUnicast() {
		return fmt.Errorf("webhook url host %s is in the link-local/metadata range", ip)
	}
	if u.Scheme == "https" {
		return nil
	}
	// Plaintext inward only. A hostname that is not an IP literal (other
	// than localhost) is treated as public: vismod cannot know at boot
	// what a name will resolve to at request time.
	if isLocalHostname(host) {
		return nil
	}
	if isIP && (ip.IsLoopback() || ip.IsPrivate()) {
		return nil
	}
	return fmt.Errorf("webhook url host %s requires https (http is permitted only for loopback and private ranges)", host)
}

// isLocalHostname covers the names that resolve to loopback by
// definition. Mirrors the shieldgemma adapter's helper of the same name.
func isLocalHostname(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// Secret returns the env-backed secret accessor handed to adapters via
// AdapterConfig. Keys are logical names ("microsoft.api_key") resolved to
// VISMOD_MICROSOFT_API_KEY. Secrets never appear in yaml or Options.
func Secret() func(key string) string {
	return func(key string) string {
		env := EnvPrefix + "_" + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(key))
		return os.Getenv(env)
	}
}

// ConfigHash is a SHA-256 over the canonicalized verdict-affecting config:
// adapter name + model version + the resolved per-category threshold map.
// Secrets, log level, and addresses are excluded. It is stamped into every
// envelope's ModelIdentity for audit.
func ConfigHash(adapterName, modelVersion string, th Thresholds) string {
	type entry struct {
		FlagAt  *float64 `json:"flag_at"`
		BlockAt *float64 `json:"block_at"`
	}
	resolved := map[string]entry{}
	for name, t := range th {
		resolved[name] = entry{FlagAt: t.FlagAt, BlockAt: t.BlockAt}
	}
	payload := struct {
		Adapter      string           `json:"adapter"`
		ModelVersion string           `json:"model_version"`
		Thresholds   map[string]entry `json:"thresholds"`
	}{adapterName, modelVersion, resolved}
	// encoding/json sorts map keys, giving a canonical encoding here.
	b, err := json.Marshal(payload)
	if err != nil {
		// Marshal of plain structs/maps cannot fail; guard anyway.
		return "config-hash-error"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
