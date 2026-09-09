package tracker_test

import (
	"database/sql"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// ddl opens a store and returns every tracker object's own CREATE statement.
//
// READ BACK OUT OF THE DATABASE rather than out of the .sql file, because what
// these tests are about is what the driver actually created: a clause the file
// carries and the driver silently ignored would pass a text scan and fail in
// production.
func ddl(t *testing.T) map[string]string {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	out := map[string]string{}
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
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
func TestEveryTrackerIndexNamesItsQuery(t *testing.T) {
	t.Parallel()
	source, err := trackerSchemaSource()
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	lines := strings.Split(source, "\n")
	seen := 0
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "CREATE INDEX") {
			continue
		}
		seen++
		// Walk to the line that closes the statement; the comment goes
		// there, because a multi-line index ends with its WHERE clause.
		closing := i
		for closing < len(lines) && !strings.Contains(lines[closing], ";") {
			closing++
		}
		if closing == len(lines) {
			t.Fatalf("the CREATE INDEX beginning at line %d never terminates", i+1)
		}
		_, tail, _ := strings.Cut(lines[closing], ";")
		if !strings.Contains(tail, "--") || len(strings.TrimSpace(tail)) < 8 {
			t.Errorf("this index names no reader:\n\t%s", strings.TrimSpace(lines[closing]))
		}
	}
	if seen == 0 {
		t.Fatal("the migration declares no indexes at all, so this guard is " +
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
	// Fifteen plain and nine partial, each enumerated in the migration with
	// the query it serves.
	const want = 24
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
	for _, table := range tracker.ReproducibleTables {
		if !slices.Contains(present, table) {
			t.Errorf("%s is named as reproducible and the schema does not have it",
				table)
		}
	}
	for _, table := range tracker.MachineryTables {
		if !slices.Contains(present, table) {
			t.Errorf("%s is named as the log's own machinery and the schema does "+
				"not have it", table)
		}
	}
	for _, table := range present {
		if tracker.Reproducible(table) ||
			slices.Contains(tracker.MachineryTables, table) {
			continue
		}
		t.Errorf("%s is in the schema and in neither list — it is durable state "+
			"that either a record must rebuild or the log owns, and there is no "+
			"third answer", table)
	}
}

// THE SPEND COLUMNS ARE ONE LIST, AND THE DDL IS DRIVEN FROM IT.
//
// Eight column names written out in the struct, the DDL and the applier's
// statement is three chances to add the ninth to two of them — and the failure
// is silent: a counter nothing increments reads zero for ever, which looks
// exactly like a task nobody has worked on.
func TestTheSpendColumnsMatchTheSchema(t *testing.T) {
	t.Parallel()
	statement := ddl(t)["table:tracker_tasks"]
	for _, column := range tracker.SpendColumns {
		if !strings.Contains(statement, column+" ") {
			t.Errorf("%s is in the spend list and not in tracker_tasks", column)
		}
	}
	found := regexp.MustCompile(`\bspend_[a-z_]+\b`).FindAllString(statement, -1)
	slices.Sort(found)
	found = slices.Compact(found)
	for _, column := range found {
		if !slices.Contains(tracker.SpendColumns, column) {
			t.Errorf("%s is a spend column in the schema and not in the list, so "+
				"the applier will never write it", column)
		}
	}
	if len(found) != len(tracker.SpendColumns) {
		t.Fatalf("the schema carries %d spend columns and the list has %d",
			len(found), len(tracker.SpendColumns))
	}
}

// THE RANK COLUMN'S ALPHABET IS A CHECK, AND IT IS THE NEGATED FORM.
//
// The obvious spelling — GLOB '[0-9A-Za-z]*' — ACCEPTS 'a-0', because it means
// "one character from the class, then anything". Measured, and the reason the
// constraint is written the way it is.
func TestTheRankCheckRefusesWhatTheObviousSpellingAccepts(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	defer func() { _ = db.Close() }()

	insert := func(rank string) error {
		return db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
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

// trackerSchemaSource reads the migration file itself, for the one assertion
// that is about the SOURCE rather than about what the driver created: a
// trailing comment is not part of the schema the database keeps.
func trackerSchemaSource() (string, error) {
	body, err := store.SchemaFile(store.EstateReplicated, "0023_the_tracker_lands.sql")
	if err != nil {
		return "", err
	}
	return string(body), nil
}
