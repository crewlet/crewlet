package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// Custom-field VALUES: how a value on a task's document becomes a row a filter
// can reach.
//
// # Why the document is not enough
//
// A task carries its field values as [Task.Fields] — a map keyed by field ID,
// opaque JSON per entry — which is the right shape for a record: it survives a
// declaration this build has never seen, and a rename never re-points a stored
// value. What it cannot do is be FILTERED. `tracker_field_values` exists for
// that, with four partial indexes each named in the DDL for a different
// column, and until this the applier wrote no row into it: every `f.<slug>`
// filter in the grammar had an index, a plan-test entry, and no data.
//
// # Why the column depends on the DECLARATION and not on the JSON
//
// A value is stored under the column its field's TYPE says — a number in
// `num`, an instant in `at`, a choice's id in `ref`, prose in `text` — because
// that is the column its index is on. Reading the type off the JSON instead
// would put `"3"` in `text` and `3` in `num` for one field, and a filter on
// either would silently miss half the corpus. So the apply reads the
// declaration, and a value whose field is not declared ANYWHERE is kept on the
// document and written to no row: it is a value for a field nobody can filter
// on yet, which is exactly what it is.
//
// # A DECLARATION THAT ARRIVES LATER DOES NOT BACKFILL
//
// The rows are written when the TASK is applied, from the declarations in
// force at that instant — so a field declared after a task was last written
// has no value row for it, even though the task's document carries one. That
// is deliberate rather than a gap to sweep: backfilling would mean walking
// every task in the company on every catalogue edit, inside the apply
// transaction that edit commits in, and a company declaring a field would
// stall its own fleet for as long as that walk took.
//
// What makes it harmless is that a value for a field nobody had declared was
// never filterable in the first place: the filter could not resolve the ref.
// The row appears on the task's next write, which is the first moment the
// value means anything to anybody.
//
// # And why `hidden` is a column rather than a delete
//
// An ARCHIVED field's values leave the filterable set and stay on the
// document, which is what makes the archive one-way rather than destructive —
// see [FieldDef.Archived]. `hidden = 1` is that state, and every index is
// partial on `hidden = 0`.

// The value KINDS, which are not the field's type.
//
// The DDL's own head comment on `tracker_field_values` says what this column
// is for: "`hidden` and `kind` carry the two non-live states: every filter and
// total adds `hidden = 0 AND kind <> 'foreign'`". So `hidden` is the archived
// DECLARATION and `kind` is the value's PROVENANCE — a value this engine's
// write path produced, or one mirrored in from a tracker a company runs
// beside it.
//
// A FOREIGN VALUE IS STORED AND NOT FILTERED ON. It is there so a renderer can
// show what the other system holds, and it is out of every predicate because
// the native grammar's operators are defined against the native catalogue's
// declared types — comparing them against a foreign system's own would answer
// a question nobody asked with a number nobody can check.
const (
	FieldValueNative  = "native"
	FieldValueForeign = "foreign"
)

// liveFieldValue is the predicate every filter and every total adds.
//
// ONE STRING, because the DDL states the rule once and three call sites read
// it — the filter, the aggregate and the group — and a fourth spelling is how
// one of them stops matching the other three.
const liveFieldValue = `v.hidden = 0 AND v.kind <> '` + FieldValueForeign + `'`

// MaxFieldValueSeq bounds how many entries one multi-valued field contributes.
//
// A LABELS OR PEOPLE FIELD IS A SET, and each member is its own row so an
// `f.<slug>=<one>` filter is an index seek rather than a JSON scan. The cap is
// [MaxOptions]'s, because the set a value can draw from is the declaration's
// own option list and nothing can honestly exceed it.
const MaxFieldValueSeq = MaxOptions

// explodeFieldValues rewrites the rows one task's field values produce.
//
// The delete is the caller's, exactly as it is for every other collection —
// see [Applier.explodeTask] — so a record that cleared a field leaves no row
// behind.
func (a *Applier) explodeFieldValues(ctx context.Context, tx *sql.Tx,
	task Task) (int, error) {

	if len(task.Fields) == 0 {
		return 0, nil
	}
	declared, err := declaredFields(ctx, tx, task.Project)
	if err != nil {
		return 0, err
	}
	written := 0
	// SORTED BY FIELD ID, so two nodes applying one record write the same
	// rows in the same order — which is what makes the `seq` a multi-value
	// takes a property of the RECORD rather than of a map iteration.
	for _, id := range sortedRawKeys(task.Fields) {
		field, held := declared[id]
		if !held {
			// A VALUE FOR A FIELD NOBODY DECLARES stays on the
			// document and reaches no row. It is not dropped — a
			// declaration that arrives later re-explodes it on the
			// task's next write — and it is not guessed at either:
			// with no declared type there is no column to put it in.
			continue
		}
		rows, err := writeFieldValue(ctx, tx, task.ID, field, task.Fields[id])
		if err != nil {
			return 0, err
		}
		written += rows
	}
	return written, nil
}

