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
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
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
	// More, where named, is the path to the answer's own word that rows lie
	// past this page: a cursor to the next one (`next`, `next_cursor`) or a
	// flag (`more`, `is_truncated`). A page that says so is a page, never
	// the whole: the model is told the count is "at least" what it holds,
	// and the person that there are more ([Built.More]).
	More string
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
	// Seat marks a text column whose value is a seat's handle, drawn by the
	// seat's name where the build is told it ([Options.SeatName]): a person
	// knows "QA Engineer", not "qa", and the model reads what the person
	// sees, which the catalogue's resolver takes back as a name.
	Seat bool
	// Prefix is written before every value the column draws, never before
	// an empty one: a room's "#", as every screen draws a room's name, so
	// the card and the screen say "#launch" alike.
	Prefix string
}

// Options are what a build is told beyond the spec and the answer.
type Options struct {
	// SeatName is a seat's name by its handle, false where it is not
	// known; nil draws every seat column by its handle.
	SeatName func(handle string) (string, bool)
	// Zone is the company's clock, which every instant the model reads of
	// the card is said on ([SummaryOf]); nil says them in UTC. The card the
	// person sees keeps the instant, which the dashboard draws in the
	// viewer's own zone.
	Zone *time.Location
	// Room is a chat room as the turn says it by its id or its name, read
	// through the person's own grant, false where it could not be read; nil
	// says none. A direct conversation has no name, and is said by its people
	// ([Entity.Room], and a read's scope).
	Room func(ref string) (Room, bool)
}

// Room is a chat room as the turn says it where an answer cannot: a named room
// by its name, and a direct conversation, which has none, by the people in it
// beside the reader, as the chat screen names one.
type Room struct {
	// Name is the room's name, without its "#"; empty for a direct
	// conversation.
	Name string
	// With is who a direct conversation is with beside the reader, by their
	// seats' names in words ("Ada Lovelace and Bo"), empty for one the reader
	// holds alone.
	With string
}

// Heading is the room as a card heads it: "#launch", or a direct conversation
// by its people.
func (r Room) Heading() string {
	switch {
	case r.Name != "":
		return "#" + r.Name
	case r.With != "":
		return "Direct conversation with " + r.With
	}
	return "Direct conversation with yourself"
}

// Where is where a read of the room is, as its step and its card's title say
// it: "in #launch", or "with Ada Lovelace and Bo".
func (r Room) Where() string {
	switch {
	case r.Name != "":
		return "in #" + r.Name
	case r.With != "":
		return "with " + r.With
	}
	return "with yourself"
}

// MaxBrief is the most columns a table draws in the drawer: what its narrowest
// width (360px) holds without cutting a word.
const MaxBrief = 4

