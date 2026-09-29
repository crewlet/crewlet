package store_test

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/store"
)

// NO STATEMENT SPANS TWO FILES.
//
// A node is its own database file and one file per partition it holds, and
// the two rules that makes true are that no transaction spans two of them and
// no read joins across them. Both fail the same way if they are broken: not
// with an error a caller can handle, but with a driver refusing a table it
// cannot see, from a query nobody thought was crossing a boundary — because in
// one file it was not.
//
// # What it walks
//
// Every non-test .go file under internal/ and cmd/, parsed with go/parser,
// looking at *ast.BasicLit of kind STRING. A literal is a literal wherever it
// sits, so a doc comment discussing `tracker_tasks` and `crewlet_events` in
// one sentence — which several package docs legitimately do — is not one.
//
// # What it looks for
//
// Two things. A literal that looks like SQL and names a table the node's own
// schema declares AND one the partition schema declares — the table names are
// DERIVED from each schema, so a table added by a migration is covered by that
// migration and nothing else. And any ATTACH, which is the one way a statement
// on one connection reaches a SECOND file, and so the one way it could join two
// partitions: every partition carries the same table names, so no table name
// can tell a statement that crossed from one partition into another from one
// that did not, and the only statement that can cross is one that attaches.
//
// # Why the control strings are the load-bearing half
//
// A guard asserting an absence passes identically when the thing is absent and
// when the guard has stopped working. They run the matchers on strings whose
// verdict is known, so this is a test that can fail today rather than one that
// starts working later and is trusted meanwhile.
func TestNoStatementSpansTwoFiles(t *testing.T) {
	t.Parallel()

	node := tablesIn(t, store.EstateNode)
	if len(node) == 0 {
		t.Fatal("no tables were derived from the node estate's schema; the " +
			"derivation no longer recognises the DDL it is meant to cover")
	}
	partition := tablesIn(t, store.EstatePartition)

	// The matchers, exercised on strings whose verdict is known. Both
	// estates are named explicitly here rather than taken from the schema,
	// so these cases keep their meaning whatever either schema does next.
	fakeNode := map[string]bool{"crewlet_events": true}
	fakePartition := map[string]bool{"tracker_tasks": true}
	for _, positive := range []string{
		`SELECT t.id FROM tracker_tasks t JOIN crewlet_events e ON e.id = t.turn_id`,
		`INSERT INTO crewlet_events (id) SELECT id FROM tracker_tasks`,
		`UPDATE tracker_tasks SET n = (SELECT count(*) FROM crewlet_events)`,
	} {
		if !bothEstates(positive, fakeNode, fakePartition) {
			t.Errorf("control: %q crosses the estate boundary and the matcher "+
				"did not flag it", positive)
		}
	}
	for _, negative := range []string{
		`SELECT id FROM tracker_tasks WHERE project = ?`,
		`SELECT id FROM crewlet_events WHERE id = ?`,
		// The two names in one sentence, which is what a package doc
		// and a commit message do all the time.
		`tracker_tasks is replicated; crewlet_events is this node's own`,
		// A column that merely contains another table's name.
		`SELECT tracker_tasks_seen FROM crewlet_events`,
	} {
		if bothEstates(negative, fakeNode, fakePartition) {
			t.Errorf("control: %q does not cross the boundary but the matcher "+
				"flagged it", negative)
		}
	}
	for _, positive := range []string{
		`ATTACH DATABASE 'l1-tracker.008.db' AS other`,
		`attach '/data/crewlet-replicated.db' as estate`,
		`ATTACH ? AS peer`,
	} {
		if !attaches(positive) {
			t.Errorf("control: %q attaches a second file and the matcher did "+
				"not flag it", positive)
		}
	}
	for _, negative := range []string{
		`the artefact is attached to the manifest`,
		`SELECT attachment FROM tracker_files`,
		`ATTACHED files are refused`,
	} {
		if attaches(negative) {
			t.Errorf("control: %q attaches nothing but the matcher flagged it", negative)
		}
	}

	root := sourcetree.Root(t)
	var crossings []string
	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), func(fset *token.FileSet, file *ast.File) {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if bothEstates(text, node, partition) || attaches(text) {
					crossings = append(crossings, shortPos(root, fset.Position(lit.Pos()).String())+
						": "+strings.Join(strings.Fields(text), " "))
				}
				return true
			})
		})
	}
	for _, c := range crossings {
		t.Errorf("a statement reaches two database files — a node's own and a "+
			"partition, or a second file it attaches: %s", c)
	}
	t.Logf("estate boundary: %d node table(s), %d partition table(s)",
		len(node), len(partition))
}

