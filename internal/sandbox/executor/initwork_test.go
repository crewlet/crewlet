package executor_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// workingAtInit is every package this package's test binary links whose init
// starts work that runs beside the cases, each with what it does, from reading
// its init. It may only shrink: a package added to the binary that starts work
// at init fails [TestTheTestBinaryLinksNothingThatWorksFromInit], and an entry
// the binary no longer links fails it too, so it goes.
var workingAtInit = map[string]string{
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest": "its init starts a goroutine only in the " +
		"copy of the test binary it re-executes (exitedMainEnv set), which runs no case; in this process it " +
		"returns at once",
}

// neverLinked is refused by name, whatever reading its init finds: nats-server's
// process sampler, which #2160's readings carried, and the package that links
// it. A change to how pse arms its timer must not let it back unseen.
var neverLinked = []string{
	"github.com/nats-io/nats-server/v2/server",
	"github.com/nats-io/nats-server/v2/server/pse",
}

// THE TEST BINARY LINKS NOTHING THAT WORKS FROM INIT BUT WHAT workingAtInit
// NAMES: such work runs beside every case that reads a reader's allocations
// off the process's counts, as nats-server's process sampler (server/pse, its
// init re-arming a time.AfterFunc every second) ran beside #2160's, linked
// through internal/jsprovision. Each linked package outside the standard
// library is read: an init whose body, or a function of its package it calls,
// starts a goroutine or arms a timer (time.AfterFunc, NewTimer, NewTicker,
// Tick) works from init. A timer is no goroutine until it fires, so only
// reading the init sees one.
//
// Mutation: blank-import internal/jsapi, which links nats-server's server
// package, from a test file of this package.
func TestTheTestBinaryLinksNothingThatWorksFromInit(t *testing.T) {
	t.Parallel()
	pkgs, err := testBinaryPackages(t)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	read := 0
	for _, p := range pkgs {
		if slices.Contains(neverLinked, p.ImportPath) {
			t.Errorf("this package's test binary links %s, nats-server's process sampler or the package "+
				"that links it, which allocates every second beside every case measuring a reader's "+
				"allocations; drop the import that reaches it", p.ImportPath)
		}
		// The test binary's own main, generated into the build cache, is
		// none of the linked packages.
		if p.Standard || strings.HasSuffix(p.ImportPath, ".test") {
			continue
		}
		read++
		if worksFromInit(t, p.Dir, p.GoFiles) {
			found[p.ImportPath] = true
		}
	}
	if read == 0 {
		t.Fatal("the walk read no package outside the standard library, so it proved nothing")
	}
	for _, path := range slices.Sorted(func(yield func(string) bool) {
		for p := range found {
			if !yield(p) {
				return
			}
		}
	}) {
		if _, named := workingAtInit[path]; !named {
			t.Errorf("this package's test binary links %s, whose init starts a goroutine or arms a timer "+
				"that runs beside every case measuring a reader's allocations; drop the import that links "+
				"it, or name it in workingAtInit with what it does", path)
		}
	}
	for path := range workingAtInit {
		if !found[path] {
			t.Errorf("workingAtInit names %s, which this package's test binary no longer links or which no "+
				"longer works from init; remove it", path)
		}
	}
}

// linkedPackage is one package the test binary links, as go list names it.
type linkedPackage struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Standard   bool
}

// testBinaryPackages is every package this package's test binary links, with
// the files each builds on this platform. It asks the go command, whose
// -deps -test closure is the package graph it compiles into the binary
// (the test files' imports, transitively, under this platform's constraints),
// rather than walking go/build over the module and resolving it a second way.
func testBinaryPackages(t *testing.T) ([]linkedPackage, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-test",
		"-json=ImportPath,Dir,GoFiles,Standard", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, errors.New("go list -deps -test: " + err.Error() + ": " + stderr.String())
	}
	var pkgs []linkedPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p linkedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return pkgs, nil
		} else if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
}

// worksFromInit reports whether a package of files in dir starts a goroutine
// or arms a timer from an init: in its body, or in a function of the package
// it calls, followed by name through the package's own functions.
func worksFromInit(t *testing.T, dir string, files []string) bool {
	t.Helper()
	fset := token.NewFileSet()
	funcs := map[string][]*ast.BlockStmt{}
	var inits []*ast.BlockStmt
	for _, name := range files {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil {
				continue
			}
			if fn.Name.Name == "init" {
				inits = append(inits, fn.Body)
				continue
			}
			funcs[fn.Name.Name] = append(funcs[fn.Name.Name], fn.Body)
		}
	}
	seen := map[string]bool{}
	queue := inits
	for len(queue) > 0 {
		body := queue[0]
		queue = queue[1:]
		works := false
		ast.Inspect(body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.GoStmt:
				works = true
			case *ast.CallExpr:
				switch fun := n.Fun.(type) {
				case *ast.SelectorExpr:
					if x, ok := fun.X.(*ast.Ident); ok && x.Name == "time" &&
						strings.Contains(" AfterFunc NewTimer NewTicker Tick ", " "+fun.Sel.Name+" ") {
						works = true
					}
				case *ast.Ident:
					if !seen[fun.Name] {
						seen[fun.Name] = true
						queue = append(queue, funcs[fun.Name]...)
					}
				}
			}
			return !works
		})
		if works {
			return true
		}
	}
	return false
}
