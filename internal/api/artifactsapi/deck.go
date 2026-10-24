package artifactsapi

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/artifacts/docindex"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/authz"
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

	// Dashboard is a board's outline: its widgets and its first stats'
	// labels with their datasets' titles, and never a value, which is each
	// viewer's (D64).
	Dashboard *dashspec.Outline `json:"dashboard,omitempty"`

	// Design is a design's boards and their frames, drawn as a schematic.
	Design *canvasformat.Outline `json:"design,omitempty"`

	// Doc is a doc's tabs and the opening of its first, drawn at card size.
	Doc *docindex.Outline `json:"doc,omitempty"`

	// Brand is a brand kit's index, which its card draws: the name, the
	// colours and the faces, and the logos by path (each read through the
	// version's file route).
	Brand *BrandOutline `json:"brand,omitempty"`
}

// BrandOutline is a brand kit's outline: its whole `brand.json` as the brand
// format reads it, which is small (at most brandformat.MaxIndexBytes) and holds
// nothing a reader of the kit may not see.
type BrandOutline struct {
	Document brandformat.Brand `json:"document"`
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
	case artifacts.FormatDashboard:
		b, ok := s.boards.read(w, r, a, v)
		if !ok {
			return
		}
		o := b.Spec.Outline()
		out.Dashboard = &o
	case artifacts.FormatDesign:
		canvas, ok := s.canvases.read(w, r, a, v)
		if !ok {
			return
		}
		o := canvas.Outline()
		out.Design = &o
	case artifacts.FormatBrand:
		kit, ok := s.brands.read(w, r, a, v)
		if !ok {
			return
		}
		out.Brand = &BrandOutline{Document: kit}
	case artifacts.FormatNone:
	}
	httpjson.Write(w, http.StatusOK, out)
}

// SlidesStartersAnswer is what a deck can begin from (`GET
// /artifacts/starters/slides`): every theme, drawn as a deck of every layout
// through the format a publish is held to, the layouts in a picker's order and
// the plan a starter opens with when it names none. The create page draws its
// theme picker and its preview from it, and the deck editor adds a slide from
// the sample of the theme its deck names, so neither parses a slide's markup.
//
// THE COMPANY'S BRAND COMES FIRST whenever one is adopted: its theme,
// [slideformat.BrandTheme], resolved from the adopted brand and sampled like
// the engine's own. With none adopted the brand is absent, never a theme a
// create would refuse.
type SlidesStartersAnswer struct {
	Themes  []SlidesStarterTheme `json:"themes"`
	Layouts []slideformat.Layout `json:"layouts"`
	Default slideformat.Plan     `json:"default"`
}

// SlidesStarterTheme is one theme and its sample deck.
type SlidesStarterTheme struct {
	slideformat.Theme
	Sample slideformat.Deck `json:"sample"`
}

// slidesStarters answers what a deck can begin from, to anyone who may list
// artifacts: the engine's own themes, the same for every caller, after the
// company's adopted brand.
//
// COMPOSED PER REQUEST, because the brand theme is the company's and moves
// with every adoption: an answer kept for the life of the process would offer
// a brand the company has since cleared, or miss the one it adopted.
func (s *Service) slidesStarters(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(w, r, authz.ActionArtifactList); !ok {
		return
	}
	themes := slideformat.Themes()
	adoption, adopted, err := s.reads.Brand(r.Context(), decisionRead)
	if err != nil {
		readFailed(w, r, err, "the adopted brand")
		return
	}
	if adopted {
		b, err := brandformat.Decode(adoption.BrandJSON) //nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err != nil {
			// THE SNAPSHOT WAS CANONICAL WHEN IT WAS ADOPTED; one that no
			// longer reads is the engine's fault, answered as such rather
			// than dropped from the picker.
			log.ErrorContext(r.Context(), "artifacts_adopted_brand_unreadable", "error", err.Error())
			httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
			return
		}
		themes = append([]slideformat.Theme{brandformat.SlideTheme(b)}, themes...)
	}
	out := SlidesStartersAnswer{Themes: make([]SlidesStarterTheme, len(themes)),
		Layouts: slideformat.Layouts(), Default: slideformat.DefaultPlan()}
	for i, t := range themes {
		sample, err := slideformat.Sample(t)
		if err != nil {
			// THE TESTS HOLD EVERY THEME, THE BRAND'S INCLUDED, TO THE
			// FORMAT; a failure here is a broken build, answered as the
			// engine's fault rather than hidden.
			log.ErrorContext(r.Context(), "artifacts_slides_starters_unreadable", "theme", t.ID,
				"error", err.Error())
			httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
			return
		}
		out.Themes[i] = SlidesStarterTheme{Theme: t, Sample: sample}
	}
	httpjson.Write(w, http.StatusOK, out)
}
