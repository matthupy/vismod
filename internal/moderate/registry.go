// Package moderate holds the adapter registry. The registry never imports
// adapter packages; each adapter self-registers in init() and is pulled in
// by a blank import at the composition root (internal/cli/root.go).
package moderate

import (
	"fmt"
	"sort"
	"sync"

	"github.com/vismod/vismod/pkg/moderation"
)

// AdapterConfig is the provider-opaque configuration carrier. It lives in
// internal/moderate (it carries secret wiring), not pkg/.
type AdapterConfig struct {
	Name string
	// Options is decoded by each adapter into its OWN typed config inside
	// its Factory.
	Options map[string]any
	// Secret is an env-backed secret accessor. API keys never appear in
	// Options or yaml.
	Secret func(key string) string
	// ProviderThresholdMode is the RAW provider_thresholds.mode as written
	// by the operator (off / hybrid / override / ""). config.Load resolves
	// the mode away for the runtime threshold map, so nothing downstream of
	// Load branches on it — but an adapter whose scores are only meaningful
	// under one mode has to refuse the others at construction, and
	// construction IS boot validation.
	ProviderThresholdMode string
}

// Factory builds a Moderator from its config.
type Factory func(cfg AdapterConfig) (moderation.Moderator, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a factory under name. Called from adapter init(); a
// duplicate name is a programmer error and panics.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if name == "" || f == nil {
		panic("moderate: Register with empty name or nil factory")
	}
	if _, dup := factories[name]; dup {
		panic(fmt.Sprintf("moderate: adapter %q registered twice", name))
	}
	factories[name] = f
}

// Registered returns the sorted registry keys.
func Registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// New instantiates exactly the one configured factory — this is what makes
// "exactly one model active per process" hold. An unknown adapter is fatal
// and lists the registered names.
func New(name string, cfg AdapterConfig) (moderation.Moderator, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("moderate: unknown adapter %q (registered: %v)", name, Registered())
	}
	cfg.Name = name
	return f(cfg)
}
