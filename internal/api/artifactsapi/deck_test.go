package artifactsapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

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
