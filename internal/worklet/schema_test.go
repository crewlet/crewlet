package worklet_test

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ddl opens a store and returns every tracker object's own CREATE statement.
//
// READ BACK OUT OF THE DATABASE rather than out of the .sql file, because what
// these tests are about is what the driver actually created: a clause the file
// carries and the driver silently ignored would pass a text scan and fail in
// production.
func ddl(t *testing.T) map[string]string {
	t.Helper()
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	out := map[string]string{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `
			SELECT name, type, COALESCE(tbl_name, ''), COALESCE(sql, '')
			FROM sqlite_master
			WHERE name LIKE 'tracker_%' OR tbl_name LIKE 'tracker_%'`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name, kind, table, statement string
			if err := rows.Scan(&name, &kind, &table, &statement); err != nil {
				return err
			}
			out[kind+":"+name] = statement
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the schema back: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the schema carries no tracker objects at all, so every " +
			"assertion below is about an empty set")
	}
	return out
}

// tablesIn returns the tracker table names present in the schema.
func tablesIn(schema map[string]string) []string {
	var out []string
	for key := range schema {
		if name, ok := strings.CutPrefix(key, "table:"); ok &&
			strings.HasPrefix(name, "tracker_") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// NO FOREIGN KEY ANYWHERE IN THE TRACKER'S SCHEMA.
//
// A cascade is a delete nobody committed: the applier removes a parent's
// children in the same statement list, where the deletion is part of the
// record's own effect and therefore identical on every node. A cascade would
// make one node's delete depend on a pragma the next node might not have set.
func TestNoForeignKeyInTracker(t *testing.T) {
	t.Parallel()
	// The word, not the substring: `tracker_references` is a table whose
	// NAME contains it, and a plain Contains flagged that table and its
	// index — a guard that fails on a correct schema is one somebody
	// deletes rather than fixes.
	clause := regexp.MustCompile(`(?i)(^|[^A-Za-z_])REFERENCES\s`)
	for name, statement := range ddl(t) {
		if clause.MatchString(statement) {
			t.Errorf("%s declares a foreign key:\n%s", name, statement)
		}
	}
}

// NO `COLLATE` ANYWHERE, and the reason is the manual order.
//
// The default TEXT collation is BINARY, so ORDER BY rank reproduces the Go
// comparison exactly. The pinned driver ACCEPTS `COLLATE NOCASE` — it is not
// refused, it is applied — which would silently collapse 'A' and 'a' and
// destroy the order the whole rank algebra exists to produce. There is no
// symptom until somebody notices a board that sorts differently from the
// engine that wrote it.
func TestNoCollateInTracker(t *testing.T) {
	t.Parallel()
	for name, statement := range ddl(t) {
		if strings.Contains(strings.ToUpper(statement), "COLLATE") {
			t.Errorf("%s carries a collation:\n%s", name, statement)
		}
	}
}

// NO `UNIQUE` OUTSIDE A PRIMARY KEY.
//
// A constraint violation inside the apply transaction aborts it
// deterministically on every node — which turns a rare cosmetic anomaly into a
// fleet-wide stalled log. The worked example is the task key: a restored
// counter beside newer tasks flags a collision and keeps serving, where a
// unique index would wedge every node at once.
func TestNoUniqueOutsideAPrimaryKey(t *testing.T) {
	t.Parallel()
	for name, statement := range ddl(t) {
		upper := strings.ToUpper(statement)
		if strings.HasPrefix(name, "index:") && strings.Contains(upper, "UNIQUE") {
			t.Errorf("%s is a unique index:\n%s", name, statement)
		}
		if strings.HasPrefix(name, "table:") &&
			regexp.MustCompile(`\bUNIQUE\b`).MatchString(upper) {
			t.Errorf("%s declares a unique constraint outside its primary key:\n%s",
				name, statement)
		}
	}
}

// EVERY INDEX NAMES THE QUERY IT SERVES.
//
// Twenty-four indexes on the hottest table is a write cost every commit
// carries, and an index nobody reads is that cost with no reader. Naming the
// reader in the DDL is what makes deleting one a decision somebody can check
// rather than a guess — and it is how two partial indexes with no writer at
// all survived a design review in the shape this replaces.
//
// EVERY LIVE INDEX, WHICHEVER MIGRATION MADE IT. This read 0002 alone, so an
// index a later migration added was never asked for its reader — and one had
// none. What it checks now is the schema a database actually ends up with:
// every tracker index present there is found at the LAST migration that
// creates it, and that statement must name what reads it.
func TestEveryTrackerIndexNamesItsQuery(t *testing.T) {
	t.Parallel()
	sources, err := trackerSchemaSources()
	if err != nil {
		t.Fatalf("read the migrations: %v", err)
	}
	// closing is each index's creating statement's last line, from the
	// latest migration that creates it.
	closing := map[string]string{}
	for _, source := range sources {
		lines := strings.Split(source, "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "CREATE INDEX") {
				continue
			}
			fields := strings.Fields(trimmed)
			if len(fields) < 3 {
				t.Fatalf("an index statement with no name: %q", trimmed)
			}
			end := i
			for end < len(lines) && !strings.Contains(lines[end], ";") {
				end++
			}
			if end == len(lines) {
				t.Fatalf("the CREATE INDEX %s never terminates", fields[2])
			}
			closing[fields[2]] = lines[end]
		}
	}
	seen := 0
	for name := range ddl(t) {
		index, ok := strings.CutPrefix(name, "index:")
		if !ok || !strings.HasPrefix(index, "tracker_") || strings.HasPrefix(index, "sqlite_autoindex") {
			continue
		}
		seen++
		line, found := closing[index]
		if !found {
			t.Errorf("index %s is in the schema and no migration creates it", index)
			continue
		}
		_, tail, _ := strings.Cut(line, ";")
		if !strings.Contains(tail, "--") || len(strings.TrimSpace(tail)) < 8 {
			t.Errorf("index %s names no reader:\n\t%s", index, strings.TrimSpace(line))
		}
	}
	if seen == 0 {
		t.Fatal("the schema holds no tracker indexes at all, so this guard is " +
			"measuring its own scanner")
	}
}

// THE INDEX COUNT ON THE HOTTEST TABLE IS ASSERTED, NOT ASSUMED.
//
// It is the number the apply drain's own throughput is measured against, so an
// index added without a reader — or one deleted with one — moves a figure
// three constants are sized from.
func TestTrackerTasksIndexCount(t *testing.T) {
	t.Parallel()
	schema := ddl(t)
	count := 0
	for name, statement := range schema {
		if strings.HasPrefix(name, "index:tracker_tasks_") && statement != "" {
			count++
		}
	}
	// Eighteen: eleven plain and seven partial, the abandoned move's among
	// them (replicated migration 0018) and the move stragglers' (0041) —
	// each partial on a flag a healthy company holds on no row, so neither is
	// a cost any ordinary commit pays. EVERY ONE IS REACHED BY A PLAN, which
	// is what TestEveryIndexServesARegisteredQuery establishes and this count
	// makes visible — an index added without a reader moves this number
	// before it moves a benchmark, nine that no plan reached were deleted
	// along the way, the embed duty's on `embed_rev` went at 0039: a column
	// nothing wrote, kept by a gate entry for a selection nothing ran, and the
	// batch filter's on `batch_id` at 0053, for the same reason: a plan
	// reached it, for a `batch=` that could only ever match nothing.
	const want = 18
	if count != want {
		t.Fatalf("tracker_tasks carries %d indexes and the enumeration is %d — "+
			"every one of them is a write cost on every commit, so the two have "+
			"to agree", count, want)
	}
}

// EVERY REPRODUCIBLE TABLE EXISTS, AND EVERY TRACKER TABLE IS ACCOUNTED FOR.
//
// Both directions, because either alone is half a check: a list naming a table
// the schema does not have is a replay that will fail on a table nobody
// created, and a schema table missing from the list is durable state no
// record has to be able to rebuild — which is exactly how a replay ends with a
// history table and no tasks.
func TestEveryTrackerTableIsAccountedFor(t *testing.T) {
	t.Parallel()
	present := tablesIn(ddl(t))
	for _, table := range worklet.ReproducibleTables {
		if !slices.Contains(present, table) {
			t.Errorf("%s is named as reproducible and the schema does not have it",
				table)
		}
	}
	for _, table := range worklet.MachineryTables {
		if !slices.Contains(present, table) {
			t.Errorf("%s is named as the log's own machinery and the schema does "+
				"not have it", table)
		}
	}
	for _, table := range present {
		if worklet.Reproducible(table) ||
			slices.Contains(worklet.MachineryTables, table) {
			continue
		}
		t.Errorf("%s is in the schema and in neither list — it is durable state "+
			"that either a record must rebuild or the log owns, and there is no "+
			"third answer", table)
	}
}

// EVERY COLUMN OF A REPRODUCIBLE TABLE IS NAMED BY A STATEMENT THIS PACKAGE
// RUNS.
//
// A column no statement here names is one no applier writes and no reader
// reads: it holds its default on every row of every node, rides every
// snapshot, and says nothing. Two were found only by reading the code —
// `embed_rev`, gone at replicated 0039, and `batch_id` on tasks and history,
// gone at 0053 — because nothing compared the schema with the statements. So
// the columns are read out of the migrated database, as every case here reads
// the schema, and each is held to the STRING LITERALS of this package's own
// source, which is where every statement it runs is written; a comment naming
// a column does not count, because a comment runs nothing.
//
// The log's own machinery tables are not held here: the state-log framework
// writes them, from its own package.
//
// Mutation: drop the history half of replicated 0053 and this fails on
// tracker_history.batch_id.
func TestEveryTrackerColumnIsNamedByAStatement(t *testing.T) {
	t.Parallel()
	statements := trackerLiterals(t)
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	checked := 0
	for _, table := range worklet.ReproducibleTables {
		var columns []string
		if err := db.Read(t.Context(), func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(t.Context(),
				`SELECT name FROM pragma_table_info(?)`, table)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					return err
				}
				columns = append(columns, name)
			}
			return rows.Err()
		}); err != nil {
			t.Fatalf("read %s's columns: %v", table, err)
		}
		if len(columns) == 0 {
			t.Errorf("%s has no columns: it is not in the schema at all", table)
		}
		for _, column := range columns {
			checked++
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(column) + `\b`).MatchString(statements) {
				t.Errorf("%s.%s is named by no statement this package runs: no "+
					"applier writes it and no reader reads it, so a new numbered "+
					"replicated migration drops it", table, column)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no reproducible table has a column, so this guard read nothing")
	}
}