// declaredFields is every field a task's project can carry, keyed by id.
//
// THE WORKSPACE'S PLUS THE PROJECT'S, which is the union [MaxFieldsPerDocument]
// bounds at twice one document by construction. A project declaration of the
// same id SHADOWS the workspace one, because that is what a field in the middle
// of a move between them looks like and the project is the nearer scope.
func declaredFields(ctx context.Context, tx *sql.Tx, project string) (
	map[string]FieldDef, error) {

	out := map[string]FieldDef{}
	catalogue, _, err := readFieldCatalogue(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, field := range catalogue.Fields {
		out[field.ID] = field
	}
	if project == "" {
		return out, nil
	}
	declaring, held, err := readProject(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	if held {
		for _, field := range declaring.Fields {
			out[field.ID] = field
		}
	}
	return out, nil
}

// writeFieldValue writes one field's value as the rows its type calls for.
func writeFieldValue(ctx context.Context, tx *sql.Tx, taskID string,
	field FieldDef, raw json.RawMessage) (int, error) {

	values, err := fieldRows(field, raw)
	if err != nil {
		// A VALUE THAT DOES NOT DECODE IS NOT AN APPLY FAILURE. The
		// document keeps it, every node reaches the same conclusion, and
		// the alternative — refusing the record — would let one
		// malformed field value stop a task's every later change on
		// every node in the fleet.
		return 0, nil
	}
	if len(values) > MaxFieldValueSeq {
		values = values[:MaxFieldValueSeq]
	}
	written := 0
	for seq, value := range values {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_field_values
				(task_id, field_id, seq, kind, hidden, num, text, at, ref)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			taskID, field.ID, seq, FieldValueNative, boolInt(field.Archived),
			value.Num, value.Text, value.At, value.Ref)
		if err != nil {
			return 0, fmt.Errorf("tracker: write %s's value for field %s: %w",
				taskID, field.Slug, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// fieldValueRow is one row's worth of a value, with exactly one column set.
type fieldValueRow struct {
	Num  any
	Text any
	At   any
	Ref  any
}

// MultiValued reports whether one task may hold several values of this field.
//
// IT DECIDES TWO THINGS and they pull in opposite directions: a board grouped
// on such a field puts a task on EVERY column it chose (a tag board), and a
// sort on one has to pick exactly ONE value or the join multiplies every task
// by its own value count. Naming it once is what keeps the two agreeing about
// which fields those are.
func MultiValued(t FieldType) bool {
	switch t {
	case FieldLabels, FieldPeople, FieldRelationship:
		return true
	}
	return false
}

// FieldValueColumn is the column a field type's values are filtered on.
//
// ONE PLACE, because the apply writes it and the query reads it and a
// disagreement between the two is a filter that silently matches nothing.
func FieldValueColumn(t FieldType) string {
	switch t {
	case FieldNumber, FieldProgress, FieldCheckbox, FieldRollup:
		return "num"
	case FieldDate:
		return "at"
	case FieldDropdown, FieldLabels, FieldRelationship, FieldPeople:
		// THE ID, NOT THE LABEL: a stored value holds the option's id,
		// so a renamed option keeps every task that chose it. The query
		// resolves a slug a caller typed to that id before it compares.
		return "ref"
	}
	return "text"
}

// fieldRows turns one declared field's JSON value into its rows.
//
// A MULTI-VALUED FIELD IS N ROWS, one per member, because that is what makes
// `f.<slug>=<one>` an index seek rather than a scan over every task's JSON.
func fieldRows(field FieldDef, raw json.RawMessage) ([]fieldValueRow, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	column := FieldValueColumn(field.Type)
	var members []json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		// NOT A LIST IS ONE MEMBER, which is what a single-valued field
		// carries and what a multi-valued one carries when somebody set
		// exactly one.
		members = []json.RawMessage{raw}
	}
	out := make([]fieldValueRow, 0, len(members))
	for _, member := range members {
		row, ok, err := fieldRow(column, field.Type, member)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// fieldRow places one member in its column.
func fieldRow(column string, kind FieldType, raw json.RawMessage) (
	fieldValueRow, bool, error) {

	var row fieldValueRow
	switch column {
	case "num":
		number, ok := decodeNumber(kind, raw)
		if !ok {
			return row, false, fmt.Errorf("tracker: %s is not a number", raw)
		}
		row.Num = number
	case "at":
		at, ok := decodeInstant(raw)
		if !ok {
			return row, false, fmt.Errorf("tracker: %s is not an instant", raw)
		}
		row.At = store.EncodeTime(at)
	default:
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			// A NON-STRING RENDERS RATHER THAN FAILING: a caller that
			// set a dropdown's option as a number means the option
			// whose id reads that way, and refusing it would leave
			// the value on the document and out of every filter.
			text = strings.Trim(string(raw), `"`)
		}
		if text = strings.TrimSpace(text); text == "" {
			return row, false, nil
		}
		if column == "ref" {
			row.Ref = text
			return row, true, nil
		}
		row.Text = text
	}
	return row, true, nil
}

// decodeNumber reads a numeric member, taking a checkbox's bool as 0 or 1.
func decodeNumber(kind FieldType, raw json.RawMessage) (float64, bool) {
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, true
	}
	var flag bool
	if err := json.Unmarshal(raw, &flag); err == nil {
		// A CHECKBOX IS A NUMBER IN THE STORE, because its index is the
		// numeric one and `checked = true` and `checked = 1` are the
		// same fact written two ways.
		if flag {
			return 1, true
		}
		return 0, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if number, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
			return number, true
		}
	}
	_ = kind
	return 0, false
}

// decodeInstant reads a date member as RFC3339 or as epoch microseconds.
func decodeInstant(raw json.RawMessage) (time.Time, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if at, err := time.Parse(layout, strings.TrimSpace(text)); err == nil {
				return at.UTC(), true
			}
		}
		return time.Time{}, false
	}
	var micros int64
	if err := json.Unmarshal(raw, &micros); err == nil {
		return store.DecodeTime(micros), true
	}
	return time.Time{}, false
}

// sortedRawKeys is a raw-message map's keys in a stable order.
func sortedRawKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}
