package cli

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/pkg/moderation"
)

// TestBuildPipelineStampsModelVersionUnderServeWiring pins the wiring order
// serve actually uses: instrument first, build the pipeline second. scan does
// not instrument, so if the wrapper swallows ModelVersion() the same model
// gets two different ModelIdentity stamps — and two different config_hashes —
// depending on which command produced the envelope.
func TestBuildPipelineStampsModelVersionUnderServeWiring(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Thresholds: config.Thresholds{}}

	direct := buildPipeline(cfg, devFakeModerator{}, nil, nil, nil, log)
	instrumented := buildPipeline(cfg, observe.InstrumentModerator(devFakeModerator{}, observe.NewMetrics()), nil, nil, nil, log)

	if instrumented.ModelID.ModelVersion != direct.ModelID.ModelVersion {
		t.Errorf("instrumenting changed model_version: %q under serve vs %q under scan",
			instrumented.ModelID.ModelVersion, direct.ModelID.ModelVersion)
	}
	if instrumented.ModelID.ConfigHash != direct.ModelID.ConfigHash {
		t.Errorf("instrumenting changed config_hash: %q under serve vs %q under scan; envelopes from the two commands would not be comparable",
			instrumented.ModelID.ConfigHash, direct.ModelID.ConfigHash)
	}
}

// labelingModerator is a moderator that declares its emitted provider
// labels, like the shieldgemma adapter does.
type labelingModerator struct{ labels []string }

func (labelingModerator) Name() string                  { return "labeling-fake" }
func (labelingModerator) Capabilities() moderation.Caps { return moderation.Caps{} }
func (labelingModerator) Close() error                  { return nil }
func (m labelingModerator) ProviderLabels() []string    { return m.labels }
func (labelingModerator) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	return moderation.NormalizedResult{}, nil
}

func thresholdsWith(labels ...string) config.Thresholds {
	t := config.Thresholds{}
	for _, l := range labels {
		t[l] = config.CategoryThreshold{}
	}
	return t
}

// TestValidateProviderLabelBoot: in override mode a declared label with no
// configured entry has NO boundaries, so it can never flag and never block.
// A typo must refuse to boot instead of silently disarming that hazard.
func TestValidateProviderLabelBoot(t *testing.T) {
	declared := []string{"sexually_explicit", "dangerous_content", "violence_gore"}

	t.Run("every declared label configured", func(t *testing.T) {
		cfg := config.Config{ProviderThresholds: config.ProviderThresholds{
			Mode:   config.ProviderModeOverride,
			Labels: thresholdsWith(declared...),
		}}
		if err := validateProviderLabelBoot(cfg, labelingModerator{declared}); err != nil {
			t.Fatalf("boot rejected a complete config: %v", err)
		}
	})

	t.Run("typo refuses to boot", func(t *testing.T) {
		// "violence_gor" is the typo: the real label is left unarmed.
		cfg := config.Config{ProviderThresholds: config.ProviderThresholds{
			Mode:   config.ProviderModeOverride,
			Labels: thresholdsWith("sexually_explicit", "dangerous_content", "violence_gor"),
		}}
		err := validateProviderLabelBoot(cfg, labelingModerator{declared})
		if err == nil {
			t.Fatal("a declared label with no configured entry must refuse to boot")
		}
		if !strings.Contains(err.Error(), "violence_gore") {
			t.Errorf("error should name the unconfigured label, got: %v", err)
		}
	})

	t.Run("key presence not value: unarmed is a valid decision", func(t *testing.T) {
		// Both fields nil means "deliberately unarmed" — an operator
		// decision, written down, and it still lands in the merged map so
		// config_hash changes and stays attributable.
		labels := thresholdsWith(declared...)
		flag := 0.8
		labels["sexually_explicit"] = config.CategoryThreshold{FlagAt: &flag}
		cfg := config.Config{ProviderThresholds: config.ProviderThresholds{
			Mode:   config.ProviderModeOverride,
			Labels: labels,
		}}
		if err := validateProviderLabelBoot(cfg, labelingModerator{declared}); err != nil {
			t.Fatalf("an explicitly unarmed label is valid: %v", err)
		}
	})

	t.Run("label keys match case-insensitively", func(t *testing.T) {
		// Viper lowercases yaml map keys and vendors are inconsistent, so
		// matching must not depend on case.
		cfg := config.Config{ProviderThresholds: config.ProviderThresholds{
			Mode:   config.ProviderModeOverride,
			Labels: thresholdsWith("SEXUALLY_EXPLICIT"),
		}}
		if err := validateProviderLabelBoot(cfg, labelingModerator{[]string{"sexually_explicit"}}); err != nil {
			t.Fatalf("case difference must not disarm a label: %v", err)
		}
	})

	t.Run("unarmed_labels satisfy the check", func(t *testing.T) {
		// "Reviewed, deliberately off" is spelled as an unarmed_labels entry
		// because viper cannot carry a valueless yaml key under labels.
		cfg := config.Config{ProviderThresholds: config.ProviderThresholds{
			Mode:          config.ProviderModeOverride,
			Labels:        thresholdsWith("sexually_explicit", "violence_gore"),
			UnarmedLabels: []string{"dangerous_content"},
		}}
		if err := validateProviderLabelBoot(cfg, labelingModerator{declared}); err != nil {
			t.Fatalf("an unarmed_labels entry must satisfy the check: %v", err)
		}
	})

	t.Run("adapter that declares nothing is unaffected", func(t *testing.T) {
		cfg := config.Config{}
		if err := validateProviderLabelBoot(cfg, devFakeModerator{}); err != nil {
			t.Fatalf("an adapter with no declaration must not be blocked: %v", err)
		}
	})
}
