package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/pkg/moderation"
)

// countingModerator stands in for the eval harness's capturing moderator
// WITHOUT internal/cli knowing anything about it: it wraps whatever it is
// handed, counts the calls it forwards, and forwards ModelVersion() the way
// a capture wrapper has to (see observe.InstrumentModerator's godoc — a
// swallowed optional interface fails silently).
type countingModerator struct {
	inner moderation.Moderator
	calls *atomic.Int64
}

func (c countingModerator) Name() string                  { return c.inner.Name() }
func (c countingModerator) Capabilities() moderation.Caps { return c.inner.Capabilities() }
func (c countingModerator) Close() error                  { return c.inner.Close() }
func (c countingModerator) ModelVersion() string          { return modelVersion(c.inner) }

func (c countingModerator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	c.calls.Add(1)
	return c.inner.AnalyzeImage(ctx, img)
}

// opaqueModerator is the other half of the contract: a wrapper that forwards
// NOTHING optional. It is what proves a boot check reading an optional
// interface must run before any wrapping.
type opaqueModerator struct{ inner moderation.Moderator }

func (o opaqueModerator) Name() string                  { return o.inner.Name() }
func (o opaqueModerator) Capabilities() moderation.Caps { return o.inner.Capabilities() }
func (o opaqueModerator) Close() error                  { return o.inner.Close() }
func (o opaqueModerator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	return o.inner.AnalyzeImage(ctx, img)
}

// labelDeclaringModerator is a registered adapter that declares the provider
// labels it can emit, like shieldgemma does. It exists so a decorator can be
// installed on a boot that validateProviderLabelBoot must still refuse.
type labelDeclaringModerator struct{ scriptedModerator }

func (labelDeclaringModerator) Name() string             { return "cli-test-labeled" }
func (labelDeclaringModerator) ProviderLabels() []string { return []string{"cli-test/violence"} }

// closeCountingModerator records that the adapter it wraps was closed. A
// boot that fails after buildModerator must not leak the adapter it built.
type closeCountingModerator struct{ scriptedModerator }

var testAdapterCloses atomic.Int64

func (closeCountingModerator) Name() string { return "cli-test-closes" }
func (closeCountingModerator) Close() error {
	testAdapterCloses.Add(1)
	return nil
}

func init() {
	moderate.Register("cli-test-labeled", func(moderate.AdapterConfig) (moderation.Moderator, error) {
		return labelDeclaringModerator{}, nil
	})
	moderate.Register("cli-test-closes", func(moderate.AdapterConfig) (moderation.Moderator, error) {
		return closeCountingModerator{}, nil
	})
}

// TestNewServerAppliesModeratorDecorator: the seam itself. An in-process
// caller must be able to wrap the ONE Moderator the process builds
// (invariant 8 — wrap it, never build a second), and the wrap has to land
// where a capture wrapper can see every billed call: on what buildModerator
// returned, before instrumentation and before buildPipeline.
func TestNewServerAppliesModeratorDecorator(t *testing.T) {
	var applied atomic.Int64
	var analyzed atomic.Int64
	var handed moderation.Moderator

	s, err := newServer(serveConfig(t), withModeratorDecorator(func(m moderation.Moderator) moderation.Moderator {
		applied.Add(1)
		handed = m
		return countingModerator{inner: m, calls: &analyzed}
	}))
	if err != nil {
		t.Fatalf("newServer with a decorator: %v", err)
	}
	defer s.close()

	if got := applied.Load(); got != 1 {
		t.Fatalf("decorator applied %d times, want exactly 1: the process has ONE Moderator (invariant 8)", got)
	}
	// Before instrumentation: the hook must be handed the adapter itself,
	// not observe.InstrumentModerator's wrapper. A capture wrapper installed
	// outside the instrumentation would count the wrapper's calls, and a
	// replay wrapper would still let the real vendor call through.
	if _, ok := handed.(scriptedModerator); !ok {
		t.Errorf("hook received %T, want the unwrapped adapter (scriptedModerator); the hook is being applied after instrumentation", handed)
	}
	// Before buildPipeline: everything the pipeline analyzes must route
	// through the decorator, or a capture sees nothing.
	if _, err := s.pipeline.Moderator.AnalyzeImage(t.Context(), moderation.Image{Bytes: []byte("OK"), MIME: "image/jpeg"}); err != nil {
		t.Fatalf("AnalyzeImage through the pipeline's moderator: %v", err)
	}
	if got := analyzed.Load(); got != 1 {
		t.Errorf("the decorator saw %d of 1 analyze calls; the pipeline was built around the undecorated moderator", got)
	}
}

