package config_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// tierALoaders are the functions that produce a Tier A document.
//
// Each one takes the resolver that expands its `${VAR}` references, and that
// resolver decides what Tier A is allowed to see.
var tierALoaders = map[string]bool{
	"LoadBootstrap":  true,
	"ParseBootstrap": true,
	"ResolveNodeID":  true,
}

// storeBacked is the constructor for a resolver that reads the SEALED STORE
// first and the environment behind it. It is Tier B's, and only Tier B's.
const storeBacked = "WithStore"

// TIER A RESOLVES FROM THE ENVIRONMENT AND NOTHING ELSE.
//
// It is ADR-0011, and this is the gate behind it. Tier A holds the address of
// the secret store and the credentials that open it, so a Tier A document that
// could expand a reference OUT of that store would be asking the store for the
// key to itself — and the failure is not a loop, it is worse: whichever half
// resolves first decides, silently, and an operator who moves one value from
// the environment into the store gets a node that boots with a different
// identity, a different broker or a different database.
//
// The violation is ONE ARGUMENT at a call site, it compiles, and it makes a
// `${VAR}` in bootstrap.yaml start working — which is exactly what somebody
// will be trying to achieve when they write it.
func TestTierAIsNeverResolvedFromTheSecretStore(t *testing.T) {
	t.Parallel()
	root := moduleRootForTrust(t)

	seen := 0
	files := 0
	for _, dir := range []string{"internal", "cmd"} {
		walkSources(t, filepath.Join(root, dir), func(fset *token.FileSet, file *ast.File) {
			files++
			rel := shortName(root, fset.Position(file.Pos()).Filename)
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name, ok := calleeName(call)
				if !ok || !tierALoaders[name] {
					return true
				}
				seen++
				for i, arg := range call.Args {
					inner, ok := arg.(*ast.CallExpr)
					if !ok {
						continue
					}
					if got, ok := calleeName(inner); ok && got == storeBacked {
						t.Errorf("%s:%d: %s is given config.%s as argument %d, "+
							"so Tier A would resolve out of the sealed store it "+
							"holds the keys to — pass config.EnvOnly(), or nil, "+
							"which means the same thing. See adr/0011.",
							rel, fset.Position(inner.Pos()).Line, name,
							storeBacked, i+1)
					}
				}
				return true
			})
		})
	}
	if files == 0 {
		t.Fatal("parsed no source files — this guard was certifying nothing")
	}
	// TWO-SIDED. A loader renamed out from under this list leaves a guard
	// watching for a call nobody makes, which passes for ever.
	if seen == 0 {
		t.Fatalf("found no call to any of %v anywhere in the tree, so this "+
			"guard is watching for a shape nobody writes: the Tier A loaders "+
			"were renamed and the list needs following", keysOf(tierALoaders))
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// calleeName is the function a call names, whether it is called bare or
// through a package qualifier.
func calleeName(call *ast.CallExpr) (string, bool) {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name, true
	case *ast.SelectorExpr:
		return fn.Sel.Name, true
	}
	return "", false
}

func walkSources(t *testing.T, dir string, fn func(*token.FileSet, *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	err := sourcetree.Walk(dir, func(p string, d fs.DirEntry, err error) error {
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

func moduleRootForTrust(t *testing.T) string {
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

func shortName(root, pos string) string {
	if rel, err := filepath.Rel(root, pos); err == nil {
		return rel
	}
	return pos
}
