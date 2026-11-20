package cdp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// EVERY DECLARED METHOD AND EVENT IS IN THE INVENTORY, and every entry is
// declared: a walk of this package's own files (its tests aside) finds every
// Method[…]{…} and Event[…]{…} literal, each name: "…" alone, and the inventory
// holds exactly those, none twice. testOnly, which lets the test files' own
// commands through Call's, Send's, Listen's and Of's refusal of whatever the
// inventory lacks, is named in those files twice alone: its declaration, and
// the one read that builds what they take.
//
// Mutations, each red: drop an entry from the inventory; declare one unkeyed,
// Method[P, R]{[0]*P{}, [0]*R{}, "Log.enable"}, its entry dropped; register a
// command from a file of the package's own, func init() { testOnly =
// append(testOnly, PageCrash) }.
func TestEveryDeclaredMethodAndEventIsInTheInventory(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	declared := map[string]int{}
	var registered []string // where a file the engine is built from names testOnly
	files := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "testOnly" {
				registered = append(registered, fset.Position(id.Pos()).String())
			}
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !descriptor(lit.Type) {
				return true
			}
			// One element, name: "Domain.member": an unkeyed literal, or a
			// name that is no string literal, would be a declaration the walk
			// cannot read, and so one the inventory could miss.
			name, ok := descriptorName(lit)
			if !ok {
				t.Errorf("%s: a descriptor that is not name: \"Domain.member\" alone", fset.Position(lit.Pos()))
				return true
			}
			declared[name]++
			return true
		})
	}
	// A walk that read part of the package fails the comparison below; one
	// that read nothing compares an empty walk with whatever the inventory is.
	if files < 10 || len(declared) == 0 {
		t.Fatalf("the walk read %d files and found %d declarations: it is reading nothing", files, len(declared))
	}
	// Its declaration and declarations' one read of it, in inventory.go: any
	// other is a file the engine is built from reaching the test files' own
	// registrations, which only export_test.go writes.
	if len(registered) != 2 || !strings.HasPrefix(registered[0], "inventory.go:") ||
		!strings.HasPrefix(registered[1], "inventory.go:") {
		t.Errorf("testOnly is named at %q: want its declaration and declarations' one read of it, both in "+
			"inventory.go, and nothing else, since only a test file registers a command the inventory lacks", registered)
	}
	held := map[string]int{}
	for _, d := range inventory {
		held[d.Name()]++
	}
	for name, n := range declared {
		switch {
		case n > 1:
			t.Errorf("%s is declared %d times", name, n)
		case held[name] == 0:
			t.Errorf("%s is declared and missing from the inventory, so the floor was never measured against it", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(held)) {
		switch {
		case held[name] > 1:
			t.Errorf("%s is in the inventory %d times", name, held[name])
		case declared[name] == 0:
			t.Errorf("%s is in the inventory and declared nowhere", name)
		}
	}
}

// descriptorName is the name a descriptor literal declares, when the literal
// is name: "…" alone.
func descriptorName(lit *ast.CompositeLit) (string, bool) {
	if len(lit.Elts) != 1 {
		return "", false
	}
	kv, ok := lit.Elts[0].(*ast.KeyValueExpr)
	if !ok {
		return "", false
	}
	key, ok := kv.Key.(*ast.Ident)
	value, isString := kv.Value.(*ast.BasicLit)
	if !ok || key.Name != "name" || !isString || value.Kind != token.STRING {
		return "", false
	}
	name, err := strconv.Unquote(value.Value)
	return name, err == nil
}

// descriptor reports whether a composite literal's type is Method[…] or
// Event[…].
func descriptor(typ ast.Expr) bool {
	var x ast.Expr
	switch t := typ.(type) {
	case *ast.IndexExpr:
		x = t.X
	case *ast.IndexListExpr:
		x = t.X
	default:
		return false
	}
	id, ok := x.(*ast.Ident)
	return ok && (id.Name == "Method" || id.Name == "Event")
}
