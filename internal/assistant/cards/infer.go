package cards

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// INFERRED CARDS: a table drawn from an answer no spec was written for, the
// answer of a generic read of any route (internal/assistant/catalog's
// api_request). Two shapes, from the answer's own JSON:
//
//   - a list of records, or a record whose largest member is one, is a table
//     of those records: a column per plain field the first rows carry, the
//     fields a record is known by first;
//   - any other record is a table of its fields, one row per plain value
//     beneath it, named by its path.
//
// Every text column is prose, because a generic answer can carry anything
// anybody wrote; the reading tool is fenced for it. Nothing here decides what
// the person may see: the route answered as them, and its answer is redacted
// before a card is drawn from it.

// The bounds an inferred table is drawn to.
const (
	// inferColumns is the most columns a table of records carries.
	inferColumns = 8
	// inferSample is how many rows the columns are read from.
	inferSample = 20
)

// knownBy are the fields a record is known by, drawn first.
var knownBy = []string{"name", "title", "label", "handle", "key", "id", "status", "kind"}

// Infer is the spec a card is drawn from answer by, and the document to draw
// it from (an answer reshaped into rows where it is a record), titled title.
// False for an answer no table can show: a plain value, or nothing.
func Infer(answer any, title string) (Spec, any, bool) {
	switch t := answer.(type) {
	case []any:
		return records(t, "", title, answer)
	case map[string]any:
		if path := mainList(t); path != "" {
			return records(t[path].([]any), path, title, answer)
		}
		rows := fieldRows(t)
		if len(rows) == 0 {
			return Spec{}, nil, false
		}
		return Spec{Table: &Table{Title: title, Columns: []Column{
			{Key: "field", Label: "Field", Path: "field", Type: wire.ColumnText},
			{Key: "value", Label: "Value", Path: "value", Type: wire.ColumnText, Prose: true},
		}}}, rows, true
	}
	return Spec{}, nil, false
}

// records is the table of a list of records at path within doc.
func records(list []any, path, title string, doc any) (Spec, any, bool) {
	counts := map[string]int{}
	numeric := map[string]bool{}
	for _, row := range list[:min(len(list), inferSample)] {
		record, ok := row.(map[string]any)
		if !ok {
			return Spec{}, nil, false
		}
		for k, v := range record {
			if !plain(v) || k == "" || strings.Contains(k, ".") {
				continue
			}
			if counts[k] == 0 {
				numeric[k] = true
			}
			counts[k]++
			if _, isNumber := v.(json.Number); !isNumber && v != nil {
				numeric[k] = false
			}
		}
	}
	if len(counts) == 0 {
		return Spec{}, nil, false
	}
	keys := slices.SortedFunc(maps.Keys(counts), func(a, b string) int {
		ia, ib := rank(a), rank(b)
		switch {
		case ia != ib:
			return ia - ib
		case counts[a] != counts[b]:
			return counts[b] - counts[a]
		}
		return strings.Compare(a, b)
	})
	keys = keys[:min(len(keys), inferColumns)]
	t := &Table{Title: title, Rows: path}
	for _, k := range keys {
		c := Column{Key: k, Label: labelOf(k), Path: k, Type: wire.ColumnText, Prose: true}
		if numeric[k] {
			c.Type, c.Prose = wire.ColumnNumber, false
		}
		t.Columns = append(t.Columns, c)
	}
	t.Brief = keys[:min(len(keys), MaxBrief)]
	return Spec{Table: t}, doc, true
}

// rank is where a field is drawn: the fields a record is known by first.
func rank(key string) int {
	if i := slices.Index(knownBy, key); i >= 0 {
		return i
	}
	return len(knownBy)
}

// mainList is the member of a record that is its largest list of records,
// where it has one.
func mainList(record map[string]any) string {
	best, size := "", 0
	for _, k := range slices.Sorted(maps.Keys(record)) {
		list, ok := record[k].([]any)
		if !ok || len(list) <= size {
			continue
		}
		if _, isRecord := list[0].(map[string]any); isRecord {
			best, size = k, len(list)
		}
	}
	return best
}

// fieldRows are a record's plain values as rows of a field table.
func fieldRows(record map[string]any) []any {
	var out []any
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(t)) {
				walk(joinPath(prefix, k), t[k])
			}
			return
		case []any:
			if !slices.ContainsFunc(t, func(e any) bool { return !plain(e) }) {
				break
			}
			for i, e := range t {
				walk(joinPath(prefix, strconv.Itoa(i+1)), e)
			}
			return
		}
		if s := text(v); s != "" && prefix != "" {
			out = append(out, map[string]any{"field": labelOf(prefix), "value": s})
		}
	}
	walk("", record)
	return out
}

// plain reports a value one cell holds: a string, a number, a truth, nothing,
// or a list of those.
func plain(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return false
	case []any:
		return !slices.ContainsFunc(t, func(e any) bool {
			_, isRecord := e.(map[string]any)
			_, isList := e.([]any)
			return isRecord || isList
		})
	}
	return true
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// labelOf is a field's path as a person reads it.
func labelOf(path string) string {
	label := strings.ReplaceAll(strings.ReplaceAll(path, "_", " "), ".", " › ")
	if label == "" {
		return path
	}
	return strings.ToUpper(label[:1]) + label[1:]
}