// Entity is one record, or the record and a list beneath it.
type Entity struct {
	// Kind is the dashboard object the record is, or empty for a record that
	// is none of them (a generic read's answer, [Infer]).
	Kind wire.ResourceKind
	// Root is the path to the record, "" for the answer itself.
	Root string
	// ID, Title and Subtitle are paths within the record.
	ID, Title, Subtitle string
	// Key is the path to the short name the record is written by in prose
	// (a task's key, a project's, a room's name), empty for a record with
	// none.
	Key string
	// KeyPrefix is written before the key, as TitlePrefix is before the
	// title and never before an empty key: a room is written by its name
	// after its "#", "#launch".
	KeyPrefix string
	// TitlePrefix is written before the heading the record's title gives,
	// as [Column.Prefix] is before a cell, and never before an empty title,
	// so a record with none is still headed by Name.
	TitlePrefix string
	// TitleCut, where set, is the most bytes of the title a heading takes:
	// a record headed by somebody's words (a thread by its first message) is
	// headed by them on one line, cut with a marker where they pass it,
	// and its whole words are its Text.
	TitleCut int
	// Room, where set, is the path within the record to the id of the chat
	// room it is: a room with no name of its own (a direct conversation) is
	// headed by its people, as the turn says the room ([Options.Room]),
	// rather than by Name alone, so two conversations read as two.
	Room string
	// Name is the card's heading where the record names none of its own:
	// the company's principles are three values, not a record with a name
	// (Title is empty), and a direct conversation is a room with no name
	// (Title names a field this record leaves empty).
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
	// Kind is the dashboard object each item is, or empty for none.
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
	// More, where named, is the path to the answer's own word that items lie
	// past this page (a thread's `next_cursor`), as [Table.More] is a
	// table's: the record's list is then a page, never the whole.
	More string
	// Noun is what one item and several are called ("policy", "policies"),
	// which the drawer counts a folded card's list with.
	Noun wire.Noun
	Open *Open
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
		for _, p := range []string{e.ID, e.Key, e.Title, e.Subtitle, e.Text} {
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
			if (c.Seat || c.Prefix != "") && c.Type != wire.ColumnText {
				return fmt.Errorf("cards: table column %q names a seat or a prefix and is "+
					"not text", c.Key)
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
		if (e.Kind != "" && !e.Kind.Valid()) || (e.Title == "" && e.Name == "") {
			return fmt.Errorf("cards: an entity spec needs either no kind or one of %s, and "+
				"a title path, a name, or both", words.Or(wire.ResourceKinds))
		}
		// A KEYED KIND CARRIES ITS KEY, and no other kind does: the drawer
		// finds a record in the answer's words by it (ADR-0061), so a spec
		// that left it out would draw a task it could never name.
		if e.Kind.Keyed() != (e.Key != "") {
			return fmt.Errorf("cards: an entity spec of kind %q needs a key path exactly when "+
				"its kind is written by a key (a task's, a project's, a room's)", e.Kind)
		}
		if e.TitleCut < 0 || (e.Room != "" && e.Kind != wire.ResourceRoom) {
			return fmt.Errorf("cards: an entity spec of kind %q cuts its title at %d bytes "+
				"or heads it by a room, and only a room is headed by one", e.Kind, e.TitleCut)
		}
		for _, f := range e.Facts {
			if f.Label == "" || f.Path == "" || !f.Type.Valid() {
				return fmt.Errorf("cards: fact %q needs a label, a path and a type", f.Label)
			}
		}
		links = append(links, e.Open)
		if it := e.Items; it != nil {
			if it.Path == "" || (it.Label == "") != it.Plain || (it.Kind != "" && !it.Kind.Valid()) {
				return errors.New("cards: an entity's items need a path, a kind or none, and " +
					"a label path, or are plain values with none")
			}
			if it.Noun.One == "" || it.Noun.Many == "" {
				return fmt.Errorf("cards: the items at %q need a noun for one and for many",
					it.Path)
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

// Built is a card a read's answer built. A card that is not the whole answer
// says so itself (a table's Truncated and More, an entity's ItemsTotal), never
// in a notice beside it (ADR-0061).
type Built struct {
	Card wire.Card
	// Empty is an answer that holds nothing to show: a table no row reached,
	// or an entity whose record carries no value at all. NO CARD IS DRAWN
	// FROM ONE (the catalogue's Shape): an empty table under every search
	// that found nothing is noise the step already says, and the model is
	// told in words instead.
	Empty bool
	// Note is the answer's own sentence on why it holds nothing, read at
	// the spec's Note path; empty where it says none.
	Note string
	// More is a table whose answer says rows lie past this page
	// ([Table.More]).
	More bool
	// Zone is [Options.Zone], carried to the model's summary.
	Zone *time.Location
}

// ErrNotJSON is an answer that is not one JSON value, which no card is built
// from.
var ErrNotJSON = errors.New("cards: the answer is not JSON")

// Build is the card spec reads from answer, the route's own JSON, as of now.
//
// A table holds at most [MaxRows] rows and [MaxBytes] encoded, and a table cut
// to fit says so on itself: it is marked truncated with the read's own row
// count, and More where the answer says rows lie past its page. A field the
// answer does not hold is an empty cell, never a guess.
func Build(spec Spec, answer []byte, now time.Time) (Built, error) {
	return BuildWith(spec, answer, now, Options{})
}

// BuildWith is [Build] told what o says.
func BuildWith(spec Spec, answer []byte, now time.Time, o Options) (Built, error) {
	if err := spec.Validate(); err != nil {
		return Built{}, err
	}
	doc, err := decode(answer)
	if err != nil {
		return Built{}, err
	}
	var built Built
	if spec.Table != nil {
		built, err = buildTable(spec.Table, doc, now, o)
	} else {
		built, err = buildEntity(spec.Entity, doc, o)
	}
	if err != nil {
		return Built{}, err
	}
	if spec.Note != "" {
		built.Note = text(lookup(doc, spec.Note))
	}
	built.Zone = o.Zone
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

func buildTable(t *Table, doc any, now time.Time, o Options) (Built, error) {
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
			if handle, ok := cells[i].(string); ok && c.Seat && handle != "" &&
				o.SeatName != nil {
				if name, known := o.SeatName(handle); known {
					cells[i] = name
				}
			}
			if s, ok := cells[i].(string); ok && s != "" {
				cells[i] = c.Prefix + s
			}
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
	if t.More != "" {
		built.More = morePast(lookup(doc, t.More))
	}
	card.More = built.More && card.RowCount > 0
	return built, out.Validate()
}

// morePast reads an answer's word that rows lie past its page: a cursor that
// is present and not empty, or a flag that is true.
func morePast(v any) bool {
	switch m := v.(type) {
	case bool:
		return m
	case string:
		return m != ""
	case nil:
		return false
	}
	return true
}

func buildEntity(e *Entity, doc any, o Options) (Built, error) {
	record := lookup(doc, e.Root)
	card := &wire.EntityCard{Kind: e.Kind, ID: text(field(record, e.ID)),
		Key: text(field(record, e.Key)), Title: text(field(record, e.Title)),
		Subtitle: text(field(record, e.Subtitle))}
	if card.Key != "" {
		card.Key = e.KeyPrefix + card.Key
	}
	if e.TitleCut > 0 {
		card.Title = textcut.Within(strings.Join(strings.Fields(card.Title), " "), e.TitleCut)
	}
	if card.Title != "" {
		card.Title = e.TitlePrefix + card.Title
	}
	// AN ID HEADS A RECORD ONLY WHERE A PERSON READS IT (a task's key): one
	// known by nothing but its UUID is headed by its spec's name, never by
	// the id, which reads as nothing to anybody.
	if card.Title == "" && !uuidLike(card.ID) {
		card.Title = card.ID
	}
	// NOTHING THE RECORD HOLDS, asked before the heading a spec names is
	// put in: a fixed name over no value is still an empty answer.
	recordless := card.Title == "" && card.Subtitle == ""
	if card.Title == "" && e.Room != "" && o.Room != nil {
		if room, ok := o.Room(text(field(record, e.Room))); ok {
			card.Title = room.Heading()
		}
	}
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
		card.Text = text(field(record, e.Text))
	}
	if e.Open != nil {
		if link, err := linkFrom(e.Open, record); err == nil {
			card.Open = &link
		}
	}
	if it := e.Items; it != nil {
		noun := it.Noun
		card.ItemsNoun = &noun
		list, _ := lookup(doc, it.Path).([]any)
		for at, raw := range list {
			// AN ITEM'S PATHS NAME ITS FIELDS, and one left unnamed reads
			// nothing: read with lookup, an empty path is the item itself, and
			// a plain policy came out as its own label and again as its fact.
			item := wire.EntityItem{Kind: it.Kind, ID: text(field(raw, it.ID)),
				Label: text(field(raw, it.Label)), Fact: text(field(raw, it.Fact))}
			if it.Plain {
				// A PLAIN VALUE HAS NO ID OF ITS OWN: its place in the list
				// is what tells two apart, from 1 as the page numbers them.
				item.ID, item.Label = strconv.Itoa(at+1), text(raw)
			}
			if it.Depth != "" {
				if n, err := strconv.Atoi(text(field(raw, it.Depth))); err == nil && n > 0 {
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
		card.ItemsTotal = total
	}
	if e.Items != nil && e.Items.More != "" {
		built.More = morePast(lookup(doc, e.Items.More))
	}
	card.ItemsMore = built.More && total > 0
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

// field is the value at a path that names a field, nil where the path names
// none. Unlike [lookup], whose empty path is the value itself (an answer that
// is its own record or list), an unnamed field is no value.
func field(v any, path string) any {
	if path == "" {
		return nil
	}
	return lookup(v, path)
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
