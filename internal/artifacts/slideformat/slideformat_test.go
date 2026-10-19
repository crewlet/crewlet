package slideformat

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
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
		{"magic", `<section id="agenda" data-transition="magic" style="background:#fff"></section>`, "fade or push", RuleAttribute, 1},
		{"build", head + `<p data-build-in="fade">x</p></section>`, "builds are not drawn", RuleAttribute, 1},
		{"svg script", head + `<svg aria-label="x"><script>x()</script></svg></section>`, "runs no code", RuleElement, 1},
		{"svg style", head + `<svg aria-label="x"><rect style="fill:red"/></svg></section>`, "presentation attributes", RuleAttribute, 1},
		{"svg foreign ref", head + `<svg aria-label="x"><use href="https://x.test/s.svg#a"/></svg></section>`, "its own shapes", RuleAttribute, 1},
		{"svg dangling ref", head + `<svg aria-label="x"><rect fill="url(#g)"/></svg></section>`, "#g", RuleReference, 1},
		{"svg no label", head + `<svg viewBox="0 0 1 1"></svg></section>`, "aria-label", RuleAttribute, 1},
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
			d.files["deck.json"] = []byte(`{"title":"T","order":["agenda"],"theme":"dark"}`)
		}, "unknown field"},
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
