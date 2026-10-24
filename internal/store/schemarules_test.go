package store_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// THE REPLICATED ESTATE'S THREE SCHEMA RULES, checked rather than written down.
//
// `0005_the_pages_domain_lands.sql` states them in prose and nothing enforced
// them: NO FOREIGN KEY, NO `UNIQUE` OUTSIDE A PRIMARY KEY, NO `COLLATE`. Each
// is load-bearing in the same way and for the same reason — every one of them
// makes a statement inside an apply transaction abort DETERMINISTICALLY on
// every node at once, which turns what would be a rare cosmetic anomaly on one
// node into a fleet-wide stalled log with no partial-failure arm to recover
// through. A cascade is a delete nobody committed; a unique index outside the
// primary key wedges every applier on a row a restore could produce; and the
// pinned driver ACCEPTS `COLLATE NOCASE`, so nothing else in the build stops a
// migration from quietly collapsing 'A' and 'a' under a normalisation Go
// already did once.
//
// THE NODE ESTATE IS DELIBERATELY UNGOVERNED BY THESE. It is one file owned by
// one process with no applier and no peer, so a constraint there fails one
// statement for one caller, which is what a constraint is for. The rules are a
// property of rows N nodes must derive identically, not of SQL in general.

// A statement's text, once comments and string literals are gone. The scan
// runs over that rather than over the raw file, because every one of these
// files explains itself at length and every explanation names the thing it is
// explaining — the prose above this test would trip a naive grep three times.
var (
	lineComment  = regexp.MustCompile(`--[^\n]*`)
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	stringLit    = regexp.MustCompile(`'[^']*'`)

	foreignKey = regexp.MustCompile(`(?i)\bREFERENCES\b|\bFOREIGN\s+KEY\b`)
	collate    = regexp.MustCompile(`(?i)\bCOLLATE\b`)
	// UNIQUE that is not part of a PRIMARY KEY declaration. `PRIMARY KEY`
	// is what a table's own identity is stated with and carries no UNIQUE
	// keyword, so any UNIQUE at all in a replicated migration is one of the
	// two forms the rule refuses: a table constraint or a unique index.
	unique = regexp.MustCompile(`(?i)\bUNIQUE\b`)
)

func sqlCode(body string) string {
	body = blockComment.ReplaceAllString(body, " ")
	body = lineComment.ReplaceAllString(body, " ")
	return stringLit.ReplaceAllString(body, "''")
}

// TestNoReplicatedMigrationDeclaresAConstraintTheApplyCannotSurvive is the
// walk. It reads every replicated migration this build ships.
func TestNoReplicatedMigrationDeclaresAConstraintTheApplyCannotSurvive(t *testing.T) {
	t.Parallel()
	names := store.SchemaVersions(store.EstateReplicated)
	if len(names) == 0 {
		t.Fatal("the replicated estate ships no migrations, so this guard is checking nothing")
	}
	for _, name := range names {
		body, err := store.SchemaFile(store.EstateReplicated, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, fault := range replicatedFaults(string(body)) {
			t.Errorf("%s: %s", name, fault)
		}
	}
}

// replicatedFaults is the rule itself, separated from the walk so the controls
// can hand it SQL that is not in the tree.
func replicatedFaults(body string) []string {
	code := sqlCode(body)
	var faults []string
	if foreignKey.MatchString(code) {
		faults = append(faults, "declares a foreign key. A cascade is a delete nobody "+
			"committed: an applier removes a row's children in its own statement list, "+
			"where the deletion is part of the record's effect and therefore identical "+
			"on every node")
	}
	if unique.MatchString(code) {
		faults = append(faults, "declares UNIQUE outside its primary key. A constraint "+
			"violation inside the apply transaction aborts it deterministically on every "+
			"node, which turns a rare cosmetic anomaly into a fleet-wide stalled log; "+
			"uniqueness a writer must see whole is arbitrated on the record's subject")
	}
	if collate.MatchString(code) {
		faults = append(faults, "declares COLLATE. The default TEXT collation is BINARY, "+
			"so an ORDER BY reproduces the Go comparison exactly; the pinned driver "+
			"accepts COLLATE NOCASE, which would silently collapse 'A' and 'a' against "+
			"a normalisation Go already did once")
	}
	return faults
}

// TestTheReplicatedSchemaRulesCatchWhatTheyName is the control, one per rule.
// A guard that cannot go red is a claim, not a check.
func TestTheReplicatedSchemaRulesCatchWhatTheyName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rule string
		sql  string
		want string
	}{
		{
			rule: "foreign key",
			sql:  "CREATE TABLE a (id TEXT PRIMARY KEY, b TEXT REFERENCES other(id));",
			want: "foreign key",
		},
		{
			rule: "foreign key, table constraint form",
			sql:  "CREATE TABLE a (id TEXT PRIMARY KEY, b TEXT, FOREIGN KEY (b) REFERENCES other(id));",
			want: "foreign key",
		},
		{
			rule: "unique index",
			sql:  "CREATE UNIQUE INDEX a_b_idx ON a (b);",
			want: "UNIQUE",
		},
		{
			rule: "unique table constraint",
			sql:  "CREATE TABLE a (id TEXT PRIMARY KEY, b TEXT UNIQUE);",
			want: "UNIQUE",
		},
		{
			rule: "collate",
			sql:  "CREATE TABLE a (id TEXT PRIMARY KEY, b TEXT COLLATE NOCASE);",
			want: "COLLATE",
		},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			faults := replicatedFaults(tc.sql)
			if len(faults) == 0 {
				t.Fatalf("the guard accepted a migration that %s", tc.rule)
			}
			if !strings.Contains(strings.Join(faults, " "), tc.want) {
				t.Errorf("the refusal does not name the rule: %v", faults)
			}
		})
	}
}

// TestTheReplicatedSchemaRulesReadPastProse is the other half of the control:
// every one of these files explains the rules at length, naming them, so a
// guard that read the raw bytes would refuse the very migration that states
// them.
func TestTheReplicatedSchemaRulesReadPastProse(t *testing.T) {
	t.Parallel()
	prose := `-- NO FOREIGN KEY ANYWHERE, and NO UNIQUE OUTSIDE A PRIMARY KEY.
-- NO COLLATE ANYWHERE: the driver accepts COLLATE NOCASE (measured).
/* REFERENCES, UNIQUE and COLLATE again, in a block comment. */
CREATE TABLE a (id TEXT PRIMARY KEY, note TEXT NOT NULL DEFAULT 'see UNIQUE and COLLATE');`
	if faults := replicatedFaults(prose); len(faults) != 0 {
		t.Fatalf("the guard refused a migration that only names the rules in prose and a "+
			"string literal: %v", faults)
	}
}
