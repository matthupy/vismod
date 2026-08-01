package moderate

import (
	"context"
	"strings"
	"testing"

	"github.com/vismod/vismod/pkg/moderation"
)

type stubModerator struct{ name string }

func (s stubModerator) Name() string { return s.name }
func (s stubModerator) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	return moderation.NormalizedResult{}, nil
}
func (s stubModerator) Capabilities() moderation.Caps { return moderation.Caps{} }
func (s stubModerator) Close() error                  { return nil }

func TestRegistryRegisterAndNew(t *testing.T) {
	Register("test-a", func(cfg AdapterConfig) (moderation.Moderator, error) {
		if cfg.Name != "test-a" {
			t.Errorf("factory got Name=%q, want test-a", cfg.Name)
		}
		return stubModerator{name: "test-a"}, nil
	})
	Register("test-b", func(AdapterConfig) (moderation.Moderator, error) {
		return stubModerator{name: "test-b"}, nil
	})

	m, err := New("test-a", AdapterConfig{})
	if err != nil {
		t.Fatalf("New(test-a): %v", err)
	}
	if m.Name() != "test-a" {
		t.Errorf("got %q, want test-a", m.Name())
	}
}

func TestRegistryUnknownAdapterListsRegistered(t *testing.T) {
	_, err := New("nope", AdapterConfig{})
	if err == nil {
		t.Fatal("want error for unknown adapter")
	}
	if !strings.Contains(err.Error(), "unknown adapter") || !strings.Contains(err.Error(), "test-a") {
		t.Errorf("error should name the unknown adapter and list registered ones, got: %v", err)
	}
}

func TestRegistryDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register should panic")
		}
	}()
	Register("dup", func(AdapterConfig) (moderation.Moderator, error) { return nil, nil })
	Register("dup", func(AdapterConfig) (moderation.Moderator, error) { return nil, nil })
}
