package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// A SETTLE MOVES EVERY ROW TO THE SIDE ITS DECLARATION SAYS, AND NO OTHER ROW.
//
// [settleStatements] writes each side of `hidden` through the index that holds
// that side, so it reads only what can move — and this is what holds the split
// to the answer the single statement it replaced gave: through each shape of
// declaration in turn, every native row of the field ends hidden exactly when
// the field is archived, or applies to types its task's is not (case-folded),
// or has no task at all; another field's rows and a foreign row are left as
// they were. Hundreds of rows, so a side whose writes moved rows out of the
// index it was walking would be seen leaving some behind.
func TestASettleMovesEveryRowToTheSideItsDeclarationSays(t *testing.T) {
	t.Parallel()
	const tasks = 600
	db := seededPlanStore(t, func(ctx context.Context, tx *sql.Tx, maxVariables int) error {
		if err := insertAll(ctx, tx, maxVariables, insertTask, taskValues,
			tasks, taskRow); err != nil {
			return err
		}
		for _, field := range []struct{ id, column string }{
			{"field-effort", "num"}, {"field-owner", "text"},
		} {
			if err := insertAll(ctx, tx, maxVariables, `INSERT INTO
				tracker_field_values (task_id, field_id, seq, kind, hidden, `+
				field.column+`) VALUES`, `(?,?,0,?,0,?)`, tasks, func(i int) []any {
				return []any{fmt.Sprintf("t-%04d", i), field.id, FieldValueNative, i}
			}); err != nil {
				return err
			}
		}
		// A FOREIGN VALUE OF THE FIELD, and values whose task is gone.
		if _, err := tx.ExecContext(ctx, `INSERT INTO tracker_field_values
			(task_id, field_id, seq, kind, hidden, num) VALUES
			('t-0001', 'field-effort', 1, ?, 0, 1),
			('t-gone-1', 'field-effort', 0, ?, 0, 1),
			('t-gone-2', 'field-effort', 0, ?, 1, 1)`,
			FieldValueForeign, FieldValueNative, FieldValueNative); err != nil {
			return err
		}
		return nil
	})
	// THE ANSWER, computed by the definition rather than by the settle.
	want := func(field FieldDef) string {
		hide := "0"
		switch {
		case field.Archived:
			hide = "1"
		case len(field.AppliesTo) > 0:
			list := ""
			for i, name := range field.AppliesTo {
				if i > 0 {
					list += ","
				}
				list += "LOWER('" + name + "')"
			}
			hide = "COALESCE((SELECT LOWER(t.type) NOT IN (" + list + ") " +
				"FROM tracker_tasks t WHERE t.id = v.task_id), 1)"
		}
		return hide
	}
	count := func(t *testing.T, query string) int {
		t.Helper()
		var n int
		if err := db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), query).Scan(&n)
		}); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	for _, field := range []FieldDef{
		{ID: "field-effort", Slug: "effort", Type: FieldNumber, Archived: true},
		{ID: "field-effort", Slug: "effort", Type: FieldNumber},
		{ID: "field-effort", Slug: "effort", Type: FieldNumber, AppliesTo: []string{"Bug"}},
		{ID: "field-effort", Slug: "effort", Type: FieldNumber,
			AppliesTo: []string{"bug", "EPIC"}},
		{ID: "field-effort", Slug: "effort", Type: FieldNumber, AppliesTo: []string{"task"}},
		{ID: "field-effort", Slug: "effort", Type: FieldNumber},
	} {
		name := fmt.Sprintf("archived=%t applies_to=%v", field.Archived, field.AppliesTo)
		if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := (&Applier{}).settleFieldVisibility(t.Context(), tx, []FieldDef{field})
			return err
		}); err != nil {
			t.Fatalf("%s: settle: %v", name, err)
		}
		if wrong := count(t, `SELECT COUNT(*) FROM tracker_field_values v
			WHERE v.field_id = 'field-effort' AND v.kind = 'native'
			  AND v.hidden <> `+want(field)); wrong != 0 {
			t.Errorf("%s: %d of the field's rows are on the wrong side of hidden",
				name, wrong)
		}
		if hidden := count(t, `SELECT COUNT(*) FROM tracker_field_values
			WHERE field_id = 'field-owner' AND hidden <> 0`); hidden != 0 {
			t.Errorf("%s: the settle of one field moved %d rows of another",
				name, hidden)
		}
		if foreign := count(t, `SELECT hidden FROM tracker_field_values
			WHERE kind = 'foreign'`); foreign != 0 {
			t.Errorf("%s: the settle moved a foreign row, which is hidden by "+
				"what it is rather than by any declaration", name)
		}
	}
}
