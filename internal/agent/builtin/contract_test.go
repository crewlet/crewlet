package builtin_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tools"
)

// everySchema is every builtin's advertised schema, by where it is served and
// its name: each one a seat is registered with, and each one the operator
// surface serves (an operator's assistant, the work API's dispatch), which
// is a set of its own.
func everySchema(t *testing.T) map[string]map[string]any {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(fullDeps(t))); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, e := range reg.Snapshot().Entries() {
		out["seat:"+e.Tool.Name()] = e.Tool.Parameters()
	}
	operator := builtin.OperatorTools(operatorDeps(t))
	if len(operator) == 0 {
		t.Fatal("the operator surface serves no tool")
	}
	for _, tool := range operator {
		out["operator:"+tool.Name()] = tool.Parameters()
	}
	return out
}

// EVERY BUILTIN'S SCHEMA IS ONE THE CHECK CAN KEEP (ADR-0059), the seat's and
// the operator surface's alike, since every caller calls a tool through
// tools.Invoke: a keyword or a type the check does not implement is a promise
// to the model nothing holds a call to.
//
// Mutation: add `"pattern"` to a property and this names it.
func TestEveryBuiltinSchemaIsOneTheCheckKeeps(t *testing.T) {
	t.Parallel()
	schemas := everySchema(t)
	if len(schemas) < 60 {
		t.Fatalf("%d builtins registered, which is not the set", len(schemas))
	}
	for name, schema := range schemas {
		if bad := tools.UnsupportedSchema(schema); len(bad) > 0 {
			t.Errorf("%s advertises what the check cannot keep: %v", name, bad)
		}
	}
}

// declaredTypes is every type each argument name is declared with, at any
// depth, across every builtin's schema.
func declaredTypes(schemas map[string]map[string]any) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	var walk func(s map[string]any)
	walk = func(s map[string]any) {
		props, _ := s["properties"].(map[string]any)
		for key, raw := range props {
			sub, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if out[key] == nil {
				out[key] = map[string]bool{}
			}
			switch typ := sub["type"].(type) {
			case string:
				out[key][typ] = true
			case []any:
				for _, x := range typ {
					if s, ok := x.(string); ok {
						out[key][s] = true
					}
				}
			case nil:
				out[key]["any"] = true
			}
			walk(sub)
		}
		if items, ok := s["items"].(map[string]any); ok {
			walk(items)
		}
		if extra, ok := s["additionalProperties"].(map[string]any); ok {
			walk(extra)
		}
	}
	for _, s := range schemas {
		walk(s)
	}
	return out
}

// THE ADVERTISED TYPE IS THE TYPE THE TOOL READS (ADR-0059): an argument no
// schema declares as a string is never read as free text (argString, or a
// `.(string)` assertion) except to quote it back in a refusal. A field
// advertised as a number, a boolean or a list but read as text is one the
// surface's check would refuse a value of that the tool reads, which is the
// drift this holds the tree to. Read with any helper of argval's, a value
// is read by the same rule the check holds it to.
//
// Mutation: read `limit` with argString in a tool and this names the call.
func TestNoArgumentIsReadAsTextAgainstItsType(t *testing.T) {
	t.Parallel()
	declared := declaredTypes(everySchema(t))
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		var quoting []ast.Node // the fmt calls being walked, whose arguments quote
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				if len(quoting) > 0 {
					quoting = quoting[:len(quoting)-1]
				}
				return true
			}
			quoting = append(quoting, n)
			key, how := textRead(n)
			if key == "" {
				return true
			}
			types, known := declared[key]
			if !known || types["string"] || types["any"] || inFmt(quoting) {
				return true
			}
			t.Errorf("%s: %s reads %q as text, and every schema declares it %v",
				fset.Position(n.Pos()), how, key, slices.Sorted(maps.Keys(types)))
			return true
		})
	}
}

// textRead is the argument a node reads as text, and how: argString(x, "k")
// or x["k"].(string).
func textRead(n ast.Node) (string, string) {
	switch n := n.(type) {
	case *ast.CallExpr:
		if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "argString" && len(n.Args) == 2 {
			if lit, ok := n.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				key, _ := strconv.Unquote(lit.Value)
				return key, "argString"
			}
		}
	case *ast.TypeAssertExpr:
		idx, ok := n.X.(*ast.IndexExpr)
		if typ, isIdent := n.Type.(*ast.Ident); !ok || !isIdent || typ.Name != "string" {
			return "", ""
		}
		if lit, ok := idx.Index.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			key, _ := strconv.Unquote(lit.Value)
			return key, "a .(string) assertion"
		}
	}
	return "", ""
}

// inFmt reports whether the node being walked is an argument of a fmt call:
// a value quoted back in a sentence, not read.
func inFmt(stack []ast.Node) bool {
	for _, n := range stack[:len(stack)-1] {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" {
				return true
			}
		}
	}
	return false
}

// notArguments are the type assertions in this package that read no
// argument, each with what it reads instead.
var notArguments = map[string]string{
	"fence.go": "a JSON token of an answer the engine wrote, re-read to fence it",
}

// readKinds are the kinds a raw type assertion would read an argument as by a
// rule of its own: argval reads each, the way the check at the surface does.
var readKinds = []string{"string", "[]any", "[]string", "bool", "float64", "float32",
	"json.Number", "int", "int64"}

