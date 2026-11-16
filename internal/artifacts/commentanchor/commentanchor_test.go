package commentanchor_test

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/commentanchor"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
)

// el is an element node; txt a run of text.
func el(tag string, children ...slideformat.Node) slideformat.Node {
	return slideformat.Node{Tag: tag, Children: children}
}

func txt(s string) slideformat.Node { return slideformat.Node{Text: s} }

// deck is a deck of slides, each slide id and its section's children.
func deck(slides ...slideformat.Slide) slideformat.Deck { return slideformat.Deck{Slides: slides} }

func slide(id string, children ...slideformat.Node) slideformat.Slide {
	return slideformat.Slide{ID: id, Path: slideformat.SlidePath(id), Root: el("section", children...)}
}

// canvas is a design's canvas with one board.
func canvas(board string, w, h int) canvasformat.Canvas {
	return canvasformat.Canvas{Boards: map[string]canvasformat.Board{board: {W: w, H: h}},
		Order: []string{board}}
}

// A thread finds its place by WHAT IT POINTS AT, not by where that sat when the
// thread was made: an element pushed down its slide is found by its tag and
// text and answered as moved, with the path it has now; a name declared on a
// page is found on that page whatever else the page changed; a point on a board
// is found while the board still holds it, wherever the board moved on the canvas.
//
// Mutation: find a slide's element by its path alone, or drop the shared
// script's declaration, and this goes red.
func TestAPlaceIsFoundByItsContentNotItsPosition(t *testing.T) {
	t.Parallel()

	made := commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "results",
		Path: "1", Tag: "p", Text: "Revenue grew 40%"}
	v1 := commentanchor.IndexSlides(deck(slide("results",
		el("h1", txt("Results")), el("p", txt("Revenue  grew\n40%")))))
	if got := commentanchor.Place(made, 1, 1, v1); got.State != commentanchor.StateHere ||
		got.Anchor == nil || got.Anchor.Path != "1" {
		t.Fatalf("on its own version the element answered %+v", got)
	}
	// A heading and a chart were added above it, so path 1 is now another element.
	v2 := commentanchor.IndexSlides(deck(slide("results",
		el("h1", txt("Results")), el("div", el("p", txt("Chart"))),
		el("div", el("p", txt("Revenue grew "), el("b", txt("40%")))))))
	got := commentanchor.Place(made, 1, 2, v2)
	if got.State != commentanchor.StateMoved || got.Anchor == nil {
		t.Fatalf("an element pushed down its slide answered %+v, want moved", got)
	}
	if got.Anchor.Path != "2/0" || got.Anchor.Tag != "p" || got.Anchor.Text != made.Text {
		t.Errorf("the moved element was found at %+v, want path 2/0 with its tag and text", *got.Anchor)
	}
	if made.Path != "1" {
		t.Errorf("placing the anchor rewrote the thread's own anchor to %q", made.Path)
	}

	// A name is found on its page, declared by the attribute or by a script.
	site := commentanchor.IndexHTML([]commentanchor.File{
		{Path: "index.html", Data: []byte(`<main><section data-crewlet-anchor="comparison">` +
			`</section></main><script src="app.js"></script>`)},
		{Path: "about/team.htm", Data: []byte(`<div data-crewlet-anchor = 'people'></div>`)},
		{Path: "app.js", Data: []byte(`crewlet.comments.anchor("pricing", el);`)},
	})
	for _, a := range []commentanchor.Anchor{
		{Kind: commentanchor.KindHTML, Page: "index.html", Name: "comparison"},
		{Kind: commentanchor.KindHTML, Page: "index.html", Name: "pricing"},
		{Kind: commentanchor.KindHTML, Page: "about/team.htm", Name: "people"},
		{Kind: commentanchor.KindHTML, Page: "about/team.htm", Point: &commentanchor.Point{X: 0.5, Y: 0.1}},
	} {
		if got := commentanchor.Place(a, 1, 3, site); got.State != commentanchor.StateHere {
			t.Errorf("%+v answered %s on a version that declares it", a, got.State)
		}
	}

	// A point on a board stays on the board wherever the board sits on the canvas.
	design := canvas("boards/welcome.html", 1440, 900)
	moved := design.Boards["boards/welcome.html"]
	moved.X, moved.Y = 5000, -300
	design.Boards["boards/welcome.html"] = moved
	point := commentanchor.Anchor{Kind: commentanchor.KindDesign, Board: "boards/welcome.html", X: 195, Y: 760}
	if got := commentanchor.Place(point, 1, 4, commentanchor.IndexCanvas(design)); got.State != commentanchor.StateHere {
		t.Errorf("a point inside a board moved on the canvas answered %s", got.State)
	}

	// The whole version is here on every version at or after the one it was made on.
	if got := commentanchor.Place(commentanchor.Whole(), 2, 9, nil); got.State != commentanchor.StateHere {
		t.Errorf("a whole-version thread answered %s", got.State)
	}
}

