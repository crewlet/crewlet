package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
)

// A task's custom fields, as a READER sees them.
//
// # Why the document alone is not an answer
//
// [Task.Fields] is a map keyed by field ID with opaque JSON per entry, which
// is the right shape for a RECORD — it survives a declaration this build has
// never seen, and a rename never re-points a stored value (see fieldvalues.go
// for the same argument on the write side). It is the wrong shape for an
// answer: a reader handed `{"fld_01H…": 3.5}` cannot tell what the field is,
// what it means, or whether it still applies, and the model on the other end
// of a tool call has to spend a second call on the catalogue to find out —
// while the WRITE side accepts an id, a slug or a name interchangeably. The
// engine was asking for words and answering with identifiers.
//
// # The two non-live states, which only a reader can be told about
//
// The applier already distinguishes them in `tracker_field_values` and every
// filter excludes both — `hidden` for a value whose DECLARATION was archived,
// `foreign` for one mirrored in from a tracker the company runs beside this
// one. A reader that saw neither would show an archived field's value beside
// a live one and let somebody act on a rule nobody has used for a year.
//
// A THIRD state has no row at all: a value whose field is declared NOWHERE.
// It stays on the document by design and is reported here as undeclared,
// because "this task carries a value nothing explains" is exactly what it is,
// and dropping it would make the answer disagree with the record.

// FieldValue is one custom-field value with the declaration that explains it.
type FieldValue struct {
	// Slug and Name are what a caller writes back and what a person
	// reads. Empty on an UNDECLARED value, which is the one case where
	// the id below is the only name there is.
	Slug string `json:"slug,omitempty"`
	Name string `json:"name,omitempty"`

	// ID is the key the value is stored under, carried so a caller can
	// write back exactly what it read — the write side resolves an id, a
	// slug or a name, and an undeclared value has nothing else.
	ID string `json:"id"`

	Type  FieldType       `json:"type,omitempty"`
	Value json.RawMessage `json:"value"`

	// Hidden is a value whose declaration was ARCHIVED. It stays on the
	// task and leaves every filter, which is what makes the archive
	// one-way rather than destructive — see [FieldDef.Archived].
	Hidden bool `json:"hidden,omitempty"`

	// Foreign is a value mirrored in from another tracker. Stored so a
	// renderer can show what that system holds, and outside every
	// predicate because this grammar's operators are defined against
	// THIS catalogue's declared types.
	Foreign bool `json:"foreign,omitempty"`

	// Undeclared is a value whose field this company declares nowhere —
	// the state fieldvalues.go keeps on the document and writes no row
	// for. Not an error and not a fault: it is a value for a field
	// nobody can filter on yet.
	Undeclared bool `json:"undeclared,omitempty"`

	// Applies is false when the field is declared but not for THIS task's
	// type — a value left behind by a type change, which renders and
	// filters as itself and is no longer asked for on an edit.
	Applies bool `json:"applies"`
}

// readFieldValues annotates a task's own document against the declarations in
// force, in ONE pass over the map and with no second query of its own.
//
// FROM THE DOCUMENT rather than from `tracker_field_values`, because the
// document is the record and the rows are a filtering projection of it: a
// value whose field is declared nowhere has no row, and an answer assembled
// from the rows would silently drop it.
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

// appliesToType is the same rule the write path's required check reads: an
// empty AppliesTo is every type, and a listed one is those types.
func appliesToType(field FieldDef, taskType string) bool {
	if len(field.AppliesTo) == 0 {
		return true
	}
	return slices.ContainsFunc(field.AppliesTo, func(name string) bool {
		return NormName(name) == NormName(taskType)
	})
}

// foreignFieldIDs is which of a task's values were mirrored in from another
// tracker, read from the one place that records it.
//
// THE ROWS, because provenance is the APPLIER's conclusion rather than
// anything the document says: a foreign value looks exactly like a native one
// on the record, and `kind` is the column that tells them apart.
func foreignFieldIDs(ctx context.Context, tx *sql.Tx, taskID string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT field_id FROM tracker_field_values
		 WHERE task_id = ? AND kind = ?`, taskID, FieldValueForeign)
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
