// Package toolnametest is the set of tool names internal/toolname's walk holds
// every string literal in the module to, for the suites that hold a tool
// surface to it.
//
// ONE SET, READ IN ONE PLACE: the walk itself (TestNoToolNameIsSpelledTwice)
// and every suite that checks a surface against it ask [Names], so the set a
// surface is held to is the set the walk reads, and a tool a surface offers
// that the set lacks fails in that surface's suite rather than dropping out of
// the walk unread. TEST-ONLY: it reads source files and the authority table,
// and no build of the engine imports it.
package toolnametest

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// Names is every tool name the engine offers a model, sorted: every
// tool-shaped verb of the authority table (a verb with no dot, which
// internal/agent/builtin's walks hold equal to the tools its registry and the
// operator catalogue register, in both directions) and every name
// internal/toolname declares, among them the tools a phase surface offers of
// its own, which no row names.
func Names(t testing.TB) []string {
	t.Helper()
	names := Declared(t)
	for _, action := range authz.Actions() {
		if !strings.Contains(string(action), ".") && !slices.Contains(names, string(action)) {
			names = append(names, string(action))
		}
	}
	slices.Sort(names)
	return names
}

// Declared is every string constant internal/toolname declares, read off its
// own non-test source, so a name is in the set the day it is added there,
// with nothing else to update.
func Declared(t testing.TB) []string {
	t.Helper()
	dir := filepath.Join(sourcetree.Root(t), "internal", "toolname")
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("listing internal/toolname's files: %v", err)
	}
	var names []string
	fset := token.NewFileSet()
	for _, name := range pkg.GoFiles {
		parsed, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				for _, value := range spec.(*ast.ValueSpec).Values {
					lit, ok := value.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: a constant in internal/toolname is not a string "+
							"literal, so the walk cannot read its value: %s", name,
							fset.Position(value.Pos()))
					}
					unquoted, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					names = append(names, unquoted)
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("read no constant in %s, so the set asserts nothing", dir)
	}
	return names
}
