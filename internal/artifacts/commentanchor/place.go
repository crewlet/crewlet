package commentanchor

import (
	"slices"

	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
)

// State is where a thread's anchor is on the version asked.
type State string

const (
	// StateHere is the anchor found where it was made.
	StateHere State = "here"

	// StateMoved is the anchor's content found at another place on the
	// version, which the placement's anchor names.
	StateMoved State = "moved"

	// StateOutdated is the anchor's content not found on the version: the
	// thread is kept, listed and answerable, and labelled as made.
	StateOutdated State = "outdated"

	// StateLater is a thread made on a version after the one asked.
	StateLater State = "later"
)

// States are every state a placement has.
var States = []State{StateHere, StateMoved, StateOutdated, StateLater}

// Valid reports whether s is a state this build answers.
func (s State) Valid() bool { return slices.Contains(States, s) }

// Placement is a thread's place on one version: its state, and the anchor as
// found there, nil when outdated or later.
type Placement struct {
	State  State   `json:"state"`
	Anchor *Anchor `json:"anchor,omitempty"`
}

// Place finds a thread's anchor, made on version madeOn, on version on, whose
// index is idx. A nil idx finds only the whole version.
func Place(a Anchor, madeOn, on int, idx *Index) Placement {
	if madeOn > on {
		return Placement{State: StateLater}
	}
	if a.Kind == KindVersion {
		return here(a)
	}
	if idx == nil {
		return Placement{State: StateOutdated}
	}
	switch a.Kind {
	case KindHTML:
		return placeHTML(a, idx)
	case KindSlides:
		return placeSlides(a, idx)
	case KindDesign:
		return placeDesign(a, idx)
	}
	return Placement{State: StateOutdated}
}

// here is a placement found where it was made.
func here(a Anchor) Placement { return Placement{State: StateHere, Anchor: &a} }

// placeHTML finds a page, and on it a declared name or, for a point, the page alone.
func placeHTML(a Anchor, idx *Index) Placement {
	if _, ok := idx.pages[a.Page]; !ok {
		return Placement{State: StateOutdated}
	}
	if a.Name != "" && !idx.declared(a.Page, a.Name) {
		return Placement{State: StateOutdated}
	}
	return here(a)
}

// placeSlides finds the slide, then the element at its path with its tag, else
// the first element on the slide with the same tag and text.
func placeSlides(a Anchor, idx *Index) Placement {
	els, ok := idx.slides[a.Slide]
	if !ok {
		return Placement{State: StateOutdated}
	}
	if a.Path == "" {
		return here(a)
	}
	for _, e := range els {
		if e.path == a.Path && e.tag == a.Tag {
			return here(a)
		}
	}
	// Content with no text cannot be told from its siblings, so it is not searched for.
	if a.Text == "" {
		return Placement{State: StateOutdated}
	}
	for _, e := range els {
		if e.tag == a.Tag && e.text == a.Text {
			found := a
			found.Path = e.path
			return Placement{State: StateMoved, Anchor: &found}
		}
	}
	return Placement{State: StateOutdated}
}

// placeDesign finds the board and the point inside it as the canvas sizes it.
func placeDesign(a Anchor, idx *Index) Placement {
	c := canvasformat.Canvas{Boards: idx.boards}
	if c.CheckAnchor(canvasformat.Anchor{Board: a.Board, X: a.X, Y: a.Y}) != nil {
		return Placement{State: StateOutdated}
	}
	return here(a)
}