// TestServeWiringUnchangedWithoutDecorator: the hook is optional, and an
// optional seam that changes the default wiring is not optional. With no
// hook the moderator serve runs is exactly the instrumented adapter — no
// extra layer, and the same ModelIdentity (adapter, model_version and the
// config_hash computed from them) as before the seam existed.
func TestServeWiringUnchangedWithoutDecorator(t *testing.T) {
	c := serveConfig(t)
	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()

	want := observe.InstrumentModerator(scriptedModerator{}, observe.NewMetrics())
	if got, wantT := fmt.Sprintf("%T", s.mod), fmt.Sprintf("%T", want); got != wantT {
		t.Errorf("serve's moderator is %s, want %s: an unrequested layer is wrapping the adapter", got, wantT)
	}
	if got, wantT := fmt.Sprintf("%T", s.pipeline.Moderator), fmt.Sprintf("%T", want); got != wantT {
		t.Errorf("the pipeline's moderator is %s, want %s", got, wantT)
	}
	ref := buildPipeline(c, want, nil, nil, nil, testLogger())
	if s.pipeline.ModelID != ref.ModelID {
		t.Errorf("ModelIdentity = %+v, want %+v: envelopes would stop being comparable across versions", s.pipeline.ModelID, ref.ModelID)
	}
	if s.pipeline.ModelID.ModelVersion != "cli-test-v1" {
		t.Errorf("model_version = %q, want the adapter's own version", s.pipeline.ModelID.ModelVersion)
	}
}

