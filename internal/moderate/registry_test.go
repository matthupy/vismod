package moderate

import (
	"context"
	"strings"
	"testing"

	"github.com/matthupy/vismod/pkg/moderation"
)

type fakeMod struct{ name string }

func (f fakeMod) Name() string                  { return f.name }
func (f fakeMod) Capabilities() moderation.Caps { return moderation.Caps{} }
func (f fakeMod) Close() error                  { return nil }
func (f fakeMod) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	return moderation.NormalizedResult{}, nil
}

func TestRegisterAndNew(t *testing.T) {
	Register("fake_reg_test", func(cfg AdapterConfig) (moderation.Moderator, error) {
		return fakeMod{name: cfg.Name}, nil
	})

	m, err := New("fake_reg_test", AdapterConfig{Name: "fake_reg_test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.Name() != "fake_reg_test" {
		t.Fatalf("got name %q", m.Name())
	}
}

func TestNewUnknownListsRegistered(t *testing.T) {
	_, err := New("does_not_exist", AdapterConfig{})
	if err == nil {
		t.Fatal("expected error for unknown adapter")
	}
	if !strings.Contains(err.Error(), "unknown adapter") {
		t.Fatalf("error should name the problem: %v", err)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	Register("dup_test", func(AdapterConfig) (moderation.Moderator, error) { return nil, nil })
	Register("dup_test", func(AdapterConfig) (moderation.Moderator, error) { return nil, nil })
}
