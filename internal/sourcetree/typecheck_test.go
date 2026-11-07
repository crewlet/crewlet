package sourcetree

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// typedModule is a module of three packages, r importing q importing p, whose
// p has an in-package test, an export_test.go declaring a name only its test
// binary sees, and an external test package that uses that name and hands
// the result to q.
func typedModule(t *testing.T) string {
	t.Helper()
	return layout(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.25\n",
		"p/p.go": "package p\n\ntype T struct{}\n\nfunc New() T { return T{} }\n",
		"p/p_test.go": "package p\n\nimport \"testing\"\n\n" +
			"func inTest() T { return New() }\n\nfunc TestP(t *testing.T) { _ = inTest() }\n",
		"p/export_test.go": "package p\n\nfunc Exported() T { return New() }\n",
		"p/p_ext_test.go": "package p_test\n\nimport (\n\t\"testing\"\n\n" +
			"\t\"example.com/m/p\"\n\t\"example.com/m/q\"\n)\n\n" +
			"func TestExt(t *testing.T) { _ = q.Use(p.Exported()) }\n",
		"q/q.go": "package q\n\nimport \"example.com/m/p\"\n\nfunc Use(t p.T) p.T { return t }\n",
		"r/r.go": "package r\n\nimport \"example.com/m/q\"\n\nvar V = q.Use\n",
	})
}

// checked is one unit as a test reads it: the package, the part and the
// files' base names, sorted.
type checked struct {
	path  string
	part  Part
	files string
}