// attachStatement is SQLite's ATTACH: the keyword, an optional DATABASE, and
// the file — a string literal or a parameter.
var attachStatement = regexp.MustCompile(`(?is)\bATTACH\s+(?:DATABASE\s+)?(?:'|\?|:)`)

// attaches reports whether a statement attaches a second database file.
func attaches(text string) bool { return attachStatement.MatchString(text) }

// sqlVerb introduces a table name in the statements this engine writes.
var sqlVerb = regexp.MustCompile(`(?is)\b(?:FROM|JOIN|INTO|UPDATE|TABLE)\s+([a-z_][a-z0-9_]*)`)

// bothEstates reports whether one statement names a table of the node's own
// schema and one of the partition schema.
//
// TABLES IN VERB POSITION ONLY. A bare containment test reads a column named
// `tracker_tasks_seen` as the table `tracker_tasks`, and reads a package doc
// that mentions two tables in one sentence as a join.
func bothEstates(text string, node, replicated map[string]bool) bool {
	var sawNode, sawReplicated bool
	for _, m := range sqlVerb.FindAllStringSubmatch(text, -1) {
		name := strings.ToLower(m[1])
		if node[name] {
			sawNode = true
		}
		if replicated[name] {
			sawReplicated = true
		}
	}
	return sawNode && sawReplicated
}

// tablesIn is every table one estate's embedded schema LEAVES BEHIND, read
// from a database that schema has just been applied to.
//
// FROM THE DATABASE, NOT FROM THE DDL'S TEXT. The derivation was a regular
// expression over the migrations, and it read a COMMENT as a table:
// replicated/0014 explains itself with "a CREATE TABLE plus an applier", and
// every gate built on this saw a replicated table called `plus`. A phantom
// table is a boundary a statement can cross by naming a word, and a missing
// one — a table built by a statement the pattern did not anticipate — is a
// write no gate watches. sqlite_master is what the migrations actually built,
// after every drop and every rebuild, which is also why no ordering rule over
// creates and drops has to be restated here.
func tablesIn(t *testing.T, estate store.Estate) map[string]bool {
	t.Helper()
	tables, err := estateTables(estate)
	if err != nil {
		t.Fatalf("derive the %s estate's tables: %v", estate, err)
	}
	return maps.Clone(tables)
}

