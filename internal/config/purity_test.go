package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// VALIDATION IS A PURE FUNCTION OF THE DOCUMENT, and this is what says so.
//
// # Why it needs a gate at all
//
// `crewlet validate` checks one file, on a laptop, with no broker, no
// coordination store and no database — that is the whole of what makes it
// usable before a deployment exists. Nothing in the type system says so: a
// validation rule that wanted to check a handle against the chart's rows would
// compile perfectly, pass every test written against a running engine, and
// turn the one command an operator runs BEFORE they have an engine into one
// that needs one.
//
// The chart's split makes that temptation concrete rather than theoretical.
// Every reference this document carries — a unit's lead, a seat's `manages`, a
// root seat's `unit:` — now resolves against rows that live somewhere else, so
// "is this handle real" is a question with a database answer. The rule stays
// what it was: this layer checks the document's own shape and its internal
// consistency, and a reference that resolves to nothing is REPORTED by
// [Company.DanglingRefs] rather than refused.
//
// # What it walks
//
// The call graph reachable from the validation entry points, inside this
// package, looking for a parameter or a call that could only come from a
// running system. It is a source walk because the fault is a shape rather
// than a behaviour: a validation that took a context still validates, it just
// cannot be run where it has to be.
func TestValidationTakesNoContextNoStoreAndNoSQL(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(f fs.FileInfo) bool {
		return !strings.HasSuffix(f.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the package: %v", err)
	}
	files := map[string]*ast.File{}
	for _, p := range pkg {
		for name, file := range p.Files {
			files[filepath.Base(name)] = file
		}
	}
	if len(files) == 0 {
		t.Fatal("parsed no source files — this guard was certifying nothing")
	}

	// THE ENTRY POINTS, named rather than discovered: what this protects is
	// the path `crewlet validate` takes, and a walk that started from every
	// exported function would report the config WRITE path — which
	// legitimately holds a store.
	roots := []string{
		"Validate", "ValidateRunnable", "ValidateAdmission",
		"validateAdmission", "Warnings",
	}
	decls := declarations(files)
	for _, root := range roots {
		if _, found := decls[root]; !found {
			t.Fatalf("%s is not a function in this package, so this guard "+
				"walks from nothing", root)
		}
	}

	// THE FORBIDDEN SHAPES, and each is a different way the same mistake
	// arrives.
	forbidden := map[string]string{
		"context.Context": "a validation that takes a context is one that " +
			"expects to do I/O, and `crewlet validate` runs where there is " +
			"none to do",
		"sql.Tx": "a validation that reads a transaction cannot run before " +
			"a database exists",
		"sql.DB": "a validation that reads a database cannot run before one " +
			"exists",
		"store.DB": "a validation that reads the estate turns the one " +
			"command an operator runs before deploying into one that needs " +
			"a deployment",
	}

	seen := map[string]bool{}
	var walk func(name string, path []string)
	walk = func(name string, path []string) {
		if seen[name] {
			return
		}
		seen[name] = true
		fn, found := decls[name]
		if !found {
			return
		}
		for shape, why := range forbidden {
			if takes(fn, shape) {
				t.Errorf("%s takes a %s, reached from %s.\n\t%s",
					name, shape, strings.Join(path, " -> "), why)
			}
		}
		for _, callee := range callees(fn) {
			// THE PATH ALREADY ENDS AT THIS FUNCTION, so the callee is what
			// gets appended: a path that re-appended the caller would report
			// every root twice and read as a cycle the walk does not have.
			walk(callee, append(slices.Clone(path), callee))
		}
	}
	for _, root := range roots {
		walk(root, []string{root})
	}

	// THE CONTROL: the walk has to actually reach things. A walk that
	// visited five functions would pass for a package whose validation had
	// grown a store call three levels down.
	if len(seen) < 20 {
		t.Errorf("the walk reached %d functions, which is too few for this "+
			"package's validation to have been covered — check that callees "+
			"still resolves a method call's name", len(seen))
	}
}

// declarations indexes every function and method in the package by its own
// name.
//
// BY NAME RATHER THAN BY RECEIVER, deliberately: a method and a function of
// one name would collide, and this package has none — which a lookup miss
// would show up as a walk that stopped early, caught by the control above.
func declarations(files map[string]*ast.File) map[string]*ast.FuncDecl {
	out := map[string]*ast.FuncDecl{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				continue
			}
			out[fn.Name.Name] = fn
		}
	}
	return out
}

// takes reports whether fn has a parameter of this qualified type.
func takes(fn *ast.FuncDecl, qualified string) bool {
	pkg, name, _ := strings.Cut(qualified, ".")
	found := false
	ast.Inspect(fn.Type, func(n ast.Node) bool {
		sel, isSel := n.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != name {
			return true
		}
		ident, isIdent := sel.X.(*ast.Ident)
		if isIdent && ident.Name == pkg {
			found = true
		}
		return true
	})
	return found
}

// callees is every function this one calls, by name.
//
// A METHOD CALL RESOLVES TO ITS SELECTOR, which is what lets the walk follow
// `c.validateSetupStepNames()` into this package's own declaration — and also
// what makes it follow a call into another package whose name happens to
// match. That over-reach is harmless here: a function this package does not
// declare is simply not found, and the walk stops.
func callees(fn *ast.FuncDecl) []string {
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch target := call.Fun.(type) {
		case *ast.Ident:
			out = append(out, target.Name)
		case *ast.SelectorExpr:
			out = append(out, target.Sel.Name)
		}
		return true
	})
	return out
}
