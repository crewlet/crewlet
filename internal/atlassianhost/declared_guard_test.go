package atlassianhost_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// clientPackages are the packages whose ClientOptions carry the company's
// declaration, by import path.
var clientPackages = map[string]bool{
	"github.com/crewlet/crewlet/internal/jira":       true,
	"github.com/crewlet/crewlet/internal/confluence": true,
}

// TestEveryAtlassianClientIsGivenTheCompanysDeclaration fails the build when
// engine code builds a Jira or Confluence client without saying what the
// company declared.
//
// The declaration is what makes an address no host list names — an Isolated
// Cloud site's — Cloud, and the validator reads it. A construction that left
// it out compiles, runs and answers Data Center for that address, so the
// client would read /rest/api/2 (Jira) or register a Data Center hook
// (Confluence) for a company the validator admitted as Cloud — the
// disagreement this package exists to end, reopened one call site at a time
// and with nothing to notice it. So every composite literal of either
// package's ClientOptions in non-test code under internal/ and cmd/ must name
// Declared, even where the value it names is a seat client inheriting the org
// client's. A literal that names it is not checked for WHAT it passes: that is
// review's, and the call sites read the one accessor
// ([config.Atlassian.Declared]).
//
// Test code is out of scope, deliberately: a test server stands in for one
// deployment and says so, or takes the host rule's answer for 127.0.0.1,
// which is Data Center.
func TestEveryAtlassianClientIsGivenTheCompanysDeclaration(t *testing.T) {
	t.Parallel()

	// A guard asserting an absence passes identically when the absence is
	// real and when the matcher has stopped matching, so the matcher runs
	// on literals whose verdicts are known first.
	for src, wantMissing := range map[string]bool{
		`package p; import "github.com/crewlet/crewlet/internal/jira"; var _ = jira.ClientOptions{URL: "u"}`:                                     true,
		`package p; import c "github.com/crewlet/crewlet/internal/confluence"; var _ = c.ClientOptions{URL: "u", Declared: d}`:                   false,
		`package jira; var _ = ClientOptions{URL: "u"}`:                                                                                          true,
		`package p; import "github.com/crewlet/crewlet/internal/github"; var _ = github.ClientOptions{APIBase: "u"}`:                             false,
		`package p; import "github.com/crewlet/crewlet/internal/jira"; func f() { _ = &jira.ClientOptions{Token: "t", Declared: jira.Unknown} }`: false,
	} {
		found, missing := scanForDeclared(t, "control.go", src)
		if wantMissing && len(missing) != 1 {
			t.Errorf("control: %q lacks Declared and the matcher did not flag it (found %d)", src, found)
		}
		if !wantMissing && len(missing) != 0 {
			t.Errorf("control: %q was flagged: %v", src, missing)
		}
	}

	root := sourcetree.Root(t)
	var found int
	for _, dir := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			n, missing := scanForDeclared(t, path, "")
			found += n
			for _, at := range missing {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%s builds an Atlassian client without the company's "+
					"declaration: set Declared to the company's "+
					"Integrations.Atlassian.Declared()", rel, at)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The engine and the CLI build both clients in more than one place; a
	// walk that found none is a walk that stopped reading the tree.
	if found < 10 {
		t.Fatalf("found only %d Jira or Confluence client constructions; the walk "+
			"no longer reaches the code that builds them", found)
	}
}

// scanForDeclared parses one file (src, or the file at path when src is
// empty) and answers how many ClientOptions literals of the Atlassian client
// packages it holds and the position of each that does not name Declared.
func scanForDeclared(t *testing.T, path, src string) (found int, missing []string) {
	t.Helper()
	fset := token.NewFileSet()
	var source any
	if src != "" {
		source = src
	}
	file, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	// Which local names this file reaches the client packages by, and
	// whether it IS one of them, where the type is written unqualified.
	local := map[string]bool{}
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !clientPackages[p] {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		local[name] = true
	}
	inside := (file.Name.Name == "jira" || file.Name.Name == "confluence") &&
		(src != "" || clientPackages["github.com/crewlet/crewlet/internal/"+filepath.Base(filepath.Dir(path))])

	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		switch typ := lit.Type.(type) {
		case *ast.SelectorExpr:
			pkg, ok := typ.X.(*ast.Ident)
			if !ok || !local[pkg.Name] || typ.Sel.Name != "ClientOptions" {
				return true
			}
		case *ast.Ident:
			if !inside || typ.Name != "ClientOptions" {
				return true
			}
		default:
			return true
		}
		found++
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Declared" {
					return true
				}
			}
		}
		pos := fset.Position(lit.Pos())
		missing = append(missing, strconv.Itoa(pos.Line)+":"+strconv.Itoa(pos.Column))
		return true
	})
	return found, missing
}
