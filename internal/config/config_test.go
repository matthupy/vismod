package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vismod/vismod/pkg/moderation"
)

func writeTempYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsValidate(t *testing.T) {
	if err := Validate(Defaults()); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestThresholdResolveFallback(t *testing.T) {
	th := Thresholds{
		"default": {FlagAt: f64(0.5), BlockAt: f64(0.8)},
		"SEXUAL":  {FlagAt: f64(0.3)}, // block_at inherits default
	}
	r := th.Resolve(moderation.CategorySexual)
	if *r.FlagAt != 0.3 || *r.BlockAt != 0.8 {
		t.Errorf("resolve = %+v", r)
	}
	r = th.Resolve(moderation.CategoryViolence)
	if *r.FlagAt != 0.5 || *r.BlockAt != 0.8 {
		t.Errorf("unlisted category must inherit default: %+v", r)
	}
}

func TestLoadYAMLAndEnvOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
adapter:
  name: microsoft
thresholds:
  SEXUAL:
    flag_at: 0.2
ffmpeg:
  max_frames: 32
queue:
  driver: memory
  workers: 8
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISMOD_QUEUE_WORKERS", "16")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Adapter.Name != "microsoft" {
		t.Errorf("adapter.name = %q", cfg.Adapter.Name)
	}
	if *cfg.Thresholds["SEXUAL"].FlagAt != 0.2 {
		t.Errorf("yaml threshold not applied")
	}
	if cfg.FFmpeg.MaxFrames != 32 {
		t.Errorf("ffmpeg.max_frames = %d", cfg.FFmpeg.MaxFrames)
	}
	if cfg.Queue.Workers != 16 {
		t.Errorf("env overlay must win: workers = %d, want 16", cfg.Queue.Workers)
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	bad := Defaults()
	bad.FFmpeg.MaxFrames = 0
	if err := Validate(bad); err == nil {
		t.Error("max_frames=0 must fail validation (required hard cap)")
	}

	bad = Defaults()
	bad.Queue.Driver = "kafka"
	if err := Validate(bad); err == nil {
		t.Error("unknown queue driver must fail validation")
	}

	bad = Defaults()
	bad.Thresholds["SEXUAL"] = CategoryThreshold{FlagAt: f64(1.5)}
	if err := Validate(bad); err == nil {
		t.Error("out-of-range threshold must fail validation")
	}
}

func TestExtractBudget(t *testing.T) {
	f := FFmpegConfig{MaxFrames: 15}
	if f.ExtractBudget() != 60 {
		t.Errorf("default budget = %d, want 4x max_frames = 60", f.ExtractBudget())
	}
	f.MaxExtractFrames = 100
	if f.ExtractBudget() != 100 {
		t.Errorf("explicit budget = %d, want 100", f.ExtractBudget())
	}

	bad := Defaults()
	bad.FFmpeg.MaxFrames = 20
	bad.FFmpeg.MaxExtractFrames = 10 // below the scan cap
	if err := Validate(bad); err == nil {
		t.Error("max_extract_frames < max_frames must fail validation")
	}
}

func TestConfigHashStableAndSensitive(t *testing.T) {
	th := Defaults().Thresholds
	h1 := ConfigHash("microsoft", "2024-09-01", th)
	h2 := ConfigHash("microsoft", "2024-09-01", th)
	if h1 != h2 {
		t.Error("ConfigHash must be deterministic")
	}
	th2 := Thresholds{"default": {FlagAt: f64(0.4), BlockAt: f64(0.8)}}
	if ConfigHash("microsoft", "2024-09-01", th2) == h1 {
		t.Error("threshold change must change ConfigHash")
	}
	if ConfigHash("google", "2024-09-01", th) == h1 {
		t.Error("adapter change must change ConfigHash")
	}
}

func TestSecretAccessor(t *testing.T) {
	t.Setenv("VISMOD_MICROSOFT_API_KEY", "sekrit")
	if got := Secret()("microsoft.api_key"); got != "sekrit" {
		t.Errorf("Secret() = %q", got)
	}
}