// TestDecoratedModeratorKeepsModelVersionOnEnvelope: instrumentation reads
// ModelVersion() through a type assertion, so a decorator sits between the
// adapter and that assertion. If the seam broke the chain every envelope
// would silently stamp "unversioned" and compute config_hash over it — the
// same audit question answered two ways depending on who booted the server.
// Asserted on a real envelope off the sink, not on the wiring.
func TestDecoratedModeratorKeepsModelVersionOnEnvelope(t *testing.T) {
	c := serveConfig(t)
	var analyzed atomic.Int64
	s, err := newServer(c, withModeratorDecorator(func(m moderation.Moderator) moderation.Moderator {
		return countingModerator{inner: m, calls: &analyzed}
	}))
	if err != nil {
		t.Fatalf("newServer with a decorator: %v", err)
	}
	defer s.close()

	input := writeInput(t, "bad.jpg", "BLOCK")
	if _, err := s.queue.Enqueue(context.Background(), queue.Job{
		ID:          "serve-decorated-1",
		Source:      moderation.Source{Kind: "file", Ref: input, MediaType: "image"},
		SubmittedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	stop := runServer(t, s)
	body := waitForFile(t, sinkPath(c))
	stop()

	var env struct {
		ModelID struct {
			Adapter      string `json:"adapter"`
			ModelVersion string `json:"model_version"`
			ConfigHash   string `json:"config_hash"`
		} `json:"model_id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &env); err != nil {
		t.Fatalf("envelope is not JSON (%v): %s", err, body)
	}
	if env.ModelID.ModelVersion != "cli-test-v1" {
		t.Errorf("model_version = %q, want %q: the decorator swallowed the adapter's version", env.ModelID.ModelVersion, "cli-test-v1")
	}
	if env.ModelID.ConfigHash == "" {
		t.Error("config_hash is empty; a verdict that cannot be traced to its tuning is not auditable")
	}
	if analyzed.Load() != 1 {
		t.Errorf("the decorator saw %d of 1 billed calls; a capture wrapper installed this way would undercount", analyzed.Load())
	}
}

// TestCLIHasNoEvalImports is the dependency-direction assertion the generic
// hook exists to make true. internal/cli is the composition root — the one
// place adapters are wired — so if it ever names a capturing or replay type
// the eval tooling becomes reachable from the shipped binary. The hook knows
// only that a caller may wrap what buildModerator returned.
//
// The walk is over the module's own source (no toolchain subprocess), and it
// is transitive: an eval package pulled in through internal/pipeline would
// be just as fatal as a direct import.
func TestCLIHasNoEvalImports(t *testing.T) {
	const mod = "github.com/vismod/vismod"
	forbidden := []string{"eval", "captur", "replay", "cassette"}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate module root: %v", err)
	}
	pkgs := modulePackages(t, root, mod)

	// BFS from the composition root over module-internal edges.
	reached := map[string]bool{}
	frontier := []string{mod + "/internal/cli"}
	for len(frontier) > 0 {
		p := frontier[0]
		frontier = frontier[1:]
		if reached[p] {
			continue
		}
		reached[p] = true
		for _, imp := range pkgs[p] {
			low := strings.ToLower(imp)
			for _, bad := range forbidden {
				if strings.Contains(low, bad) {
					t.Errorf("%s imports %q: internal/cli must not reach eval/capturing/replay code, or the eval binary becomes reachable from the production image", p, imp)
				}
			}
			if strings.HasPrefix(imp, mod+"/") && !reached[imp] {
				frontier = append(frontier, imp)
			}
		}
	}
	// A walk that reached nothing would pass this test for the wrong
	// reason, so pin a few packages internal/cli demonstrably depends on.
	for _, must := range []string{"/internal/pipeline", "/internal/queue", "/internal/observe", "/pkg/moderation"} {
		if !reached[mod+must] {
			t.Fatalf("import walk never reached %s%s; the assertion is not actually walking the graph", mod, must)
		}
	}
}

// modulePackages parses every non-test source file under root and returns
// each module package's import list, keyed by import path.
func modulePackages(t *testing.T, root, mod string) map[string][]string {
	t.Helper()
	pkgs := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		pkg := mod
		if rel != "." {
			pkg = mod + "/" + filepath.ToSlash(rel)
		}
		for _, spec := range f.Imports {
			pkgs[pkg] = append(pkgs[pkg], strings.Trim(spec.Path.Value, `"`))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module source: %v", err)
	}
	return pkgs
}

// flagName matches a pflag declaration line ("  -c, --config string   ...")
// and captures the long name.
var flagName = regexp.MustCompile(`^\s+(?:-\w, )?--([\w-]+)`)

// TestVismodCommandSurfaceUnchanged: the seam is for an in-process caller,
// so the shipped binary must not grow a way to reach it. cmd/vismod is a
// three-line main over this command tree, so the tree IS the surface — a new
// subcommand or flag here is a new operator-facing feature, and an
// eval/capture switch on the production binary is exactly what the design
// forbids.
func TestVismodCommandSurfaceUnchanged(t *testing.T) {
	want := map[string]string{
		"vismod":                    "config",
		"vismod adapters":           "",
		"vismod audit":              "",
		"vismod audit verify":       "",
		"vismod healthcheck":        "url",
		"vismod scan":               "dedup-threshold,metadata,workflow",
		"vismod serve":              "",
		"vismod version":            "",
		"vismod workflows":          "",
		"vismod workflows list":     "",
		"vismod workflows validate": "",
	}
	got := map[string]string{}
	var walk func(prefix string, c *cobra.Command)
	walk = func(prefix string, c *cobra.Command) {
		name := c.Name()
		if name == "help" || name == "completion" { // cobra's own, not ours
			return
		}
		path := strings.TrimSpace(prefix + " " + name)
		var flags []string
		// Read the names out of the rendered usage rather than visiting the
		// FlagSet: VisitAll needs *pflag.Flag by name, and importing pflag
		// here would promote a transitive dependency to a direct one in
		// go.mod, which is a lot of churn for a guard test. flagName only
		// matches at the start of a line, where pflag renders declarations.
		for _, line := range strings.Split(c.LocalFlags().FlagUsages(), "\n") {
			m := flagName.FindStringSubmatch(line)
			if m != nil && m[1] != "help" { // cobra adds help itself
				flags = append(flags, m[1])
			}
		}
		sort.Strings(flags)
		got[path] = strings.Join(flags, ",")
		for _, sub := range c.Commands() {
			walk(path, sub)
		}
	}
	walk("", rootCmd)

	if len(got) != len(want) {
		t.Errorf("command tree has %d commands, want %d:\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
	for path, flags := range got {
		w, ok := want[path]
		if !ok {
			t.Errorf("new command %q on the shipped binary; the eval seam is for in-process callers only", path)
			continue
		}
		if flags != w {
			t.Errorf("command %q flags = %q, want %q", path, flags, w)
		}
	}
}

// TestProviderLabelBootValidatesUnwrappedModerator: the boot check reads an
// optional interface (ProviderLabels) off the adapter, and a wrapper that
// does not forward it makes the check silently pass. A check that depends on
// what a wrapper happens to forward is a check that has stopped checking, so
// it must run before the hook — an unarmed label in override mode can never
// flag and never block, with nothing logged.
func TestProviderLabelBootValidatesUnwrappedModerator(t *testing.T) {
	labeled := func(t *testing.T) config.Config {
		t.Helper()
		c := serveConfig(t)
		c.Adapter = config.AdapterSection{Name: "cli-test-labeled"}
		c.ProviderThresholds = config.ProviderThresholds{Mode: config.ProviderModeOverride}
		return c
	}

	t.Run("an unconfigured label refuses to boot with no decorator", func(t *testing.T) {
		s, err := newServer(labeled(t))
		if err == nil {
			s.close()
			t.Fatal("boot succeeded with a declared label that has no threshold entry")
		}
		if !strings.Contains(err.Error(), "cli-test/violence") {
			t.Errorf("boot error does not name the unconfigured label: %v", err)
		}
	})

	t.Run("a decorator that hides ProviderLabels cannot disarm the check", func(t *testing.T) {
		s, err := newServer(labeled(t), withModeratorDecorator(func(m moderation.Moderator) moderation.Moderator {
			return opaqueModerator{inner: m}
		}))
		if err == nil {
			s.close()
			t.Fatal("a wrapper that swallows ProviderLabels disarmed the boot check; validation must run on the unwrapped moderator")
		}
		if !strings.Contains(err.Error(), "cli-test/violence") {
			t.Errorf("boot error does not name the unconfigured label: %v", err)
		}
	})

	// The subject here is the label check, not opacity, so the decorator is
	// a FORWARDING one. Booting an opaque wrapper would encode "a wrapper
	// may swallow ModelVersion()" as expected behaviour, which
	// TestDecoratorMustNotDropOptionalInterfaces exists to forbid.
	t.Run("a configured label boots with the decorator installed", func(t *testing.T) {
		c := labeled(t)
		c.ProviderThresholds.Labels = config.Thresholds{"cli-test/violence": config.CategoryThreshold{}}
		s, err := newServer(c, withModeratorDecorator(func(m moderation.Moderator) moderation.Moderator {
			return countingModerator{inner: m, calls: &atomic.Int64{}}
		}))
		if err != nil {
			t.Fatalf("a fully configured adapter must boot with a decorator installed: %v", err)
		}
		s.close()
	})
}

// TestNilDecoratorResultIsBootError: a hook that returns nil (or is itself
// nil) is a caller bug, and the fail-safe answer is a boot failure with a
// named cause — not a nil Moderator that panics on the first job, after the
// worker has already accepted it. The adapter built moments earlier must be
// closed on the way out, like every other boot failure in newServer.
func TestNilDecoratorResultIsBootError(t *testing.T) {
	cases := []struct {
		name string
		hook func(moderation.Moderator) moderation.Moderator
	}{
		{"hook returns nil", func(moderation.Moderator) moderation.Moderator { return nil }},
		{"hook is nil", nil},
		// A typed nil pointer produces a NON-nil interface, so `wrapped ==
		// nil` is false and the worker would panic on the first job it had
		// already accepted — the trap wire.go's newFetcher guards for
		// fetch.New, arriving here from caller code instead.
		{"hook returns a typed nil", func(moderation.Moderator) moderation.Moderator {
			return (*nilPointerModerator)(nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := serveConfig(t)
			c.Adapter = config.AdapterSection{Name: "cli-test-closes"}
			before := testAdapterCloses.Load()

			s, err := newServer(c, withModeratorDecorator(tc.hook))
			if err == nil {
				s.close()
				t.Fatal("boot succeeded with no moderator to run jobs against")
			}
			if s != nil {
				t.Error("newServer returned both a server and an error")
			}
			if !strings.Contains(err.Error(), "decorator") {
				t.Errorf("boot error does not name the decorator as the cause: %v", err)
			}
			if got := testAdapterCloses.Load() - before; got != 1 {
				t.Errorf("adapter closed %d times on the failed boot, want 1: a failed boot must not leak the adapter", got)
			}
		})
	}
}

// videoNativeModerator is a registered adapter that analyzes video natively,
// like a video-capable vendor would. No SHIPPED adapter satisfies
// moderation.VideoModerator today, so the seam's video guard would otherwise
// have nothing to guard against until the day one does — which is exactly
// when a silently dropped AnalyzeVideo would start costing frame extractions
// against a provider that never needed them.
type videoNativeModerator struct{ scriptedModerator }

func (videoNativeModerator) Name() string { return "cli-test-video" }

func (videoNativeModerator) Capabilities() moderation.Caps {
	c := scriptedModerator{}.Capabilities()
	c.SupportsVideo = true
	return c
}

func (videoNativeModerator) AnalyzeVideo(context.Context, moderation.Source) (moderation.NormalizedResult, error) {
	return moderation.NormalizedResult{Provider: "cli-test-video"}, nil
}

// nilPointerModerator exists to be returned as a TYPED nil. A hook that
// returns (*nilPointerModerator)(nil) hands back a non-nil interface holding
// a nil pointer, so `wrapped == nil` is false — the same trap wire.go's
// newFetcher guards for fetch.New, arriving here from CALLER code rather
// than from a constructor this repo controls.
type nilPointerModerator struct{}

func (*nilPointerModerator) Name() string                  { return "cli-test-typed-nil" }
func (*nilPointerModerator) ModelVersion() string          { return "cli-test-v1" }
func (*nilPointerModerator) Close() error                  { return nil }
func (*nilPointerModerator) Capabilities() moderation.Caps { return moderation.Caps{} }
func (*nilPointerModerator) AnalyzeImage(context.Context, moderation.Image) (moderation.NormalizedResult, error) {
	return moderation.NormalizedResult{}, nil
}

// videoForwardingModerator is what a decorator over a video-native adapter
// has to look like to be legal: it forwards ModelVersion() (via
// countingModerator) AND AnalyzeVideo(). Forwarding by composition costs one
// type per combination — the same price observe.InstrumentModerator pays,
// and for the same reason.
type videoForwardingModerator struct {
	countingModerator
	video moderation.VideoModerator
}

func (v videoForwardingModerator) AnalyzeVideo(ctx context.Context, src moderation.Source) (moderation.NormalizedResult, error) {
	return v.video.AnalyzeVideo(ctx, src)
}

// bareModerator is a registered adapter that satisfies moderation.Moderator
// and NOTHING else — no ModelVersion(), no AnalyzeVideo(). It proves the
// capability guard checks FORWARDING, not invention: wrapping it opaquely is
// legal, because nothing was lost. It is written out rather than embedding
// scriptedModerator because embedding would promote ModelVersion() and make
// this exact case untestable.
type bareModerator struct{}

func (bareModerator) Name() string                  { return "cli-test-bare" }
func (bareModerator) Close() error                  { return nil }
func (bareModerator) Capabilities() moderation.Caps { return scriptedModerator{}.Capabilities() }
func (bareModerator) AnalyzeImage(ctx context.Context, img moderation.Image) (moderation.NormalizedResult, error) {
	return scriptedModerator{}.AnalyzeImage(ctx, img)
}

func init() {
	moderate.Register("cli-test-video", func(moderate.AdapterConfig) (moderation.Moderator, error) {
		return videoNativeModerator{}, nil
	})
	moderate.Register("cli-test-bare", func(moderate.AdapterConfig) (moderation.Moderator, error) {
		return bareModerator{}, nil
	})
}

// TestDecoratorMustNotDropOptionalInterfaces: the hook lands on the adapter
// itself, so every optional-interface type assertion downstream — in
// observe.InstrumentModerator, in buildPipeline, in the pipeline's video
// branch — now runs against caller code. A wrapper that forwards nothing
// does not error and does not log; the capability just disappears, and the
// worker keeps running with an audit trail that answers the "which model
// scored this?" question wrong. Fail-safe means refusing to boot.
func TestDecoratorMustNotDropOptionalInterfaces(t *testing.T) {
	cases := []struct {
		name    string
		adapter string
		hook    func(moderation.Moderator) moderation.Moderator
		wantErr string
	}{
		{
			// scriptedModerator (via closeCountingModerator) declares
			// ModelVersion(); opaqueModerator forwards nothing.
			name:    "a wrapper that drops ModelVersion refuses to boot",
			adapter: "cli-test-closes",
			hook:    func(m moderation.Moderator) moderation.Moderator { return opaqueModerator{inner: m} },
			wantErr: "ModelVersion",
		},
		{
			// countingModerator forwards ModelVersion() but has no
			// AnalyzeVideo, so it passes the first guard and must trip the
			// second: silently falling back to frame extraction is a cost
			// and fidelity change nobody asked for.
			name:    "a wrapper that drops AnalyzeVideo refuses to boot",
			adapter: "cli-test-video",
			hook: func(m moderation.Moderator) moderation.Moderator {
				return countingModerator{inner: m, calls: &atomic.Int64{}}
			},
			wantErr: "AnalyzeVideo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := serveConfig(t)
			c.Adapter = config.AdapterSection{Name: tc.adapter}

			s, err := newServer(c, withModeratorDecorator(tc.hook))
			if err == nil {
				s.close()
				t.Fatalf("boot succeeded with a decorator that swallowed %s(); the capability disappears with nothing logged", tc.wantErr)
			}
			if s != nil {
				t.Error("newServer returned both a server and an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("boot error does not name the dropped capability %s(): %v", tc.wantErr, err)
			}
			if !strings.Contains(err.Error(), "decorator") {
				t.Errorf("boot error does not name the decorator as the cause: %v", err)
			}
		})
	}
}

// TestDecoratorDroppingModelVersionClosesTheAdapter: the capability guard is
// a boot-failure arm like every other one in newServer, so it must not leak
// the adapter built moments earlier.
func TestDecoratorDroppingModelVersionClosesTheAdapter(t *testing.T) {
	c := serveConfig(t)
	c.Adapter = config.AdapterSection{Name: "cli-test-closes"}
	before := testAdapterCloses.Load()

	s, err := newServer(c, withModeratorDecorator(func(m moderation.Moderator) moderation.Moderator {
		return opaqueModerator{inner: m}
	}))
	if err == nil {
		s.close()
		t.Fatal("boot succeeded with a decorator that swallowed ModelVersion()")
	}
	if got := testAdapterCloses.Load() - before; got != 1 {
		t.Errorf("adapter closed %d times on the failed boot, want 1: a failed boot must not leak the adapter", got)
	}
}

// TestDecoratorForwardingOptionalInterfacesBoots is the other half of the
// guard: it constrains what a decorator may be, and the constraint must be
// "forward what you were handed", not "add nothing". A wrapper that forwards
// both — which the one intended caller does — still boots, and a wrapper on
// an adapter that never had the capability is not penalised for not
// inventing it.
func TestDecoratorForwardingOptionalInterfacesBoots(t *testing.T) {
	t.Run("a wrapper that forwards ModelVersion and AnalyzeVideo boots", func(t *testing.T) {
		c := serveConfig(t)
		c.Adapter = config.AdapterSection{Name: "cli-test-video"}
		s, err := newServer(c, withModeratorDecorator(func(m moderation.Moderator) moderation.Moderator {
			return videoForwardingModerator{
				countingModerator: countingModerator{inner: m, calls: &atomic.Int64{}},
				video:             m.(moderation.VideoModerator),
			}
		}))
		if err != nil {
			t.Fatalf("a forwarding decorator must boot: %v", err)
		}
		s.close()
	})

	t.Run("a wrapper over an adapter with no optional interfaces boots", func(t *testing.T) {
		c := serveConfig(t)
		c.Adapter = config.AdapterSection{Name: "cli-test-bare"}
		s, err := newServer(c, withModeratorDecorator(func(m moderation.Moderator) moderation.Moderator {
			return opaqueModerator{inner: m}
		}))
		if err != nil {
			t.Fatalf("the guard must check forwarding, not invention: %v", err)
		}
		s.close()
	})
}

// TestWithModeratorDecoratorLastOptionWins pins the documented behaviour of
// passing the option twice: the last hook is the one that runs, and it runs
// once. One Moderator, one wrap (invariant 8) — not two wraps, and not the
// first hook silently winning over the caller's later intent.
func TestWithModeratorDecoratorLastOptionWins(t *testing.T) {
	var first, second atomic.Int64
	s, err := newServer(serveConfig(t),
		withModeratorDecorator(func(m moderation.Moderator) moderation.Moderator {
			first.Add(1)
			return countingModerator{inner: m, calls: &atomic.Int64{}}
		}),
		withModeratorDecorator(func(m moderation.Moderator) moderation.Moderator {
			second.Add(1)
			return countingModerator{inner: m, calls: &atomic.Int64{}}
		}),
	)
	if err != nil {
		t.Fatalf("newServer with the option passed twice: %v", err)
	}
	defer s.close()

	if got := first.Load(); got != 0 {
		t.Errorf("the superseded hook ran %d times, want 0", got)
	}
	if got := second.Load(); got != 1 {
		t.Errorf("the last hook ran %d times, want exactly 1", got)
	}
}
