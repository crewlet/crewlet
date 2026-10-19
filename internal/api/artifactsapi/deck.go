package artifactsapi

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/docindex"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
)

// HOW THE DASHBOARD DRAWS A NATIVE KIND: it never reads a file's markup. The
// engine reads the version through the same format the publish was held to (a
// deck through internal/artifacts/slideformat, a doc through
// internal/artifacts/docindex) and answers it as JSON, which the dashboard's
// element-constructing renderers turn into elements. One format decides what a
// version is, on publish and on read.

// DeckAnswer is a version's whole deck (`GET /artifacts/{id}/versions/{n}/deck`).
type DeckAnswer struct {
	Artifact artifacts.ID     `json:"artifact"`
	Version  int              `json:"version"`
	Deck     slideformat.Deck `json:"deck"`
}

// OutlineAnswer is a version's structure without every file (`GET
// /artifacts/{id}/versions/{n}/outline`): what the gallery draws a native
// cover from. A kind with no structure to outline answers its kind alone.
type OutlineAnswer struct {
	Artifact artifacts.ID   `json:"artifact"`
	Kind     artifacts.Kind `json:"kind"`
	Version  int            `json:"version"`

	// Slides is a deck's outline and its cover slide, drawn at card size.
	Slides *SlidesOutline `json:"slides,omitempty"`

	// Design is a design's boards and their frames, drawn as a schematic.
	Design *canvasformat.Outline `json:"design,omitempty"`

	// Doc is a doc's tabs and the opening of its first, drawn at card size.
	Doc *docindex.Outline `json:"doc,omitempty"`
}

// DocAnswer is a version's whole doc (`GET /artifacts/{id}/versions/{n}/doc`):
// every tab, in order, with its Markdown.
type DocAnswer struct {
	Artifact artifacts.ID `json:"artifact"`
	Version  int          `json:"version"`
	Doc      docindex.Doc `json:"doc"`
}

// SlidesOutline is a deck's outline and the one slide its cover draws.
type SlidesOutline struct {
	slideformat.Outline
	CoverSlide slideformat.Slide `json:"cover_slide"`
}

// deck answers a version's whole deck, decided by `get_artifact`, recording
// the open as reading a native kind's content does.
func (s *Service) deck(w http.ResponseWriter, r *http.Request) {
	a, v, n, ok := s.formatVersion(w, r, artifacts.FormatSlides, "a deck")
	if !ok {
		return
	}
	deck, ok := s.decks.read(w, r, a, v)
	if !ok {
		return
	}
	s.gate.markOpened(r.Context(), a)
	httpjson.Write(w, http.StatusOK, DeckAnswer{Artifact: a.Head.ID, Version: n, Deck: deck})
}

// doc answers a version's whole doc, decided by `get_artifact`, recording the
// open as reading a native kind's content does.
func (s *Service) doc(w http.ResponseWriter, r *http.Request) {
	a, v, n, ok := s.formatVersion(w, r, artifacts.FormatDoc, "a doc")
	if !ok {
		return
	}
	doc, ok := s.docs.read(w, r, a, v)
	if !ok {
		return
	}
	s.gate.markOpened(r.Context(), a)
	httpjson.Write(w, http.StatusOK, DocAnswer{Artifact: a.Head.ID, Version: n, Doc: doc})
}

// outline answers a version's outline, decided by `get_artifact`. A cover
// drawn in the gallery is not an open, so none is recorded.
func (s *Service) outline(w http.ResponseWriter, r *http.Request) {
	n, ok := pathVersion(w, r)
	if !ok {
		return
	}
	a, ok := s.admitContent(w, r, n)
	if !ok {
		return
	}
	v, ok := s.versionOf(w, r, a, n)
	if !ok {
		return
	}
	out := OutlineAnswer{Artifact: a.Head.ID, Kind: a.Head.Kind, Version: n}
	spec, _ := a.Head.Kind.Spec()
	switch spec.Format {
	case artifacts.FormatSlides:
		deck, ok := s.decks.read(w, r, a, v)
		if !ok {
			return
		}
		cover, _ := deck.CoverSlide()
		out.Slides = &SlidesOutline{Outline: deck.Outline(), CoverSlide: cover}
	case artifacts.FormatDoc:
		doc, ok := s.docs.read(w, r, a, v)
		if !ok {
			return
		}
		o := doc.Outline()
		out.Doc = &o
	case artifacts.FormatDesign:
		canvas, ok := s.canvases.read(w, r, a, v)
		if !ok {
			return
		}
		o := canvas.Outline()
		out.Design = &o
	case artifacts.FormatNone:
	}
	httpjson.Write(w, http.StatusOK, out)
}