func TestProviderLabelResolutionChain(t *testing.T) {
	base := Thresholds{
		"default": {FlagAt: f64(0.5), BlockAt: f64(0.8)},
		"SEXUAL":  {FlagAt: f64(0.4), BlockAt: f64(0.7)},
	}
	th := base.Merge(ProviderThresholds{
		Mode: ProviderModeHybrid,
		Labels: Thresholds{
			"yes_gambling": {FlagAt: f64(0.9)},                     // looser than default
			"yes_genitals": {BlockAt: f64(0.6)},                    // stricter than SEXUAL
			"YES_SMOKING":  {FlagAt: f64(0.2), BlockAt: f64(0.95)}, // mixed case key
		},
	})

	// Label wins; the field it does not set falls through to the category,
	// and then to default.
	r := th.ResolveFor(moderation.CategorySexual, "yes_genitals")
	if *r.FlagAt != 0.4 || *r.BlockAt != 0.6 {
		t.Errorf("label block_at over category flag_at: %+v", r)
	}
	// A label looser than what it would inherit still wins — full override
	// is the documented semantic, not a clamp.
	r = th.ResolveFor(moderation.CategoryOther, "yes_gambling")
	if *r.FlagAt != 0.9 || *r.BlockAt != 0.8 {
		t.Errorf("looser label override must apply: %+v", r)
	}
	// Labels match case-insensitively: vendors disagree on casing.
	r = th.ResolveFor(moderation.CategoryAlcoholTobacco, "yes_smoking")
	if *r.FlagAt != 0.2 || *r.BlockAt != 0.95 {
		t.Errorf("label lookup must be case-insensitive: %+v", r)
	}
	// An unlisted label is untouched by the feature.
	r = th.ResolveFor(moderation.CategorySexual, "general_nsfw")
	if *r.FlagAt != 0.4 || *r.BlockAt != 0.7 {
		t.Errorf("unlisted label must resolve exactly as before: %+v", r)
	}
}

func TestProviderModeOffIgnoresLabels(t *testing.T) {
	base := Thresholds{"default": {FlagAt: f64(0.5), BlockAt: f64(0.8)}}
	th := base.Merge(ProviderThresholds{
		Mode:   ProviderModeOff,
		Labels: Thresholds{"yes_gambling": {FlagAt: f64(0.1)}},
	})
	r := th.ResolveFor(moderation.CategoryOther, "yes_gambling")
	if *r.FlagAt != 0.5 {
		t.Errorf("mode=off must ignore labels entirely: %+v", r)
	}
}

func TestProviderModeOverrideDropsCategories(t *testing.T) {
	base := Thresholds{
		"default": {FlagAt: f64(0.5), BlockAt: f64(0.8)},
		"SEXUAL":  {FlagAt: f64(0.4), BlockAt: f64(0.7)},
	}
	th := base.Merge(ProviderThresholds{
		Mode:   ProviderModeOverride,
		Labels: Thresholds{"yes_gambling": {FlagAt: f64(0.9), BlockAt: f64(0.95)}},
	})
	r := th.ResolveFor(moderation.CategoryOther, "yes_gambling")
	if *r.FlagAt != 0.9 || *r.BlockAt != 0.95 {
		t.Errorf("configured label must apply in override mode: %+v", r)
	}
	// The documented consequence: an unconfigured label has NO boundaries
	// in override mode, so it cannot flag or block. Category and default
	// are gone.
	r = th.ResolveFor(moderation.CategorySexual, "general_nsfw")
	if r.FlagAt != nil || r.BlockAt != nil {
		t.Errorf("override mode must drop category and default thresholds: %+v", r)
	}
}

func TestProviderThresholdValidation(t *testing.T) {
	cfg := Defaults()
	cfg.ProviderThresholds = ProviderThresholds{Mode: "sometimes"}
	if err := Validate(cfg); err == nil {
		t.Error("unknown mode must fail validation")
	}
	// override with no labels disarms the classifier entirely.
	cfg.ProviderThresholds = ProviderThresholds{Mode: ProviderModeOverride}
	if err := Validate(cfg); err == nil {
		t.Error("override mode with no labels must fail validation")
	}
	// labels configured but mode left off is a silent no-op, so it is an error.
	cfg.ProviderThresholds = ProviderThresholds{Labels: Thresholds{"a": {FlagAt: f64(0.5)}}}
	if err := Validate(cfg); err == nil {
		t.Error("labels without a mode must fail validation rather than be ignored")
	}
	cfg.ProviderThresholds = ProviderThresholds{
		Mode:   ProviderModeHybrid,
		Labels: Thresholds{"a": {FlagAt: f64(1.5)}},
	}
	if err := Validate(cfg); err == nil {
		t.Error("out-of-range label threshold must fail validation")
	}
}

