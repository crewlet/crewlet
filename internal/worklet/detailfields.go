package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
)

// A task's custom fields, as a READER sees them. [Task.Fields] is keyed by
// field ID with opaque JSON, right for the record (it survives an unknown
// declaration and a rename; see fieldvalues.go) but not for an answer: a model
// handed `{"fld_01H…": 3.5}` would need a second call to learn what it is.
//
// A reader is also told the states every filter excludes: `hidden` (the
// declaration was archived) and `foreign` (mirrored from another tracker), so
// an archived value is not shown as live; and undeclared (a field declared
// nowhere, kept on the document with no row), so the answer agrees with the
// record.

// FieldValue is one custom-field value with the declaration that explains it.
type FieldValue struct {
	// Slug and Name are empty on an UNDECLARED value, whose only name is ID.
	Slug string `json:"slug,omitempty"`
	Name string `json:"name,omitempty"`

	// ID is the key the value is stored under, so a caller can write back
	// exactly what it read, an undeclared value included.
	ID string `json:"id"`

	Type  FieldType       `json:"type,omitempty"`
	Value json.RawMessage `json:"value" untrusted:"name"`

	// Hidden is a value whose declaration was ARCHIVED: kept on the task,
	// out of every filter (see [FieldDef.Archived]).
	Hidden bool `json:"hidden,omitempty"`

	// Foreign is a value mirrored in from another tracker: shown, but outside
	// every predicate, whose operators are defined by THIS catalogue's types.
	Foreign bool `json:"foreign,omitempty"`

	// Undeclared is a value whose field this company declares nowhere: kept
	// on the document with no row (fieldvalues.go), and not an error.
	Undeclared bool `json:"undeclared,omitempty"`

	// Applies is false when the field is declared but not for THIS task's
	// type (left behind by a type change): still shown and filtered, no
	// longer asked for on an edit.
	Applies bool `json:"applies"`
}

// readFieldValues annotates a task's document against the declarations in
// force. It reads the document, not `tracker_field_values`: the rows are a
// filtering projection with no row for an undeclared value, which an answer
// built from them would drop.
func readFieldValues(ctx context.Context, tx *sql.Tx, task Task) ([]FieldValue, error) {
	if len(task.Fields) == 0 {
		return nil, nil
	}
	declared, err := declaredFields(ctx, tx, task.Project)
	if err != nil {
		return nil, err
	}
	foreign, err := foreignFieldIDs(ctx, tx, task.ID)
	if err != nil {
		return nil, err
	}
	out := make([]FieldValue, 0, len(task.Fields))
	for _, id := range sortedRawKeys(task.Fields) {
		value := FieldValue{ID: id, Value: task.Fields[id], Foreign: foreign[id]}
		field, held := declared[id]
		if !held {
			value.Undeclared = true
			out = append(out, value)
			continue
		}
		value.Slug, value.Name, value.Type = field.Slug, field.Name, field.Type
		value.Hidden = field.Archived
		value.Applies = appliesToType(field, task.Type)
		out = append(out, value)
	}
	return out, nil
}

// appliesToType is the write path's required-check rule: an empty AppliesTo
// is every type.
func appliesToType(field FieldDef, taskType string) bool {
	if len(field.AppliesTo) == 0 {
		return true
	}
	return slices.ContainsFunc(field.AppliesTo, func(name string) bool {
		return NormName(name) == NormName(taskType)
	})
}

// foreignFieldsStatement is [foreignFieldIDs]' read, named for the plan test.
const foreignFieldsStatement = `
		SELECT DISTINCT field_id FROM tracker_field_values
		 WHERE task_id = ? AND kind = ?`

// foreignFieldIDs is which of a task's values were mirrored in from another
// tracker. It reads the rows: provenance is the applier's conclusion, recorded
// only in `kind`; on the document a foreign value looks native.
func foreignFieldIDs(ctx context.Context, tx *sql.Tx, taskID string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, foreignFieldsStatement, taskID, FieldValueForeign)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
