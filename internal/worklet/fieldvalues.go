package worklet

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
// can reach. [Task.Fields] stays the record (it survives an unseen declaration
// and a rename never re-points a value); `tracker_field_values` exists so the
// `f.<slug>` filters have something to seek.
//
// A value goes in the column its field's DECLARED type names, never the JSON's:
// reading the JSON would put `"3"` in `text` and `3` in `num` for one field.
// Rows are written from the declarations in force when the TASK applies, and a
// later declaration does not backfill: that would walk every task inside the
// catalogue edit's apply. The value was unfilterable until declared anyway, and
// gains its row on the task's next write. An archived field's rows stay with
// `hidden = 1` ([FieldDef.Archived]); every index is partial on `hidden = 0`.

// The value KINDS: a value's PROVENANCE, not its field's type — produced by
// this engine's write path, or mirrored in from a tracker run beside it. Per
// the `tracker_field_values` DDL, `hidden` (the archived declaration) and
// `kind` are the two non-live states. A FOREIGN value is stored for a renderer
// and kept out of every predicate: the native grammar's operators are defined
// against the native catalogue's declared types, not a foreign system's.
const (
	FieldValueNative  = "native"
	FieldValueForeign = "foreign"
)

// liveFieldValue is the predicate every filter, aggregate and group adds,
// spelled once so the three cannot drift apart.
const liveFieldValue = `v.hidden = 0 AND v.kind <> '` + FieldValueForeign + `'`

// MaxFieldValueSeq bounds how many entries one multi-valued field contributes:
// each member of a labels or people set is its own row (so `f.<slug>=<one>` is
// an index seek), and a set cannot honestly exceed its declaration's option
// list, [MaxOptions].
const MaxFieldValueSeq = MaxOptions

// explodeFieldValues rewrites the rows one task's field values produce; the
// delete is the caller's ([Applier.explodeTask]), so a cleared field leaves no
// row.
//
// The declarations are handed in because [TaskDeltas] reads them too, and two
// reads in one transaction can drift. [Applier.applyTask] reads them whenever
// a side carries values, so a non-empty `task.Fields` never meets a nil map.
func (a *Applier) explodeFieldValues(ctx context.Context, tx *sql.Tx,
	task Task, declared map[string]FieldDef, c applyContext) (int, error) {

	if len(task.Fields) == 0 {
		return 0, nil
	}
	written := 0
	// Sorted, so every node writes the same rows in the same order and a
	// multi-value's `seq` is the record's, not a map iteration's.
	for _, id := range sortedRawKeys(task.Fields) {
		field, held := declared[id]
		if !held {
			// AN UNDECLARED FIELD'S VALUE IS FOREIGN: with no type there
			// is no column, so its row holds the raw JSON, hidden and
			// kinded `foreign` (out of [liveFieldValue]). It still gets a
			// row because a cross-project move orphans values, and a task
			// holding them must not read as having lost them. A later
			// declaration re-explodes it as native on the next write.
			rows, err := writeForeignValue(ctx, tx, task.ID, id, task.Fields[id])
			if err != nil {
				return 0, err
			}
			written += rows
			continue
		}
		rows, err := writeFieldValue(ctx, tx, task.ID, field,
			task.Fields[id], task.Type, c.maxVariables)
		if err != nil {
			return 0, err
		}
		written += rows
	}
	return written, nil
}

// appliesTo reports whether a declaration covers a task of this type. An
// empty list is EVERY type: reading it as none would hide every value of every
// field nobody restricted.
func appliesTo(field FieldDef, taskType string) bool {
	if len(field.AppliesTo) == 0 {
		return true
	}
	for _, name := range field.AppliesTo {
		if strings.EqualFold(name, taskType) {
			return true
		}
	}
	return false
}

