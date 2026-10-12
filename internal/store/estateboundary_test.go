package store_test

import (
	"context"
	"database/sql"
	"fmt"
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
// A data node is two database files — its own and the replicated estate — and
// the two rules that makes true are that no transaction spans them and no read
// joins across them. Both fail the same way if they are broken: not
// with an error a caller can handle, but with a driver refusing a table it
// cannot see, from a query nobody thought was crossing a boundary — because in
// one file it was not.
//
// # What it walks
//
// Every non-test .go file under internal/ and cmd/, parsed with go/parser,
// looking at every STRING the code composes — a literal, a concatenation, a
// fmt.Sprintf format — rendered as the applier gate renders them
// ([composedString]), with what it cannot resolve marked rather than dropped.
// A comment is not a string, so a doc discussing `tracker_tasks` and
// `crewlet_events` in one sentence — which several package docs legitimately
// do — is not one.
//
// # What it looks for
//
// Two things. A literal that looks like SQL and names a table the node's own
// schema declares AND one the replicated schema declares — the table names are
// DERIVED from each schema, so a table added by a migration is covered by that
// migration and nothing else. And any ATTACH, which is the one way a statement
// on one connection reaches a SECOND file — a copy of the replicated estate, a
// snapshot artefact, a backup member — whose table names are the same as the
// live file's, so no table name can tell a statement that crossed into a copy
// from one that did not, and the only statement that can cross is one that
// attaches.
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
	replicated := tablesIn(t, store.EstateReplicated)

	// The matchers, exercised on strings whose verdict is known. Both
	// estates are named explicitly here rather than taken from the schema,
	// so these cases keep their meaning whatever either schema does next.
	fakeNode := map[string]bool{"crewlet_events": true}
	fakeReplicated := map[string]bool{"tracker_tasks": true}
	for _, positive := range []string{
		`SELECT t.id FROM tracker_tasks t JOIN crewlet_events e ON e.id = t.turn_id`,
		`INSERT INTO crewlet_events (id) SELECT id FROM tracker_tasks`,
		`UPDATE tracker_tasks SET n = (SELECT count(*) FROM crewlet_events)`,
	} {
		if !bothEstates(positive, fakeNode, fakeReplicated) {
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
		if bothEstates(negative, fakeNode, fakeReplicated) {
			t.Errorf("control: %q does not cross the boundary but the matcher "+
				"flagged it", negative)
		}
	}
	for _, positive := range []string{
		`ATTACH DATABASE 'snapshot-41-1700000000.db' AS other`,
		`attach '/data/crewlet-replicated.db' as estate`,
		`ATTACH ? AS peer`,
		// THE SHAPES A PATH COMPUTED IN GO TAKES, which is the natural
		// way to attach a copy's file: a format string, the
		// literal prefix of a concatenation, a double-quoted name and
		// the other parameter spellings.
		`ATTACH DATABASE %q AS peer`,
		`ATTACH DATABASE `,
		`ATTACH DATABASE "store-replicated.db" AS other`,
		`ATTACH $1 AS peer`,
		`ATTACH @path AS peer`,
		`ATTACH :path AS peer`,
		`ATTACH %s AS peer`,
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
		`attach it to the report`,
		`attach a file to the task`,
	} {
		if attaches(negative) {
			t.Errorf("control: %q attaches nothing but the matcher flagged it", negative)
		}
	}
	// AND THROUGH THE RENDERER the walk reads the tree with, which is what
	// turns a concatenation into one string: its variable operand renders
	// as the rune the walk cannot resolve, and that is an attach too.
	for _, src := range []string{
		`"ATTACH " + quote(path) + " AS peer"`,
		`fmt.Sprintf("ATTACH DATABASE %q AS peer", path)`,
		`"ATTACH DATABASE " + quote(path) + " AS peer"`,
	} {
		text, ok := composedString(mustParse(t, src), map[string]string{})
		if !ok || !attaches(text) {
			t.Errorf("control: %s attaches a second file and the walk did not "+
				"flag it (rendered %q)", src, text)
		}
	}

	// COMPOSED STRINGS, not literals alone, for the reason the applier
	// gate's renderer gives: a statement built by concatenation or by
	// fmt.Sprintf names its file — or its second table — in no single
	// literal, and an ATTACH of a path computed in Go is exactly that.
	root := sourcetree.Root(t)
	var crossings []string
	for _, f := range moduleTree(t) {
		ast.Inspect(f.file, func(n ast.Node) bool {
			text, ok := composedString(n, f.consts)
			if !ok {
				return true
			}
			if bothEstates(text, node, replicated) || attaches(text) {
				crossings = append(crossings, shortPos(root, f.fset.Position(n.Pos()).String())+
					": "+strings.Join(strings.Fields(text), " "))
			}
			// A composed string's own operands are literals the walk
			// would otherwise report again, at a worse position.
			_, isLit := n.(*ast.BasicLit)
			return isLit
		})
	}
	for _, c := range crossings {
		t.Errorf("a statement reaches two database files — a node's own and the "+
			"replicated estate, or a second file it attaches: %s", c)
	}
	t.Logf("estate boundary: %d node table(s), %d replicated table(s)",
		len(node), len(replicated))
}

// attachStatement is SQLite's ATTACH: the keyword, then either DATABASE or
// whatever can name the file — a quoted string, any parameter spelling, a
// format verb, or the rune [composedString] renders an operand it cannot
// resolve as.
//
// THE KEYWORD MUST BE FOLLOWED BY SPACE, which is what keeps prose out: no
// English sentence puts "attach" before one of these, and "attached" and
// "attachment" never match the keyword at all. What it must not require is a
// literal path after DATABASE: the first version did, and every attach whose
// path is computed in Go — `fmt.Sprintf("ATTACH DATABASE %q AS p", path)`,
// `"ATTACH DATABASE " + quote(path)` — passed.
var attachStatement = regexp.MustCompile(`(?is)\bATTACH\s+(?:DATABASE\b|['"?:$@%` + unresolved + `])`)

// attaches reports whether a statement attaches a second database file.
func attaches(text string) bool { return attachStatement.MatchString(text) }

// sqlVerb introduces a table name in the statements this engine writes.
var sqlVerb = regexp.MustCompile(`(?is)\b(?:FROM|JOIN|INTO|UPDATE|TABLE)\s+([a-z_][a-z0-9_]*)`)

// bothEstates reports whether one statement names a table of the node's own
// schema and one of the replicated schema.
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

// parsedFile is one non-test source file the gates in this package read, with
// the names they judge it by.
type parsedFile struct {
	fset  *token.FileSet
	file  *ast.File
	rel   string
	names fileNames
	// consts is the file's own string constants (see [stringConsts]).
	consts map[string]string
}

// moduleTree is every non-test .go file under the module's internal/ and
// cmd/, parsed ONCE per test binary and shared by every gate here — the four
// estate gates each parsed the same 1,087 files, eighteen megabytes, for
// themselves, and three of those parses bought nothing. The source is the
// build's, so no gate can see it change, and the gates only read what is
// parsed: ast.Inspect, a position, a map lookup.
//
// Shared rather than FILTERED: a gate here joins what it reads across files
// — the applier gate's accessor list, a statement composed from a constant —
// so no one file's bytes can say it holds nothing the gate needs.
func moduleTree(t *testing.T) []parsedFile {
	t.Helper()
	files, err := parsedModuleTree()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

var parsedModuleTree = sync.OnceValues(func() ([]parsedFile, error) {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return nil, err
	}
	return parseTree(root, "internal", "cmd")
})

// parseTree parses every non-test .go file under each of dirs, relative to
// root — for a gate that needs a first pass over the whole tree before it can
// judge any one file.
func parseTree(root string, dirs ...string) ([]parsedFile, error) {
	var out []parsedFile
	for _, dir := range dirs {
		fset := token.NewFileSet()
		files := 0
		err := sourcetree.Walk(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			files++
			file, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel := shortPos(root, p)
			out = append(out, parsedFile{
				fset: fset, file: file, rel: rel,
				names:  namesOf(file, filepath.ToSlash(filepath.Dir(rel))),
				consts: stringConsts(file),
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", dir, err)
		}
		// Every gate asserts an absence over what this parsed, and a walk
		// that parsed nothing asserts it just as confidently.
		if files == 0 {
			return nil, fmt.Errorf("parsed no Go files under %s — this guard was "+
				"certifying nothing", dir)
		}
	}
	return out, nil
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
// A node's own file and its replicated estate carry two migration sequences, and
// nothing stops a table name being declared in both — at which point every rule
// above it is meaningless: a statement naming that table is in neither estate
// and in both, and a reader tracing a row has two places to look.
func TestNoTableIsDeclaredInBothEstates(t *testing.T) {
	t.Parallel()
	node, replicated := tablesIn(t, store.EstateNode), tablesIn(t, store.EstateReplicated)
	var shared []string
	for name := range node {
		if replicated[name] {
			shared = append(shared, name)
		}
	}
	slices.Sort(shared)
	if len(shared) > 0 {
		t.Errorf("declared in both estates: %v — a table lives in one file, "+
			"or a reader tracing a row has two places to look", shared)
	}
}
