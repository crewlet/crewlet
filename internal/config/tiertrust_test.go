package config_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
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
	scan := tierALoads(t, sourcetree.Root(t))
	for _, offence := range scan.offences {
		t.Error(offence)
	}
	if scan.files == 0 {
		t.Fatal("read no source files — this guard was certifying nothing")
	}
	// TWO-SIDED. A loader renamed out from under this list leaves a guard
	// watching for a call nobody makes, which passes for ever.
	if scan.seen == 0 {
		t.Fatalf("found no call to any of %v anywhere in the tree, so this "+
			"guard is watching for a shape nobody writes: the Tier A loaders "+
			"were renamed and the list needs following", keysOf(tierALoaders))
	}
}

// tierAScan is what one walk for Tier A loads found.
type tierAScan struct {
	// files is every non-test Go file read, counted before any is skipped;
	// seen is the loader calls among them.
	files, seen int
	offences    []string
}

// tierALoads walks root's internal/ and cmd/ for calls of a Tier A loader,
// and reports each handed the store-backed resolver.
//
// ONLY A FILE THAT NAMES A LOADER IS PARSED: a call spells its callee, and an
// identifier has no escapes, so a file whose bytes hold none of tierALoaders'
// names makes no such call (sourcetree.Identifiers). Parsing all eleven
// hundred files to find the handful that do was four seconds under the race
// detector.
func tierALoads(t *testing.T, root string) tierAScan {
	t.Helper()
	names := sourcetree.MustIdentifiers(keysOf(tierALoaders)...)
	var scan tierAScan
	for _, dir := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			scan.files++
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if !names.In(src) {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel := shortName(root, p)
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name, ok := calleeName(call)
				if !ok || !tierALoaders[name] {
					return true
				}
				scan.seen++
				for i, arg := range call.Args {
					inner, ok := arg.(*ast.CallExpr)
					if !ok {
						continue
					}
					if got, ok := calleeName(inner); ok && got == storeBacked {
						scan.offences = append(scan.offences, fmt.Sprintf("%s:%d: %s is "+
							"given config.%s as argument %d, so Tier A would resolve out "+
							"of the sealed store it holds the keys to — pass "+
							"config.EnvOnly(), or nil, which means the same thing. See "+
							"adr/0011.", rel, fset.Position(inner.Pos()).Line, name,
							storeBacked, i+1))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return scan
}

// THE WALK AND THE MATCHER, ON A TREE WHOSE VERDICT IS KNOWN: a loader handed
// the store-backed resolver, one handed the environment, and a file naming no
// loader that is not even Go — which the walk must never parse.
func TestTheTierAWalkFindsALoaderGivenTheStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"cmd/crewlet/boot.go": "package main\n\nfunc boot() {\n" +
			"\t_, _ = config.LoadBootstrap(path, config.WithStore(s))\n" +
			"\t_, _ = config.ParseBootstrap(raw, config.EnvOnly())\n}\n",
		"internal/x/x.go": "package x\n\nthis is not Go and names no loader\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scan := tierALoads(t, root)
	if scan.files != 2 || scan.seen != 2 || len(scan.offences) != 1 ||
		!strings.HasPrefix(scan.offences[0], filepath.FromSlash("cmd/crewlet/boot.go")+":4:") {
		t.Errorf("files %d, loader calls %d, offences %q; want 2, 2 and the one at boot.go:4",
			scan.files, scan.seen, scan.offences)
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

func shortName(root, pos string) string {
	if rel, err := filepath.Rel(root, pos); err == nil {
		return rel
	}
	return pos
}
