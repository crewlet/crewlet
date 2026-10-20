package artifactupload_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactupload"
)

// A STARTER TAKES ONLY ITS OWN KIND'S FIELD: a doc's text, a deck's plan and a
// design's layout each begin their own kind, so one sent beside a valid field
// of the kind being made is refused before a byte moves, never ignored.
func TestAStarterRefusesAnotherKindsField(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	doc := &artifactupload.DocStarter{Body: "Text"}
	deck := &slideformat.Plan{Slides: []slideformat.PlanSlide{{Layout: "cover"}}}
	layout := &canvasformat.Layout{Boards: []canvasformat.LayoutBoard{{Title: "Home", W: 1440, H: 1024}}}
	for name, req := range map[string]artifactupload.CreateRequest{
		"a deck with a doc's text": {Kind: artifacts.KindSlides, Title: "Deck", Deck: deck, Doc: doc},
		"a deck with a layout":     {Kind: artifacts.KindSlides, Title: "Deck", Deck: deck, Layout: layout},
		"a doc with a deck plan":   {Kind: artifacts.KindDoc, Title: "Doc", Doc: doc, Deck: deck},
		"a doc with a layout":      {Kind: artifacts.KindDoc, Title: "Doc", Doc: doc, Layout: layout},
		"a design with doc text":   {Kind: artifacts.KindDesign, Title: "Design", Layout: layout, Doc: doc},
		"a design with a plan":     {Kind: artifacts.KindDesign, Title: "Design", Layout: layout, Deck: deck},
	} {
		c := h.person("ada", h.opKey(name))
		req.Owner, req.OwnerKind, req.Starter = "ada", c.Authority.Actor.Kind, true
		if _, _, err := h.svc.Create(t.Context(), c, req); !errors.Is(err, artifactupload.ErrInvalid) {
			t.Errorf("%s answered %v", name, err)
		}
	}
}
