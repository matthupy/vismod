package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vismod.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// TestResultAPICredentialsInYAMLRefuseBoot: invariant 4 says secrets are
// env-only. Viper silently drops a yaml key that no struct field claims,
// so without an explicit refusal an operator who wrote the credentials in
// yaml would get a config that LOOKS configured and an endpoint running on
// whatever the environment happened to hold. There is no fallback here on
// purpose: a boot refusal is the only answer that cannot be misread.
func TestResultAPICredentialsInYAMLRefuseBoot(t *testing.T) {
	for _, key := range []string{"user", "password"} {
		t.Run(key, func(t *testing.T) {
			path := writeConfig(t, `
intake:
  result_api:
    enabled: true
    auth: basic
    `+key+`: hunter2
`)
			_, err := Load(path)
			if err == nil {
				t.Fatal("a credential in yaml booted; the read side would serve on an unreviewed secret")
			}
			for _, want := range []string{"intake.result_api." + key, "env-only", EnvPrefix + "_RESULT_API_"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}

	// The environment is the supported path and must still work — the
	// refusal reads the config FILE only, never the env overlay.
	t.Setenv(EnvPrefix+"_RESULT_API_USER", "operator")
	t.Setenv(EnvPrefix+"_RESULT_API_PASSWORD", "correct-horse")
	cfg, err := Load(writeConfig(t, "intake:\n  result_api:\n    enabled: true\n    auth: basic\n"))
	if err != nil {
		t.Fatalf("env-supplied credentials must load cleanly: %v", err)
	}
	if !cfg.Intake.ResultAPI.Enabled || cfg.Intake.ResultAPI.Auth != AuthModeBasic {
		t.Errorf("intake.result_api did not load: %+v", cfg.Intake.ResultAPI)
	}
	if got := Secret()("result_api.user"); got != "operator" {
		t.Errorf("Secret()(\"result_api.user\") = %q, want the env value", got)
	}
	if got := Secret()("result_api.password"); got != "correct-horse" {
		t.Errorf("Secret()(\"result_api.password\") = %q, want the env value", got)
	}
}

// TestResultAPIDefaultsAreOff: off by default is the security posture, and
// the bounds have to be usable values rather than zeros that a validator
// would reject the first time someone flips enabled.
func TestResultAPIDefaultsAreOff(t *testing.T) {
	d := Defaults().Intake.ResultAPI
	if d.Enabled {
		t.Error("intake.result_api.enabled defaults to true")
	}
	if d.Auth != AuthModeBasic {
		t.Errorf("intake.result_api.auth defaults to %q, want %q", d.Auth, AuthModeBasic)
	}
	if d.MaxEntries <= 0 || d.TTL <= 0 {
		t.Errorf("intake.result_api bounds default to %d / %s; the store would be unusable", d.MaxEntries, d.TTL)
	}
	// intake_addr stays exactly where it was; the new block is about what
	// the intake listener SERVES, not where it listens.
	if Defaults().IntakeAddr != "127.0.0.1:8080" {
		t.Errorf("intake_addr moved: %q", Defaults().IntakeAddr)
	}
}

// TestResultAPIValidation: a typo in a block that is off today is still a
// typo, and finding it the day an operator flips enabled — during an
// incident, most likely — is the worst possible time.
func TestResultAPIValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    ResultAPIConfig
	}{
		{"unknown auth mode", ResultAPIConfig{Auth: "oauth", MaxEntries: 10, TTL: time.Minute}},
		{"empty auth mode", ResultAPIConfig{MaxEntries: 10, TTL: time.Minute}},
		{"zero max_entries", ResultAPIConfig{Auth: AuthModeBasic, TTL: time.Minute}},
		{"negative max_entries", ResultAPIConfig{Auth: AuthModeBasic, MaxEntries: -1, TTL: time.Minute}},
		{"zero ttl", ResultAPIConfig{Auth: AuthModeBasic, MaxEntries: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Intake.ResultAPI = tc.r
			if err := Validate(cfg); err == nil {
				t.Errorf("%+v passed validation", tc.r)
			}
		})
	}
	cfg := Defaults()
	cfg.Intake.ResultAPI = ResultAPIConfig{Enabled: true, Auth: AuthModeNone, MaxEntries: 1, TTL: time.Second}
	if err := Validate(cfg); err != nil {
		t.Errorf("auth %q must be permitted: %v", AuthModeNone, err)
	}
}