// A thread whose place is gone is OUTDATED, never dropped: a page, a name, a
// slide, an element or a board the version no longer holds, or a point the
// board no longer covers, each answers a placement saying so, and the thread
// still labels as it was made. A thread made after the version asked answers
// later rather than outdated.
//
// Mutation: answer a missing slide as here, or a point past the board's edge as
// inside it, and this goes red.
func TestAnAnchorThatIsGoneIsOutdatedNeverDropped(t *testing.T) {
	t.Parallel()
	site := commentanchor.IndexHTML([]commentanchor.File{
		{Path: "index.html", Data: []byte(`<div data-crewlet-anchor="kept"></div>`)},
		{Path: "other.js", Data: []byte(`comments.anchor(name)`)},
	})
	slides := commentanchor.IndexSlides(deck(slide("intro", el("h1", txt("Welcome")))))
	boards := commentanchor.IndexCanvas(canvas("boards/welcome.html", 400, 300))

	cases := []struct {
		name string
		a    commentanchor.Anchor
		idx  *commentanchor.Index
	}{
		{"a removed page", commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "gone.html",
			Point: &commentanchor.Point{X: 0.2, Y: 0.2}}, site},
		{"a name no longer declared", commentanchor.Anchor{Kind: commentanchor.KindHTML,
			Page: "index.html", Name: "comparison"}, site},
		{"a removed slide", commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "results"}, slides},
		{"a removed element", commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "intro",
			Path: "1", Tag: "p", Text: "Gone"}, slides},
		{"an element whose tag changed at its path", commentanchor.Anchor{
			Kind: commentanchor.KindSlides, Slide: "intro", Path: "0", Tag: "h2"}, slides},
		{"a removed board", commentanchor.Anchor{Kind: commentanchor.KindDesign,
			Board: "boards/pricing.html", X: 1, Y: 1}, boards},
		{"a point past the board's edge", commentanchor.Anchor{Kind: commentanchor.KindDesign,
			Board: "boards/welcome.html", X: 400, Y: 10}, boards},
		{"a place with no index", commentanchor.Anchor{Kind: commentanchor.KindHTML,
			Page: "index.html", Name: "kept"}, nil},
		{"an html anchor on a deck's index", commentanchor.Anchor{Kind: commentanchor.KindHTML,
			Page: "index.html", Name: "kept"}, slides},
	}
	for _, c := range cases {
		got := commentanchor.Place(c.a, 1, 2, c.idx)
		if got.State != commentanchor.StateOutdated {
			t.Errorf("%s answered %s, want outdated", c.name, got.State)
		}
		if got.Anchor != nil {
			t.Errorf("%s answered a place %+v for an anchor that is gone", c.name, *got.Anchor)
		}
		if label := commentanchor.Label(c.a, slides.SlideOrder()); label == "" {
			t.Errorf("%s labels as nothing", c.name)
		}
	}
	if got := commentanchor.Label(cases[2].a, slides.SlideOrder()); got != "A removed slide" {
		t.Errorf("a removed slide labels as %q", got)
	}

	kept := commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "index.html", Name: "kept"}
	if got := commentanchor.Place(kept, 1, 2, site); got.State != commentanchor.StateHere {
		t.Fatalf("control: a declared name answered %s", got.State)
	}
	if got := commentanchor.Place(kept, 3, 2, site); got.State != commentanchor.StateLater || got.Anchor != nil {
		t.Errorf("a thread made on version 3 asked about version 2 answered %+v, want later", got)
	}
	if got := commentanchor.Place(commentanchor.Whole(), 3, 2, nil); got.State != commentanchor.StateLater {
		t.Errorf("a whole-version thread made later answered %s, want later", got.State)
	}
}

