package cards

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// Spec is how one tool's answer becomes a card: exactly one of a table and an
// entity, each reading named fields of the route's own JSON.
//
// A DECLARATION RATHER THAN A FUNCTION, so the fields a card reads can be held
// against the answer's Go type by a walk (the catalogue's card walk) and the
// fields a model must read fenced can be derived from it ([Spec.Prose]),
// rather than being two facts a reviewer has to keep in step.
type Spec struct {
	Table  *Table
	Entity *Entity
}

// Table is a table card over a list in the answer.
type Table struct {
	Title string
	// Rows is the path to the list of rows, "" for an answer that is
	// itself the list.
	Rows string
	// Expand, when named, is a list inside each of those rows whose every
	// entry is a row of its own, its parent row under `parent` (a seat's
	// budget windows, one row per window).
	Expand  string
	Columns []Column
	// Open links the whole card to its screen.
	Open *Open
}

// Column is one table column: where each row holds it, and how it is drawn.
type Column struct {
	Key   string
	Label string
	// Path is the field within a row, dotted for a nested one.
	Path string
	Type wire.ColumnType
	// Prose marks text a person or an agent wrote (a title, a question, a
	// snippet), which a model reads fenced.
	Prose bool
}

// Entity is one record, or the record and a list beneath it.
type Entity struct {
	Kind wire.ResourceKind
	// Root is the path to the record, "" for the answer itself.
	Root string
	// ID, Title and Subtitle are paths within the record.
	ID, Title, Subtitle string
	Facts               []Fact
	// Text is the path to a body the record carries, which is always
	// somebody's prose.
	Text  string
	Items *Items
	// Prose marks a record whose title, subtitle or facts a person or an
	// agent wrote.
	Prose bool
	Open  *Open
}

// Fact is one labelled value of an entity.
type Fact struct {
	Label string
	Path  string
	Type  wire.ColumnType
	Mono  bool
}

// Items is the list an entity card carries beneath its record.
type Items struct {
	// Path is the list's path within the answer (not the record).
	Path string
	Kind wire.ResourceKind
	// ID, Label, Fact and Depth are paths within one item; Depth, when
	// named, indents a tree.
	ID, Label, Fact, Depth string
	// Prose marks an item list a person or an agent wrote.
	Prose bool
	Open  *Open
}

// Open is the link a card, a row or an item opens: a screen of the link table
// and where each of its parameters is read.
type Open struct {
	Screen string
	// Params map the screen's parameters to paths within the row or the
	// record.
	Params map[string]string
	// Label is the link's text; empty, it is the value at LabelFrom, and
	// with neither the first parameter's value.
	Label     string
	LabelFrom string
}

// Prose reports whether the card reads any text a person or an agent wrote,
// which is what makes its tool's answer one a model reads fenced.
func (s Spec) Prose() bool {
	switch {
	case s.Table != nil:
		return slices.ContainsFunc(s.Table.Columns, func(c Column) bool { return c.Prose })
	case s.Entity != nil:
		e := s.Entity
		return e.Prose || e.Text != "" || (e.Items != nil && e.Items.Prose)
	}
	return false
}

// Paths are every path the spec reads, each with the root it is read from:
// "" for the answer, "row" for a table row, "record" for an entity's record,
// "item" for one of its items. For the walk that holds a spec to its answer's
// type.
func (s Spec) Paths() []Path {
	var out []Path
	add := func(from, p string) {
		if p != "" {
			out = append(out, Path{From: from, Path: p})
		}
	}
	opens := func(from string, o *Open) {
		if o != nil {
			for _, p := range o.Params {
				add(from, p)
			}
			add(from, o.LabelFrom)
		}
	}
	if t := s.Table; t != nil {
		add("", t.Rows)
		add("row", t.Expand)
		for _, c := range t.Columns {
			add("row", c.Path)
		}
		opens("", t.Open)
	}
	if e := s.Entity; e != nil {
		add("", e.Root)
		for _, p := range []string{e.ID, e.Title, e.Subtitle, e.Text} {
			add("record", p)
		}
		for _, f := range e.Facts {
			add("record", f.Path)
		}
		opens("record", e.Open)
		if it := e.Items; it != nil {
			add("", it.Path)
			for _, p := range []string{it.ID, it.Label, it.Fact, it.Depth} {
				add("item", p)
			}
			opens("item", it.Open)
		}
	}
	return out
}

// Path is one path a spec reads and the root it is read from.
type Path struct{ From, Path string }

