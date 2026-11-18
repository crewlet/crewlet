package gitlet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// fieldFormat is a refusal format whose field leads it: "%w: <field>: ", the field a name or "%s".
var fieldFormat = regexp.MustCompile(`^%w: (%s|[a-z_][A-Za-z0-9_.\[\]-]*): `)

// EVERY REFUSAL BY ONE FIELD IS SPELLED SO ITS FIELD IS A VALUE: a refusal formed with
// fmt.Errorf("%w: <field>: ...", ErrInvalid or ErrInvalidProposal, ...) names its field only in
// its sentence, which no surface answering a person may show (ADR-0060), so this package forms
// every one with refusedf, which carries the field as a FieldRefused.
//
// Mutation: spell one refusal with fmt.Errorf again and this names it.
func TestEveryRefusalByAFieldNamesItAsAValue(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var found []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Errorf" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
				return true
			}
			sentinel, ok := call.Args[1].(*ast.Ident)
			if !ok || (sentinel.Name != "ErrInvalid" && sentinel.Name != "ErrInvalidProposal") {
				return true
			}
			if format, ok := literal(call.Args[0]); ok && fieldFormat.MatchString(format) &&
				!strings.Contains(format[4:], "%w") {
				found = append(found, fset.Position(call.Pos()).String())
			}
			return true
		})
	}
	for _, at := range found {
		t.Errorf("%s refuses by a field with fmt.Errorf: form it with refusedf, so the field is a value", at)
	}
}

// literal is a string literal, or a "+" concatenation of them, as its value.
func literal(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		left, ok := literal(v.X)
		if !ok {
			return "", false
		}
		right, ok := literal(v.Y)
		return left + right, ok
	}
	return "", false
}
