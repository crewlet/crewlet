package code_test

import (
	"database/sql"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// spendWords are the words a column that counts what a turn spent is named
// by, in the estate's own spelling: the tracker's task row counts `spend_*`
// (its tokens among them), the usage history counts tokens and their cost,
// and a budget is the window those counts are held to. A column is named of
// words joined by `_`, and one of these among them is a column of spend.
var spendWords = []string{"spend", "spent", "tokens", "cost", "usd", "cents", "price", "budget", "credits"}

// A CHANGE CARRIES NO SPEND (ADR-0090): no table of the code domain — a
// change's rows, its conversation's, every repository row beside them — has a
// column that counts what a turn spent. A turn woken about a change is charged
// to the work item the change implements (ADR-0022's trigger), and a sum kept
// on the change as well would be a second total of one turn, disagreeing with
// the item's the moment a turn touches two changes of one item.
//
// Read from the MIGRATED estate's own schema, every table the domain declares
// ([code.Domain.Tables]), so a migration that adds such a column fails here
// however it names its table.
//
// Mutation: add `spend_tokens` to code_changes in a migration, or treat
// `token` (code_names' claim) as a spend word, and the case fails.
func TestChangesHaveNoSpendColumns(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	var tables []string
	for table := range (code.Domain{}).Tables() {
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
							"charged to the work item the change implements (ADR-0090)", table, column)
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
