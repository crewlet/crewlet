package cards

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// Summary is what a model is handed of a card: its columns or facts and as many
// whole rows or items as fit in budget bytes, with how many there were, so it
// can describe a result it never authors.
//
// NOTHING IS CUT MID-VALUE. Rows and items are dropped whole from the end and
// counted; an entity's body that does not fit beside its facts is left out and
// described by its size, because a body cut part way reads to the model as a
// body that ended there. The card the person sees is the whole answer either
// way, so the model is told to point at it rather than guess past the cut.
// A link's address never reaches a model: it offers open_in_app instead.
func Summary(card wire.Card, budget int) ([]byte, error) {
	return SummaryOf(Built{Card: card}, budget)
}

// SummaryOf is [Summary] of a built card, which also knows what the answer
// said beyond its rows: that more lie past its page ([Built.More]).
func SummaryOf(b Built, budget int) ([]byte, error) {
	card := b.Card
	switch card.Type {
	case wire.CardTable:
		return tableSummary(card.Table, b.More, b.Zone, budget)
	case wire.CardEntity:
		return entitySummary(card.Entity, b.Zone, budget)
	case wire.CardNotice:
		return json.Marshal(map[string]string{"notice": card.Notice.Text})
	case wire.CardError:
		return json.Marshal(map[string]any{"refused": card.Error.Refusal})
	}
	return nil, fmt.Errorf("cards: a %s card has no summary for a model", card.Type)
}

type tableDigest struct {
	Title     string   `json:"title"`
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Shown     int      `json:"rows_shown"`
	Truncated bool     `json:"truncated,omitempty"`
	// More says the answer holds rows past this page, so row_count is a
	// floor and never the total.
	More bool   `json:"more_exist,omitempty"`
	Note string `json:"note,omitempty"`
	// Access is the sentence every list carries: what the person may not
	// see is left out, so the model never reads an absence as proof.
	Access string `json:"access"`
}

// MoreNote is what a table summary tells the model of a page with rows past
// it: its count is a floor. A capped page read as the whole once answered
// "five turns in a row" for a seat that had failed twenty-five.
const MoreNote = "This is one page of %d rows and the answer says more exist past it. " +
	"Never give %[1]d as the total: say \"at least %[1]d\", or read further with a " +
	"larger limit or a narrower filter."

// AccessNote is what every table summary tells the model about its rows.
const AccessNote = "Rows the person may not see are left out, so a row missing here " +
	"is not proof that none exists."

func tableSummary(t *wire.TableCard, more bool, zone *time.Location, budget int) ([]byte, error) {
	d := tableDigest{Title: t.Title, RowCount: t.RowCount, Rows: onClock(t, zone),
		Access: AccessNote, More: more}
	for _, c := range t.Columns {
		d.Columns = append(d.Columns, c.Label)
	}
	rows := d.Rows
	return fit(func() (any, int) { return &d, len(d.Rows) }, func(n int) {
		d.Rows, d.Shown = rows[:n], n
		d.Truncated = n < t.RowCount
		switch {
		case d.Truncated:
			d.Note = fmt.Sprintf("Only %d of %d rows are listed here; the person "+
				"sees the table on the card.", n, t.RowCount)
		case d.More:
			d.Note = fmt.Sprintf(MoreNote, t.RowCount)
		}
	}, budget)
}

type entityDigest struct {
	Kind         wire.ResourceKind `json:"kind,omitempty"`
	ID           string            `json:"id,omitempty"`
	Key          string            `json:"key,omitempty"`
	Title        string            `json:"title"`
	Subtitle     string            `json:"subtitle,omitempty"`
	Facts        map[string]string `json:"facts,omitempty"`
	Text         string            `json:"text,omitempty"`
	TextOmitted  int               `json:"text_omitted_bytes,omitempty"`
	Items        []itemDigest      `json:"items,omitempty"`
	ItemsShown   int               `json:"items_shown,omitempty"`
	ItemsTotal   int               `json:"items_total,omitempty"`
	MoreExist    bool              `json:"more_exist,omitempty"`
	ItemsPartial string            `json:"note,omitempty"`
}

type itemDigest struct {
	ID    string `json:"id,omitempty"`
	Label string `json:"label"`
	Fact  string `json:"fact,omitempty"`
	Depth int    `json:"depth,omitempty"`
}

