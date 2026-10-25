package cards

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// INFERRED CARDS: the card drawn from an answer no spec was written for, the
// answer of a generic read of any route (internal/assistant/catalog's
// api_request). The answer's own JSON decides the shape, by one rule:
//
//   - a LIST is a table: of its records, a column per plain field the first
//     rows carry, the fields a record is known by first; a list of plain
//     values is a list card of them;
//   - a record that WRAPS ONE LIST (a page of a collection: the list, beside
//     nothing but plain values such as a cursor or a count) is the table of
//     that list;
//   - ANY OTHER RECORD IS ONE THING, and an entity card: its plain values are
//     its facts, a nested record's beneath its name, and every list of
//     records it holds is the card's list, each entry named as the record it
//     is.
//
// The last rule is the one a table got wrong: a company document holding its
// roles and its units was drawn as the table of whichever list was longer,
// "2 rows" of a document that is one thing, its every other field dropped.
//
// Every text value is prose, because a generic answer can carry anything
// anybody wrote; the reading tool is fenced for it. Nothing here decides what
// the person may see: the route answered as them, and its answer is redacted
// before a card is drawn from it. An inferred record names no
// [wire.ResourceKind]: it is the API's, not one of the dashboard's objects.

// The bounds an inferred card is drawn to.
const (
	// inferColumns is the most columns a table of records carries.
	inferColumns = 8
	// inferSample is how many rows the columns are read from.
	inferSample = 20
	// inferFacts is the most facts a record's card carries: what the
	// drawer's property list shows at its narrowest width without the card
	// outgrowing the answer it sits under. The model reads the whole answer
	// either way (the generic read hands it the answer, not the card).
	inferFacts = 12
	// inferDepth is how deep a record's nested records are read for facts
	// and lists: the record, and the records within it and within those.
	inferDepth = 3
)

// knownBy are the fields a record is known by, drawn first.
var knownBy = []string{"name", "title", "label", "handle", "key", "id", "status", "kind"}

// Infer is the spec a card is drawn from answer by, and the document to draw
// it from (an answer reshaped where it is a record), titled title. False for
// an answer no card can show: a plain value, or nothing.
func Infer(answer any, title string) (Spec, any, bool) {
	switch t := answer.(type) {
	case []any:
		return list(t, "", title, answer)
	case map[string]any:
		if path, ok := wrappedList(t); ok {
			return list(t[path].([]any), path, title, answer)
		}
		return entityOf(t, title)
	}
	return Spec{}, nil, false
}

// list is the card of a list at path within doc: the table of its records, a
// list card of its plain values, or an empty table, which draws nothing and
// says the read held nothing.
func list(items []any, path, title string, doc any) (Spec, any, bool) {
	if len(items) == 0 {
		return Spec{Table: &Table{Title: title, Rows: path, Columns: []Column{
			{Key: "value", Label: "Value", Path: "value", Type: wire.ColumnText, Prose: true},
		}}}, doc, true
	}
	if !slices.ContainsFunc(items, func(e any) bool { return !plain(e) }) {
		return Spec{Entity: &Entity{Name: title, Prose: true, Items: &Items{
			Path: "items", Plain: true, Prose: true, Noun: entries,
		}}}, map[string]any{"items": items}, true
	}
	return records(items, path, title, doc)
}

// entries is what an inferred list is counted in when nothing names it.
var entries = wire.Noun{One: "entry", Many: "entries"}

// wrappedList is the member of a record that is the one list it wraps, where
// the record is a page of a collection rather than one thing: its only list
// of records (or its only list, empty), beside members that hold no list of
// records at any depth (a cursor, a count, which nodes answered), and no field
// it is known by. A RECORD THAT NAMES ITSELF IS ONE THING whatever it holds (a
// project in full over its statuses, a page over its comments, a company
// over its roles), and so is one holding two lists: [entityOf] draws both.
func wrappedList(record map[string]any) (string, bool) {
	found := ""
	for _, k := range slices.Sorted(maps.Keys(record)) {
		v := record[k]
		if slices.Contains(identity, k) && text(v) != "" && plain(v) {
			return "", false
		}
		items, isList := v.([]any)
		switch {
		case isList && (len(items) == 0 || holdsRecords(items)):
			if found != "" {
				return "", false
			}
			found = k
		case holdsRecordList(v):
			return "", false
		}
	}
	if found == "" {
		return "", false
	}
	items := record[found].([]any)
	if slices.ContainsFunc(items, func(e any) bool { _, ok := e.(map[string]any); return !ok }) {
		return "", false
	}
	return found, true
}

// identity are the fields a record names itself by: [knownBy] less the two
// that say what state or sort it is.
var identity = []string{"name", "title", "label", "handle", "key", "id"}

// holdsRecords reports a list holding a record.
func holdsRecords(items []any) bool {
	return slices.ContainsFunc(items, func(e any) bool { _, ok := e.(map[string]any); return ok })
}

