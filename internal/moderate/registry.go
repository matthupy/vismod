// Package moderate holds the adapter registry. It maps adapter names to
// factories so that exactly one Moderator is instantiated per process from
// config at startup.
//
// CRITICAL: this package NEVER imports adapter packages. Adapters self-register
// via init() and are pulled in by blank import at the composition root
// (internal/cli). This keeps the registry free of provider dependencies and
// makes "exactly one model active" a structural guarantee — New instantiates
// only the single configured factory.
package moderate

import (
	"fmt"
	"sort"
	"sync"

	"github.com/matthupy/vismod/pkg/moderation"
)

// AdapterConfig is a provider-opaque carrier passed to a Factory. It lives here
// (not in pkg/) because it carries secret wiring. Each adapter decodes Options
// into its own typed config inside its Factory; secrets come only via Secret.
type AdapterConfig struct {
	Name    string
	Options map[string]any          // adapter-specific, decoded inside the Factory
	Secret  func(key string) string // env-backed secret accessor; keeps keys out of yaml
}

// Factory builds a Moderator from an AdapterConfig.
type Factory func(cfg AdapterConfig) (moderation.Moderator, error)

var (
	mu       sync.RWMutex
	registry = map[string]Factory{}
)

// Register adds an adapter factory under name. Called from adapter init().
// Panics on duplicate registration (a programming error).
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("moderate: adapter %q registered twice", name))
	}
	registry[name] = f
}

// New instantiates the single configured adapter. Unknown name is fatal and
// lists the registered names.
func New(name string, cfg AdapterConfig) (moderation.Moderator, error) {
	mu.RLock()
	f, ok := registry[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("moderate: unknown adapter %q; registered: %v", name, Names())
	}
	return f(cfg)
}

// Names returns the sorted registered adapter names.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
