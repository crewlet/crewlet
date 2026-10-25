package artifactsapi_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

var updateStarters = flag.Bool("update", false, "rewrite testdata/slides-starters.golden.json from the route's answer")

// A DECK IS ANSWERED AS ITS CANONICAL TREE, never its markup: the slides in
// order with their notes taken out of the drawn tree, read once per version,
// and opening it is recorded as opening any native kind is.
func TestADeckIsAnsweredAsItsCanonicalTree(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	w := g.do(person("bob"), "GET", at(deck, "/versions/1/deck"), ``)
	if w.Code != http.StatusOK {
		t.Fatalf("the deck: %d %s", w.Code, w.Body)
	}
	var got struct {
		Deck struct {
			Title  string
			Cover  string
			Slides []struct {
				ID    string
				Title string
				Notes string
				Root  struct{ Tag string }
			}
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	d := got.Deck
	if d.Title != "Plan" || d.Cover != "next" || len(d.Slides) != 2 || d.Slides[0].Notes != "Hello." ||
		d.Slides[1].Title != "Next steps" || d.Slides[0].Root.Tag != "section" {
		t.Errorf("the deck answered %+v", d)
	}
	if strings.Contains(w.Body.String(), "<section") || strings.Contains(w.Body.String(), `"aside"`) {
		t.Errorf("the answer carries markup or the notes' element: %s", w.Body)
	}
	if len(g.marks.opened) != 1 {
		t.Errorf("reading the deck recorded %d openings", len(g.marks.opened))
	}
	opened := g.objects.opens()
	if w := g.do(person("bob"), "GET", at(deck, "/versions/1/deck"), ``); w.Code != http.StatusOK ||
		g.objects.opens() != opened {
		t.Errorf("a second read of one version opened %d more objects", g.objects.opens()-opened)
	}
}

// THE OUTLINE DRAWS A GALLERY COVER without every slide's tree, records no
// opening, and is decided like any read: a caller with no relation is told
// nothing exists, and a page asked for its deck is refused by kind.
func TestTheOutlineCarriesTheCoverSlideAlone(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	w := g.do(person("bob"), "GET", at(deck, "/versions/1/outline"), ``)
	f := fields(t, w)
	slides, _ := f["slides"].(map[string]any)
	if w.Code != http.StatusOK || f["kind"] != "slides" || slides["count"] != float64(2) {
		t.Fatalf("the outline: %d %s", w.Code, w.Body)
	}
	if cover := slides["cover_slide"].(map[string]any); cover["id"] != "next" {
		t.Errorf("the cover slide is %v, want the index's cover", cover["id"])
	}
	if len(g.marks.opened) != 0 {
		t.Error("a gallery cover recorded an opening")
	}
	if w := g.do(person("bob"), "GET", at(notes, "/versions/1/outline"), ``); w.Code != http.StatusOK ||
		fields(t, w)["slides"] != nil {
		t.Errorf("a document's outline: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("dana"), "GET", at(deck, "/versions/1/outline"), ``); w.Code != http.StatusNotFound {
		t.Errorf("a stranger's outline: %d", w.Code)
	}
	if w := g.do(person("bob"), "GET", at(page, "/versions/2/deck"), ``); w.Code != http.StatusConflict {
		t.Errorf("a page's deck: %d %s", w.Code, w.Body)
	}
}

// A DOC IS ANSWERED WHOLE, every tab in its index's order with its Markdown,
// read once per version, and opening it is recorded as opening any native kind
// is. A doc of one file is one tab.
func TestADocIsAnsweredTabByTab(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	w := g.do(person("bob"), "GET", at(handbook, "/versions/1/doc"), ``)
	if w.Code != http.StatusOK {
		t.Fatalf("the doc: %d %s", w.Code, w.Body)
	}
	var got struct {
		Doc struct {
			Indexed bool
			Tabs    []struct{ Path, Title, Text string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	d := got.Doc
	if !d.Indexed || len(d.Tabs) != 2 || d.Tabs[0].Path != "tabs/start.md" ||
		d.Tabs[1].Title != "On call" || d.Tabs[0].Text != string(startMD) {
		t.Errorf("the doc answered %+v", d)
	}
	if len(g.marks.opened) != 1 {
		t.Errorf("reading the doc recorded %d openings", len(g.marks.opened))
	}
	opened := g.objects.opens()
	if w := g.do(person("bob"), "GET", at(handbook, "/versions/1/doc"), ``); w.Code != http.StatusOK ||
		g.objects.opens() != opened {
		t.Errorf("a second read of one version opened %d more objects", g.objects.opens()-opened)
	}

	w = g.do(person("bob"), "GET", at(notes, "/versions/1/doc"), ``)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK ||
		got.Doc.Indexed || len(got.Doc.Tabs) != 1 || got.Doc.Tabs[0].Text != string(notesMD) {
		t.Errorf("a doc of one file: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("bob"), "GET", at(deck, "/versions/1/doc"), ``); w.Code != http.StatusConflict {
		t.Errorf("a deck's doc: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("dana"), "GET", at(handbook, "/versions/1/doc"), ``); w.Code != http.StatusNotFound {
		t.Errorf("a stranger's doc: %d", w.Code)
	}
}

// A DOC'S OUTLINE names its tabs and carries the first tab's opening for the
// gallery's cover, never the rest of its text, and records no opening.
func TestADocsOutlineCarriesItsOpeningAlone(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	w := g.do(person("bob"), "GET", at(handbook, "/versions/1/outline"), ``)
	f := fields(t, w)
	doc, _ := f["doc"].(map[string]any)
	if w.Code != http.StatusOK || f["kind"] != "doc" || doc == nil || f["slides"] != nil {
		t.Fatalf("the outline: %d %s", w.Code, w.Body)
	}
	tabs, _ := doc["tabs"].([]any)
	cover, _ := doc["cover"].(map[string]any)
	if len(tabs) != 2 || cover["tab"] != "tabs/start.md" || cover["markdown"] != "# Welcome\n\nRead this first." {
		t.Errorf("the doc outline is %v", doc)
	}
	if strings.Contains(w.Body.String(), "Page the lead") {
		t.Errorf("the outline carries a tab past the first: %s", w.Body)
	}
	if len(g.marks.opened) != 0 {
		t.Error("a gallery cover recorded an opening")
	}
}

// A DECK'S PLAN REACHES THE PIPELINE AS SENT: the theme and the slides a
// slides starter opens with, decoded from `deck`.
func TestACreateCarriesItsDeckPlan(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	body := `{"kind":"slides","title":"Plan","starter":true,` +
		`"deck":{"theme":"grove","slides":[{"layout":"cover"},{"layout":"points","title":"Risks"}]}}`
	if w := g.do(person("dana"), "POST", "/artifacts", body, keyed()...); w.Code != http.StatusCreated {
		t.Fatalf("a create with a plan: %d %s", w.Code, w.Body)
	}
	req := g.pipeline.creates[0]
	if !req.Starter || req.Deck == nil || req.Deck.Theme != "grove" || len(req.Deck.Slides) != 2 ||
		req.Deck.Slides[1].Title != "Risks" {
		t.Errorf("the pipeline was asked %+v (plan %+v)", req, req.Deck)
	}
}

// WHAT A DECK CAN BEGIN FROM is answered to anyone who may list artifacts,
// the same for every caller:
// every theme with its sample drawn as the canonical tree, never markup, every
// layout, and the plan a starter opens with when it names none.
func TestTheSlidesStartersAreAnsweredAsTrees(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	w := g.do(person("bob"), "GET", "/artifacts/starters/slides", ``)
	if w.Code != http.StatusOK {
		t.Fatalf("the starters: %d %s", w.Code, w.Body)
	}
	var got struct {
		Themes []struct {
			ID, Name, Tag string
			Sample        struct {
				Theme  string
				Slides []struct{ ID, Title string }
			}
		}
		Layouts []struct{ ID, Name, Description, Heading string }
		Default struct {
			Theme  string
			Slides []struct{ Layout string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Themes) < 2 || len(got.Layouts) < 4 || got.Default.Theme == "" || len(got.Default.Slides) == 0 {
		t.Fatalf("the starters answered %+v", got)
	}
	for _, th := range got.Themes {
		if th.Name == "" || th.Tag == "" || th.Sample.Theme != th.ID || len(th.Sample.Slides) != len(got.Layouts) {
			t.Errorf("theme %s answered %+v", th.ID, th)
		}
	}
	if strings.Contains(w.Body.String(), "<section") || strings.Contains(w.Body.String(), `"Feature"`) {
		t.Errorf("the answer carries markup or a theme's palette fields: %.300s", w.Body)
	}
}

// THE CREATE PAGE IS TESTED ON THIS ROUTE'S OWN ANSWER: what a deck can begin
// from is held in testdata/slides-starters.golden.json, which the dashboard's
// plan and create page tests draw. A theme or layout change fails here until
// the golden is rewritten (`go test ./internal/api/artifactsapi -run
// StartersGolden -update`), so those tests never certify trees the engine no
// longer sends.
func TestTheSlidesStartersGoldenIsTheRoutesAnswer(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	w := g.do(person("bob"), "GET", "/artifacts/starters/slides", ``)
	if w.Code != http.StatusOK {
		t.Fatalf("the starters: %d %s", w.Code, w.Body)
	}
	var tree any
	if err := json.Unmarshal(w.Body.Bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/slides-starters.golden.json"
	if *updateStarters {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: run with -update to write it", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is not the route's answer: run with -update and review the diff", path)
	}
}

// THE COMPANY'S BRAND IS THE FIRST THEME A DECK CAN BEGIN IN, whenever one is
// adopted: answered per request, so an adoption shows at once and a cleared
// brand is gone at once, its sample drawn through the slide format like any
// theme's, its cover drawing the logo a deck begun in it shows (at the path
// the starter copies it to, named with the adopted brand's slot it is drawn
// from), and the default plan untouched (a plan naming no theme is the
// engine's own).
func TestTheSlidesStartersOfferTheAdoptedBrandFirst(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	starters := func() artifactsapi.SlidesStartersAnswer {
		t.Helper()
		w := g.do(person("bob"), "GET", "/artifacts/starters/slides", ``)
		if w.Code != http.StatusOK {
			t.Fatalf("the starters: %d %s", w.Code, w.Body)
		}
		var got artifactsapi.SlidesStartersAnswer
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := starters()
	for _, th := range before.Themes {
		if th.ID == slideformat.BrandTheme {
			t.Fatalf("with no brand adopted the starters offer %q", th.ID)
		}
	}

	b := brandformat.Brand{V: brandformat.Version, Name: "Acme",
		Logos:  brandformat.Logos{Primary: "logo.svg"},
		Colors: brandformat.Colors{Primary: "#0f7b5f"},
		Type:   brandformat.Type{Heading: brandformat.Face{Kit: "geist"}, Body: brandformat.Face{Stack: "system-sans"}}}
	canonical, err := brandformat.Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	g.estate.mu.Lock()
	g.estate.adoption = &artifactsdomain.Adoption{BrandJSON: canonical, Files: []artifactsdomain.BrandFile{
		{Slot: string(brandformat.SlotLogoPrimary), ContentType: "image/svg+xml", Object: entryLogo.Object},
	}}
	g.estate.mu.Unlock()
	after := starters()
	if len(after.Themes) != len(before.Themes)+1 {
		t.Fatalf("an adopted brand answered %d themes, want %d", len(after.Themes), len(before.Themes)+1)
	}
	first := after.Themes[0]
	if first.ID != slideformat.BrandTheme || first.Name != "Acme" || first.Sample.Theme != slideformat.BrandTheme ||
		len(first.Sample.Slides) != len(after.Layouts) {
		t.Fatalf("the brand theme answered %+v with a sample of %d slides", first.Theme, len(first.Sample.Slides))
	}
	if after.Default.Theme != before.Default.Theme {
		t.Errorf("the default plan moved to %q", after.Default.Theme)
	}
	const logo = "brand/logo-primary.svg"
	if first.Logo == nil || *first.Logo != (artifactsapi.SampleLogo{Path: logo, Slot: "logo.primary"}) {
		t.Errorf("the brand theme names its cover logo %+v", first.Logo)
	}
	if !holdsImage(first.Sample.Slides[0].Root, logo) {
		t.Errorf("the brand sample's cover draws no logo at %s", logo)
	}
	for _, th := range after.Themes[1:] {
		if th.Logo != nil {
			t.Errorf("the engine's theme %q names a logo", th.ID)
		}
	}

	g.estate.mu.Lock()
	g.estate.adoption = nil
	g.estate.mu.Unlock()
	if cleared := starters(); len(cleared.Themes) != len(before.Themes) {
		t.Errorf("a cleared brand still answers %d themes", len(cleared.Themes))
	}
}

// holdsImage reports whether n or any node under it is an image of src.
func holdsImage(n slideformat.Node, src string) bool {
	if n.Tag == "img" && n.Attrs["src"] == src {
		return true
	}
	for _, c := range n.Children {
		if holdsImage(c, src) {
			return true
		}
	}
	return false
}