// TestUnarmedLabelsLoad pins how an operator writes down "this label is
// reviewed and deliberately has no boundaries".
//
// It cannot be spelled `some_label: {}` under labels: viper drops a yaml
// key whose value has no scalar leaf, so the entry never reaches the
// decoded struct at all (verified 2026-07-29 against `{}`, a nil value,
// `null`, and `flag_at: null` — every one loses the key). Hence the
// explicit unarmed_labels list, which viper does carry. Load folds each
// name in as a keyed entry with no boundaries, so the boot-time
// completeness check in internal/cli sees a KEY and config_hash covers the
// decision.
func TestUnarmedLabelsLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vismod.yaml")
	yaml := `
adapter:
  name: shieldgemma
provider_thresholds:
  mode: override
  labels:
    sexually_explicit:
      flag_at: 0.6
  unarmed_labels:
    - dangerous_content
ffmpeg:
  max_frames: 8
queue:
  driver: memory
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.ProviderThresholds.Labels["dangerous_content"]
	if !ok {
		t.Fatal("an unarmed_labels entry must appear as a keyed label")
	}
	if entry.FlagAt != nil || entry.BlockAt != nil {
		t.Errorf("an unarmed label must have NO boundaries: %+v", entry)
	}
	// It reaches the merged runtime map, so config_hash changes with the
	// decision and the tuning stays attributable.
	if _, ok := cfg.Thresholds[ProviderLabelKey("dangerous_content")]; !ok {
		t.Error("an unarmed label must still reach the merged map")
	}
	// And it stays unable to flag or block, which is the whole point.
	r := cfg.Thresholds.ResolveFor(moderation.CategoryOther, "dangerous_content")
	if r.FlagAt != nil || r.BlockAt != nil {
		t.Errorf("an unarmed label must not acquire boundaries: %+v", r)
	}
}

func TestUnarmedLabelValidation(t *testing.T) {
	cfg := Defaults()
	// Armed and unarmed at once is a contradiction, not a precedence puzzle.
	cfg.ProviderThresholds = ProviderThresholds{
		Mode:          ProviderModeOverride,
		Labels:        Thresholds{"a": {FlagAt: f64(0.5)}},
		UnarmedLabels: []string{"a"},
	}
	if err := Validate(cfg); err == nil {
		t.Error("a label that is both armed and unarmed must fail validation")
	}
	// Unarmed labels with no mode are a silent no-op, like labels.
	cfg.ProviderThresholds = ProviderThresholds{UnarmedLabels: []string{"a"}}
	if err := Validate(cfg); err == nil {
		t.Error("unarmed_labels without a mode must fail validation")
	}
	// Override mode is satisfied by unarmed labels alone only if something
	// else is armed: an all-unarmed override can never flag or block.
	cfg.ProviderThresholds = ProviderThresholds{
		Mode:          ProviderModeOverride,
		UnarmedLabels: []string{"a"},
	}
	if err := Validate(cfg); err == nil {
		t.Error("override mode with only unarmed labels must fail validation")
	}
}

func TestOutputDefaultsToStdout(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Output.Sinks) != 1 || cfg.Output.Sinks[0].Type != "stdout" {
		t.Errorf("absent output block must default to stdout, got %+v", cfg.Output.Sinks)
	}
}

func TestOutputSinksParse(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
output:
  sinks:
    - type: stdout
    - type: file
      path: /tmp/results.jsonl
    - type: webhook
      url: https://collector.internal/results
      timeout: 5s
      max_attempts: 4
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Output.Sinks) != 3 {
		t.Fatalf("want 3 sinks, got %d", len(cfg.Output.Sinks))
	}
	if cfg.Output.Sinks[1].Path != "/tmp/results.jsonl" {
		t.Errorf("file path: got %q", cfg.Output.Sinks[1].Path)
	}
	if cfg.Output.Sinks[2].Timeout != 5*time.Second || cfg.Output.Sinks[2].MaxAttempts != 4 {
		t.Errorf("webhook opts: got %+v", cfg.Output.Sinks[2])
	}
}

func TestOutputSinkNegativeCases(t *testing.T) {
	for name, body := range map[string]string{
		"unknown type": `
output:
  sinks:
    - type: carrier-pigeon
`,
		"file without path": `
output:
  sinks:
    - type: file
`,
		"webhook without url": `
output:
  sinks:
    - type: webhook
`,
		"webhook with userinfo": `
output:
  sinks:
    - type: webhook
      url: https://user:pw@collector.internal/results
`,
		"webhook on metadata range": `
output:
  sinks:
    - type: webhook
      url: http://169.254.169.254/results
`,
		"negative max_attempts": `
output:
  sinks:
    - type: webhook
      url: https://collector.internal/results
      max_attempts: -1
`,
		// SECURITY.md's operator-endpoint rules: http is inward-only.
		"plaintext http to a public hostname": `
output:
  sinks:
    - type: webhook
      url: http://collector.example.com/results
`,
		"plaintext http to a public IP literal": `
output:
  sinks:
    - type: webhook
      url: http://203.0.113.10/results
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTempYAML(t, "ffmpeg:\n  max_frames: 8\n"+body)); err == nil {
				t.Fatal("want boot refusal, got nil error")
			}
		})
	}
}