// writeForeignValue records one value whose field this project does not
// declare, as ONE row whatever it is: with no type there is nothing to explode
// by. [MaxFieldValueBytes] already bounds it, so nothing is cut here.
func writeForeignValue(ctx context.Context, tx *sql.Tx, taskID, fieldID string,
	raw json.RawMessage) (int, error) {

	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_field_values
			(task_id, field_id, seq, kind, hidden, num, text, at, ref)
		VALUES (?,?,0,?,1,NULL,?,NULL,NULL)`,
		taskID, fieldID, FieldValueForeign, string(raw))
	if err != nil {
		return 0, fmt.Errorf("worklet: write %s's foreign value for field %s: %w",
			taskID, fieldID, err)
	}
	return affected(res)
}

// declaredFields is every field a task's project can carry, keyed by id: the
// workspace's plus the project's, at most twice [MaxFieldsPerDocument]. A
// project declaration of the same id SHADOWS the workspace one: that is a
// field mid-move between them, and the project is the nearer scope.
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
	field FieldDef, raw json.RawMessage, taskType string,
	maxVariables int) (int, error) {

	values, err := fieldRows(field, raw)
	if err != nil {
		// A VALUE THAT DOES NOT DECODE IS NOT AN APPLY FAILURE: the
		// document keeps it, every node agrees, and refusing the record
		// would let one malformed value block the task's every later
		// change fleet-wide.
		//nolint:nilerr // Deliberate: see the paragraph above — the
		// applier salvages, because refusing here would wedge the task.
		return 0, nil
	}
	if len(values) > MaxFieldValueSeq {
		values = values[:MaxFieldValueSeq]
	}
	// HIDDEN IS TWO FACTS: an archived field, and a field whose AppliesTo
	// excludes this task's type (what a re-typed task leaves behind), else
	// a value of a field that does not apply is filtered and totalled.
	hidden := field.Archived || !appliesTo(field, taskType)
	// ONE STATEMENT PER FIELD, not per value (a set is up to
	// [MaxFieldValueSeq] members) nor per task: `seq` is the member's index
	// within this field's set, and the error names the field.
	// [store.InsertRows] directly, not [insertMany], whose builder gets the
	// item rather than the index `seq` needs.
	written, err := store.InsertRows(ctx, tx, maxVariables, `
		INSERT INTO tracker_field_values
			(task_id, field_id, seq, kind, hidden, num, text, at, ref)
		VALUES`,
		`(?,?,?,?,?,?,?,?,?)`, "",
		len(values), func(seq int) []any {
			value := values[seq]
			return []any{taskID, field.ID, seq, FieldValueNative, boolInt(hidden),
				value.Num, value.Text, value.At, value.Ref}
		})
	if err != nil {
		return 0, fmt.Errorf("worklet: write %s's value for field %s: %w",
			taskID, field.Slug, err)
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
// It decides both that a board grouped on it puts a task in EVERY column it
// chose and that a sort on it picks exactly ONE value (or the join multiplies
// tasks); named once so the two agree on which fields those are.
func MultiValued(t FieldType) bool {
	switch t {
	case FieldLabels, FieldPeople, FieldRelationship:
		return true
	}
	return false
}

// FieldValueColumn is the column a field type's values are filtered on, named
// once because the apply writes it, the query reads it, and a disagreement is
// a filter that silently matches nothing.
func FieldValueColumn(t FieldType) string {
	switch t {
	case FieldNumber, FieldProgress, FieldCheckbox, FieldRollup:
		return "num"
	case FieldDate:
		return "at"
	case FieldDropdown, FieldLabels, FieldRelationship, FieldPeople:
		// The option's ID, not its label, so a renamed option keeps its
		// tasks; the query resolves a typed slug to the id first.
		return "ref"
	}
	return "text"
}

// fieldValueIndex is the partial index a filter on a field type's values
// seeks: `(field_id, <its value column>) WHERE hidden = 0`, one per column,
// declared by replicated migration 0002. Derived from [FieldValueColumn] so
// the compared column and the named index cannot disagree: another column's
// index would seek `field_id` alone.
func fieldValueIndex(t FieldType) string {
	return "tracker_field_values_" + FieldValueColumn(t) + "_idx"
}

// fieldValuesKey is the value table's primary key, `(task_id, field_id, seq)`,
// as this engine names it: the index every read of ONE TASK'S values of a
// field names (a sort's join, a grouping's join and column filter, a total's
// read of its answer's values). The [fieldValueIndex] indexes lead with
// `field_id` and are partial on `hidden = 0`, which these reads also state, so
// with no statistics (nothing runs ANALYZE) the planner picks one and reads
// the field's every value once per task. Named, each read is a seek on the
// task and the field. A FILTER asks which tasks hold a value, so it is driven
// from the field's column index instead ([fieldClause]).
const fieldValuesKey = "sqlite_autoindex_tracker_field_values_1"

// fieldRows turns one declared field's JSON value into its rows: one per
// member of a multi-valued field, so `f.<slug>=<one>` is an index seek.
func fieldRows(field FieldDef, raw json.RawMessage) ([]fieldValueRow, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	column := FieldValueColumn(field.Type)
	var members []json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		// Not a list is one member: a single-valued field, or a
		// multi-valued one set to exactly one.
		members = []json.RawMessage{raw}
	}
	// OPTION SPELLINGS RESOLVE HERE too: a stored ref holds the option's ID,
	// so a value written as its slug or name must become that id or every
	// filter, grouping and total misses it. [OptionIDs] is the rule both
	// sides read. A field with no option list (relationship, people)
	// resolves nothing: its ref is a task id or a handle.
	var options map[string]string
	if column == "ref" {
		options = OptionIDs(field.Config.Options)
	}
	out := make([]fieldValueRow, 0, len(members))
	for _, member := range members {
		row, ok, err := fieldRow(column, field.Type, member)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if text, held := row.Ref.(string); held && len(options) > 0 {
			if id, known := options[strings.ToLower(text)]; known {
				row.Ref = id
			}
			// AN UNKNOWN WORD IS STORED AS WRITTEN, as the query passes
			// one through: refusing would drop a value the document
			// keeps, and both sides holding the raw text still match.
		}
		out = append(out, row)
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
			return row, false, fmt.Errorf("worklet: %s is not a number", raw)
		}
		row.Num = number
	case "at":
		at, ok := decodeInstant(raw)
		if !ok {
			return row, false, fmt.Errorf("worklet: %s is not an instant", raw)
		}
		row.At = store.EncodeTime(at)
	default:
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			// A NON-STRING RENDERS RATHER THAN FAILING: a dropdown
			// option set as a number means the option whose id reads
			// that way; refusing would keep it out of every filter.
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
		// A CHECKBOX IS A NUMBER: its index is the numeric one, and
		// `checked = true` and `checked = 1` are one fact.
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
