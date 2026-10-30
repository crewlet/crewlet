package authz_test

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// authzImport is this package's import path, which is how a file that names
// one of its identifiers is found.
const authzImport = "github.com/crewlet/crewlet/internal/authz"

// verbScan is what ONE walk of the tree's non-test Go files outside this
// package found about the verbs this package declares.
//
// ONE WALK for every gate that asks which code names which verb, because the
// gates differ only in the question they ask of the answer: whether a dotted
// verb is asked anywhere, and whether the verbs a surface that cannot carry a
// step-up names ever ask one. Each walking the tree its own way, one of them
// would stop seeing a file the other reads — an aliased import, a directory
// one of them skipped — and pass having read less.
type verbScan struct {
	// wire is every `Action…` constant rules.go declares, by its Go name.
	wire map[string]string
	// namedIn is, per Go name of an identifier this package exports, the
	// directories (slash-separated, relative to the module root) whose
	// non-test files select it.
	namedIn map[string]map[string]bool
	// conversions is, per directory, every `authz.Action(…)` conversion its
	// non-test files make, as a module-relative file:line — a verb spelled as
	// a string rather than named by its constant, which no walk over the
	// constants sees.
	conversions map[string][]string
	// files is how many non-test Go files the walk read.
	files int
}

// scanTree walks the tree once per test binary.
var scanTree = sync.OnceValues(func() (*verbScan, error) {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return nil, err
	}
	names, err := stringConstants(filepath.Join(root, "internal", "toolname"))
	if err != nil {
		return nil, err
	}
	wire, err := actionWireNames(filepath.Join(root, "internal", "authz", "rules.go"), names)
	if err != nil {
		return nil, err
	}
	scan := &verbScan{wire: wire, namedIn: map[string]map[string]bool{},
		conversions: map[string][]string{}}
	self := filepath.Join(root, "internal", "authz")
	err = sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "testdata", "static", "dashboard":
				return filepath.SkipDir
			}
			if path == self {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scan.files++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A FILE THAT DOES NOT IMPORT THE PACKAGE NAMES NONE OF IT, so only
		// the files that do are parsed.
		if !bytes.Contains(body, []byte(strconv.Quote(authzImport))) {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		return scan.read(path, filepath.ToSlash(rel), body)
	})
	if err != nil {
		return nil, err
	}
	return scan, nil
})

// read records what one file names of this package. The package is found by
// its IMPORT, under whatever name the file gives it, because a gate keyed on
// the spelling `authz.` passes every file that imports it as something else.
func (s *verbScan) read(path, dir string, body []byte) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, body, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	name := ""
	for _, spec := range file.Imports {
		if imported, _ := strconv.Unquote(spec.Path.Value); imported != authzImport {
			continue
		}
		name = "authz"
		if spec.Name != nil {
			name = spec.Name.Name
		}
	}
	switch name {
	case "":
		return nil
	case ".":
		// A DOT IMPORT names the constants bare, which no selector walk
		// can attribute — so the walks refuse it rather than read less.
		return fmt.Errorf("%s imports %s with a dot, so the verbs it names "+
			"cannot be read off its selectors", path, authzImport)
	case "_":
		return nil
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == name {
				if s.namedIn[n.Sel.Name] == nil {
					s.namedIn[n.Sel.Name] = map[string]bool{}
				}
				s.namedIn[n.Sel.Name][dir] = true
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Action" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == name {
					s.conversions[dir] = append(s.conversions[dir],
						fmt.Sprintf("%s/%s:%d", dir, filepath.Base(path),
							fset.Position(n.Pos()).Line))
				}
			}
		}
		return true
	})
	return nil
}

// treeScan is the one walk, failed on for a test that cannot use it.
func treeScan(t *testing.T) *verbScan {
	t.Helper()
	scan, err := scanTree()
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if len(scan.wire) != len(authz.Actions()) {
		t.Fatalf("read %d Action constants out of rules.go and the table holds %d "+
			"verbs — the parse no longer sees the declarations", len(scan.wire),
			len(authz.Actions()))
	}
	if scan.files < 100 {
		t.Fatalf("read %d Go files — the root is not this module's, so an "+
			"absence found here proves nothing", scan.files)
	}
	return scan
}

// asked reports whether any non-test file outside this package names the
// constant.
func (s *verbScan) asked(constant string) bool {
	return len(s.namedIn[constant]) > 0
}