// estateTables applies each estate's schema ONCE per test binary: every gate
// in this package asks, several of them in parallel, and the answer is a
// property of the binary.
var estateTables = func() func(store.Estate) (map[string]bool, error) {
	var mu sync.Mutex
	done := map[store.Estate]map[string]bool{}
	return func(estate store.Estate) (map[string]bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if tables, ok := done[estate]; ok {
			return tables, nil
		}
		dir, err := os.MkdirTemp("", "estate-tables-*")
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		db, err := store.OpenEstate(context.Background(), estate,
			filepath.Join(dir, string(estate)+".db"), store.Options{})
		if err != nil {
			return nil, err
		}
		defer func() { _ = db.Close() }()
		tables := map[string]bool{}
		err = db.Read(context.Background(), func(tx *sql.Tx) error {
			rows, err := tx.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					return err
				}
				tables[strings.ToLower(name)] = true
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
		// schema_migrations is created by the migrator rather than by a
		// file, and exists in BOTH estates — so it is neither estate's and
		// naming it is never a crossing. The database engine's own
		// bookkeeping is nobody's either: SQLite's `sqlite_` tables, and
		// the sequence table Turso keeps behind an AUTOINCREMENT column,
		// which no statement of ours names.
		for name := range tables {
			if name == "schema_migrations" || strings.HasPrefix(name, "sqlite_") ||
				strings.HasPrefix(name, "__turso_internal_") {
				delete(tables, name)
			}
		}
		done[estate] = tables
		return tables, nil
	}
}()

// walkGoFiles parses every non-test .go file under dir.
func walkGoFiles(t *testing.T, dir string, fn func(*token.FileSet, *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	files := 0
	err := sourcetree.Walk(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		files++
		parsed, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		fn(fset, parsed)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	// Every caller asserts an absence over what this parsed, and a walk
	// that parsed nothing asserts it just as confidently.
	if files == 0 {
		t.Fatalf("parsed no Go files under %s — this guard was certifying nothing", dir)
	}
}

// parsedFile is one non-test source file the gates in this package read, with
// the names they judge it by.
type parsedFile struct {
	fset  *token.FileSet
	file  *ast.File
	rel   string
	names fileNames
}

// parseTree parses every non-test .go file under each of dirs, relative to
// root, once — for a gate that needs a first pass over the whole tree before
// it can judge any one file.
func parseTree(t *testing.T, root string, dirs ...string) []parsedFile {
	t.Helper()
	var out []parsedFile
	for _, dir := range dirs {
		walkGoFiles(t, filepath.Join(root, dir), func(fset *token.FileSet, file *ast.File) {
			rel := shortPos(root, fset.Position(file.Pos()).Filename)
			out = append(out, parsedFile{
				fset: fset, file: file, rel: rel,
				names: namesOf(file, filepath.ToSlash(filepath.Dir(rel))),
			})
		})
	}
	return out
}

// fileNames is what a name-based walk knows about one file: its package's
// directory and what it imports under which local name.
type fileNames struct {
	// dir is the file's directory, repository-relative with forward slashes,
	// which is how a package is told from another without the type checker.
	dir string
	// imports is every local import name, to its path.
	imports map[string]string
	// store and statelog are the names those two packages are imported
	// under, empty where the file does not import them.
	store, statelog string
}

// module is the module path, which an import of this tree's own packages
// starts with.
const module = "github.com/crewlet/crewlet"

// namesOf reads a file's imports.
//
// An import with no explicit name is named by its path's last element. That
// is Go's default for every package in this tree; for a path whose package
// clause differs from its last element (a `.v3` suffix) the name read here
// matches nothing, which makes a call through it look like a method call —
// the safe direction for every gate that asks.
func namesOf(file *ast.File, dir string) fileNames {
	f := fileNames{dir: dir, imports: map[string]string{}}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		f.imports[name] = path
		switch path {
		case module + "/internal/store":
			f.store = name
		case module + "/internal/statelog":
			f.statelog = name
		}
	}
	return f
}

// moduleDir is an import path as this tree's directory, or the path itself
// for a package outside it.
func moduleDir(path string) string {
	if rest, ok := strings.CutPrefix(path, module+"/"); ok {
		return rest
	}
	return path
}

// inspectWithParent is [ast.Inspect] with each node's parent, for a matcher
// whose verdict on a call depends on what is done with its answer. fn's result
// means what it means to Inspect: false skips the node's children.
func inspectWithParent(root ast.Node, fn func(n, parent ast.Node) bool) {
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		if !fn(n, parent) {
			return false
		}
		stack = append(stack, n)
		return true
	})
}

func shortPos(root, pos string) string {
	if rel, err := filepath.Rel(root, pos); err == nil {
		return rel
	}
	return pos
}

// EVERY TABLE BELONGS TO EXACTLY ONE ESTATE.
//
// A node's own file and its partitions carry two migration sequences, and
// nothing stops a table name being declared in both — at which point every rule
// above it is meaningless: a statement naming that table is in neither estate
// and in both, and a reader tracing a row has two places to look.
func TestNoTableIsDeclaredInBothEstates(t *testing.T) {
	t.Parallel()
	node, partition := tablesIn(t, store.EstateNode), tablesIn(t, store.EstatePartition)
	var shared []string
	for name := range node {
		if partition[name] {
			shared = append(shared, name)
		}
	}
	slices.Sort(shared)
	if len(shared) > 0 {
		t.Errorf("declared in both estates: %v — a table lives in one file, "+
			"or a reader tracing a row has two places to look", shared)
	}
}