// EVERY ARTIFACT KIND SAYS WHERE ITS COMMENTS MAY POINT: a row for each entry of
// the kind table, each taking the whole version and otherwise only the anchor
// named for its own kind, and every anchor kind the row leaves out refused with
// a sentence naming whole-version comments. A kind added to the table and not
// here fails, so a new kind states its anchors.
//
// Mutation: drop a kind's row from the table, or let a doc take an html anchor,
// and this goes red.
func TestEveryKindAnswersWhereItsCommentsMayPoint(t *testing.T) {
	t.Parallel()
	examples := map[commentanchor.Kind]commentanchor.Anchor{
		commentanchor.KindVersion: commentanchor.Whole(),
		commentanchor.KindHTML: {Kind: commentanchor.KindHTML, Page: "index.html",
			Name: "comparison"},
		commentanchor.KindSlides: {Kind: commentanchor.KindSlides, Slide: "intro", Path: "0",
			Tag: "h1", Text: "Welcome"},
		commentanchor.KindDesign: {Kind: commentanchor.KindDesign, Board: "boards/a.html", X: 1, Y: 2},
	}
	for _, k := range commentanchor.Kinds {
		if _, ok := examples[k]; !ok {
			t.Fatalf("anchor kind %s has no example here", k)
		}
	}
	for _, kind := range artifacts.Kinds {
		accepted, ok := commentanchor.Accepts(kind)
		if !ok {
			t.Errorf("artifact kind %s has no row saying where its comments may point", kind)
			continue
		}
		if !slices.Contains(accepted, commentanchor.KindVersion) {
			t.Errorf("artifact kind %s does not take whole-version comments: %v", kind, accepted)
		}
		for _, k := range accepted {
			if k != commentanchor.KindVersion && string(k) != string(kind) {
				t.Errorf("artifact kind %s takes the %s anchor, which is another kind's", kind, k)
			}
		}
		for _, k := range commentanchor.Kinds {
			err := commentanchor.Check(kind, examples[k])
			if slices.Contains(accepted, k) {
				if err != nil {
					t.Errorf("a %s artifact refused the %s anchor it takes: %v", kind, k, err)
				}
				continue
			}
			if !errors.Is(err, commentanchor.ErrInvalid) {
				t.Errorf("a %s artifact took a %s anchor (%v)", kind, k, err)
				continue
			}
			if !strings.Contains(err.Error(), "whole-version comments") {
				t.Errorf("a %s artifact refused a %s anchor without naming whole-version "+
					"comments: %v", kind, k, err)
			}
		}
	}
	if _, ok := commentanchor.Accepts("spreadsheet"); ok {
		t.Error("a kind the artifacts table does not serve has a row")
	}
	if err := commentanchor.Check("spreadsheet", commentanchor.Whole()); !errors.Is(err, commentanchor.ErrInvalid) {
		t.Errorf("a comment on a kind this build does not serve answered %v", err)
	}
}

