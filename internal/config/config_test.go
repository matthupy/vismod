package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matthupy/vismod/pkg/moderation"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Adapter.Name != "stub" {
		t.Errorf("default adapter = %q, want stub", c.Adapter.Name)
	}
	if c.Queue.Driver != "memory" {
		t.Errorf("default driver = %q, want memory", c.Queue.Driver)
	}
	if c.Frames.MaxFrames != 64 {
		t.Errorf("default max_frames = %d, want 64", c.Frames.MaxFrames)
	}
	if c.Queue.RetryBackoff != 500*time.Millisecond {
		t.Errorf("retry_backoff = %v, want 500ms (duration string must decode)", c.Queue.RetryBackoff)
	}
	if c.Queue.DrainTimeout != 30*time.Second {
		t.Errorf("drain_timeout = %v, want 30s", c.Queue.DrainTimeout)
	}
	if got := c.Thresholds.Default.FlagAt; got != 0.5 {
		t.Errorf("default flag_at = %v, want 0.5", got)
	}
	if got := c.Thresholds.For(moderation.CategorySexual).BlockAt; got != 0.667 {
		t.Errorf("SEXUAL block_at = %v, want 0.667", got)
	}
	if c.Thresholds.SexualPotentialCSAM != 0.667 {
		t.Errorf("SEXUAL potential_csam = %v, want 0.667", c.Thresholds.SexualPotentialCSAM)
	}
}

func TestLoadFileOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	yaml := `
adapter:
  name: azure
queue:
  driver: redis
  workers: 9
thresholds:
  default:
    flag_at: 0.3
    block_at: 0.6
  VIOLENCE:
    flag_at: 0.4
    block_at: 0.7
frames:
  max_frames: 12
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Adapter.Name != "azure" {
		t.Errorf("adapter = %q", c.Adapter.Name)
	}
	if c.Queue.Driver != "redis" || c.Queue.Workers != 9 {
		t.Errorf("queue override failed: %+v", c.Queue)
	}
	if c.Frames.MaxFrames != 12 {
		t.Errorf("max_frames = %d, want 12", c.Frames.MaxFrames)
	}
	vt := c.Thresholds.For(moderation.CategoryViolence)
	if vt.FlagAt != 0.4 || vt.BlockAt != 0.7 {
		t.Errorf("per-category VIOLENCE override failed: %+v", vt)
	}
	// Unlisted category falls back to default.
	ht := c.Thresholds.For(moderation.CategoryHate)
	if ht.FlagAt != 0.3 || ht.BlockAt != 0.6 {
		t.Errorf("default fallback failed: %+v", ht)
	}
}

func TestLoadEnvOverlay(t *testing.T) {
	t.Setenv("VISMOD_LOG_LEVEL", "debug")
	t.Setenv("VISMOD_METRICS_ADDR", ":7777")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LogLevel != "debug" {
		t.Errorf("log level env overlay failed: %q", c.LogLevel)
	}
	if c.MetricsAddr != ":7777" {
		t.Errorf("metrics addr env overlay failed: %q", c.MetricsAddr)
	}
}

func TestLoadConfigPathFromEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("metrics:\n  addr: \":7654\"\nframes:\n  max_frames: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Empty path arg + VISMOD_CONFIG set: the env file is read. This is the
	// container HEALTHCHECK path (no --config flag), and it must resolve the same
	// metrics.addr a serve started against the file would.
	t.Setenv("VISMOD_CONFIG", path)
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") with VISMOD_CONFIG: %v", err)
	}
	if c.MetricsAddr != ":7654" {
		t.Errorf("metrics addr from VISMOD_CONFIG file = %q, want :7654", c.MetricsAddr)
	}
}

func TestLoadFlagPathBeatsEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env.yaml")
	flagFile := filepath.Join(dir, "flag.yaml")
	if err := os.WriteFile(envFile, []byte("metrics:\n  addr: \":1111\"\nframes:\n  max_frames: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flagFile, []byte("metrics:\n  addr: \":2222\"\nframes:\n  max_frames: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Explicit path arg (the --config flag) wins over VISMOD_CONFIG: flag > env.
	t.Setenv("VISMOD_CONFIG", envFile)
	c, err := Load(flagFile)
	if err != nil {
		t.Fatalf("Load(flagFile): %v", err)
	}
	if c.MetricsAddr != ":2222" {
		t.Errorf("metrics addr = %q, want :2222 (flag must beat VISMOD_CONFIG)", c.MetricsAddr)
	}
}

func TestValidateErrors(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"bad driver":  "queue:\n  driver: rabbitmq\nframes:\n  max_frames: 10\n",
		"zero frames": "queue:\n  driver: memory\nframes:\n  max_frames: 0\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".yaml")
			if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}

func TestConfigHashDeterministicAndSensitive(t *testing.T) {
	c, _ := Load("")
	base := c.ConfigHash("v1")

	if base != c.ConfigHash("v1") {
		t.Fatal("ConfigHash not deterministic for identical inputs")
	}
	if base == c.ConfigHash("v2") {
		t.Fatal("ConfigHash must change with model version")
	}

	c2, _ := Load("")
	c2.Thresholds.Default.BlockAt = 0.99
	if base == c2.ConfigHash("v1") {
		t.Fatal("ConfigHash must change when a verdict-affecting threshold changes")
	}
}

func TestModelFingerprintDeterministicAndSensitive(t *testing.T) {
	c, _ := Load("")
	base := c.ModelFingerprint()

	if base != c.ModelFingerprint() {
		t.Fatal("ModelFingerprint not deterministic for identical inputs")
	}

	// Sensitive to the adapter name (the deployed model).
	cName, _ := Load("")
	cName.Adapter.Name = "azure"
	if base == cName.ModelFingerprint() {
		t.Fatal("ModelFingerprint must change when adapter.name changes")
	}

	// Sensitive to adapter.options (api_version/model-id/endpoint live here, so a
	// model change => different fingerprint).
	cOpt, _ := Load("")
	cOpt.Adapter.Options = map[string]any{"api_version": "2024-09-01"}
	if base == cOpt.ModelFingerprint() {
		t.Fatal("ModelFingerprint must change when adapter.options changes")
	}

	// Sensitive to a verdict-affecting threshold.
	cThr, _ := Load("")
	cThr.Thresholds.Default.BlockAt = 0.99
	if base == cThr.ModelFingerprint() {
		t.Fatal("ModelFingerprint must change when a threshold changes")
	}

	// Distinct purpose from ConfigHash — do NOT collapse the two.
	if c.ModelFingerprint() == c.ConfigHash("v1") {
		t.Fatal("ModelFingerprint and ConfigHash must be distinct hashes")
	}
}

// The #1 correctness landmine: adapter.options is a map[string]any, and naive Go
// map formatting yields random key order => a non-deterministic hash => replicas
// with identical config dead-letter each other. json.Marshal sorts keys
// recursively, so the fingerprint must be invariant to map insertion order, even
// for nested maps.
func TestModelFingerprintStableAcrossOptionMapOrder(t *testing.T) {
	mk := func(build func(m map[string]any)) Config {
		c, _ := Load("")
		opts := map[string]any{}
		build(opts)
		c.Adapter.Options = opts
		return c
	}

	// Same logical options, keys inserted in two different orders, with a nested map.
	a := mk(func(m map[string]any) {
		m["api_version"] = "2024-09-01"
		m["endpoint"] = "https://x.cognitiveservices.azure.com"
		m["retry"] = map[string]any{"max": 3, "backoff": "500ms"}
	})
	b := mk(func(m map[string]any) {
		m["retry"] = map[string]any{"backoff": "500ms", "max": 3}
		m["endpoint"] = "https://x.cognitiveservices.azure.com"
		m["api_version"] = "2024-09-01"
	})

	if a.ModelFingerprint() != b.ModelFingerprint() {
		t.Fatal("ModelFingerprint must be invariant to adapter.options map key order (incl. nested)")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	// The shipped example must be valid.
	if _, err := Load(filepath.Join("..", "..", "config.example.yaml")); err != nil {
		t.Fatalf("config.example.yaml failed to load: %v", err)
	}
}

func TestRedisDriverRequiresAddr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("queue:\n  driver: redis\n  redis_addr: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("redis driver with empty redis_addr must fail validation")
	}
}
