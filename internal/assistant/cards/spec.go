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
	// Note, where named, is the path to the answer's own sentence on why it
	// holds nothing (a search that could not run, and why), which an empty
	// answer hands the model in place of a card ([Built.Empty]).
	Note string
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
	// Brief are the keys of the columns the drawer draws, in the order it
	// draws them, at most [MaxBrief]: the name a row is known by first. The
	// card leads with them and keeps every other column after them, marked
	// as detail, for the downloads and the model. None draws every column.
	Brief []string
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

// MaxBrief is the most columns a table draws in the drawer: what its narrowest
// width (360px) holds without cutting a word.
const MaxBrief = 4

// Entity is one record, or the record and a list beneath it.
type Entity struct {
	Kind wire.ResourceKind
	// Root is the path to the record, "" for the answer itself.
	Root string
	// ID, Title and Subtitle are paths within the record.
	ID, Title, Subtitle string
	// Name is the card's heading where the record names none of its own
	// (Title is empty): the company's principles are three values, not a
	// record with a name.
	Name  string
	Facts []Fact
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
	// Detail marks a fact the card keeps for the model (a version an update
	// is proposed against) that the drawer leaves out.
	Detail bool
}

// Items is the list an entity card carries beneath its record.
type Items struct {
	// Path is the list's path within the answer (not the record).
	Path string
	Kind wire.ResourceKind
	// ID, Label, Fact and Depth are paths within one item; Depth, when
	// named, indents a tree.
	ID, Label, Fact, Depth string
	// Plain marks a list of plain values, each item its own label (the
	// standing policies, a list of sentences), so Label names no path, and
	// its id its place in the list.
	Plain bool
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
	add("", s.Note)
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
		if len(t.Brief) > MaxBrief {
			return fmt.Errorf("cards: table %q draws %d columns in the drawer, past the %d "+
				"it holds", t.Title, len(t.Brief), MaxBrief)
		}
		drawn := map[string]bool{}
		for _, k := range t.Brief {
			if !keys[k] || drawn[k] {
				return fmt.Errorf("cards: table %q draws %q, which is not one of its columns "+
					"or is drawn twice", t.Title, k)
			}
			drawn[k] = true
		}
		links = append(links, t.Open)
	}
	if e := s.Entity; e != nil {
		if !e.Kind.Valid() || (e.Title == "") == (e.Name == "") {
			return fmt.Errorf("cards: an entity spec needs a kind of %v and exactly one of "+
				"a title path and a name", wire.ResourceKinds)
		}
		for _, f := range e.Facts {
			if f.Label == "" || f.Path == "" || !f.Type.Valid() {
				return fmt.Errorf("cards: fact %q needs a label, a path and a type", f.Label)
			}
		}
		links = append(links, e.Open)
		if it := e.Items; it != nil {
			if it.Path == "" || (it.Label == "") != it.Plain || !it.Kind.Valid() {
				return errors.New("cards: an entity's items need a path, a kind and a label " +
					"path, or are plain values with none")
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
	// Empty is an answer that holds nothing to show: a table no row reached,
	// or an entity whose record carries no value at all. NO CARD IS DRAWN
	// FROM ONE (the catalogue's Shape): an empty table under every search
	// that found nothing is noise the step already says, and the model is
	// told in words instead.
	Empty bool
	// Note is the answer's own sentence on why it holds nothing, read at
	// the spec's Note path; empty where it says none.
	Note string
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
	var built Built
	if spec.Table != nil {
		built, err = buildTable(spec.Table, doc, now)
	} else {
		built, err = buildEntity(spec.Entity, doc)
	}
	if err != nil {
		return Built{}, err
	}
	if spec.Note != "" {
		built.Note = text(lookup(doc, spec.Note))
	}
	return built, nil
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
	columns := t.ordered()
	for _, c := range columns {
		col := wire.Column{Key: c.Key, Label: c.Label, Type: c.Type,
			Detail: len(t.Brief) > 0 && !slices.Contains(t.Brief, c.Key)}
		if c.Type != wire.ColumnText && c.Type != wire.ColumnDatetime {
			col.Align = wire.AlignEnd
		}
		card.Columns = append(card.Columns, col)
	}
	card.Rows = make([][]any, 0, min(len(rows), MaxRows))
	for _, row := range rows[:min(len(rows), MaxRows)] {
		cells := make([]any, len(columns))
		for i, c := range columns {
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
	built := Built{Card: out, Empty: card.RowCount == 0}
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
	// NOTHING THE RECORD HOLDS, asked before the heading a spec names is
	// put in: a fixed name over no value is still an empty answer.
	recordless := card.Title == "" && card.Subtitle == ""
	if card.Title == "" {
		card.Title = e.Name
	}
	for _, f := range e.Facts {
		if v := factValue(f.Type, lookup(record, f.Path)); v != "" {
			fact := wire.Fact{Label: f.Label, Value: v, Mono: f.Mono, Detail: f.Detail}
			if f.Type != wire.ColumnText {
				fact.Type = f.Type
			}
			card.Facts = append(card.Facts, fact)
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
		for at, raw := range list {
			item := wire.EntityItem{Kind: it.Kind, ID: text(lookup(raw, it.ID)),
				Label: text(lookup(raw, it.Label)), Fact: text(lookup(raw, it.Fact))}
			if it.Plain {
				// A PLAIN VALUE HAS NO ID OF ITS OWN: its place in the list
				// is what tells two apart, from 1 as the page numbers them.
				item.ID, item.Label = strconv.Itoa(at+1), text(raw)
			}
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
	built := Built{Card: out, Empty: recordless && card.Text == "" &&
		len(card.Facts) == 0 && total == 0}
	if len(card.Items) < total {
		notice := Notice(wire.NoticeInfo, fmt.Sprintf("Showing the first %d of %d entries. "+
			"Open it in the app to see the rest.", len(card.Items), total))
		built.Notice = &notice
	}
	return built, out.Validate()
}

// ordered are the table's columns in the card's order: the drawn ones first,
// in the order [Table.Brief] names them, then every other in the spec's order.
func (t *Table) ordered() []Column {
	if len(t.Brief) == 0 {
		return t.Columns
	}
	out := make([]Column, 0, len(t.Columns))
	for _, k := range t.Brief {
		for _, c := range t.Columns {
			if c.Key == k {
				out = append(out, c)
			}
		}
	}
	for _, c := range t.Columns {
		if !slices.Contains(t.Brief, c.Key) {
			out = append(out, c)
		}
	}
	return out
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

// factValue is a fact's value in the form its type names, as a table cell of
// that type carries it: a number as written, a datetime as the RFC 3339
// instant, anything else as its text. The dashboard words it, as it words the
// same field on its screens, so the card and the screens never disagree.
func factValue(typ wire.ColumnType, v any) string {
	switch c := cell(typ, v).(type) {
	case json.Number:
		return c.String()
	case string:
		return c
	}
	return ""
}
