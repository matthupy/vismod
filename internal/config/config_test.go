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

	// Sensitive to a verdict-affecting adapter.options key (api_version/endpoint/
	// auth_mode/model live here, so a model change => different fingerprint).
	cOpt, _ := Load("")
	cOpt.Adapter.Options = map[string]any{"api_version": "2024-09-01"}
	if base == cOpt.ModelFingerprint() {
		t.Fatal("ModelFingerprint must change when a verdict-affecting adapter.options key changes")
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

// Each verdict-affecting key, changed in isolation, must move the fingerprint.
// Guards against a whitelist that silently drops a key (e.g. endpoint/auth_mode/
// model) and stops guarding a real model swap.
func TestModelFingerprintSensitiveToEachVerdictKey(t *testing.T) {
	for _, key := range []string{"api_version", "model", "model_id", "endpoint", "auth_mode"} {
		base, _ := Load("")
		baseFP := base.ModelFingerprint()

		c, _ := Load("")
		c.Adapter.Options = map[string]any{key: "changed-value"}
		if baseFP == c.ModelFingerprint() {
			t.Errorf("ModelFingerprint must change when verdict key %q is set", key)
		}
	}
}

// Operational-only knobs (rps, max_retries, timeout, retry/backoff) have no
// verdict impact. Tuning them in a rolling deploy must NOT change the fingerprint,
// or it would spuriously trip the §L dead-letter guard.
func TestModelFingerprintIgnoresOperationalKeys(t *testing.T) {
	base, _ := Load("")
	baseFP := base.ModelFingerprint()

	for _, kv := range []map[string]any{
		{"rps": 50.0},
		{"max_retries": 7},
		{"timeout": "30s"},
		{"retry_backoff": "500ms"},
	} {
		c, _ := Load("")
		c.Adapter.Options = kv
		if baseFP != c.ModelFingerprint() {
			t.Errorf("ModelFingerprint must NOT change for operational-only option %v", kv)
		}
	}
}

// A verdict key must dominate: even alongside operational noise, only the
// verdict key's value drives the fingerprint, and operational churn around a
// fixed verdict key leaves it stable.
func TestModelFingerprintScopesToVerdictKeysOnly(t *testing.T) {
	withNoise := func(rps float64, retries int) Config {
		c, _ := Load("")
		c.Adapter.Options = map[string]any{
			"endpoint":    "https://x.cognitiveservices.azure.com",
			"api_version": "2024-09-01",
			"rps":         rps,
			"max_retries": retries,
		}
		return c
	}
	a := withNoise(10, 3)
	b := withNoise(99, 1) // same verdict keys, different operational knobs
	if a.ModelFingerprint() != b.ModelFingerprint() {
		t.Fatal("ModelFingerprint must ignore operational knobs when verdict keys match")
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

// --- Adapter-option registry guard (PR #14 issue 5: whitelist fail-open) ---
//
// verdictAffectingOptionKeys is FAIL-OPEN: an adapter.options key absent from it
// is silently un-guarded by ModelFingerprint, so a model swap via that key could
// slip a rolling deploy without dead-lettering (§L). The registry below is the
// single source of truth that partitions EVERY known adapter.options key into
// verdict-affecting vs operational-only; these tests back the 🔴 MAINTENANCE
// comment in load.go with a mechanical check so adding a key without classifying
// it fails CI instead of going unguarded.

// TestOptionRegistryPartitionWellFormed asserts the two partitions of the
// registry are internally consistent: each is sorted with no duplicates, and no
// key is classified as BOTH verdict-affecting and operational (which would make
// the classification ambiguous). verdictAffectingOptionKeys must equal the
// verdict partition so ModelFingerprint stays derived from — not divergent
// from — the registry.
func TestOptionRegistryPartitionWellFormed(t *testing.T) {
	assertSortedUnique := func(name string, keys []string) {
		t.Helper()
		seen := map[string]bool{}
		for i, k := range keys {
			if seen[k] {
				t.Errorf("%s contains duplicate key %q", name, k)
			}
			seen[k] = true
			if i > 0 && keys[i-1] > k {
				t.Errorf("%s is not sorted: %q before %q", name, keys[i-1], k)
			}
		}
	}
	assertSortedUnique("verdictAffectingOptionKeys", verdictAffectingOptionKeys)
	assertSortedUnique("operationalOptionKeys", operationalOptionKeys)

	verdict := map[string]bool{}
	for _, k := range verdictAffectingOptionKeys {
		verdict[k] = true
	}
	for _, k := range operationalOptionKeys {
		if verdict[k] {
			t.Errorf("key %q is classified as BOTH verdict-affecting and operational", k)
		}
	}
}

// TestKnownOptionKeysPinned pins the EXACT full set of classified adapter.options
// keys. This is the mechanical guard the 🔴 MAINTENANCE comment relies on: adding
// a new key that an adapter reads (see internal/moderate/adapters/*/options.go)
// without adding it to the registry — i.e. without DELIBERATELY classifying it as
// verdict-affecting or operational — fails here. The fix is never to edit this
// expectation blindly; it is to decide "does this key change the score a model
// returns?" and put it in the right partition, then update this pin in the SAME
// commit. The keys mirror what azure/hive decodeOptions actually read today:
// azure → endpoint, auth_mode, api_version, rps, max_retries, retry_backoff;
// hive → endpoint, rps, max_retries, retry_backoff; plus the forward-looking
// verdict selectors model/model_id and the documented operational knob timeout.
func TestKnownOptionKeysPinned(t *testing.T) {
	want := map[string]bool{
		// verdict-affecting
		"api_version": true,
		"auth_mode":   true,
		"endpoint":    true,
		"model":       true,
		"model_id":    true,
		// operational-only
		"max_retries":   true,
		"retry_backoff": true,
		"rps":           true,
		"timeout":       true,
	}
	got := map[string]bool{}
	for _, k := range knownOptionKeys() {
		got[k] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("known option key %q missing from registry — classify it in load.go", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("registry has unpinned key %q — add it here and confirm its partition", k)
		}
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
