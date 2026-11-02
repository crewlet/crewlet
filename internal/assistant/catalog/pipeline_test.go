package catalog_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pipeline are the producers of a result's model text, each of which ends in
// modelText (the one place a text is fenced or defused) and the bound.
var pipeline = map[string]bool{
	"fitModel": true, "fitAnswer": true, "emptyModel": true, "modelFact": true,
}

// catalogFiles is every non-test Go file of the package, parsed.
func catalogFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return fset, files
}

// calledName is the name a call calls: "f" for f(...), "pkg.F" for pkg.F(...).
func calledName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if x, ok := fun.X.(*ast.Ident); ok {
			return x.Name + "." + fun.Sel.Name
		}
	}
	return ""
}

// fromPipeline is every name fn assigns, and whether each assignment to it
// is a direct call to a pipeline producer.
func fromPipeline(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		viaPipeline := false
		if len(assign.Rhs) == 1 {
			if call, ok := assign.Rhs[0].(*ast.CallExpr); ok {
				viaPipeline = pipeline[calledName(call)]
			}
		}
		for _, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if prior, seen := out[id.Name]; seen {
				out[id.Name] = prior && viaPipeline
			} else {
				out[id.Name] = viaPipeline
			}
		}
		return true
	})
	return out
}

// EVERY MODEL TEXT GOES THROUGH THE PIPELINE. A read's result reaches the
// model as Result.Model, and every Result.Model the catalogue builds is a
// name assigned only from a pipeline producer ([pipeline]); each producer
// calls modelText and holds its text to cards.ModelBytes; and only
// modelText fences or defuses. So no producer can hand the model text that is
// unfenced where the tool's text should be, or past the bound, as the cut
// read's first message was (it defused a payload's own field names and
// carried them uncapped). Mutation: set Model from provenance.Neutralise or a
// string conversion, or call provenance.Fence outside modelText, and this
// fails.
func TestEveryModelTextGoesThroughThePipeline(t *testing.T) {
	t.Parallel()
	fset, files := catalogFiles(t)
	producers := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			assigned := fromPipeline(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					switch name := calledName(node); name {
					case "provenance.Neutralise", "provenance.Fence":
						if fn.Name.Name != "modelText" {
							t.Errorf("%s: %s calls %s; only modelText fences or defuses a model's text",
								fset.Position(node.Pos()), fn.Name.Name, name)
						}
					}
				case *ast.CompositeLit:
					if id, ok := node.Type.(*ast.Ident); !ok || id.Name != "Result" {
						return true
					}
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Model" {
							continue
						}
						value, ok := kv.Value.(*ast.Ident)
						if !ok || !assigned[value.Name] {
							t.Errorf("%s: %s sets a Result's Model from something no pipeline "+
								"producer made; build it with one of %v",
								fset.Position(kv.Pos()), fn.Name.Name, keys(pipeline))
						}
					}
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Model" {
							t.Errorf("%s: %s assigns .Model after the fact; set it once, from a "+
								"pipeline producer", fset.Position(sel.Pos()), fn.Name.Name)
						}
					}
				}
				return true
			})
			if !pipeline[fn.Name.Name] {
				continue
			}
			producers[fn.Name.Name] = true
			callsText, bounded := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					callsText = callsText || calledName(node) == "modelText"
				case *ast.SelectorExpr:
					if x, ok := node.X.(*ast.Ident); ok && x.Name == "cards" && node.Sel.Name == "ModelBytes" {
						bounded = true
					}
				}
				return true
			})
			if !callsText || !bounded {
				t.Errorf("%s is a pipeline producer that does not end in modelText (%v) and "+
					"cards.ModelBytes (%v)", fn.Name.Name, callsText, bounded)
			}
		}
	}
	for name := range pipeline {
		if !producers[name] {
			t.Errorf("the pipeline names %s, which the package does not declare", name)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
