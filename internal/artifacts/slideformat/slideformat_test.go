package slideformat

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// dirSource reads a deck from a directory, typing files by extension.
type dirSource struct {
	files map[string][]byte
}

func (d dirSource) Paths() []string {
	out := make([]string, 0, len(d.files))
	for p := range d.files {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func (d dirSource) Essence(p string) (string, bool) {
	if _, ok := d.files[p]; !ok {
		return "", false
	}
	return map[string]string{".json": "application/json", ".html": "text/html", ".png": "image/png",
		".svg": "image/svg+xml", ".woff2": "font/woff2", ".txt": "text/plain"}[path.Ext(p)], true
}

func (d dirSource) Read(p string) ([]byte, error) {
	b, ok := d.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return b, nil
}

// testDeck is the twelve-slide deck under testdata/deck.
func testDeck(t *testing.T) dirSource {
	t.Helper()
	root := os.DirFS("testdata/deck")
	src := dirSource{files: map[string][]byte{}}
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, p)
		src.files[p] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// withSlide is the test deck with one slide's file replaced.
func withSlide(t *testing.T, id, body string) dirSource {
	t.Helper()
	src := testDeck(t)
	src.files[SlidePath(id)] = []byte(body)
	return src
}

func refusal(t *testing.T, src dirSource) []Finding {
	t.Helper()
	_, err := Read(src)
	var refused *Refused
	if !errors.As(err, &refused) || !errors.Is(err, ErrRefused) {
		t.Fatalf("read answered %v, not a refusal", err)
	}
	return refused.Findings
}

// A DECK IN THE FORMAT READS WHOLE: every slide in order, its title, notes,
// transition and hidden flag, the outline in deck order, and values written
// back in their canonical form.
func TestADeckInTheFormatReads(t *testing.T) {
	t.Parallel()
	deck, err := Read(testDeck(t))
	if err != nil {
		t.Fatal(err)
	}
	if deck.Title != "Quarterly plan" || deck.Cover != "cover" || len(deck.Slides) != 12 {
		t.Fatalf("deck %q cover %q with %d slides", deck.Title, deck.Cover, len(deck.Slides))
	}
	ids := make([]string, len(deck.Slides))
	for i, s := range deck.Slides {
		ids[i] = s.ID
	}
	want := []string{"cover", "agenda", "problem", "plan", "numbers", "team", "timeline", "risks",
		"chart", "quote", "ask", "thanks"}
	if !slices.Equal(ids, want) {
		t.Errorf("slides in order %q", ids)
	}
	plan := deck.Slides[3]
	if plan.Title != "The plan" || plan.Transition != "push" || !strings.Contains(plan.Notes, "Take the questions") {
		t.Errorf("slide 4 read as %+v", plan)
	}
	if !deck.Slides[11].Hidden || deck.Slides[0].Transition != "fade" {
		t.Error("the hidden last slide or the cover's fade was lost")
	}
	if got := []string{deck.Sections[0].Start, deck.Sections[1].Start, deck.Sections[2].Start}; !slices.Equal(got,
		[]string{"cover", "plan", "ask"}) {
		t.Errorf("sections start at %q, not in deck order", got)
	}
	for _, n := range deck.Slides[0].Root.Children {
		if n.Tag == "aside" {
			t.Error("the notes stayed in the drawn tree")
		}
	}
	h1 := deck.Slides[0].Root.Children[0]
	if v, _ := declared(h1.Style, "letter-spacing"); v != "-2px" {
		t.Errorf("letter-spacing written back as %q", v)
	}
	if !strings.Contains(deck.Text(), "plcae") || !strings.Contains(deck.Text(), "ten minutes") {
		t.Error("the search text lacks a slide's text or its notes")
	}
	out := deck.Outline()
	if out.Count != 12 || out.Slides[3].Title != "The plan" || !out.Slides[3].Notes {
		t.Errorf("outline %+v", out.Slides[3])
	}
}

// EVERY RULE IS REFUSED WITH THE FILE, THE LINE AND WHAT TO WRITE, rather than
// dropped: each case is a slide outside the subset and the words its finding
// must carry.
func TestASlideOutsideTheSubsetIsRefused(t *testing.T) {
	t.Parallel()
	const head = `<section id="agenda" style="background:#fff">`
	cases := []struct {
		name, body, want string
		rule             Rule
		line             int
	}{
		{"script", head + "\n<script>alert(1)</script></section>", "a slide runs no code", RuleElement, 2},
		{"event", head + `<p onclick="x()">Hi</p></section>`, "event handler", RuleAttribute, 1},
		{"class", head + `<p class="big">Hi</p></section>`, "style the element inline", RuleAttribute, 1},
		{"url in style", head + `<div style="background:url(https://x.test/a.png)"></div></section>`, `"url(" is not allowed in a slide (no URLs`, RuleStyle, 1},
		{"var", head + `<p style="color:var(--a)">Hi</p></section>`, `"var(" is not allowed in a slide`, RuleStyle, 1},
		{"z-index", head + `<p style="z-index:2">Hi</p></section>`, "paint order is source order", RuleStyle, 1},
		{"em", head + `<p style="font-size:2em">Hi</p></section>`, "no em", RuleStyle, 1},
		{"font too small", head + `<p style="font-size:4px">Hi</p></section>`, "from 8px to 400px", RuleStyle, 1},
		{"misplaced", head + `<p style="display:flex">Hi</p></section>`, "does not apply to <p>", RuleStyle, 1},
		{"offset unpinned", head + `<p style="left:10px">Hi</p></section>`, "position:absolute", RuleStyle, 1},
		{"no background", `<section id="agenda"><p>Hi</p></section>`, "no background", RuleStyle, 1},
		{"wrong id", `<section id="other" style="background:#fff"></section>`, "file's name", RuleAttribute, 1},
		{"remote image", head + `<img src="https://x.test/a.png" alt="a"></section>`, "never a URL", RuleAttribute, 1},
		{"missing image", head + `<img src="images/none.png" alt="a"></section>`, "holds no file", RuleReference, 0},
		{"no alt", head + `<img src="images/team.png"></section>`, "needs alt", RuleAttribute, 1},
		{"http link", head + `<p><a href="javascript:x()">x</a></p></section>`, "https://", RuleAttribute, 1},
		{"text in div", head + `<div>loose</div></section>`, "put it in a <p>", RuleElement, 1},
		{"nested list", head + `<ul><li>a<ul><li>b</li></ul></li></ul></section>`, "may not sit in <li>", RuleElement, 1},
		{"unclosed", head + "<p>Hi</section>", "</section> closes <p>", RuleMarkup, 1},
		{"embed", head + `<x-embed style="position:absolute"></x-embed></section>`, "no live page", RuleElement, 1},
		{"magic", `<section id="agenda" data-transition="magic" style="background:#fff"></section>`, `"fade" or "push"`, RuleAttribute, 1},
		{"build", head + `<p data-build-in="fade">x</p></section>`, "builds are not drawn", RuleAttribute, 1},
		{"svg script", head + `<svg aria-label="x"><script>x()</script></svg></section>`, "runs no code", RuleElement, 1},
		{"svg style", head + `<svg aria-label="x"><rect style="fill:red"/></svg></section>`, "presentation attributes", RuleAttribute, 1},
		{"svg foreign ref", head + `<svg aria-label="x"><use href="https://x.test/s.svg#a"/></svg></section>`, "its own shapes", RuleAttribute, 1},
		{"svg dangling ref", head + `<svg aria-label="x"><rect fill="url(#g)"/></svg></section>`, "#g", RuleReference, 1},
		{"svg no label", head + `<svg viewBox="0 0 1 1"></svg></section>`, "aria-label", RuleAttribute, 1},
		{"svg escaped url", head + `<svg aria-label="x"><rect fill="u\72l(https://x.test/p.svg#a)"/></svg></section>`, "backslash", RuleAttribute, 1},
		{"svg escaped by reference", head + `<svg aria-label="x"><rect filter="u&#92;72l(https://x.test/f.svg#a)"/></svg></section>`, "backslash", RuleAttribute, 1},
		{"svg image-set", head + `<svg aria-label="x"><rect mask="image-set('https://x.test/a.png' 1x)"/></svg></section>`, "url(#id)", RuleAttribute, 1},
		{"svg var", head + `<svg aria-label="x"><rect fill="var(--x)"/></svg></section>`, "a colour is", RuleAttribute, 1},
		{"svg use cycle", head + `<svg aria-label="x"><g id="a"><use href="#a"></use></g></svg></section>`, "draws this element again", RuleReference, 1},
		{"svg use bomb", head + `<svg aria-label="x"><defs><g id="l0"><rect/></g>` + useLevels(10) + `</defs><use href="#l10"/></svg></section>`, "once its uses, markers and paint servers are expanded", RuleReference, 0},
		{"notes markup", head + `<aside><b>x</b></aside></section>`, "plain text", RuleElement, 1},
		{"after notes", head + `<aside>x</aside><p>y</p></section>`, "after the notes", RuleElement, 1},
		{"doctype", "<!doctype html>" + head + "</section>", "no doctype", RuleMarkup, 1},
		{"two sections", head + "</section><section></section>", "after the <section>", RuleMarkup, 1},
		{"icon", head + `<x-icon name="Rocket"></x-icon></section>`, "an icon is one of", RuleAttribute, 1},
		{"shape", head + `<x-shape kind="star"></x-shape></section>`, "a shape is one of", RuleAttribute, 1},
		{"gradient stops", head + `<div style="background:linear-gradient(90deg, #fff)"></div></section>`, "two to eight", RuleStyle, 1},
		{"border style", head + `<div style="border:1px #000"></div></section>`, "style word is required", RuleStyle, 1},
		{"important", head + `<p style="color:#000 !important">x</p></section>`, "!important", RuleStyle, 1},
		{"placeholder", head + "\n<p>" + artifacts.NoncePlaceholder + "</p></section>", "nonce placeholder", RuleMarkup, 2},
		{"escape", head + `<p style="color:\72 ed">x</p></section>`, `\`, RuleStyle, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			found := refusal(t, withSlide(t, "agenda", c.body))
			i := slices.IndexFunc(found, func(f Finding) bool { return strings.Contains(f.Message, c.want) })
			if i < 0 {
				t.Fatalf("no finding says %q: %v", c.want, found)
			}
			f := found[i]
			if f.Rule != c.rule || f.Line != c.line || f.Path != "slides/agenda.html" {
				t.Errorf("finding %+v, want rule %s at line %d of slides/agenda.html", f, c.rule, c.line)
			}
		})
	}
}

// EVERY FINDING OF A FILE IS REPORTED IN ONE ANSWER, a refused element read
// through to its end, so an author fixes a slide in one pass.
func TestEveryFindingIsReportedAtOnce(t *testing.T) {
	t.Parallel()
	found := refusal(t, withSlide(t, "agenda", `<section id="agenda" style="background:#fff">
<p class="x">a</p>
<video><p>inside</p></video>
<p style="z-index:1">b</p>
</section>`))
	lines := []int{}
	for _, f := range found {
		lines = append(lines, f.Line)
	}
	if !slices.Equal(lines, []int{2, 3, 4}) {
		t.Errorf("findings at lines %v, want 2, 3 and 4: %v", lines, found)
	}
}

// THE INDEX AND THE FILES AGREE: every ordered slide has its file, every slide
// file is ordered, the cover and each section name a slide, faces load from
// font files of the version, and no design system is installed.
func TestTheIndexAndTheFilesAgree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(dirSource)
		want string
	}{
		{"unordered file", func(d dirSource) { d.files["slides/extra.html"] = d.files["slides/agenda.html"] }, "not in deck.json's order"},
		{"missing file", func(d dirSource) { delete(d.files, "slides/agenda.html") }, "holds no slides/agenda.html"},
		{"stray page", func(d dirSource) { d.files["about.html"] = []byte("<p>x</p>") }, "only as its slides"},
		{"bad cover", func(d dirSource) {
			d.files["deck.json"] = []byte(`{"title":"T","order":["agenda"],"cover":"x"}`)
		}, "cover is \"x\""},
		{"remote face", func(d dirSource) {
			d.files["deck.json"] = []byte(`{"title":"T","order":["agenda"],"faces":{"inter":{"family":"Inter","href":"https://fonts.googleapis.com/css2?family=Inter"}}}`)
		}, "font file of the version"},
		{"design system", func(d dirSource) {
			d.files["deck.json"] = []byte(`{"title":"T","order":["agenda"],"designSystems":[{"title":"x"}]}`)
		}, "installs none"},
		{"unknown key", func(d dirSource) {
			d.files["deck.json"] = []byte(`{"title":"T","order":["agenda"],"palette":"dark"}`)
		}, "palette is not a key the deck index carries: the top level of deck.json takes v, createdOnFiles"},
		{"no title", func(d dirSource) { d.files["deck.json"] = []byte(`{"order":["agenda"]}`) }, "name the deck"},
		{"twice", func(d dirSource) { d.files["deck.json"] = []byte(`{"title":"T","order":["agenda","agenda"]}`) }, "twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			src := testDeck(t)
			c.edit(src)
			found := refusal(t, src)
			if !slices.ContainsFunc(found, func(f Finding) bool { return strings.Contains(f.Message, c.want) }) {
				t.Errorf("no finding says %q: %v", c.want, found)
			}
		})
	}
}

// A FACE LOADS FROM A FONT FILE OF THE VERSION, and a family a style names
// that the deck does not declare falls back as in any stylesheet.
func TestAFaceLoadsFromAFontFile(t *testing.T) {
	t.Parallel()
	src := testDeck(t)
	src.files["deck.json"] = []byte(`{"title":"T","order":["agenda"],"faces":{"source-serif-4":{"family":"Source Serif 4","src":"fonts/serif.woff2"}}}`)
	src.files["fonts/serif.woff2"] = []byte("wOF2")
	for p := range src.files {
		if strings.HasPrefix(p, SlidesDir) && p != "slides/agenda.html" {
			delete(src.files, p)
		}
	}
	deck, err := Read(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(deck.Faces) != 1 || deck.Faces[0] != (Face{Family: "Source Serif 4", Src: "fonts/serif.woff2"}) {
		t.Errorf("faces %+v", deck.Faces)
	}
}

// VALUES ARE WRITTEN BACK IN ONE FORM: bare numbers are pixels, points become
// pixels, keywords and families are spelled one way, and a no-op is dropped.
func TestValuesAreWrittenBackCanonically(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"width:480":                  "width:480px",
		"font-size:36pt":             "font-size:48px",
		"padding:0 16":               "padding:0 16px",
		"font-family:Georgia, serif": "font-family:'Georgia', serif",
		"color:RGB(10,20,30)":        "color:rgb(10, 20, 30)",
		"transform:translateX(-50%)": "transform:translateX(-50%)",
		"margin:0;color:#ABC":        "color:#abc",
		"border:2px SOLID #000":      "border:2px solid #000",
		"background:linear-gradient(to right, #fff, #000 80%)": "background:linear-gradient(to right, #fff, #000 80%)",
	}
	for in, want := range cases {
		var got []string
		for _, d := range parseStyle("div", in, func(f string, a ...any) { t.Errorf("%s refused", in) }) {
			got = append(got, d.Property+":"+d.Value)
		}
		if strings.Join(got, ";") != want {
			t.Errorf("%s written back as %s, want %s", in, strings.Join(got, ";"), want)
		}
	}
}

// NO INPUT PANICS THE PARSER: whatever a slide file holds, it is read or
// refused.
func FuzzParseSlide(f *testing.F) {
	for _, seed := range []string{
		`<section id="a" style="background:#fff"><p>x</p></section>`,
		`<section id="a"><svg aria-label="x"><g><rect/></g></svg></section>`,
		`<section id="a"><table><tr><td>1</td></tr></table><aside>n</aside></section>`,
		`<section`, `</p>`, `<section id="a"><div><div><div>`,
	} {
		f.Add(seed)
	}
	files := func(string) (string, bool) { return "image/png", true }
	f.Fuzz(func(t *testing.T, body string) {
		_, _ = ParseSlide("slides/a.html", "a", []byte(body), files)
	})
}

// composedSource is a starter's files as a deck to read.
func composedSource(t *testing.T, title string, plan Plan) dirSource {
	t.Helper()
	files, err := Compose(title, plan)
	if err != nil {
		t.Fatal(err)
	}
	return dirSource{files: files}
}

// THE DEFAULT STARTER IS IN THE FORMAT, whatever its title: it reads whole
// with no finding, its title escaped as text on the cover and as JSON in the
// index, and it names the theme it was drawn in.
func TestTheStarterDeckIsInTheFormat(t *testing.T) {
	t.Parallel()
	const title = `Q3 <plan> & "more"`
	deck, err := Read(composedSource(t, title, Plan{}))
	if err != nil {
		t.Fatal(err)
	}
	if deck.Title != title || deck.Theme != DefaultTheme || len(deck.Slides) != 4 ||
		deck.Slides[0].Title != title || deck.Cover != "cover" || deck.Slides[1].Notes == "" {
		t.Errorf("the starter read as %q (%s) with %d slides, cover %q", deck.Title, deck.Theme,
			len(deck.Slides), deck.Slides[0].Title)
	}
}

// EVERY THEME DRAWS EVERY LAYOUT IN THE FORMAT: a theme's colour or family the
// format refuses, or a layout's markup, fails here rather than at a person's
// first create.
func TestEveryThemeDrawsEveryLayout(t *testing.T) {
	t.Parallel()
	for _, theme := range Themes() {
		deck, err := Sample(theme)
		if err != nil {
			t.Fatalf("theme %s: %v", theme.ID, err)
		}
		if deck.Theme != theme.ID || len(deck.Slides) != len(Layouts()) {
			t.Errorf("theme %s sampled as %q with %d slides", theme.ID, deck.Theme, len(deck.Slides))
		}
		for i, s := range deck.Slides {
			want := Layouts()[i].Heading
			if i == 0 {
				want = theme.Name
			}
			if s.Title != want || s.Notes == "" {
				t.Errorf("theme %s, slide %s: title %q (want %q), notes %q", theme.ID, s.ID, s.Title,
					want, s.Notes)
			}
		}
	}
}

// A PLAN'S SLIDES ARE ITS OWN: in order, a layout used again numbered from two,
// each titled as the plan says (escaped), and each section slide starting a
// section of the outline under its title.
func TestAPlanComposesItsSlides(t *testing.T) {
	t.Parallel()
	deck, err := Read(composedSource(t, "Board review", Plan{Theme: "studio", Slides: []PlanSlide{
		{Layout: "cover"}, {Layout: "section", Title: "Where <we> are"}, {Layout: "points"},
		{Layout: "points", Title: "Risks"}, {Layout: "section", Title: "Next"}, {Layout: "closing"},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	var ids, titles []string
	for _, s := range deck.Slides {
		ids, titles = append(ids, s.ID), append(titles, s.Title)
	}
	if got := strings.Join(ids, " "); got != "cover section points points-2 section-2 closing" {
		t.Errorf("ids %s", got)
	}
	if got := strings.Join(titles, "|"); got != "Board review|Where <we> are|Three points|Risks|Next|What we need" {
		t.Errorf("titles %s", got)
	}
	if deck.Theme != "studio" || len(deck.Sections) != 2 {
		t.Errorf("theme %q, sections %+v", deck.Theme, deck.Sections)
	}
	starts := map[string]string{}
	for _, s := range deck.Sections {
		starts[s.Start] = s.Description
	}
	if starts["section"] != "Where <we> are" || starts["section-2"] != "Next" {
		t.Errorf("sections %+v", deck.Sections)
	}
}

// A PLAN OUTSIDE WHAT THE STARTER HOLDS IS REFUSED, naming what to write
// instead, before anything is composed.
func TestAPlanOutsideTheStarterIsRefused(t *testing.T) {
	t.Parallel()
	many := make([]PlanSlide, MaxPlanSlides+1)
	for i := range many {
		many[i] = PlanSlide{Layout: "points"}
	}
	for name, plan := range map[string]Plan{
		"theme":  {Theme: "neon"},
		"layout": {Slides: []PlanSlide{{Layout: "timeline"}}},
		"many":   {Slides: many},
		"title":  {Slides: []PlanSlide{{Layout: "points", Title: strings.Repeat("x", MaxSlideTitleBytes+1)}}},
	} {
		if _, err := Compose("Deck", plan); !errors.Is(err, ErrPlan) {
			t.Errorf("%s: answered %v", name, err)
		}
	}
}

// AN INDEX'S THEME IS OPTIONAL AND CLOSED: a deck that names none reads, one
// that names a theme there is (the company's brand among them) reads it back,
// and one naming any other is refused naming the themes there are.
func TestAnIndexNamesOneOfTheThemes(t *testing.T) {
	t.Parallel()
	src := composedSource(t, "Deck", Plan{Theme: "grove"})
	deck, err := Read(src)
	if err != nil || deck.Theme != "grove" {
		t.Fatalf("a grove deck read as %q, %v", deck.Theme, err)
	}
	branded := dirSource{files: map[string][]byte{}}
	for p, b := range src.files {
		branded.files[p] = b
	}
	branded.files[IndexPath] = []byte(strings.Replace(string(src.files[IndexPath]), `"grove"`,
		`"`+BrandTheme+`"`, 1))
	if deck, err := Read(branded); err != nil || deck.Theme != BrandTheme {
		t.Fatalf("a deck begun in the company's brand read as %q, %v", deck.Theme, err)
	}
	src.files[IndexPath] = []byte(strings.Replace(string(src.files[IndexPath]), `"grove"`, `"neon"`, 1))
	found := refusal(t, src)
	if len(found) != 1 || !strings.Contains(found[0].Message, "nightshade") {
		t.Errorf("an unknown theme was refused with %v", found)
	}
	if _, err := Read(testDeck(t)); err != nil {
		t.Errorf("a deck naming no theme: %v", err)
	}
}

// TestTheIconRuleIsWritten holds the reference and the skill to the rule the
// renderer draws by: an icon's glyph fills half its box, as in Claude's
// format, so an author sizes the box at twice the glyph.
func TestTheIconRuleIsWritten(t *testing.T) {
	t.Parallel()
	if IconGlyphPercent != 50 {
		t.Fatalf("IconGlyphPercent is %d: the pages below say half; change them with it", IconGlyphPercent)
	}
	const rule = "draws its glyph at half its box"
	for _, rel := range []string{"docs/reference/slide-format.md",
		"skills/crewlet-artifacts/SKILL.md", "examples/tool-skills/artifacts.md"} {
		raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(strings.Fields(string(raw)), " "), rule) {
			t.Errorf("%s does not say an x-icon %s", rel, rule)
		}
	}
}

// brandLook is a look as the brand format resolves one: the brand theme's
// roles, an uploaded heading family and a system body stack, and a logo.
func brandLook() Look {
	return Look{
		Theme: Theme{ID: BrandTheme, Name: "Acme", Tag: "Your company brand",
			Feature: "#0f3d2e", FeatureInk: "#ffffff", FeatureMuted: "#b8d4c8",
			Stage: "#f6f7fb", Ink: "#121625", Muted: "#5b6472", Accent: "#0f7b5f",
			Card: "#ffffff", Edge: "#e2e4e8",
			Display: "'Acme Sans', system-ui, sans-serif", Body: "system-ui, sans-serif",
			DisplayWeight: 700, Tracking: -1},
		Logo: "brand/logo-on-dark.svg", LogoAlt: `Acme & "Co"`,
		Faces: map[string]IndexFace{"heading": {Family: "Acme Sans", Src: "brand/fonts/heading.woff2"}},
	}
}

// A DECK BEGUN IN THE COMPANY'S BRAND IS A DECK IN THE FORMAT: drawn in the
// theme its caller resolved, its index naming BrandTheme and declaring the
// look's faces, its cover showing the look's logo (alt text escaped), and the
// files the look names read as the version's image and face.
func TestABrandedDeckIsInTheFormat(t *testing.T) {
	t.Parallel()
	look := brandLook()
	files, err := ComposeBranded("Board review", Plan{Slides: []PlanSlide{
		{Layout: LayoutCover}, {Layout: "points"}, {Layout: "closing"}}}, look)
	if err != nil {
		t.Fatal(err)
	}
	files[look.Logo] = []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"/>`)
	files[look.Faces["heading"].Src] = []byte("wOF2")
	deck, err := Read(dirSource{files: files})
	if err != nil {
		t.Fatal(err)
	}
	if deck.Theme != BrandTheme || len(deck.Slides) != 3 || len(deck.Faces) != 1 ||
		deck.Faces[0].Family != "Acme Sans" {
		t.Fatalf("a branded deck read as theme %q, %d slides, faces %+v", deck.Theme,
			len(deck.Slides), deck.Faces)
	}
	cover := string(files[SlidePath("cover")])
	if !strings.Contains(cover, `<img src="brand/logo-on-dark.svg" alt="Acme &amp; &#34;Co&#34;"`) {
		t.Errorf("the cover shows no logo, or an unescaped one:\n%s", cover)
	}
	if strings.Contains(string(files[SlidePath("points")]), "<img") {
		t.Error("a slide other than the cover shows the logo")
	}
	// WITHOUT THE FILES the look names, the deck is refused by name, never
	// read with an image or a face that is not there.
	delete(files, look.Logo)
	if _, err := Read(dirSource{files: files}); !errors.Is(err, ErrRefused) {
		t.Errorf("a branded deck missing its logo file read: %v", err)
	}
}

// THE BRAND IS THE CALLER'S TO RESOLVE: Compose refuses a plan drawn in it,
// ComposeBranded refuses any other theme and a look that is not the brand's,
// and Sample draws a resolved brand theme with every layout like any theme.
func TestTheBrandThemeIsResolvedByItsCaller(t *testing.T) {
	t.Parallel()
	if _, err := Compose("Deck", Plan{Theme: BrandTheme}); !errors.Is(err, ErrPlan) {
		t.Errorf("Compose drew the brand theme with no look: %v", err)
	}
	look := brandLook()
	if _, err := ComposeBranded("Deck", Plan{Theme: "grove"}, look); !errors.Is(err, ErrPlan) {
		t.Errorf("ComposeBranded drew an engine theme: %v", err)
	}
	other := look
	other.Theme.ID = "grove"
	if _, err := ComposeBranded("Deck", Plan{}, other); !errors.Is(err, ErrPlan) {
		t.Errorf("ComposeBranded took a look that is not the brand's: %v", err)
	}
	crowded := look
	crowded.Faces = map[string]IndexFace{}
	for i := range MaxFaces + 1 {
		crowded.Faces[string(rune('a'+i))] = IndexFace{Family: "Acme Sans", Src: "brand/f.woff2"}
	}
	if _, err := ComposeBranded("Deck", Plan{}, crowded); !errors.Is(err, ErrPlan) {
		t.Errorf("ComposeBranded declared more faces than a deck may: %v", err)
	}
	deck, err := Sample(look.Theme)
	if err != nil || deck.Theme != BrandTheme || len(deck.Slides) != len(Layouts()) {
		t.Fatalf("the brand theme sampled as %q with %d slides: %v", deck.Theme, len(deck.Slides), err)
	}
}

// useLevels is levels groups, each drawing the one below ten times: ten to
// the levels shapes from a hundred elements.
func useLevels(levels int) string {
	var b strings.Builder
	for level := 1; level <= levels; level++ {
		fmt.Fprintf(&b, `<g id="l%d">`, level)
		for range 10 {
			fmt.Fprintf(&b, `<use href="#l%d"/>`, level-1)
		}
		b.WriteString(`</g>`)
	}
	return b.String()
}
