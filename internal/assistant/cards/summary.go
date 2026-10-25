package cards

import (
	"encoding/json"
	"fmt"

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
	switch card.Type {
	case wire.CardTable:
		return tableSummary(card.Table, budget)
	case wire.CardEntity:
		return entitySummary(card.Entity, budget)
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
	Note      string   `json:"note,omitempty"`
	// Access is the sentence every list carries: what the person may not
	// see is left out, so the model never reads an absence as proof.
	Access string `json:"access"`
}

// AccessNote is what every table summary tells the model about its rows.
const AccessNote = "Rows the person may not see are left out, so a row missing here " +
	"is not proof that none exists."

func tableSummary(t *wire.TableCard, budget int) ([]byte, error) {
	d := tableDigest{Title: t.Title, RowCount: t.RowCount, Rows: t.Rows, Access: AccessNote}
	for _, c := range t.Columns {
		d.Columns = append(d.Columns, c.Label)
	}
	return fit(func() (any, int) { return &d, len(d.Rows) }, func(n int) {
		d.Rows, d.Shown = t.Rows[:n], n
		d.Truncated = n < t.RowCount
		if d.Truncated {
			d.Note = fmt.Sprintf("Only %d of %d rows are listed here; the person "+
				"sees the table on the card.", n, t.RowCount)
		}
	}, budget)
}

type entityDigest struct {
	Kind         wire.ResourceKind `json:"kind"`
	ID           string            `json:"id,omitempty"`
	Title        string            `json:"title"`
	Subtitle     string            `json:"subtitle,omitempty"`
	Facts        map[string]string `json:"facts,omitempty"`
	Text         string            `json:"text,omitempty"`
	TextOmitted  int               `json:"text_omitted_bytes,omitempty"`
	Items        []itemDigest      `json:"items,omitempty"`
	ItemsShown   int               `json:"items_shown,omitempty"`
	ItemsTotal   int               `json:"items_total,omitempty"`
	ItemsPartial string            `json:"note,omitempty"`
}

type itemDigest struct {
	ID    string `json:"id,omitempty"`
	Label string `json:"label"`
	Fact  string `json:"fact,omitempty"`
	Depth int    `json:"depth,omitempty"`
}

func entitySummary(e *wire.EntityCard, budget int) ([]byte, error) {
	d := entityDigest{Kind: e.Kind, ID: e.ID, Title: e.Title, Subtitle: e.Subtitle,
		Text: e.Text}
	if len(e.Facts) > 0 {
		d.Facts = map[string]string{}
		for _, f := range e.Facts {
			d.Facts[f.Label] = f.Value
		}
	}
	items := make([]itemDigest, 0, len(e.Items))
	for _, it := range e.Items {
		items = append(items, itemDigest{ID: it.ID, Label: it.Label, Fact: it.Fact,
			Depth: it.Depth})
	}
	d.ItemsTotal = len(items)
	set := func(n int) {
		d.Items, d.ItemsShown = items[:n], n
		d.ItemsPartial = ""
		if n < len(items) {
			d.ItemsPartial = fmt.Sprintf("Only %d of %d entries are listed here; the "+
				"person sees them all on the card.", n, len(items))
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
