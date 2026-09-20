package httpx_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoResponseCapIsWrittenAsALiteral fails the build when any package in
// this module bounds a reader with a number rather than with a named cap.
//
// # Why this is a guard and not a convention
//
// There are exactly three questions a client asks about a response body —
// how much may be DECODED, how much of a REFUSAL is worth quoting, and how
// much of an ignored body is DRAINED so the connection goes back to the pool
// — and [httpx.MaxResponseBody], [httpx.RefusalBytes] and [httpx.DrainBytes]
// are the three answers. Each had already been written by hand in several
// packages, and each doc comment asserted the copies matched:
//
//   - RefusalBytes' own doc said it "replaced six different answers to one
//     question". Four of the six were still in the tree when this was
//     written — errorBodyBytes in github, detailLimit in confluence and in
//     sandbox, maxDetail in datadog — all spelling 2048, plus a bare 4096 in
//     mattermost and three bare 2048s in the E2B envd client.
//   - MaxResponseBody's doc said "every third-party app client caps the body
//     it DRAINS (1 MiB)", and the mebibyte was a literal written out nine
//     times with nothing comparing them.
//
// That is the failure this module's own conventions name repeatedly: a rule
// more than one package needs, written down in prose, checked by nobody. The
// prose was already there. What was missing is this file.
//
// # What it flags
//
// A call to io.LimitReader, anywhere under internal/ or cmd/, whose LIMIT is
// built entirely out of number literals. `httpx.RefusalBytes+1` passes,
// `maxEnvdFile+1` passes — a package-local constant with its own doc is a
// considered answer, and several legitimately differ. `2048` and `1<<20` do
// not, because a number at a call site carries no reason and nothing compares
// it to the other eight.
//
// The subject is the LANGUAGE construct — a selector resolved against the
// file's own import of io — so a doc comment naming io.LimitReader (this one
// included) is not a hit, and a file that renames the import is still
// covered.
//
// If this rule ever legitimately goes away, DELETE this guard rather than
// adding an exception to it. An allowance list is how the six spellings above
// came back the first time.
func TestNoResponseCapIsWrittenAsALiteral(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	seen, bad := walkForLiteralCaps(t, root)

	// A guard asserting an ABSENCE passes identically when the thing is
	// absent and when the matcher has gone inert. The tree holds a dozen
	// legitimate io.LimitReader calls, so finding none means the walk
	// stopped working rather than the tree being clean.
	if seen == 0 {
		t.Fatal("matched no io.LimitReader call at all — this guard was " +
			"certifying nothing. Check the module root and the internal/ and cmd/ globs")
	}
	for _, where := range bad {
		t.Errorf("%s: io.LimitReader takes a number literal. Name it — "+
			"httpx.MaxResponseBody to decode, httpx.RefusalBytes to quote a "+
			"refusal, httpx.DrainBytes to drain, or a constant of this "+
			"package's own carrying the reason it differs", where)
	}
}

// TestTheLiteralCapMatcherStillMatches is the positive control for the guard
// above: it hands the matcher the shape the guard exists to catch and insists
// it is caught, so a matcher that stopped resolving selectors fails here
// rather than passing silently up there.
func TestTheLiteralCapMatcherStillMatches(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		src  string
		want bool
	}{
		{"a bare number", `io.LimitReader(r, 2048)`, true},
		{"a shifted number", `io.LimitReader(r, 1<<20)`, true},
		{"a sum of numbers", `io.LimitReader(r, 2048+1)`, true},
		{"a shared cap", `io.LimitReader(r, httpx.RefusalBytes)`, false},
		{"a shared cap plus one", `io.LimitReader(r, httpx.MaxResponseBody+1)`, false},
		{"a local constant", `io.LimitReader(r, maxEnvdFile+1)`, false},
		{"a parameter", `io.LimitReader(r, max+1)`, false},
		{"another package's LimitReader", `bufio.LimitReader(r, 2048)`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			src := "package p\nimport \"io\"\nvar _ = " + c.src + "\n"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, hits := literalCapsIn(fset, file, "x.go")
			if got := len(hits) > 0; got != c.want {
				t.Errorf("matched = %v, want %v for %s", got, c.want, c.src)
			}
		})
	}
}

// walkForLiteralCaps parses every non-test Go file under internal/ and cmd/,
// returning how many io.LimitReader calls it resolved and where the literal
// ones are.
//
// TEST FILES ARE EXCLUDED, deliberately. A test that feeds a reader four
// bytes to prove a cap bites is naming a fixture, not a policy, and forcing
// it through a constant would make the case unreadable.
func walkForLiteralCaps(t *testing.T, root string) (int, []string) {
	t.Helper()

	var seen int
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				t.Fatalf("parse %s: %v", path, parseErr)
			}
			where, _ := filepath.Rel(root, path)
			n, hits := literalCapsIn(fset, file, where)
			seen += n
			bad = append(bad, hits...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return seen, bad
}

// literalCapsIn returns how many io.LimitReader calls the file makes and
// which of them pass a purely numeric limit.
func literalCapsIn(fset *token.FileSet, file *ast.File, where string) (int, []string) {
	name, ok := ioImportName(file)
	if !ok {
		return 0, nil
	}
	var seen int
	var bad []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall || len(call.Args) != 2 {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "LimitReader" {
			return true
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name != name {
			return true
		}
		seen++
		if onlyNumbers(call.Args[1]) {
			at := fset.Position(call.Args[1].Pos())
			bad = append(bad, fmt.Sprintf("%s:%d", where, at.Line))
		}
		return true
	})
	return seen, bad
}

// onlyNumbers reports whether an expression is built from number literals and
// nothing else — no identifier, so no name and therefore no reason.
func onlyNumbers(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.INT
	case *ast.BinaryExpr:
		return onlyNumbers(v.X) && onlyNumbers(v.Y)
	case *ast.ParenExpr:
		return onlyNumbers(v.X)
	case *ast.UnaryExpr:
		return onlyNumbers(v.X)
	}
	return false
}

// ioImportName is the name this file calls the io package, so a renamed
// import is still covered and a package with no io import is skipped.
func ioImportName(file *ast.File) (string, bool) {
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != `"io"` {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return "", false
			}
			return imp.Name.Name, true
		}
		return "io", true
	}
	return "", false
}