// NO ARGUMENT IS READ BY A RULE OF ITS OWN (ADR-0059): a raw .(string),
// .([]any), .(bool) or numeric assertion reads a value differently from the
// check at the surface, which reads it through argval, so the check admitted
// `{"set": "GP-12"}` as the list of one it is while the tool read it as no
// list and cleared every relation. Every such read goes through argval; an
// object (.(map[string]any)) is read the same both ways and is not listed.
//
// Mutation: read a set with `raw.([]any)` and this names the assertion.
func TestNoArgumentIsReadByARuleOfItsOwn(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, allowed := notArguments[f]; allowed {
			continue
		}
		for _, found := range ownReads(fset, file) {
			t.Error(found)
		}
	}
}

// ownReads is every place file reads a value as one of [readKinds] by a type
// assertion of its own, in both shapes Go has: `v.(string)` and a case of a
// type switch (`switch v := raw.(type) { case string: ... }`).
func ownReads(fset *token.FileSet, file *ast.File) []string {
	var found []string
	name := func(at token.Pos, kind, shape string) {
		found = append(found, fmt.Sprintf("%s: %s %s reads a value by a rule of its own; "+
			"read an argument through argval", fset.Position(at), shape, kind))
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.TypeAssertExpr:
			if n.Type == nil {
				return true // a type switch's guard: its cases are read below
			}
			if kind := types.ExprString(n.Type); slices.Contains(readKinds, kind) {
				name(n.Pos(), kind, ".("+kind+")")
			}
		case *ast.TypeSwitchStmt:
			for _, stmt := range n.Body.List {
				clause, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, typ := range clause.List {
					if kind := types.ExprString(typ); slices.Contains(readKinds, kind) {
						name(typ.Pos(), kind, "case")
					}
				}
			}
		}
		return true
	})
	return found
}

// THE ARGUMENT GATE SEES EVERY SHAPE (#423 review): a type switch reads a value
// by its cases as surely as an assertion does, and the first version of the
// gate skipped it.
//
// Mutation: drop the type switch from ownReads and its fixture is not named.
func TestTheArgumentGateSeesEveryShape(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"an assertion": `s, _ := args["set"].([]any); _ = s`,
		"a type switch": `switch v := args["set"].(type) {
case []any:
	_ = v
}`,
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go",
			"package fixture\nfunc run(args map[string]any) {\n"+src+"\n}\n", 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if found := ownReads(fset, file); len(found) != 1 {
			t.Errorf("%s was named %d times: %v", name, len(found), found)
		}
	}
}

// numberReads are the parses a value can be read as a number by. In this
// package each is the integer or number reader's alone (argval.Whole and
// argval.Number, behind argInt, argIntValue, argFloat and argFloatValue).
var numberReads = []string{"strconv.Atoi", "strconv.ParseInt", "strconv.ParseUint",
	"strconv.ParseFloat", "fmt.Sscan", "fmt.Sscanf", "fmt.Sscanln", "big.ParseFloat"}

// A DECLARED NUMBER IS READ BY ITS TYPE'S READER ALONE (ADR-0059, #423
// re-verify): the check admitted "1e3" as an integer by argval.Whole while
// argInt read it by fmt.Sscanf's prefix as 1. So no file here parses a
// number by any other function, and every key read with argInt is declared an
// integer wherever a builtin's schema declares it, and every key read with
// argFloat a number, so the check holds the value to the rule it is read by.
//
// Mutation: read `limit` with strconv.Atoi, or declare get_inbox's `limit` a
// number again while argInt reads it, and this names it.
func TestADeclaredNumberIsReadByItsTypesReader(t *testing.T) {
	t.Parallel()
	declared := map[string]map[string]bool{}
	var walk func(schema map[string]any)
	walk = func(schema map[string]any) {
		props, _ := schema["properties"].(map[string]any)
		for key, raw := range props {
			sub, _ := raw.(map[string]any)
			if declared[key] == nil {
				declared[key] = map[string]bool{}
			}
			switch typ := sub["type"].(type) {
			case string:
				declared[key][typ] = true
			case []any:
				for _, each := range typ {
					s, _ := each.(string)
					declared[key][s] = true
				}
			}
			walk(sub)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			walk(items)
		}
	}
	for _, schema := range everySchema(t) {
		walk(schema)
	}
	readAs := map[string]string{"argInt": "integer", "argFloat": "number"}

	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		// A KEY IS HELD TO ITS OWN FILE'S DECLARATION where the file has one,
		// since one key ("since") is an integer to one tool and a timestamp
		// to another, and to every builtin's otherwise.
		local := declaredIn(file)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn := types.ExprString(call.Fun)
			if slices.Contains(numberReads, fn) {
				t.Errorf("%s: %s parses a number by a rule of its own; read an argument through argval",
					fset.Position(call.Pos()), fn)
			}
			want, reader := readAs[fn]
			if !reader || len(call.Args) < 2 {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok {
				return true
			}
			key, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			types := declared[key]
			if here, ok := local[key]; ok {
				types = here
			}
			for typ := range types {
				if typ != want {
					t.Errorf("%s: %s reads %q as an %s, and a builtin declares it a %s",
						fset.Position(call.Pos()), fn, key, want, typ)
				}
			}
			return true
		})
	}
}

// declaredIn is the type each property a file's schema literals declare, by
// key: every `"key": map[string]any{"type": "..."}` in the file.
func declaredIn(file *ast.File) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := stringLit(kv.Key)
		if !ok {
			return true
		}
		prop, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range prop.Elts {
			field, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if name, _ := stringLit(field.Key); name != "type" {
				continue
			}
			typ, ok := stringLit(field.Value)
			if !ok {
				continue
			}
			if out[key] == nil {
				out[key] = map[string]bool{}
			}
			out[key][typ] = true
		}
		return true
	})
	return out
}

// stringLit is a string literal's value, or none.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