// A MALFORMED ANCHOR IS REFUSED, each for its own reason, before a thread
// stores it; every refusal is an ErrInvalid.
func TestAMalformedAnchorIsRefused(t *testing.T) {
	t.Parallel()
	pt := func(x, y float64) *commentanchor.Point { return &commentanchor.Point{X: x, Y: y} }
	cases := []struct {
		name string
		kind artifacts.Kind
		a    commentanchor.Anchor
		says string
	}{
		{"no kind", artifacts.KindHTML, commentanchor.Anchor{}, "names no kind"},
		{"an unknown kind", artifacts.KindHTML, commentanchor.Anchor{Kind: "cell"}, "not one of"},
		{"a whole version with a page", artifacts.KindHTML,
			commentanchor.Anchor{Kind: commentanchor.KindVersion, Page: "index.html"}, `"page"`},
		{"an html anchor with a slide", artifacts.KindHTML, commentanchor.Anchor{
			Kind: commentanchor.KindHTML, Page: "index.html", Name: "a", Slide: "s"}, `"slide"`},
		{"no page", artifacts.KindHTML, commentanchor.Anchor{Kind: commentanchor.KindHTML,
			Name: "a"}, "no page"},
		{"a page that is not HTML", artifacts.KindHTML, commentanchor.Anchor{
			Kind: commentanchor.KindHTML, Page: "app.js", Name: "a"}, "not an HTML file"},
		{"a page outside the version's grammar", artifacts.KindHTML, commentanchor.Anchor{
			Kind: commentanchor.KindHTML, Page: "../index.html", Name: "a"}, "page"},
		{"a page not as stored", artifacts.KindHTML, commentanchor.Anchor{
			Kind: commentanchor.KindHTML, Page: "/index.html", Name: "a"}, "as the version stores it"},
		{"neither name nor point", artifacts.KindHTML, commentanchor.Anchor{
			Kind: commentanchor.KindHTML, Page: "index.html"}, "a declared name or a point"},
		{"both name and point", artifacts.KindHTML, commentanchor.Anchor{
			Kind: commentanchor.KindHTML, Page: "index.html", Name: "a", Point: pt(0, 0)}, "not both"},
		{"a name with a capital", artifacts.KindHTML, commentanchor.Anchor{
			Kind: commentanchor.KindHTML, Page: "index.html", Name: "Pricing"}, "not a declared name"},
		{"a name too long", artifacts.KindHTML, commentanchor.Anchor{
			Kind: commentanchor.KindHTML, Page: "index.html", Name: strings.Repeat("a", 65)}, "not a declared name"},
		{"a point past the page", artifacts.KindHTML, commentanchor.Anchor{
			Kind: commentanchor.KindHTML, Page: "index.html", Point: pt(1.2, 0)}, "outside the page"},
		{"a point that is not a number", artifacts.KindHTML, commentanchor.Anchor{
			Kind: commentanchor.KindHTML, Page: "index.html", Point: pt(math.NaN(), 0)}, "outside the page"},
		{"no slide", artifacts.KindSlides, commentanchor.Anchor{Kind: commentanchor.KindSlides},
			"names its slide"},
		{"a slide id outside its grammar", artifacts.KindSlides, commentanchor.Anchor{
			Kind: commentanchor.KindSlides, Slide: "a b"}, "not a slide id"},
		{"a whole slide with a tag", artifacts.KindSlides, commentanchor.Anchor{
			Kind: commentanchor.KindSlides, Slide: "intro", Tag: "p"}, "whole slide"},
		{"a path that is not indexes", artifacts.KindSlides, commentanchor.Anchor{
			Kind: commentanchor.KindSlides, Slide: "intro", Path: "0/x", Tag: "p"}, "child indexes"},
		{"a path too deep", artifacts.KindSlides, commentanchor.Anchor{
			Kind: commentanchor.KindSlides, Slide: "intro",
			Path: strings.Repeat("0/", commentanchor.MaxPathSteps) + "0", Tag: "p"}, "child indexes"},
		{"an element with no tag", artifacts.KindSlides, commentanchor.Anchor{
			Kind: commentanchor.KindSlides, Slide: "intro", Path: "0"}, "names the element's tag"},
		{"a tag no slide holds", artifacts.KindSlides, commentanchor.Anchor{
			Kind: commentanchor.KindSlides, Slide: "intro", Path: "0", Tag: "script"}, "not an element"},
		{"text past the bound", artifacts.KindSlides, commentanchor.Anchor{
			Kind: commentanchor.KindSlides, Slide: "intro", Path: "0", Tag: "p",
			Text: strings.Repeat("é", commentanchor.MaxTextRunes+1)}, "at most"},
		{"no board", artifacts.KindDesign, commentanchor.Anchor{Kind: commentanchor.KindDesign,
			X: 1}, "no board"},
		{"a negative point", artifacts.KindDesign, commentanchor.Anchor{
			Kind: commentanchor.KindDesign, Board: "a.html", X: -1}, "outside every board"},
		{"a point past every board", artifacts.KindDesign, commentanchor.Anchor{
			Kind: commentanchor.KindDesign, Board: "a.html", Y: canvasformat.MaxSide}, "outside every board"},
	}
	for _, c := range cases {
		err := commentanchor.Check(c.kind, c.a)
		if !errors.Is(err, commentanchor.ErrInvalid) {
			t.Errorf("%s: answered %v, want a refusal", c.name, err)
			continue
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: refused as %q, want it to say %q", c.name, err, c.says)
		}
	}

	// Controls: the edges each rule allows.
	for _, ok := range []struct {
		kind artifacts.Kind
		a    commentanchor.Anchor
	}{
		{artifacts.KindHTML, commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "a/b.HTM", Point: pt(0, 1)}},
		{artifacts.KindHTML, commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "index.html",
			Name: "0" + strings.Repeat("a._-", 15) + "abc"}},
		{artifacts.KindSlides, commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "intro"}},
		{artifacts.KindSlides, commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "intro",
			Path: "0/12/3", Tag: "linearGradient"}},
		{artifacts.KindSlides, commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "intro",
			Path: "0", Tag: "x-shape", Text: strings.Repeat("é", commentanchor.MaxTextRunes)}},
		{artifacts.KindDesign, commentanchor.Anchor{Kind: commentanchor.KindDesign, Board: "a.html"}},
		{artifacts.KindDesign, commentanchor.Anchor{Kind: commentanchor.KindDesign, Board: "a.html",
			X: canvasformat.MaxSide - 1, Y: canvasformat.MaxSide - 1}},
	} {
		if err := commentanchor.Check(ok.kind, ok.a); err != nil {
			t.Errorf("control %+v refused: %v", ok.a, err)
		}
	}
}

