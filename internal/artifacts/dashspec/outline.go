package dashspec

import (
	"fmt"
	"strings"
)

// ---- the outline, the cover and the search text --------------------------- //

// MaxCoverStats is how many stats a gallery cover draws (D64).
const MaxCoverStats = 3

// Outline is a board's structure without its data: what the gallery draws a
// cover from and an agent reads before asking for a widget's rows. It holds
// no number at all, since every number is decided as its viewer.
type Outline struct {
	Title    string           `json:"title,omitempty"`
	Widgets  []OutlineWidget  `json:"widgets"`
	Stats    []OutlineStat    `json:"stats"`
	Datasets []OutlineDataset `json:"datasets"`
}

// OutlineWidget is one widget's place and what it draws.
type OutlineWidget struct {
	ID      string     `json:"id"`
	Type    WidgetType `json:"type"`
	Title   string     `json:"title,omitempty"`
	W       int        `json:"w"`
	Dataset string     `json:"dataset,omitempty"`
	Group   string     `json:"group,omitempty"`
}

// OutlineStat is one of the first stat widgets: its label and the title of
// the dataset it is drawn from, which a cover draws in place of a value the
// gallery does not hold for its viewer.
type OutlineStat struct {
	Widget       string `json:"widget"`
	Label        string `json:"label"`
	Dataset      string `json:"dataset"`
	DatasetTitle string `json:"dataset_title"`
}

// OutlineDataset is one dataset's title and source.
type OutlineDataset struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Source SourceKind `json:"source"`
}

// Outline is s's outline.
func (s *Spec) Outline() Outline {
	out := Outline{Title: s.Title, Widgets: []OutlineWidget{}, Stats: []OutlineStat{},
		Datasets: make([]OutlineDataset, len(s.Datasets))}
	for i, d := range s.Datasets {
		out.Datasets[i] = OutlineDataset{ID: d.ID, Title: d.Title, Source: d.Source}
	}
	for _, w := range s.Widgets {
		out.Widgets = append(out.Widgets, OutlineWidget{ID: w.ID, Type: w.Type, Title: w.Title, W: w.W,
			Dataset: w.Dataset})
		for _, c := range w.Children {
			out.Widgets = append(out.Widgets, OutlineWidget{ID: c.ID, Type: c.Type, Title: c.Title, W: c.W,
				Dataset: c.Dataset, Group: w.ID})
		}
	}
	s.EachWidget(func(w Widget, _ string) bool {
		if w.Type != WidgetStat {
			return true
		}
		d, _ := s.Dataset(w.Dataset)
		out.Stats = append(out.Stats, OutlineStat{Widget: w.ID, Label: w.Title, Dataset: w.Dataset,
			DatasetTitle: d.Title})
		return len(out.Stats) < MaxCoverStats
	})
	return out
}

// Text is the readable words of s, for the gallery's search: the title, the
// filters' labels, the datasets' titles, the widgets' titles and the notes.
// Never a value: a figure is the viewer's, and the search index is shared.
func (s *Spec) Text() string {
	var b strings.Builder
	line := func(t string) {
		if t = strings.TrimSpace(t); t != "" {
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}
	line(s.Title)
	for _, p := range s.Params {
		line(p.Label)
	}
	for _, d := range s.Datasets {
		line(d.Title)
	}
	s.EachWidget(func(w Widget, _ string) bool {
		line(w.Title)
		line(w.Text)
		return true
	})
	return b.String()
}

// ---- the comment anchor (D65) --------------------------------------------- //

// Anchor is where on a board a comment points: a widget, and optionally one
// data point of it (the dataset's row, by its key, and a field). Fixed now,
// with comments in Phase 3, because an anchor shape added after a tag is a
// record format change.
type Anchor struct {
	Widget string `json:"widget"`

	// Row is the key of the row the point is on, in its text form; empty
	// for the widget as a whole.
	Row string `json:"row,omitempty"`

	// Field is the column of the point; empty for the whole row.
	Field string `json:"field,omitempty"`
}

// Region is a's presence region (`widget:<id>`), what Follow and the People
// rail carry for a reader looking at it.
func (a Anchor) Region() string { return "widget:" + a.Widget }

// Check holds a to b: the widget is the board's, and a data point is on a
// dataset with a key and names a column it has. The row itself is not looked
// up: rows are each viewer's, and a comment on a row one viewer cannot read is
// still a comment on the board.
func (b *Board) Check(a Anchor) error {
	w, ok := b.Spec.Widget(a.Widget)
	if !ok {
		return fmt.Errorf("%w: the board has no widget %q", ErrInvalid, a.Widget)
	}
	if a.Row == "" && a.Field == "" {
		return nil
	}
	shape, ok := b.Shapes[w.Dataset]
	switch {
	case !ok:
		return fmt.Errorf("%w: the widget %q draws no dataset, so it has no data point", ErrInvalid, a.Widget)
	case a.Row != "" && shape.Key == "":
		return fmt.Errorf("%w: the dataset %q has no key, so a point names no row of it", ErrInvalid, w.Dataset)
	case a.Field != "":
		if _, ok := shape.column(a.Field); !ok {
			return fmt.Errorf("%w: %q is not a column of %q", ErrInvalid, a.Field, w.Dataset)
		}
	}
	return nil
}
