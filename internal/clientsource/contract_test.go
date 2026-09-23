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
		n, err := readRow(clientsource.Tree, row)
		switch {
		case err != nil:
			t.Errorf("%s: %v", row.Name, err)
		case n == 0:
			t.Errorf("%s reads as empty, so the gate that owns it certifies nothing", row.Name)
		}
	}
}

// THE CONTRACT DIRECTORY HOLDS THE CONTRACT, AND NOTHING ELSE.
//
// Both ways. Every row is declared in `dashboard/src/contract/` — read there
// with its own reader, so a declaration that moved back beside a screen fails
// here by name rather than going on passing its gate from a file nobody
// thinks of as the engine's. And every name the directory EXPORTS is a row:
// a declaration put there without a gate is a module claiming the engine
// holds it, which nothing does, and a reader who trusts the directory's name
// trusts a copy that can drift. (What a module declares without exporting —
// a row type composed into the one interface a screen names — is held by the
// first half: it is a row because a gate reads it.)
//
// The walk refuses what it cannot see through — a re-export, a default
// export, two names in one statement — for the reason every reader here
// does: a walk that returned the names it could see would report the
// directory as exporting less than it does.
func TestTheContractDirectoryHoldsExactlyTheContract(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(clientsource.Tree, clientsource.ContractDir)
	rows := map[string]bool{}
	for _, row := range clientsource.Contract() {
		rows[row.Name] = true
		if _, err := readRow(dir, row); err != nil {
			t.Errorf("%s is in the contract and not declared once in "+
				"dashboard/src/%s/: %v", row.Name, clientsource.ContractDir, err)
		}
	}
	exported, err := clientsource.Exports(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(exported) == 0 {
		t.Fatalf("dashboard/src/%s/ exports nothing, so this certifies nothing",
			clientsource.ContractDir)
	}
	for name, files := range exported {
		if !rows[name] {
			t.Errorf("%v exports %s, which is not in the contract "+
				"(internal/clientsource/contract.go) — a declaration in the "+
				"contract directory is one a gate holds against the engine; "+
				"register it with its reader and gate, or move it out", files, name)
		}
	}
}

// readRow reads one row out of `tree` with its own reader, and reports how
// many members, strings or values it holds.
func readRow(tree string, row clientsource.Entry) (int, error) {
	switch row.Reader {
	case clientsource.ReadLiteral:
		body, err := clientsource.Literal(tree, row.Name)
		return len(clientsource.Strings(body)), err
	case clientsource.ReadUnion:
		members, err := clientsource.Union(tree, row.Name)
		return len(members), err
	case clientsource.ReadInterface:
		members, err := clientsource.Interface(tree, row.Name)
		return len(members), err
	case clientsource.ReadScalar:
		value, err := clientsource.Scalar(tree, row.Name)
		if value == "" {
			return 0, err
		}
		return 1, err
	}
	return 0, fmt.Errorf("%s is read with %q, which is not a reader", row.Name, row.Reader)
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