// verbsNamedIn is every verb whose constant a non-test file in dir names,
// sorted, dir being a slash-separated path from the module root.
func (s *verbScan) verbsNamedIn(dir string) []authz.Action {
	var out []authz.Action
	for constant, verb := range s.wire {
		if s.namedIn[constant][dir] {
			out = append(out, authz.Action(verb))
		}
	}
	slices.Sort(out)
	return out
}

// actionWireNames reads every `Action…` constant rules.go declares with an
// `Action` type and a string value, keyed by its Go name.
//
// A verb named after a tool that a package below its owner must spell takes
// its value from internal/toolname (`Action = toolname.SendDM`), so a value
// that is a selector on that package is read from its declarations, handed in
// as names.
func actionWireNames(path string, names map[string]string) (map[string]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			typ, ok := value.Type.(*ast.Ident)
			if !ok || typ.Name != "Action" {
				continue
			}
			if sel, ok := value.Values[0].(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "toolname" {
					verb, known := names[sel.Sel.Name]
					if !known {
						return nil, fmt.Errorf("%s is toolname.%s, which internal/toolname "+
							"does not declare", value.Names[0].Name, sel.Sel.Name)
					}
					out[value.Names[0].Name] = verb
				}
				continue
			}
			lit, ok := value.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			verb, err := strconv.Unquote(lit.Value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", value.Names[0].Name, err)
			}
			out[value.Names[0].Name] = verb
		}
	}
	if len(out) == 0 {
		return nil, errors.New("rules.go declares no Action constant the parse can read")
	}
	return out, nil
}

// stringConstants reads every string constant the package in dir declares in
// its non-test source, by name.
func stringConstants(dir string) (map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					text, err := strconv.Unquote(lit.Value)
					if err != nil {
						return nil, fmt.Errorf("%s: %w", name.Name, err)
					}
					out[name.Name] = text
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("read no string constant out of %s", dir)
	}
	return out, nil
}

// toolGateDir is the package whose one gate decides every tool call: a seat's
// registry, the operator's MCP and the dashboard's act transport all build
// it ([builtin.Decide]).
const toolGateDir = "internal/agent/builtin"

// toolVerb is one verb the tool gate is asked, and how it comes to be.
type toolVerb struct {
	verb authz.Action
	how  string
}

// toolVerbs is every verb the tool gate is asked: each tool's own, which
// carries the tool's name and no dot, and every verb a tool asks from inside
// itself once it has read the row the gesture is about — which is every
// constant the gate's package names.
//
// THE SECOND HALF IS WHY THIS IS A WALK of the code rather than a filter on
// the table: a dotted verb such as archiving a project is asked of the same
// gate, by the same seat, through the same tool, so a rule over the undotted
// verbs alone holds what the gate is asked first and none of what it is asked
// next.
func toolVerbs(t *testing.T) []toolVerb {
	t.Helper()
	var out []toolVerb
	for _, a := range authz.Actions() {
		if !strings.Contains(string(a), ".") {
			out = append(out, toolVerb{a, "the tool " + string(a)})
		}
	}
	named := treeScan(t).verbsNamedIn(toolGateDir)
	for _, a := range named {
		if strings.Contains(string(a), ".") {
			out = append(out, toolVerb{a, string(a) + ", asked from inside a tool in " +
				toolGateDir + ","})
		}
	}
	if len(out) < 20 || len(named) < 15 {
		t.Fatalf("read %d verbs, %d of them named in %s; a walk that stopped "+
			"reading the table or the code certifies nothing", len(out),
			len(named), toolGateDir)
	}
	return out
}

// proofAsked is the window a verb asks a proof inside on ANY of its arms, and
// whether it asks one at all: the row's own ([authz.RecencyOf]), or the one
// its self arm states ([authz.SelfRecencyOf]).
//
// BOTH, because a walk asking whether a verb can ever demand a proof is
// asking about every caller it admits, and the self arm's window is the one a
// party acting on their own record meets — a row that asks nothing of
// everybody else and a step-up of its self arm still refuses the seat or the
// socket on its own record.
func proofAsked(a authz.Action) (iam.Recency, bool) {
	if r, _ := authz.RecencyOf(a); r.Demands() {
		return r, true
	}
	if own, _ := authz.SelfRecencyOf(a); own.Demands() {
		return own, true
	}
	return "", false
}