// holdsRecordList reports a value holding a list of records at any depth.
func holdsRecordList(v any) bool {
	switch t := v.(type) {
	case []any:
		return holdsRecords(t) || slices.ContainsFunc(t, holdsRecordList)
	case map[string]any:
		for _, e := range t {
			if holdsRecordList(e) {
				return true
			}
		}
	}
	return false
}

// entityOf is the entity card of one record: its facts and its lists, read
// into a document of their own ([inferred]) the spec draws.
func entityOf(record map[string]any, title string) (Spec, any, bool) {
	var in inferred
	in.walk("", record, 1)
	e := &Entity{Name: title, Prose: true}
	doc := map[string]any{}
	if len(in.facts) > 0 {
		facts := map[string]any{}
		for i, f := range in.facts {
			key := "f" + strconv.Itoa(i)
			facts[key] = f.value
			typ := wire.ColumnText
			if _, isNumber := f.value.(json.Number); isNumber {
				typ = wire.ColumnNumber
			}
			e.Facts = append(e.Facts, Fact{Label: labelOf(f.path), Path: "facts." + key, Type: typ})
		}
		doc["facts"] = facts
	}
	if items := in.items(); len(items) > 0 {
		noun := entries
		if len(in.lists) == 1 {
			many := strings.ToLower(labelOf(lastKey(in.lists[0].path)))
			noun = wire.Noun{One: Singular(many), Many: many}
		}
		e.Items = &Items{Path: "items", ID: "id", Label: "label", Fact: "fact", Prose: true,
			Noun: noun}
		doc["items"] = items
	}
	if len(e.Facts) == 0 && e.Items == nil {
		return Spec{}, nil, false
	}
	return Spec{Entity: e}, doc, true
}

// inferred is what a walk of one record found: its plain values, in the order
// a person reads them, and its lists of records.
type inferred struct {
	facts []inferredFact
	lists []inferredList
}

type inferredFact struct {
	path  string
	value any
}

type inferredList struct {
	path    string
	records []any
}

// walk reads record at prefix: its own plain values first, the fields it is
// known by leading, then each nested record's beneath its name, and every list
// of records wherever it sits, to [inferDepth].
func (in *inferred) walk(prefix string, record map[string]any, depth int) {
	keys := slices.SortedFunc(maps.Keys(record), func(a, b string) int {
		if ia, ib := rank(a), rank(b); ia != ib {
			return ia - ib
		}
		return strings.Compare(a, b)
	})
	var nested []string
	for _, k := range keys {
		v := record[k]
		path := joinPath(prefix, k)
		switch t := v.(type) {
		case map[string]any:
			nested = append(nested, k)
		case []any:
			if plain(v) {
				in.fact(path, v)
			} else if holdsRecords(t) {
				in.lists = append(in.lists, inferredList{path: path, records: t})
			}
		default:
			in.fact(path, v)
		}
	}
	if depth >= inferDepth {
		return
	}
	for _, k := range nested {
		in.walk(joinPath(prefix, k), record[k].(map[string]any), depth+1)
	}
}

// fact keeps one plain value, within [inferFacts]; an empty one says nothing.
func (in *inferred) fact(path string, v any) {
	if len(in.facts) >= inferFacts || text(v) == "" {
		return
	}
	in.facts = append(in.facts, inferredFact{path: path, value: v})
}

// items are the walk's lists as the card's entries: each record by the name
// it is known by and its next such field, and where the record holds several
// lists, each entry beside the list it is in.
func (in *inferred) items() []any {
	var out []any
	several := len(in.lists) > 1
	for _, l := range in.lists {
		for at, raw := range l.records {
			record, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			label, fact := knownFields(record)
			id := recordID(record, at)
			if several {
				id, fact = l.path+"/"+id, labelOf(l.path)
			}
			if label == "" {
				label = "Entry " + strconv.Itoa(at+1)
			}
			out = append(out, map[string]any{"id": id, "label": label, "fact": fact})
		}
	}
	return out
}

// recordID is what tells a record from its neighbours: the first field of
// [identity], read from the most specific (an id, a key, a handle) to the
// least, or its place in the list, from 1.
func recordID(record map[string]any, at int) string {
	for i := len(identity) - 1; i >= 0; i-- {
		if v := record[identity[i]]; plain(v) && text(v) != "" {
			return text(v)
		}
	}
	return strconv.Itoa(at + 1)
}

// knownFields are a record's name, the first field it is known by, and the
// next plain one it carries, which an entry shows beside it.
func knownFields(record map[string]any) (label, fact string) {
	used := ""
	for _, k := range knownBy {
		if s := text(record[k]); s != "" && plain(record[k]) {
			label, used = s, k
			break
		}
	}
	for _, k := range knownBy {
		if k == used {
			continue
		}
		if s := text(record[k]); s != "" && plain(record[k]) {
			return label, s
		}
	}
	return label, ""
}

// lastKey is the last key of a dotted path.
func lastKey(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
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

// labelOf is a field's path as a person reads it: each key in words, nested
// keys beneath their parent's name ("Task counts › active").
func labelOf(path string) string {
	keys := strings.Split(path, ".")
	for i, k := range keys {
		keys[i] = words(k)
	}
	return capitalise(strings.Join(keys, " › "))
}
