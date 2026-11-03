package code_test

import (
	"database/sql"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// ddl opens a store and returns every code object's own CREATE statement, as
// the driver created it — a clause the migration carries and the driver
// silently ignored would pass a text scan and fail in production.
func ddl(t *testing.T) map[string]string {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	out := map[string]string{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `
			SELECT name, type, COALESCE(sql, '') FROM sqlite_master
			WHERE name LIKE 'code_%' OR tbl_name LIKE 'code_%'`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name, kind, statement string
			if err := rows.Scan(&name, &kind, &statement); err != nil {
				return err
			}
			out[kind+":"+name] = statement
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the schema back: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the schema carries no code objects at all, so every assertion below is " +
			"about an empty set")
	}
	return out
}

// NO FOREIGN KEY IN THE CODE SCHEMA: a cascade is a delete nobody committed,
// depending on a pragma the next node might not have set — the applier removes
// a repository's rows in the record's own statement list instead.
func TestNoForeignKeyInCode(t *testing.T) {
	t.Parallel()
	clause := regexp.MustCompile(`(?i)(^|[^A-Za-z_])REFERENCES\s`)
	for name, statement := range ddl(t) {
		if clause.MatchString(statement) {
			t.Errorf("%s declares a foreign key:\n%s", name, statement)
		}
	}
}

// NO `COLLATE`: a ref's order is BYTE order — packed-refs' order and Go's — and
// a collation would be a second answer to what one ref is.
func TestNoCollateInCode(t *testing.T) {
	t.Parallel()
	for name, statement := range ddl(t) {
		if strings.Contains(strings.ToUpper(statement), "COLLATE") {
			t.Errorf("%s carries a collation:\n%s", name, statement)
		}
	}
}

// NO `UNIQUE` OUTSIDE A PRIMARY KEY: a constraint violation inside the apply
// transaction aborts it on every node at once — a fleet-wide stalled log over
// what the applier's own guard should have answered.
func TestNoUniqueOutsideAPrimaryKeyInCode(t *testing.T) {
	t.Parallel()
	unique := regexp.MustCompile(`\bUNIQUE\b`)
	for name, statement := range ddl(t) {
		upper := strings.ToUpper(statement)
		if strings.HasPrefix(name, "index:") && strings.Contains(upper, "UNIQUE") {
			t.Errorf("%s is a unique index:\n%s", name, statement)
		}
		if strings.HasPrefix(name, "table:") && unique.MatchString(upper) {
			t.Errorf("%s declares a unique constraint outside its primary key:\n%s", name, statement)
		}
	}
}

// EVERY CODE INDEX NAMES THE QUERY IT SERVES, at the latest migration that
// creates it: an index is a write cost on every push, and naming its reader is
// what makes removing one a decision somebody can check.
func TestEveryCodeIndexNamesItsQuery(t *testing.T) {
	t.Parallel()
	closing := map[string]string{}
	for _, version := range store.SchemaVersions(store.EstateReplicated) {
		body, err := store.SchemaFile(store.EstateReplicated, version)
		if err != nil {
			t.Fatalf("read %s: %v", version, err)
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "CREATE INDEX") {
				continue
			}
			fields := strings.Fields(trimmed)
			end := i
			for end < len(lines) && !strings.Contains(lines[end], ";") {
				end++
			}
			if len(fields) < 3 || end == len(lines) {
				t.Fatalf("an index statement %q in %s does not parse", trimmed, version)
			}
			closing[fields[2]] = lines[end]
		}
	}
	seen := 0
	for name := range ddl(t) {
		index, ok := strings.CutPrefix(name, "index:")
		if !ok || !strings.HasPrefix(index, "code_") {
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
		t.Fatal("the schema holds no code indexes at all, so this guard measures its own scanner")
	}
}

// EVERY CODE TABLE IS DECLARED, in both directions: a declared table the
// schema lacks is a replay that fails on a table nobody created, and a schema
// table the domain does not classify is durable state outside the scrub list,
// the identity claim and the sweep alike.
func TestEveryCodeTableIsDeclared(t *testing.T) {
	t.Parallel()
	var present []string
	for name := range ddl(t) {
		if table, ok := strings.CutPrefix(name, "table:"); ok {
			present = append(present, table)
		}
	}
	declared := code.Domain{}.Tables()
	for table := range declared {
		if !slices.Contains(present, table) {
			t.Errorf("%s is declared by the domain and the schema does not have it", table)
		}
	}
	for _, table := range present {
		if _, ok := declared[table]; !ok {
			t.Errorf("%s is in the schema and the domain does not classify it", table)
		}
	}
}
