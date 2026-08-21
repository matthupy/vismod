package cli

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

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
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name != "help" { // cobra adds help itself
				flags = append(flags, f.Name)
			}
		})
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