// trackerLiterals is every string literal in this package's non-test source,
// joined by newlines.
func trackerLiterals(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(sourcetree.Root(t), "internal", "worklet")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out strings.Builder
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil {
					out.WriteString(value)
					out.WriteByte('\n')
				}
			}
			return true
		})
	}
	if out.Len() == 0 {
		t.Fatalf("%s holds no string literal, so this guard is reading nothing", dir)
	}
	return out.String()
}

// THE SPEND COLUMNS ARE ONE LIST, AND THE DDL IS DRIVEN FROM IT.
//
// Ten column names written out in the struct, the DDL and the applier's
// statement is three chances to add the eleventh to two of them — and the failure
// is silent: a counter nothing increments reads zero for ever, which looks
// exactly like a task nobody has worked on.
func TestTheSpendColumnsMatchTheSchema(t *testing.T) {
	t.Parallel()
	statement := ddl(t)["table:tracker_tasks"]
	for _, column := range worklet.SpendColumns {
		if !strings.Contains(statement, column+" ") {
			t.Errorf("%s is in the spend list and not in tracker_tasks", column)
		}
	}
	found := regexp.MustCompile(`\bspend_[a-z_]+\b`).FindAllString(statement, -1)
	slices.Sort(found)
	found = slices.Compact(found)
	for _, column := range found {
		if !slices.Contains(worklet.SpendColumns, column) {
			t.Errorf("%s is a spend column in the schema and not in the list, so "+
				"the applier will never write it", column)
		}
	}
	if len(found) != len(worklet.SpendColumns) {
		t.Fatalf("the schema carries %d spend columns and the list has %d",
			len(found), len(worklet.SpendColumns))
	}
}

