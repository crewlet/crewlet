package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// NO STATEMENT NAMES BOTH ESTATES.
//
// A node is two database FILES, and the two rules that makes true are that no
// transaction spans them and no read joins across them. Both fail the same
// way if they are broken: not with an error a caller can handle, but with a
// driver refusing a table it cannot see, from a query nobody thought was
// crossing a boundary — because in one file it was not.
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
// The table names are DERIVED from each estate's own embedded schema, so a
// table added by a migration is covered by that migration and nothing else. A
// literal that looks like SQL and names a table from BOTH estates is the
// violation.
//
// # Why the control strings are the load-bearing half
//
// A guard asserting an absence passes identically when the thing is absent
// and when the guard has stopped working — and today the replicated estate
// has no tables at all, so the walk over the tree can find nothing by
// construction. The controls below run the matcher on strings whose verdict
// is known, which is what makes this a test that can fail today rather than
// one that starts working later and is trusted in the meantime.
func TestNoStatementNamesBothEstates(t *testing.T) {
	t.Parallel()

	node := tablesIn(t, store.EstateNode)
	if len(node) == 0 {
		t.Fatal("no tables were derived from the node estate's schema; the " +
			"derivation no longer recognises the DDL it is meant to cover")
	}
	replicated := tablesIn(t, store.EstateReplicated)

	// The matcher, exercised on strings whose verdict is known. Both
	// estates are named explicitly here rather than taken from the schema,
	// so these cases keep their meaning while the replicated estate is
	// still empty.
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

	root := moduleRoot(t)
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
				if bothEstates(text, node, replicated) {
					crossings = append(crossings, shortPos(root, fset.Position(lit.Pos()).String())+
						": "+strings.Join(strings.Fields(text), " "))
				}
				return true
			})
		})
	}
	for _, c := range crossings {
		t.Errorf("a statement names tables in both estates, which are two "+
			"database files: %s", c)
	}
	t.Logf("estate boundary: %d node table(s), %d replicated table(s)",
		len(node), len(replicated))
}

// sqlVerb introduces a table name in the statements this engine writes.
var sqlVerb = regexp.MustCompile(`(?is)\b(?:FROM|JOIN|INTO|UPDATE|TABLE)\s+([a-z_][a-z0-9_]*)`)

// bothEstates reports whether one statement names a table from each estate.
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

// createTable finds the tables an estate's DDL declares.
var createTable = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)

// tablesIn is every table one estate's embedded schema creates.
func tablesIn(t *testing.T, estate store.Estate) map[string]bool {
	t.Helper()
	dir := filepath.Join(moduleRoot(t), "internal", "store", "schema", string(estate))
	out := map[string]bool{}
	for _, name := range store.SchemaVersions(estate) {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range createTable.FindAllStringSubmatch(string(body), -1) {
			out[strings.ToLower(m[1])] = true
		}
	}
	// schema_migrations is created by the migrator rather than by a file,
	// and exists in BOTH estates — so it is neither estate's and naming it
	// is never a crossing.
	delete(out, "schema_migrations")
	return out
}

// walkGoFiles parses every non-test .go file under dir.
func walkGoFiles(t *testing.T, dir string, fn func(*token.FileSet, *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
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
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}
	return root
}

func shortPos(root, pos string) string {
	if rel, err := filepath.Rel(root, pos); err == nil {
		return rel
	}
	return pos
}

// EVERY TABLE BELONGS TO EXACTLY ONE ESTATE.
//
// Two files, two migration sequences, and nothing stops a table name being
// declared in both — at which point every rule above it is meaningless: a
// statement naming that table is in neither estate and in both, and a reader
// tracing a row has two places to look.
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
