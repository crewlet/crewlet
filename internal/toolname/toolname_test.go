package toolname_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/toolname"
)

// declaredNames is every string constant this package declares, read off its
// own non-test source — so the walk below covers a name the day it is added
// here, with nothing else to update.
func declaredNames(t *testing.T) []string {
	t.Helper()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("listing the package's files: %v", err)
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
						t.Fatalf("%s: a constant here is not a string literal, "+
							"so the walk cannot read its value: %s", name,
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
	return names
}

// NO SHARED TOOL NAME IS SPELLED TWICE. Every string literal in the module's
// non-test Go source, outside this package, is read, and one that carries a
// name declared here as a word of its own fails naming the file and line: a
// prompt, a deny list or a remedy that spells the name instead of reading the
// constant is a copy that a rename leaves behind, compiling, telling a model
// to call a tool that no longer exists or exempting one that does not.
//
// Test files are not read — a test pins the literal on purpose, so that a
// rename is a decision its suite notices — and neither are test-support
// packages (a package whose name ends in "test"), which spell what they fake.
//
// Mutation: put "activate_tool" back in skills' exempt list, or in the worker
// preamble's text, and this is red.
func TestNoSharedToolNameIsSpelledTwice(t *testing.T) {
	t.Parallel()
	names := declaredNames(t)
	if len(names) < 2 {
		t.Fatalf("read %d names off this package's source, so the walk "+
			"asserts nothing", len(names))
	}
	root := sourcetree.Root(t)
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	files, literals := 0, 0
	err = sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || path == here {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if strings.HasSuffix(parsed.Name.Name, "test") {
			return nil
		}
		files++
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Errorf("%s: unquoting %s: %v", fset.Position(lit.Pos()), lit.Value, err)
				return true
			}
			literals++
			for _, name := range names {
				if spells(text, name) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s:%d spells the tool name %q: read it from "+
						"internal/toolname instead, so a rename reaches it",
						rel, fset.Position(lit.Pos()).Line, name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// THE CONTROLS: a walk rooted in the wrong place, or one that read no
	// literal, passes every assertion above and protects nothing.
	if files < 500 || literals < 5000 {
		t.Fatalf("read %d files and %d string literals; the walk is not "+
			"reading the module", files, literals)
	}
}

// spells reports whether text carries name as a word of its own: a name
// inside a longer identifier (`activate_tools`, `xactivate_tool`) is another
// word, and a check that matched it would refuse names nobody shares.
func spells(text, name string) bool {
	for from := 0; ; {
		at := strings.Index(text[from:], name)
		if at < 0 {
			return false
		}
		at += from
		end := at + len(name)
		if (at == 0 || !wordByte(text[at-1])) && (end == len(text) || !wordByte(text[end])) {
			return true
		}
		from = at + 1
	}
}

func wordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// A NAME IS MATCHED AS A WORD, so the walk refuses a copy and not a longer
// name that happens to contain one. The control the walk's verdicts rest on.
func TestANameIsMatchedAsAWordOfItsOwn(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]bool{
		"activate_tool":                true,
		"call `activate_tool(name=…)`": true,
		"activate_tools":               false,
		"preactivate_tool":             false,
		"x activate_tool":              true,
		"":                             false,
	} {
		if got := spells(text, toolname.Activate); got != want {
			t.Errorf("spells(%q) = %v, want %v", text, got, want)
		}
	}
}

// THIS PACKAGE IMPORTS NOTHING, and that is what lets every package that must
// name a tool read it here: the prompts import only the org model and the
// provenance fence, the skill guard only the prompts, the logger and the tool
// layer's argument leaves, and a name kept anywhere that imports more is a
// name one of them cannot reach. The suite may import what it likes; the
// source the build compiles may not. Mutation: add any import to toolname.go
// and this is red.
func TestThePackageImportsNothing(t *testing.T) {
	t.Parallel()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("listing the package's files: %v", err)
	}
	if len(pkg.GoFiles) == 0 {
		t.Fatal("the package has no source the build compiles, so this asserts nothing")
	}
	if len(pkg.Imports) > 0 {
		t.Errorf("the package imports %v: a leaf every caller reaches imports nothing",
			pkg.Imports)
	}
}

// DISCOVERY IS A FRESH SLICE EACH CALL: one caller sorting or appending to
// what it was handed must not change another's list.
func TestDiscoveryHandsEachCallerItsOwnSlice(t *testing.T) {
	t.Parallel()
	first := toolname.Discovery()
	first[0] = "changed"
	if second := toolname.Discovery(); slices.Contains(second, "changed") {
		t.Errorf("a change to one caller's slice reached the next: %v", second)
	}
}