// Validate reports what is wrong with a spec as written: exactly one body,
// every column keyed and typed, every link's screen one the link table holds.
func (s Spec) Validate() error {
	if (s.Table == nil) == (s.Entity == nil) {
		return errors.New("cards: a spec is exactly one of a table and an entity")
	}
	var links []*Open
	if t := s.Table; t != nil {
		if t.Title == "" || len(t.Columns) == 0 {
			return errors.New("cards: a table spec needs a title and a column")
		}
		keys := map[string]bool{}
		for _, c := range t.Columns {
			if c.Key == "" || c.Label == "" || c.Path == "" || !c.Type.Valid() || keys[c.Key] {
				return fmt.Errorf("cards: table column %q is not a unique key with a "+
					"label, a path and a type", c.Key)
			}
			keys[c.Key] = true
		}
		links = append(links, t.Open)
	}
	if e := s.Entity; e != nil {
		if !e.Kind.Valid() || e.Title == "" {
			return fmt.Errorf("cards: an entity spec needs a kind of %v and a title",
				wire.ResourceKinds)
		}
		for _, f := range e.Facts {
			if f.Label == "" || f.Path == "" || !f.Type.Valid() {
				return fmt.Errorf("cards: fact %q needs a label, a path and a type", f.Label)
			}
		}
		links = append(links, e.Open)
		if it := e.Items; it != nil {
			if it.Path == "" || it.Label == "" || !it.Kind.Valid() {
				return errors.New("cards: an entity's items need a path, a label and a kind")
			}
			links = append(links, it.Open)
		}
	}
	for _, o := range links {
		if o == nil {
			continue
		}
		if err := checkOpen(o); err != nil {
			return err
		}
	}
	return nil
}

// Built is a card and, when the card is not the whole answer, the notice that
// says so.
type Built struct {
	Card   wire.Card
	Notice *wire.Card
}

// Cards are the card and its notice, in the order they are shown.
func (b Built) Cards() []wire.Card {
	if b.Notice == nil {
		return []wire.Card{b.Card}
	}
	return []wire.Card{b.Card, *b.Notice}
}

// ErrNotJSON is an answer that is not one JSON value, which no card is built
// from.
var ErrNotJSON = errors.New("cards: the answer is not JSON")

// Build is the card spec reads from answer, the route's own JSON, as of now.
//
// A table holds at most [MaxRows] rows and [MaxBytes] encoded, and a table cut
// to fit says so twice: the card is marked truncated with the read's own row
// count, and a notice names how many rows were shown. A field the answer does
// not hold is an empty cell, never a guess.
func Build(spec Spec, answer []byte, now time.Time) (Built, error) {
	if err := spec.Validate(); err != nil {
		return Built{}, err
	}
	doc, err := decode(answer)
	if err != nil {
		return Built{}, err
	}
	if spec.Table != nil {
		return buildTable(spec.Table, doc, now)
	}
	return buildEntity(spec.Entity, doc)
}

// decode reads one JSON value, numbers kept as written.
func decode(answer []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(answer))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotJSON, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: it holds more than one value", ErrNotJSON)
	}
	return doc, nil
}

func buildTable(t *Table, doc any, now time.Time) (Built, error) {
	rows, _ := lookup(doc, t.Rows).([]any)
	if t.Expand != "" {
		rows = expand(rows, t.Expand)
	}
	card := &wire.TableCard{Title: t.Title, RowCount: len(rows), AsOf: now.UTC()}
	for _, c := range t.Columns {
		col := wire.Column{Key: c.Key, Label: c.Label, Type: c.Type}
		if c.Type != wire.ColumnText && c.Type != wire.ColumnDatetime {
			col.Align = wire.AlignEnd
		}
		card.Columns = append(card.Columns, col)
	}
	card.Rows = make([][]any, 0, min(len(rows), MaxRows))
	for _, row := range rows[:min(len(rows), MaxRows)] {
		cells := make([]any, len(t.Columns))
		for i, c := range t.Columns {
			cells[i] = cell(c.Type, lookup(row, c.Path))
		}
		card.Rows = append(card.Rows, cells)
	}
	if t.Open != nil {
		if link, err := linkFrom(t.Open, doc); err == nil {
			card.Open = &link
		}
	}
	out := wire.Card{Type: wire.CardTable, Table: card}
	// THE BYTE BOUND BY HALVING, which ends in at most log2(MaxRows) steps
	// and never cuts a row: a cell is a value someone reads whole.
	for len(card.Rows) > 0 {
		size, err := encodedSize(out)
		if err != nil {
			return Built{}, err
		}
		if size <= MaxBytes {
			break
		}
		card.Rows = card.Rows[:len(card.Rows)/2]
	}
	card.Truncated = len(card.Rows) < card.RowCount
	built := Built{Card: out}
	if card.Truncated {
		notice := Notice(wire.NoticeInfo, fmt.Sprintf("Showing the first %d of %d rows. "+
			"Open it in the app to see the rest.", len(card.Rows), card.RowCount))
		built.Notice = &notice
	}
	return built, out.Validate()
}

