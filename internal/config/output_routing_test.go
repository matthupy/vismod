package config

import (
	"strings"
	"testing"

	"github.com/vismod/vismod/pkg/moderation"
)

const discordURL = "https://discord.com/api/webhooks/123456/abcdefg"

// TestOutputSinkParsesFormatAndPredicate is the config half of a Discord
// notification: transport webhook, format discord, routed on verdict.
func TestOutputSinkParsesFormatAndPredicate(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
output:
  sinks:
    - type: stdout
    - type: webhook
      url: `+discordURL+`
      format: discord
      predicate:
        verdicts: [block]
        categories:
          sexual: 0.8
        source_kinds: [url]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Output.Sinks) != 2 {
		t.Fatalf("want 2 sinks, got %d", len(cfg.Output.Sinks))
	}
	s := cfg.Output.Sinks[1]
	if s.Format != "discord" {
		t.Errorf("format: got %q", s.Format)
	}
	if len(s.Predicate.Verdicts) != 1 || s.Predicate.Verdicts[0] != moderation.VerdictBlock {
		t.Errorf("verdicts: got %v", s.Predicate.Verdicts)
	}
	// viper lowercases yaml map keys; Load must leave them canonical.
	if got, ok := s.Predicate.Categories["SEXUAL"]; !ok || got != 0.8 {
		t.Errorf("categories must be canonicalized to SEXUAL: got %v", s.Predicate.Categories)
	}
	if len(s.Predicate.SourceKinds) != 1 || s.Predicate.SourceKinds[0] != "url" {
		t.Errorf("source_kinds: got %v", s.Predicate.SourceKinds)
	}
}

func TestOutputUnknownFormatRefusesBoot(t *testing.T) {
	_, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
output:
  sinks:
    - type: webhook
      url: `+discordURL+`
      format: smoke-signal
`))
	if err == nil {
		t.Fatal("want a boot refusal for an unknown format, got nil")
	}
	if !strings.Contains(err.Error(), "smoke-signal") {
		t.Errorf("error must name the bad format, got %q", err)
	}
}

func TestOutputBadPredicateRefusesBoot(t *testing.T) {
	_, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
output:
  sinks:
    - type: stdout
      predicate:
        categories:
          sexuall: 0.8
`))
	if err == nil {
		t.Fatal("want a boot refusal for an unknown category, got nil")
	}
	if !strings.Contains(err.Error(), "sexuall") {
		t.Errorf("error must name the bad key, got %q", err)
	}
}

// TestEveryySinkPredicatedRefusesBoot is the fail-safe rule that makes
// routing safe to ship. A predicate can only NARROW delivery, so if every
// sink is predicated there are envelopes that reach no destination at
// all — a silent drop, which is exactly what this project exists to
// prevent. At least one sink must be unconditional.
func TestEveryySinkPredicatedRefusesBoot(t *testing.T) {
	_, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
output:
  sinks:
    - type: webhook
      url: `+discordURL+`
      format: discord
      predicate:
        verdicts: [block]
`))
	if err == nil {
		t.Fatal("want a boot refusal when every sink is predicated, got nil")
	}
	if !strings.Contains(err.Error(), "allow_unrouted") {
		t.Errorf("error must name the override that permits it, got %q", err)
	}
}

func TestOneUnconditionalSinkSatisfiesTheCatchAllRule(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
output:
  sinks:
    - type: stdout
    - type: webhook
      url: `+discordURL+`
      format: discord
      predicate:
        verdicts: [block]
`))
	if err != nil {
		t.Fatalf("an unconditional stdout sink must satisfy the catch-all rule: %v", err)
	}
	if !cfg.Output.Sinks[0].Predicate.IsEmpty() {
		t.Error("the stdout sink must have an empty predicate")
	}
	if cfg.Output.Sinks[1].Predicate.IsEmpty() {
		t.Error("the discord sink must have a non-empty predicate")
	}
}

// TestAllowUnroutedIsTheGatedOverride mirrors
// failsafe.allow_empty_video_skip: an operator may accept the risk, but
// only by naming it in config.
func TestAllowUnroutedIsTheGatedOverride(t *testing.T) {
	_, err := Load(writeTempYAML(t, `
ffmpeg:
  max_frames: 8
output:
  allow_unrouted: true
  sinks:
    - type: webhook
      url: `+discordURL+`
      format: discord
      predicate:
        verdicts: [block]
`))
	if err != nil {
		t.Fatalf("allow_unrouted must permit an all-predicated config: %v", err)
	}
}

// TestDefaultStdoutSinkIsUnconditional guards the no-output-block path:
// the default config must never trip the catch-all rule.
func TestDefaultStdoutSinkIsUnconditional(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, "ffmpeg:\n  max_frames: 8\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Output.Sinks[0].Predicate.IsEmpty() {
		t.Error("the default stdout sink must be unconditional")
	}
}