// THE RANK COLUMN'S ALPHABET IS A CHECK, AND IT IS THE NEGATED FORM.
//
// The obvious spelling — GLOB '[0-9A-Za-z]*' — ACCEPTS 'a-0', because it means
// "one character from the class, then anything". Measured, and the reason the
// constraint is written the way it is.
func TestTheRankCheckRefusesWhatTheObviousSpellingAccepts(t *testing.T) {
	t.Parallel()
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	defer func() { _ = dbNode.Close() }()

	insert := func(rank string) error {
		return db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `
				INSERT INTO tracker_tasks
					(id, key, project_key, root_id, type, title, status,
					 status_group, rank, created_at, updated_at, version, document)
				VALUES (?, 'ENG-1', 'ENG', ?, 'task', 't', 'todo', 'not_started',
					?, 0, 0, 1, x'00')`, rank, rank, rank)
			return err
		})
	}
	if err := insert("a0"); err != nil {
		t.Fatalf("a well-formed rank was refused: %v", err)
	}
	for _, bad := range []string{"a-0", "a 0", "a", "a0!"} {
		if err := insert(bad); err == nil {
			t.Errorf("the rank %q was accepted; the CHECK is what stops a key "+
				"outside the alphabet ordering differently from every other", bad)
		}
	}
}

// trackerSchemaSources reads every replicated migration, in application
// order, for the one assertion that is about the SOURCE rather than about what
// the driver created: a trailing comment is not part of the schema the
// database keeps.
func trackerSchemaSources() ([]string, error) {
	var out []string
	for _, name := range store.SchemaVersions(store.EstateReplicated) {
		body, err := store.SchemaFile(store.EstateReplicated, name)
		if err != nil {
			return nil, err
		}
		out = append(out, string(body))
	}
	return out, nil
}
