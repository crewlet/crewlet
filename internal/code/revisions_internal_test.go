package code

import (
	"strings"
	"testing"
)

// THE QUALIFIED CHANGE COLUMNS ARE THE CHANGE COLUMNS: a push's reads of the
// live changes it moves name every column of a change's row qualified — the
// table function they are driven from has an id of its own — and scanChange
// reads each answer in changeColumns' order, so the two lists name the same
// columns in the same order, or a push's book would read one column into
// another's field on every data node alike, and no other test would see a
// change whose title is its state.
//
// Mutation: swap two columns of qualifiedChangeColumns, or drop one, and this
// fails naming where.
func TestTheQualifiedChangeColumnsAreTheChangeColumns(t *testing.T) {
	t.Parallel()
	columns := func(list string) []string {
		var out []string
		for _, column := range strings.Split(list, ",") {
			out = append(out, strings.TrimSpace(column))
		}
		return out
	}
	plain, qualified := columns(changeColumns), columns(qualifiedChangeColumns)
	if len(plain) != len(qualified) {
		t.Fatalf("changeColumns names %d columns and qualifiedChangeColumns %d", len(plain), len(qualified))
	}
	for i, column := range plain {
		if qualified[i] != "c."+column {
			t.Errorf("column %d is %s in changeColumns and %s in qualifiedChangeColumns, want c.%s", i+1,
				column, qualified[i], column)
		}
	}
}