func runCheck(t *testing.T, root string, c TypeCheck, each func(*Unit)) []checked {
	t.Helper()
	if c.Fset == nil {
		c.Fset = token.NewFileSet()
	}
	var out []checked
	err := c.Run(root, func(u *Unit) error {
		var names []string
		for _, f := range u.Files {
			names = append(names, filepath.Base(c.Fset.Position(f.Pos()).Filename))
		}
		slices.Sort(names)
		out = append(out, checked{u.Package.ImportPath, u.Part, strings.Join(names, " ")})
		if each != nil {
			each(u)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// usedFrom is the package path of the object the first identifier named name
// in u's files refers to, or "" when no such use is recorded.
func usedFrom(u *Unit, name string) string {
	for id, obj := range u.Info.Uses {
		if id.Name == name && obj.Pkg() != nil {
			return obj.Pkg().Path()
		}
	}
	return ""
}

// A PACKAGE IS CHECKED AFTER EVERYTHING OF THE MODULE IT IMPORTS, from its
// non-test files alone, and a name it imports is typed as the package that
// declares it: what a gate that carries what it learnt of one package into
// the next (words' joiners) depends on.
func TestATypeCheckReadsEachPackageAfterWhatItImports(t *testing.T) {
	t.Parallel()
	root := typedModule(t)
	var qUsesP string
	got := runCheck(t, root, TypeCheck{}, func(u *Unit) {
		if u.Package.ImportPath == "example.com/m/q" {
			qUsesP = usedFrom(u, "T")
		}
	})
	want := []checked{
		{"example.com/m/p", Package, "p.go"},
		{"example.com/m/q", Package, "q.go"},
		{"example.com/m/r", Package, "r.go"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("checked %v, want %v", got, want)
	}
	if qUsesP != "example.com/m/p" {
		t.Errorf("q's p.T is typed as %q's, want example.com/m/p's", qUsesP)
	}
}

// WITH TESTS, A PACKAGE IS CHECKED AS ITS TEST BINARY COMPILES IT: with its
// in-package test files, and its external test package against the package's
// test variant, which holds what export_test.go declares — a name the plain
// package's export data does not have, so an external test that uses it
// type-checks only against the variant.
func TestATypeCheckWithTestsReadsEachPackageAsItsTestBinaryCompilesIt(t *testing.T) {
	t.Parallel()
	root := typedModule(t)
	var inTest, exported string
	got := runCheck(t, root, TypeCheck{Tests: true, Patterns: []string{"./p"}}, func(u *Unit) {
		switch u.Part {
		case WithTests:
			for id, obj := range u.Info.Defs {
				if id.Name == "inTest" && obj != nil {
					inTest = obj.Pkg().Path()
				}
			}
		case External:
			exported = usedFrom(u, "Exported")
		}
	})
	want := []checked{
		{"example.com/m/p", WithTests, "export_test.go p.go p_test.go"},
		{"example.com/m/p", External, "p_ext_test.go"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("checked %v, want %v", got, want)
	}
	if inTest != "example.com/m/p" {
		t.Errorf("the in-package test's inTest is declared in %q, want example.com/m/p", inTest)
	}
	if exported != "example.com/m/p" {
		t.Errorf("the external test's p.Exported is typed as %q's, want example.com/m/p's", exported)
	}
}

// A CHECK READS ONLY WHAT IT IS ASKED FOR: what the patterns import is export
// data and never checked, Select leaves a listed package unchecked, and a
// gate's own parse of a file is the tree the unit holds.
func TestATypeCheckReadsOnlyWhatItIsAskedFor(t *testing.T) {
	t.Parallel()
	root := typedModule(t)
	if got, want := runCheck(t, root, TypeCheck{Patterns: []string{"./r"}}, nil),
		[]checked{{"example.com/m/r", Package, "r.go"}}; !slices.Equal(got, want) {
		t.Errorf("checking ./r checked %v, want %v", got, want)
	}
	notP := func(p *Listed) bool { return p.ImportPath != "example.com/m/p" }
	if got, want := runCheck(t, root, TypeCheck{Select: notP}, nil), []checked{
		{"example.com/m/q", Package, "q.go"},
		{"example.com/m/r", Package, "r.go"},
	}; !slices.Equal(got, want) {
		t.Errorf("checking all but p checked %v, want %v", got, want)
	}

	fset := token.NewFileSet()
	own := map[*ast.File]bool{}
	parse := func(filename string) (*ast.File, error) {
		f, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
		own[f] = true
		return f, err
	}
	var foreign int
	runCheck(t, root, TypeCheck{Fset: fset, Parse: parse, Patterns: []string{"./q"},
		Info: func() *types.Info { return &types.Info{Uses: map[*ast.Ident]types.Object{}} }},
		func(u *Unit) {
			for _, f := range u.Files {
				if !own[f] {
					foreign++
				}
			}
		})
	if foreign > 0 {
		t.Errorf("%d files of the unit are not the trees the gate's own parse returned", foreign)
	}
}

// A TEST BINARY THAT DOES NOT COMPILE ITS PACKAGE AGAIN LINKS THE PLAIN ONE:
// p with no in-package test files has no `p [p.test]`, so its external tests
// and every package they import name one p, the export data's — and q, which
// imports p and hands back a p.T, is plain export data too. Checking the
// external tests against p built from source would give them two p.T types,
// and the hand-over between them would not type-check.
func TestATypeCheckWithTestsReadsAPlainPackageWhereNoTestCompilesItAgain(t *testing.T) {
	t.Parallel()
	root := layout(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.25\n",
		"p/p.go": "package p\n\ntype T struct{}\n\nfunc Take(T) {}\n",
		"p/p_ext_test.go": "package p_test\n\nimport (\n\t\"testing\"\n\n" +
			"\t\"example.com/m/p\"\n\t\"example.com/m/q\"\n)\n\n" +
			"func TestExt(t *testing.T) { p.Take(q.Make()) }\n",
		"q/q.go": "package q\n\nimport \"example.com/m/p\"\n\nfunc Make() p.T { return p.T{} }\n",
	})
	got := runCheck(t, root, TypeCheck{Tests: true, Patterns: []string{"./p"}}, nil)
	want := []checked{
		{"example.com/m/p", WithTests, "p.go"},
		{"example.com/m/p", External, "p_ext_test.go"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("checked %v, want %v", got, want)
	}
}
