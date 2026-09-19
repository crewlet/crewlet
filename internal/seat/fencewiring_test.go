package seat_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// carriers are the configuration types the seat fence travels through, from
// the engine down to the loop that checks it, keyed as `package.Type`.
//
// Every one of them is a plain struct literal with an optional func field, so
// a caller that leaves the field out compiles, runs, and silently disarms the
// check for whatever it built. That is not a hypothetical: the loop has
// checked a fence at the top of every round since it was written, the field
// was on its config from the same commit, and for the whole life of this
// engine the ONLY assignment in the tree was in the loop's own test. Three
// documents described the protection; nothing ran it.
//
// The rule is therefore not "the fence exists" — it did — but "every literal
// that can carry it, does". A test cannot assert that a value is correct, but
// it can assert that nobody quietly dropped the wire.
var carriers = map[string]string{
	"engine.RunnerInput": "the engine's per-turn runner input",
	"runner.Config":      "the phase runner's configuration",
	"subagent.Config":    "the delegation tool's configuration",
	"toolloop.Config":    "the model/tool loop that performs the check",
}

// Agent mode is the one executor path with no entry here, and that is a
// decision rather than an omission. A coding CLI's run is DETACHED: the phase
// suspends the moment it starts, the turn ends, and the run outlives it —
// deliberately, since its placement is on its own row and the process that
// collects it is often not the one that launched it. Fencing it would kill an
// hour of somebody's coding run on an ordinary rebalance and lose its output,
// to guard against writes the bridge's own run-scoped, expiring credential
// already bounds. What IS fenced is the resume: the turn that comes back to
// collect the run runs under the grant the collecting node holds. See
// [seat.Host.Fence].

type literal struct {
	Type string
	File string
	Line int
}

func TestEveryConfigThatCanCarryTheSeatFenceDoes(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	seen := map[string]int{}
	var missing []literal
	files := 0
	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), func(fset *token.FileSet, file *ast.File) {
			files++
			rel := shortPos(root, fset.Position(file.Pos()).Filename)
			pkg := file.Name.Name
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				name, ok := literalType(pkg, lit)
				if !ok {
					return true
				}
				if _, watched := carriers[name]; !watched {
					return true
				}
				seen[name]++
				if !hasKey(lit, "Fence") {
					missing = append(missing, literal{
						Type: name, File: rel,
						Line: fset.Position(lit.Pos()).Line,
					})
				}
				return true
			})
		})
	}
	if files == 0 {
		t.Fatal("parsed no source files — this guard was certifying nothing")
	}

	// TWO-SIDED, in the idiom internal/skipgate and internal/adr use. An
	// entry that stops matching anything is a type that was renamed or
	// deleted, and a guard silently watching for a shape nobody writes is
	// exactly the state this whole change exists to end.
	var unseen []string
	for name, what := range carriers {
		if seen[name] == 0 {
			unseen = append(unseen, name+" ("+what+")")
		}
	}
	sort.Strings(unseen)
	for _, name := range unseen {
		t.Errorf("no literal of %s exists any more, so this guard is watching for a "+
			"shape nobody writes: drop the entry or fix the name", name)
	}

	for _, m := range missing {
		t.Errorf("%s:%d: %s is built without a Fence, so the seat check is disarmed "+
			"for whatever it builds — %s. Thread the caller's fence in, or nil it "+
			"explicitly with the reason", m.File, m.Line, m.Type, carriers[m.Type])
	}
}

// literalType renders a composite literal's type as `package.Type`, resolving
// a bare name against the file's own package so a literal written inside the
// package that declares the type is caught too.
func literalType(pkg string, lit *ast.CompositeLit) (string, bool) {
	switch t := lit.Type.(type) {
	case *ast.SelectorExpr:
		ident, ok := t.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		return ident.Name + "." + t.Sel.Name, true
	case *ast.Ident:
		return pkg + "." + t.Name, true
	}
	return "", false
}

func hasKey(lit *ast.CompositeLit, key string) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == key {
			return true
		}
	}
	return false
}

// --- the walk --------------------------------------------------------------

func walkGoFiles(t *testing.T, dir string, fn func(*token.FileSet, *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		parsed, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		fn(fset, parsed)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}
	return root
}

func shortPos(root, pos string) string {
	if rel, err := filepath.Rel(root, pos); err == nil {
		return rel
	}
	return pos
}
