package gitlet_test

import (
	"database/sql"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// spendWords are the words, in the estate's own spelling, of a column counting
// what a turn spent (the tracker's `spend_*`, the usage history's tokens and
// cost, a budget's window): a column is spend if any of its `_`-separated
// words is one.
var spendWords = []string{"spend", "spent", "tokens", "cost", "usd", "cents", "price", "budget", "credits"}

// A CHANGE CARRIES NO SPEND (ADR-0097): no table the code domain declares
// ([gitlet.Domain.Tables]) has a column counting what a turn spent. The turn
// is charged to the work item the change implements (ADR-0022's trigger); a
// sum on the change too would be a second total that disagrees once a turn
// touches two changes of one item. Read from the MIGRATED schema, so a
// migration adding such a column fails here whatever its table is named.
//
// Mutation: add `spend_tokens` to code_changes in a migration, or treat
// `token` (code_names' claim) as a spend word, and the case fails.
func TestChangesHaveNoSpendColumns(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	var tables []string
	for table := range (gitlet.Domain{}).Tables() {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	if !slices.Contains(tables, "code_changes") || !slices.Contains(tables, "code_reviews") {
		t.Fatalf("the domain declares %v, without a change's own rows — this case reads nothing", tables)
	}
	read := 0
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		for _, table := range tables {
			rows, err := tx.QueryContext(t.Context(), `SELECT name FROM pragma_table_info(?)`, table)
			if err != nil {
				return err
			}
			columns := 0
			for rows.Next() {
				var column string
				if err := rows.Scan(&column); err != nil {
					_ = rows.Close()
					return err
				}
				columns++
				for _, word := range strings.Split(column, "_") {
					if slices.Contains(spendWords, word) {
						t.Errorf("%s.%s counts what a turn spent; a change carries none — the turn is "+
							"charged to the work item the change implements (ADR-0097)", table, column)
					}
				}
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if columns == 0 {
				t.Errorf("the domain declares %s and the migrated estate has no such table", table)
			}
			read += columns
		}
		return nil
	}); err != nil {
		t.Fatalf("read the code domain's schema: %v", err)
	}
	if read == 0 {
		t.Fatal("no column was read, so no column was judged")
	}
}