// A LABEL SAYS WHERE A THREAD POINTS in a person's words: the whole version, a
// section by its name's words, a point, a slide by its number and the quoted
// text of the element, and a board by its file's name and the point.
func TestALabelSaysWhereTheThreadPoints(t *testing.T) {
	t.Parallel()
	order := []string{"intro", "results"}
	for _, c := range []struct {
		a    commentanchor.Anchor
		want string
	}{
		{commentanchor.Whole(), "The whole version"},
		{commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "index.html", Name: "comparison"},
			"Section: Comparison"},
		{commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "index.html", Name: "pricing-table_v2.1"},
			"Section: Pricing table v2 1"},
		{commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "index.html",
			Point: &commentanchor.Point{X: 0.4, Y: 0.6}}, "A point on the page"},
		{commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "results", Path: "0", Tag: "h1",
			Text: "Results"}, "Slide 2: “Results”"},
		{commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "intro"}, "Slide 1"},
		{commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "appendix", Path: "1", Tag: "p",
			Text: "Sources"}, "A removed slide: “Sources”"},
		{commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "intro", Path: "1", Tag: "p",
			Text: strings.Repeat("word ", 20)},
			"Slide 1: “" + strings.TrimSpace(strings.Repeat("word ", 12)) + "…”"},
		{commentanchor.Anchor{Kind: commentanchor.KindDesign, Board: "boards/welcome.html", X: 195, Y: 760},
			"Welcome board, at 195, 760"},
		{commentanchor.Anchor{Kind: commentanchor.KindDesign, Board: "sign-up.HTM", X: 0, Y: 4},
			"Sign up board, at 0, 4"},
	} {
		if got := commentanchor.Label(c.a, order); got != c.want {
			t.Errorf("%+v labels as %q, want %q", c.a, got, c.want)
		}
	}
}

// THE HTML INDEX READS BOTH DECLARATIONS, in either quote, per page, a
// script's names counting on every page, and refuses a name outside the
// grammar rather than keeping it.
func TestTheHTMLIndexReadsTheNamesAPageDeclares(t *testing.T) {
	t.Parallel()
	idx := commentanchor.IndexHTML([]commentanchor.File{
		{Path: "index.html", Data: []byte(`<a data-crewlet-anchor="one"></a>` +
			`<b data-crewlet-anchor='two'></b><i data-crewlet-anchor="Bad Name"></i>` +
			`<script>comments.anchor('three', x)</script>`)},
		{Path: "docs/page.htm", Data: []byte(`<p>nothing declared</p>`)},
		{Path: "lib/anchors.mjs", Data: []byte(`export const go = () => comments.anchor( "four", el)`)},
		{Path: "style.css", Data: []byte(`/* data-crewlet-anchor="css" */`)},
		{Path: "notes.txt", Data: []byte(`comments.anchor("text")`)},
	})
	if got, want := idx.Pages(), []string{"docs/page.htm", "index.html"}; !slices.Equal(got, want) {
		t.Errorf("pages are %v, want %v", got, want)
	}
	if got, want := idx.Names("index.html"), []string{"four", "one", "three", "two"}; !slices.Equal(got, want) {
		t.Errorf("index.html declares %v, want %v", got, want)
	}
	if got, want := idx.Names("docs/page.htm"), []string{"four"}; !slices.Equal(got, want) {
		t.Errorf("docs/page.htm declares %v, want the script's %v", got, want)
	}
	if got := idx.Names("style.css"); got != nil {
		t.Errorf("a stylesheet is a page declaring %v", got)
	}
	var none *commentanchor.Index
	if none.Pages() != nil || none.Names("index.html") != nil || none.SlideOrder() != nil {
		t.Error("a nil index answers something")
	}
}

