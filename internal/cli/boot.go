package cli

import (
	"context"
	"fmt"
	"reflect"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/pkg/moderation"
)

// Serve boots the worker stack and runs it until ctx is done, then drains.
//
// It is the entry point for a caller running the serve stack IN-PROCESS —
// the eval harness, whose capture and replay wrappers sit on the
// moderation.Moderator and therefore have nowhere to install themselves in a
// separately launched `vismod serve`. Such a caller boots with
// WithModeratorDecorator, submits over the intake HTTP surface, watches the
// depth metrics, and cancels ctx to drain.
//
// Serve installs NO signal handler. Cancelling ctx is the caller's business,
// and a library call that silently claimed SIGINT would fight the process
// that owns it; the cobra path keeps that responsibility (see runServe).
//
// A boot failure is wrapped as "boot: …" so a caller can tell a stack that
// never started from one that started and failed to drain — the two demand
// different answers, and for the eval harness they are the difference
// between a broken run and a scored one.
func Serve(ctx context.Context, cfg config.Config, opts ...ServerOption) error {
	s, err := newServer(cfg, opts...)
	if err != nil {
		return fmt.Errorf("boot: %w", err)
	}
	defer s.close()
	return s.run(ctx)
}

// ServerOption configures an optional seam on the assembled server.
//
// It is a variadic option rather than a second parameter for the same
// reason serveIntake takes one (see intakeOption): every existing
// caller and every existing serve test still describes boot exactly as it
// did before the seam existed, which is what makes "the wiring is unchanged
// when no option is passed" a fact rather than a claim.
type ServerOption func(*serverOptions)

type serverOptions struct {
	// decorateModerator wraps the ONE Moderator this process builds.
	// decoratorSet distinguishes "no hook" from "a hook that is nil": the
	// second is a caller bug and must fail boot, because a hook silently
	// skipped is a caller that believes it is wrapping and is not.
	decorateModerator func(moderation.Moderator) moderation.Moderator
	decoratorSet      bool
}

// WithModeratorDecorator lets a caller that builds the server IN-PROCESS
// wrap the Moderator buildModerator returned, before instrumentation and
// before the pipeline is built around it.
//
// The hook type is deliberately GENERIC. internal/cli is the composition
// root — the one place adapters are wired — and naming a concrete wrapper
// here would invert the dependency: the composition root would import that
// tooling, and code that has no business in the shipped binary would become
// reachable from it. All this package knows is that a caller may wrap what
// buildModerator returned. It wraps the single Moderator; it never permits
// a second one (invariant 8), and it is not a route for configuration or
// credentials — those stay env-only via config.Secret (invariant 4).
//
// Passing the option twice keeps the last hook: one Moderator, one wrap.
func WithModeratorDecorator(fn func(moderation.Moderator) moderation.Moderator) ServerOption {
	return func(o *serverOptions) { o.decorateModerator, o.decoratorSet = fn, true }
}

// applyModeratorDecorator applies a caller-supplied hook to the built
// adapter, and refuses to boot on any of the three ways the hook can hand
// back something the rest of boot would then use wrongly and silently.
//
// A nil hook, or one that returns nil, is a boot failure. A nil Moderator
// would panic on the first job — after the worker had already accepted it —
// and the fail-safe answer to a caller bug is refusing to start, not a
// worker that cannot score what it takes in.
//
// A hook that DROPS an optional interface the adapter satisfied is also a
// boot failure, and this is the arm that has to be structural rather than
// documented. Everything downstream of this point reads capabilities off the
// moderator by type assertion — observe.InstrumentModerator, buildPipeline's
// ModelIdentity stamp, the pipeline's video branch — and a type assertion
// sees only the method set in front of it. A wrapper that forwards nothing
// does not error and does not log; the capability just disappears (see
// observe.InstrumentModerator's godoc, which pays a type per combination for
// exactly this reason). The two that are assertable:
//
//   - ModelVersion(): dropping it stamps model_version "unversioned" on every
//     envelope and audit record and computes ConfigHash over that string, so
//     the run's central auditable question — which model scored this? — is
//     answered wrong, forever, with nothing to notice it by.
//   - AnalyzeVideo(): dropping it silently falls back to frame extraction
//     against a provider that analyzes video natively.
//
// The check is one-directional: it requires FORWARDING, not invention. A
// decorator over an adapter that never declared the capability is fine, and a
// decorator is still free to add one.
//
// Close() cannot be guarded this way — it is part of moderation.Moderator, so
// every decorator has it and none can be asserted for. A decorator that
// implements Close() without forwarding leaks the adapter at shutdown and on
// every later boot-failure arm below. That one stays a documented contract.
func applyModeratorDecorator(mod moderation.Moderator, fn func(moderation.Moderator) moderation.Moderator) (moderation.Moderator, error) {
	if fn == nil {
		return nil, fmt.Errorf("moderator decorator: the hook is nil")
	}
	_, wasVersioned := mod.(modelVersioner)
	_, wasVideo := mod.(moderation.VideoModerator)

	wrapped := fn(mod)
	if isNilModerator(wrapped) {
		return nil, fmt.Errorf("moderator decorator: the hook returned nil, leaving nothing to score the jobs this worker would accept")
	}
	if _, ok := wrapped.(modelVersioner); wasVersioned && !ok {
		return nil, fmt.Errorf("moderator decorator: the hook dropped ModelVersion(); every envelope and audit record would stamp model_version %q and hash the config over that string", unversionedModel)
	}
	if _, ok := wrapped.(moderation.VideoModerator); wasVideo && !ok {
		return nil, fmt.Errorf("moderator decorator: the hook dropped AnalyzeVideo(); video jobs would fall back to frame extraction against a provider that analyzes video natively")
	}
	return wrapped, nil
}

// isNilModerator reports whether m carries nothing usable, including the
// typed-nil case a plain `m == nil` misses: a hook returning a nil *T hands
// back a non-nil interface holding a nil pointer, so the guard above would
// pass and the worker would panic on the first job it had already accepted.
// wire.go's newFetcher guards the same trap for fetch.New; there the source
// is a constructor this repo controls, so an untyped-nil check is enough.
// Here the source is caller code, so the check has to look through the
// interface.
func isNilModerator(m moderation.Moderator) bool {
	if m == nil {
		return true
	}
	switch v := reflect.ValueOf(m); v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return v.IsNil()
	default:
		return false
	}
}
