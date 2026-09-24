package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
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
// and when the guard has stopped working. That was acute when this was
// written, because the replicated estate had no tables at all and the walk
// could find nothing by construction; it now derives sixty of them, and the
// controls are what keep the guarantee the same either way. They run the
// matcher on strings whose verdict is known, so this is a test that can fail
// today rather than one that starts working later and is trusted meanwhile.
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
	// so these cases keep their meaning whatever either schema does next —
	// including an estate emptied by a migration, which is the state this
	// was written in.
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

// createTable finds the tables an estate's DDL declares, and dropTable the
// ones a later migration takes away again.
var (
	createTable = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	dropTable   = regexp.MustCompile(`(?im)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
)

// tablesIn is every table one estate's embedded schema LEAVES BEHIND.
//
// THE DROPS COUNT, and reading only the creates is the bug that hid behind
// this being append-only: a table that moves estate is created in the new one
// and dropped in the old, and a derivation blind to the drop reports it in
// both — which fails the one-estate rule below for exactly the change that
// satisfies it, and silently makes every statement naming that table look like
// a boundary crossing. The files are walked in migration order, so a create
// after a drop is a table that came back.
func tablesIn(t *testing.T, estate store.Estate) map[string]bool {
	t.Helper()
	dir := filepath.Join(sourcetree.Root(t), "internal", "store", "schema", string(estate))
	out := map[string]bool{}
	for _, name := range store.SchemaVersions(estate) {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// DROPS FIRST WITHIN ONE FILE would be wrong for a migration
		// that drops and recreates, so each file is applied in the
		// order its statements appear.
		for _, m := range ddlStatements(string(body)) {
			if m.drop {
				delete(out, m.table)
				continue
			}
			out[m.table] = true
		}
	}
	// schema_migrations is created by the migrator rather than by a file,
	// and exists in BOTH estates — so it is neither estate's and naming it
	// is never a crossing.
	delete(out, "schema_migrations")
	return out
}

// ddlStatement is one CREATE TABLE or DROP TABLE, in the order it appears.
type ddlStatement struct {
	table string
	drop  bool
}

// ddlStatements walks one migration's creates and drops in source order.
//
// SOURCE ORDER, not creates-then-drops: a migration that drops a table and
// recreates it in a new shape is a table the estate still has, and the reverse
// reading loses it.
func ddlStatements(body string) []ddlStatement {
	type at struct {
		pos int
		st  ddlStatement
	}
	var all []at
	for _, m := range createTable.FindAllStringSubmatchIndex(body, -1) {
		all = append(all, at{m[0], ddlStatement{table: strings.ToLower(body[m[2]:m[3]])}})
	}
	for _, m := range dropTable.FindAllStringSubmatchIndex(body, -1) {
		all = append(all, at{m[0], ddlStatement{table: strings.ToLower(body[m[2]:m[3]]), drop: true}})
	}
	slices.SortFunc(all, func(a, b at) int { return a.pos - b.pos })
	out := make([]ddlStatement, 0, len(all))
	for _, a := range all {
		out = append(out, a.st)
	}
	return out
}

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