func buildEntity(e *Entity, doc any) (Built, error) {
	record := lookup(doc, e.Root)
	card := &wire.EntityCard{Kind: e.Kind, ID: text(lookup(record, e.ID)),
		Title: text(lookup(record, e.Title)), Subtitle: text(lookup(record, e.Subtitle))}
	if card.Title == "" {
		card.Title = card.ID
	}
	for _, f := range e.Facts {
		if v := factValue(f.Type, lookup(record, f.Path)); v != "" {
			card.Facts = append(card.Facts, wire.Fact{Label: f.Label, Value: v, Mono: f.Mono})
		}
	}
	if e.Text != "" {
		card.Text = text(lookup(record, e.Text))
	}
	if e.Open != nil {
		if link, err := linkFrom(e.Open, record); err == nil {
			card.Open = &link
		}
	}
	if it := e.Items; it != nil {
		list, _ := lookup(doc, it.Path).([]any)
		for _, raw := range list {
			item := wire.EntityItem{Kind: it.Kind, ID: text(lookup(raw, it.ID)),
				Label: text(lookup(raw, it.Label)), Fact: text(lookup(raw, it.Fact))}
			if it.Depth != "" {
				if n, err := strconv.Atoi(text(lookup(raw, it.Depth))); err == nil && n > 0 {
					item.Depth = n
				}
			}
			if it.Open != nil {
				if link, err := linkFrom(it.Open, raw); err == nil {
					item.Open = &link
				}
			}
			card.Items = append(card.Items, item)
		}
	}
	out := wire.Card{Type: wire.CardEntity, Entity: card}
	total := len(card.Items)
	for len(card.Items) > 0 {
		size, err := encodedSize(out)
		if err != nil {
			return Built{}, err
		}
		if size <= MaxBytes {
			break
		}
		card.Items = card.Items[:len(card.Items)/2]
	}
	if size, err := encodedSize(out); err != nil {
		return Built{}, err
	} else if size > MaxBytes {
		return Built{}, fmt.Errorf("cards: the %s record is %d bytes as a card, past the "+
			"%d one card holds", e.Kind, size, MaxBytes)
	}
	built := Built{Card: out}
	if len(card.Items) < total {
		notice := Notice(wire.NoticeInfo, fmt.Sprintf("Showing the first %d of %d entries. "+
			"Open it in the app to see the rest.", len(card.Items), total))
		built.Notice = &notice
	}
	return built, out.Validate()
}

// expand is every entry of the list at path inside each row, each a row of its
// own carrying its parent under `parent`.
func expand(rows []any, path string) []any {
	var out []any
	for _, row := range rows {
		children, _ := lookup(row, path).([]any)
		for _, child := range children {
			fields, ok := child.(map[string]any)
			if !ok {
				continue
			}
			merged := make(map[string]any, len(fields)+1)
			for k, v := range fields {
				merged[k] = v
			}
			merged["parent"] = row
			out = append(out, merged)
		}
	}
	return out
}

// encodedSize is a card's size as the log stores it.
func encodedSize(c wire.Card) (int, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return 0, fmt.Errorf("cards: encode a card: %w", err)
	}
	return len(b), nil
}

// Lookup is the value at a dotted path of a decoded answer, nil where there is
// none. An empty path is the value itself. Exported for the catalogue's
// previews, which read an object's current values from the same answers.
func Lookup(v any, path string) any { return lookup(v, path) }

// Text is a decoded value as a person reads it. See [Lookup].
func Text(v any) string { return text(v) }

// Decode reads one answer as the cards do, numbers kept as written.
func Decode(answer []byte) (any, error) { return decode(answer) }

// lookup is the value at a dotted path, nil where there is none. An empty path
// is the value itself.
func lookup(v any, path string) any {
	if path == "" {
		return v
	}
	for _, key := range strings.Split(path, ".") {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok = obj[key]; !ok {
			return nil
		}
	}
	return v
}

// cell is one value as a column of typ carries it: text for a text column,
// a number for a numeric one, an RFC 3339 instant for a datetime, and null
// where the value is not of the column's kind.
func cell(typ wire.ColumnType, v any) any {
	switch typ {
	case wire.ColumnNumber, wire.ColumnTokens, wire.ColumnDuration:
		switch n := v.(type) {
		case json.Number:
			return n
		case string:
			if _, err := strconv.ParseFloat(n, 64); err == nil {
				return json.Number(n)
			}
		}
		return nil
	case wire.ColumnDatetime:
		s, _ := v.(string)
		if at, err := time.Parse(time.RFC3339Nano, s); err == nil && !at.IsZero() {
			return at.UTC().Format(time.RFC3339)
		}
		return nil
	}
	if v == nil {
		return nil
	}
	return text(v)
}

// text is a value as a person reads it: a string as it is, a number as written,
// a list of plain values joined, and a record by the name it carries.
func text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := text(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		for _, key := range []string{"name", "title", "label", "handle", "key", "id"} {
			if s, ok := t[key].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// factValue is a fact as its line shows it: a duration in words, a datetime as
// the instant, anything else as its text.
func factValue(typ wire.ColumnType, v any) string {
	switch c := cell(typ, v).(type) {
	case nil:
		return ""
	case json.Number:
		if typ == wire.ColumnDuration {
			if ms, err := c.Float64(); err == nil {
				return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
			}
		}
		return c.String()
	case string:
		return c
	}
	return ""
}