// TestOutputEmptySinkListRefusesBoot pins the observed viper behavior for
// `output.sinks: []`: unlike a map key with no scalar leaf (which viper
// drops before decoding — see the unarmed_labels gotcha above), an empty
// YAML LIST survives decoding as a genuine zero-length slice. Load must
// therefore refuse to boot rather than silently falling back to the
// stdout default (probed empirically 2026-07-31: Load returned
// len(cfg.Output.Sinks)==0 and a non-nil error).
func TestOutputEmptySinkListRefusesBoot(t *testing.T) {
	_, err := Load(writeTempYAML(t, "ffmpeg:\n  max_frames: 8\noutput:\n  sinks: []\n"))
	if err == nil {
		t.Fatal("an explicitly empty sinks list must refuse to boot")
	}
	if !strings.Contains(err.Error(), "output.sinks") {
		t.Errorf("error must name the offending key, got %v", err)
	}
}

func TestConfigHashCoversProviderThresholds(t *testing.T) {
	base := Thresholds{"default": {FlagAt: f64(0.5), BlockAt: f64(0.8)}}
	h1 := ConfigHash("hive", "v2", base)
	h2 := ConfigHash("hive", "v2", base.Merge(ProviderThresholds{
		Mode:   ProviderModeHybrid,
		Labels: Thresholds{"yes_gambling": {FlagAt: f64(0.9)}},
	}))
	if h1 == h2 {
		t.Error("a provider-label threshold must change ConfigHash, or the audit trail cannot attribute the verdict")
	}
}

// TestOutputWebhookPlaintextInwardIsAllowed pins the other half of the
// http rule fixed alongside the public-host refusal: a receiver on
// loopback or an RFC 1918 range is exactly the deployment `http` exists
// for, and must keep booting. Same rule set as the shieldgemma adapter's
// validateEndpoint.
func TestOutputWebhookPlaintextInwardIsAllowed(t *testing.T) {
	for _, u := range []string{
		"http://localhost:9000/results",
		"http://collector.localhost/results",
		"http://127.0.0.1:9000/results",
		"http://10.1.2.3/results",
		"http://192.168.4.5:8080/results",
		"https://collector.example.com/results",
	} {
		t.Run(u, func(t *testing.T) {
			body := "ffmpeg:\n  max_frames: 8\noutput:\n  sinks:\n    - type: webhook\n      url: " + u + "\n"
			if _, err := Load(writeTempYAML(t, body)); err != nil {
				t.Fatalf("want boot, got %v", err)
			}
		})
	}
}

func TestSourceURLDefaultsDisabled(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, "ffmpeg:\n  max_frames: 8\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.URL.Enabled {
		t.Error("url sources must be OFF by default")
	}
}

func TestSourceURLParses(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
source:
  url:
    enabled: true
    allow_hosts:
      - media.example.com
    max_bytes: 1048576
    timeout: 30s
    max_attempts: 5
    allowed_media_types:
      - video/mp4
`))
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Source.URL
	if !u.Enabled || len(u.AllowHosts) != 1 || u.MaxBytes != 1048576 ||
		u.Timeout != 30*time.Second || u.MaxAttempts != 5 || len(u.AllowedMediaTypes) != 1 {
		t.Errorf("parsed wrong: %+v", u)
	}
}

func TestSourceURLEnabledWithoutAllowHostsRefusesBoot(t *testing.T) {
	_, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
source:
  url:
    enabled: true
`))
	if err == nil {
		t.Fatal("enabled with no allow_hosts must refuse to boot")
	}
	if !strings.Contains(err.Error(), "allow_hosts") {
		t.Errorf("error must name the offending key, got %v", err)
	}
}

func TestSourceURLNegativeNumbersRefuseBoot(t *testing.T) {
	for name, body := range map[string]string{
		"negative max_bytes": "    max_bytes: -1\n",
		"negative timeout":   "    timeout: -5s\n",
	} {
		t.Run(name, func(t *testing.T) {
			y := "ffmpeg:\n  max_frames: 8\nsource:\n  url:\n    enabled: true\n    allow_hosts: [media.example.com]\n" + body
			if _, err := Load(writeTempYAML(t, y)); err == nil {
				t.Fatal("want boot refusal")
			}
		})
	}
}

func TestSourceURLWildcardHostRefusesBoot(t *testing.T) {
	y := "ffmpeg:\n  max_frames: 8\nsource:\n  url:\n    enabled: true\n    allow_hosts: [\"*.example.com\"]\n"
	if _, err := Load(writeTempYAML(t, y)); err == nil {
		t.Fatal("a wildcard host must refuse to boot: matching is exact")
	}
}

func TestConfigHashIgnoresSourceAndOutput(t *testing.T) {
	th := Thresholds{"default": {FlagAt: f64(0.5), BlockAt: f64(0.8)}}
	// ConfigHash takes only adapter, model version, and thresholds — so
	// changing fetch or sink settings CANNOT perturb it. This test exists
	// so a future signature change cannot silently make every previously
	// written envelope incomparable.
	a := ConfigHash("microsoft", "v1", th)
	b := ConfigHash("microsoft", "v1", th)
	if a != b {
		t.Fatal("hash is not stable")
	}
}
