package clientsource_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// moduleRoot is the repository root, from this package's directory.
const moduleRoot = "../.."

// EVERY ROW OF THE CONTRACT IS A DECLARATION THE DASHBOARD MAKES, ONCE.
//
// Read from the REAL tree with the row's own reader, so a row naming a
// declaration that was renamed, removed, duplicated or turned into another
// kind of declaration fails here, under the table's name, as well as in the
// gate that owns it. A row that reads as empty is a gate certifying nothing,
// and fails too.
func TestEveryContractRowIsDeclaredOnceInTheDashboard(t *testing.T) {
	t.Parallel()
	rows := clientsource.Contract()
	if len(rows) == 0 {
		t.Fatal("the contract names no declarations, so it certifies nothing")
	}
	for _, row := range rows {
		var n int
		var err error
		switch row.Reader {
		case clientsource.ReadLiteral:
			var body clientsource.Body
			if body, err = clientsource.Literal(clientsource.Tree, row.Name); err == nil {
				n = len(clientsource.Strings(body))
			}
		case clientsource.ReadUnion:
			var members []string
			members, err = clientsource.Union(clientsource.Tree, row.Name)
			n = len(members)
		case clientsource.ReadInterface:
			var members []clientsource.Member
			members, err = clientsource.Interface(clientsource.Tree, row.Name)
			n = len(members)
		default:
			t.Errorf("%s is read with %q, which is not a reader", row.Name, row.Reader)
			continue
		}
		switch {
		case err != nil:
			t.Errorf("%s: %v", row.Name, err)
		case n == 0:
			t.Errorf("%s reads as empty, so the gate that owns it certifies nothing", row.Name)
		}
	}
}

// AND EVERY ROW NAMES A GATE THAT EXISTS AND READS IT.
//
// The other direction from the one the readers enforce: a reader refuses a
// name that is NOT here, and this refuses a row whose gate is gone, or is in a
// file that no longer reads the dashboard at all, or never names the
// declaration it is said to own. A row whose gate was deleted is a comparison
// the table claims is made and nothing makes.
func TestEveryContractRowNamesAGateThatReadsIt(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, row := range clientsource.Contract() {
		if seen[row.Name] {
			t.Errorf("%s is in the contract twice — one declaration has one owning gate",
				row.Name)
		}
		seen[row.Name] = true

		dir, test, ok := strings.Cut(row.Gate, ".")
		if !ok || !strings.HasPrefix(dir, "internal/") || !strings.HasPrefix(test, "Test") {
			t.Errorf("%s names the gate %q, which is not <package directory>.<TestName>",
				row.Name, row.Gate)
			continue
		}
		file, err := gateFile(filepath.Join(moduleRoot, filepath.FromSlash(dir)), test)
		if err != nil {
			t.Errorf("%s: %v", row.Name, err)
			continue
		}
		if !imports(file, "github.com/crewlet/crewlet/internal/clientsource") {
			t.Errorf("%s is owned by %s, whose file does not import clientsource — "+
				"it cannot be reading the declaration", row.Name, row.Gate)
		}
		if !namesString(file, row.Name) {
			t.Errorf("%s is owned by %s, whose file never names %q — the gate the "+
				"table credits is not the one that reads it", row.Name, row.Gate, row.Name)
		}
	}
}

// gateFile parses the test file in `dir` that declares the test `name`.
func gateFile(dir, name string) (*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil,
			parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				return file, nil
			}
		}
	}
	return nil, fmt.Errorf("no test file in %s declares %s", dir, name)
}

func imports(file *ast.File, path string) bool {
	for _, spec := range file.Imports {
		if value, err := strconv.Unquote(spec.Path.Value); err == nil && value == path {
			return true
		}
	}
	return false
}

// namesString reports whether a Go string literal in the file spells `name`.
func namesString(file *ast.File, name string) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if value, err := strconv.Unquote(lit.Value); err == nil && value == name {
				found = true
			}
		}
		return !found
	})
	return found
}