// AN ELEMENT'S TEXT is every run beneath it, a break as a space, whitespace
// collapsed, cut at the bound: the one rule the index and an anchor's maker share.
func TestAnElementsTextIsItsRunsCollapsed(t *testing.T) {
	t.Parallel()
	n := el("p", txt("  Revenue\n"), el("b", txt("grew")), el("br"), txt("40%  "))
	if got := commentanchor.ElementText(n); got != "Revenue grew 40%" {
		t.Errorf("the text is %q", got)
	}
	long := el("p", txt(strings.Repeat("é", commentanchor.MaxTextRunes+5)))
	if got := []rune(commentanchor.ElementText(long)); len(got) != commentanchor.MaxTextRunes {
		t.Errorf("a long text keeps %d characters, want %d", len(got), commentanchor.MaxTextRunes)
	}
	idx := commentanchor.IndexSlides(deck(slide("a"), slide("b"), slide("c")))
	if got, want := idx.SlideOrder(), []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("the slide order is %v, want %v", got, want)
	}
}

// files is a version's files as a format reads them.
type files map[string]string

func (f files) Paths() []string {
	var out []string
	for p := range f {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func (f files) Essence(p string) (string, bool) {
	if _, ok := f[p]; !ok {
		return "", false
	}
	if strings.HasSuffix(p, ".js") {
		return "text/javascript", true
	}
	return "text/html", true
}

func (f files) Read(p string) ([]byte, error) {
	data, ok := f[p]
	if !ok {
		return nil, errors.New("no such file")
	}
	return []byte(data), nil
}

// A VERSION'S INDEX IS READ BY WHAT ITS KIND'S COMMENTS POINT AT: an html
// version's pages and scripts, and nothing at all for a kind whose comments
// take the whole version, which reads no file.
//
// Mutation: read every file, or skip the scripts, and this goes red.
func TestAVersionsIndexIsReadByWhatItsKindPointsAt(t *testing.T) {
	t.Parallel()
	site := files{"index.html": `<section data-crewlet-anchor="comparison"></section>`,
		"app.js": `crewlet.comments.anchor('pricing', el)`, "data.csv": "a,b"}
	idx, err := commentanchor.Read(artifacts.KindHTML, site, artifacts.Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.Names("index.html"); !slices.Equal(got, []string{"comparison", "pricing"}) {
		t.Errorf("index.html declares %v, want comparison and pricing", got)
	}
	if idx.Size() == 0 {
		t.Error("an index holding names weighs nothing")
	}
	doc, err := commentanchor.Read(artifacts.KindDoc, files{}, artifacts.Manifest{})
	if err != nil || doc != nil {
		t.Errorf("a doc's index is (%v, %v), want none: its comments take the whole version", doc, err)
	}
	if _, err := commentanchor.Read("poster", files{}, artifacts.Manifest{}); !errors.Is(err,
		commentanchor.ErrInvalid) {
		t.Errorf("an unknown kind answered %v", err)
	}
}

// AN ELEMENT'S SOURCE IS ITS NODE ON THE DECK, written back as markup: the
// whole slide for an empty path, the element at the path otherwise, and none
// for a path the slide does not hold.
func TestAnElementsSourceIsItsNodeAsMarkup(t *testing.T) {
	t.Parallel()
	d := deck(slide("results", el("h1", txt("Results")),
		slideformat.Node{Tag: "p", Attrs: map[string]string{"class": "lead"},
			Style:    []slideformat.Decl{{Property: "color", Value: "red"}},
			Children: []slideformat.Node{txt("Up <40%>"), el("br")}}))
	a := commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "results", Path: "1", Tag: "p"}
	n, ok := commentanchor.Element(d, a)
	if !ok {
		t.Fatal("the element at path 1 was not found")
	}
	if got, want := commentanchor.Markup(n), `<p class="lead" style="color: red">Up &lt;40%&gt;<br></p>`; got != want {
		t.Errorf("the element's markup is %q, want %q", got, want)
	}
	whole := a
	whole.Path = ""
	if n, ok := commentanchor.Element(d, whole); !ok || n.Tag != "section" {
		t.Errorf("an empty path found %+v, want the slide's section", n)
	}
	for _, path := range []string{"5", "1/0", "0/9"} {
		a.Path = path
		if _, ok := commentanchor.Element(d, a); ok {
			t.Errorf("path %s found an element the slide does not hold", path)
		}
	}
}
