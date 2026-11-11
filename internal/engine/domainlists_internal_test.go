package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"reflect"
	"testing"
)

// THE STORE'S SCHEMA SUITE CERTIFIES EVERY REGISTERED DOMAIN.
//
// internal/store's TestTheShippedSchemaAcceptsTheFrameworksOwnStatements holds
// each domain's shipped DDL to the statements the framework generates, over a
// list of domains it cannot read off the register: the store's tests do not
// import the engine. A domain registered here and missing there is a log whose
// deferral and ledger tables nothing checks against the migration, and nothing
// says so, which is how a per-domain list fails silently when a domain joins.
// So the list is read here, from that test's source, and held to the register.
//
// Mutation: drop atlasdomain.Domain{} from the store test's list and this names
// the atlas.
func TestEveryRegisteredDomainIsInTheStoresSchemaSuite(t *testing.T) {
	t.Parallel()
	listed := storeSchemaSuiteDomains(t)
	if len(listed) == 0 {
		t.Fatal("read no domain from the store's schema suite, so this compares nothing")
	}
	for _, d := range registeredDomains() {
		pkg := path.Base(reflect.TypeOf(d).PkgPath())
		if !listed[pkg] {
			t.Errorf("the %s domain (package %s) is registered and internal/store's "+
				"TestTheShippedSchemaAcceptsTheFrameworksOwnStatements does not list it: add %s.Domain{}",
				d.Name(), pkg, pkg)
		}
	}
}

// storeSchemaSuiteDomains is every package whose Domain{} the store's schema
// suite names, read from its source.
func storeSchemaSuiteDomains(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join("..", "store", "schema_test.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse the store's schema suite: %v", err)
	}
	out := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "TestTheShippedSchemaAcceptsTheFrameworksOwnStatements" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Domain" {
				if pkg, ok := sel.X.(*ast.Ident); ok {
					out[pkg.Name] = true
				}
			}
			return true
		})
	}
	return out
}