func entitySummary(e *wire.EntityCard, zone *time.Location, budget int) ([]byte, error) {
	d := entityDigest{Kind: e.Kind, ID: e.ID, Key: e.Key, Title: e.Title,
		Subtitle: e.Subtitle, Text: e.Text}
	if len(e.Facts) > 0 {
		d.Facts = map[string]string{}
		for _, f := range e.Facts {
			d.Facts[f.Label] = f.Value
			if f.Type == wire.ColumnDatetime {
				d.Facts[f.Label] = Clock(f.Value, zone)
			}
		}
	}
	items := make([]itemDigest, 0, len(e.Items))
	for _, it := range e.Items {
		items = append(items, itemDigest{ID: it.ID, Label: it.Label, Fact: it.Fact,
			Depth: it.Depth})
	}
	d.ItemsTotal = len(items)
	d.MoreExist = e.ItemsMore
	set := func(n int) {
		d.Items, d.ItemsShown = items[:n], n
		d.ItemsPartial = ""
		switch {
		case n < len(items):
			d.ItemsPartial = fmt.Sprintf("Only %d of %d entries are listed here; the "+
				"person sees them all on the card.", n, len(items))
		case e.ItemsMore:
			d.ItemsPartial = fmt.Sprintf(MoreNote, len(items))
		}
	}
	set(len(items))
	out, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("cards: encode a summary: %w", err)
	}
	if len(out) > budget && d.Text != "" {
		d.TextOmitted, d.Text = len(d.Text), ""
	}
	return fit(func() (any, int) { return &d, len(items) }, set, budget)
}

// onClock is a table's rows with every instant in its datetime columns said on
// the company's clock ([Clock]), a copy: the card the person sees keeps them.
func onClock(t *wire.TableCard, zone *time.Location) [][]any {
	var at []int
	for i, c := range t.Columns {
		if c.Type == wire.ColumnDatetime {
			at = append(at, i)
		}
	}
	if len(at) == 0 {
		return t.Rows
	}
	out := make([][]any, len(t.Rows))
	for r, row := range t.Rows {
		out[r] = append([]any(nil), row...)
		for _, i := range at {
			if i < len(row) {
				if v, ok := row[i].(string); ok {
					out[r][i] = Clock(v, zone)
				}
			}
		}
	}
	return out
}

// Clock is an RFC 3339 instant as the model reads it: on the company's clock
// with the zone's name ("Mon 26 Oct 2026 01:45 PDT"), and a moment that is
// midnight there as the date alone ("Mon 26 Oct 2026"), which is what a task
// due all day is stored as. Said on the company's clock because the model
// answers with what it reads: handed UTC with a Z, it told people times eight
// hours off their own screen, and read a date due all day as due at
// midnight UTC, the day before. A value that is not an instant is as it was;
// a nil zone is UTC.
func Clock(v string, zone *time.Location) string {
	at, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return v
	}
	if zone == nil {
		zone = time.UTC
	}
	local := at.In(zone)
	if local.Hour() == 0 && local.Minute() == 0 && local.Second() == 0 {
		return local.Format("Mon 2 Jan 2006")
	}
	return local.Format("Mon 2 Jan 2006 15:04 MST")
}

// fit encodes the digest current returns, dropping entries from its end
// (through set) until it is within budget. A digest that fits with none is
// returned with none; one that does not fit even then is an error, because
// the facts themselves are past what a model is handed.
func fit(current func() (any, int), set func(int), budget int) ([]byte, error) {
	_, n := current()
	set(n)
	for {
		v, _ := current()
		out, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("cards: encode a summary: %w", err)
		}
		if len(out) <= budget {
			return out, nil
		}
		if n == 0 {
			return nil, fmt.Errorf("cards: the summary is %d bytes with no rows, past the "+
				"%d a model is handed", len(out), budget)
		}
		n /= 2
		set(n)
	}
}

// emptyDigest is what a model is handed of an answer that held nothing: what
// was read, with the arguments the call asked it with, and the answer's own
// reason where it gave one.
type emptyDigest struct {
	Empty bool   `json:"empty"`
	Read  string `json:"read"`
	// Asked are the call's own arguments, so the model can say what it
	// looked for without restating it from memory.
	Asked map[string]any `json:"asked,omitempty"`
	// Note is the answer's own sentence on why it holds nothing.
	Note   string `json:"note,omitempty"`
	Shown  string `json:"shown"`
	Access string `json:"access"`
}

// EmptyShown is what every empty summary tells the model about the person's
// view of it.
const EmptyShown = "Nothing matched, so the person was shown no table or card. Say in " +
	"a sentence that nothing was found, naming what you looked for."

// EmptySummary is what a model is handed of a read whose answer held nothing
// ([Built.Empty]): the read in the tool's own words, the arguments it was
// asked with and the answer's note, within budget bytes. Arguments that do not
// fit are left out whole rather than cut.
func EmptySummary(read string, asked map[string]any, note string, budget int) ([]byte, error) {
	d := emptyDigest{Empty: true, Read: read, Asked: asked, Note: note, Shown: EmptyShown,
		Access: AccessNote}
	out, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("cards: encode a summary: %w", err)
	}
	if len(out) <= budget {
		return out, nil
	}
	d.Asked = nil
	if out, err = json.Marshal(d); err != nil {
		return nil, fmt.Errorf("cards: encode a summary: %w", err)
	}
	if len(out) > budget {
		return nil, fmt.Errorf("cards: an empty answer's summary is %d bytes, past the %d "+
			"a model is handed", len(out), budget)
	}
	return out, nil
}
