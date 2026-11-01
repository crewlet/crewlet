package slideformat

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/words"
)

// ---- the starter deck ----------------------------------------------------- //
//
// What a person starts a deck from in the dashboard: a PLAN, the theme it is
// drawn in and the slides it opens with, each a layout and its title. It is
// the ENGINE'S, not the browser's, because a create from the dashboard carries
// no upload: the pipeline publishes the composed files inline, held to this
// format like any deck, so a theme or a layout that drifts from the format
// fails its own test rather than a person's first save.
//
// A plan names layouts rather than slides because the deck editor adds a
// slide the same way: from the layouts of the theme the index names, drawn
// with this package's markup, so a slide added on day ten looks like the
// slides the deck began with.

// Layout is one kind of slide a starter can hold.
type Layout struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Description says what the layout is for, under its name in a picker.
	Description string `json:"description"`
	// Heading is the title a slide of this layout is drawn with when its
	// plan names none. A cover's is the deck's own title.
	Heading string `json:"heading"`

	draw func(t Theme, s slideText) string
}

// slideText is what a layout draws a slide from: its id and its title,
// escaped for markup, and the logo a cover shows (a path and its alt text,
// escaped too), empty for none.
type slideText struct {
	id, title     string
	logo, logoAlt string
}

// coverLogo is the cover's logo above its rule, or nothing: a fixed height so
// a wide logo and a square mark sit on the same line, its width following the
// file's own ratio and held inside the slide's margins.
func coverLogo(s slideText) string {
	if s.logo == "" {
		return ""
	}
	return fmt.Sprintf(`<img src="%s" alt="%s" style="height:96px;width:auto;max-width:960px;`+
		`align-self:flex-start;object-fit:contain">
`, s.logo, s.logoAlt)
}

// LayoutCover is the layout whose title is the deck's own.
const LayoutCover = "cover"

// layouts are the layouts a plan may name, in the order a picker offers them.
var layouts = []Layout{
	{ID: LayoutCover, Name: "Cover", Description: "The deck's title and who it is for",
		draw: func(t Theme, s slideText) string {
			return fmt.Sprintf(`<section id="%s" style="%s">
%s<div style="width:96px;height:8px;background:%s;border-radius:4px"></div>
<h1 style="%s">%s</h1>
<p style="font-size:40px;color:%s">A subtitle: who it is for, and when</p>
<aside>The cover. Say what the deck is for in one sentence before the first slide.</aside>
</section>
`, s.id, featureStyle(t), coverLogo(s), t.Accent, headingStyle(t, 112, "1.05"), s.title, t.FeatureMuted)
		}},
	{ID: "agenda", Name: "Agenda", Description: "A numbered list of the parts to come",
		Heading: "Agenda",
		draw: func(t Theme, s slideText) string {
			return fmt.Sprintf(`<section id="%s" style="%s">
<h2 style="%s">%s</h2>
<ol style="font-size:40px;line-height:1.5">
<li>Where we are</li>
<li>What we will do</li>
<li>What we need</li>
</ol>
<aside>Three parts at most. Give each one a section slide where it begins.</aside>
</section>
`, s.id, stageStyle(t), headingStyle(t, 72, "1.1"), s.title)
		}},
	{ID: "section", Name: "Section", Description: "Opens a part of the deck and starts it in the outline",
		Heading: "A new part of the story",
		draw: func(t Theme, s slideText) string {
			return fmt.Sprintf(`<section id="%s" style="%s">
<p style="font-size:28px;font-weight:600;letter-spacing:4px;text-transform:uppercase;color:%s">Section</p>
<h2 style="%s">%s</h2>
<aside>A section slide opens a part of the deck. The outline starts a section here.</aside>
</section>
`, s.id, featureStyle(t), t.Accent, headingStyle(t, 96, "1.05"), s.title)
		}},
	{ID: "points", Name: "Points", Description: "A title and a short list",
		Heading: "Three points",
		draw: func(t Theme, s slideText) string {
			return fmt.Sprintf(`<section id="%s" style="%s">
<h2 style="%s">%s</h2>
<ul style="font-size:40px;line-height:1.5">
<li>The first point, in one line</li>
<li>The second, with the detail that matters</li>
<li>The third, and what it means for us</li>
</ul>
<aside>Three points a person can repeat after the meeting. Click any text to change it.</aside>
</section>
`, s.id, stageStyle(t), headingStyle(t, 72, "1.1"), s.title)
		}},
	{ID: "columns", Name: "Two columns", Description: "Two cards side by side, to compare or pair",
		Heading: "One idea per slide",
		draw: func(t Theme, s slideText) string {
			card := fmt.Sprintf("flex:1;display:flex;flex-direction:column;gap:12px;background:%s;"+
				"padding:40px;border:1px solid %s;border-top:6px solid %s;border-radius:16px",
				t.Card, t.Edge, t.Accent)
			return fmt.Sprintf(`<section id="%s" style="%s">
<h2 style="%s">%s</h2>
<div style="display:flex;gap:32px">
<div style="%s">
<h3 style="font-family:%s;font-size:40px;font-weight:%d">The point</h3>
<p style="font-size:30px;color:%s">Say it in one sentence a person remembers.</p>
</div>
<div style="%s">
<h3 style="font-family:%s;font-size:40px;font-weight:%d">The evidence</h3>
<p style="font-size:30px;color:%s">A number, a quote or a picture that backs it.</p>
</div>
</div>
<aside>Two cards side by side. Click any text to change it.</aside>
</section>
`, s.id, stageStyle(t), headingStyle(t, 72, "1.1"), s.title,
				card, t.Display, t.DisplayWeight, t.Muted, card, t.Display, t.DisplayWeight, t.Muted)
		}},
	{ID: "number", Name: "Big number", Description: "One figure, large, and what it measures",
		Heading: "The number that matters",
		draw: func(t Theme, s slideText) string {
			return fmt.Sprintf(`<section id="%s" style="%s;justify-content:center">
<h2 style="%s">%s</h2>
<p style="font-family:%s;font-size:240px;font-weight:%d;line-height:1;letter-spacing:-8px;color:%s">42%%</p>
<p style="font-size:40px;color:%s">What it measures, and since when</p>
<aside>One figure the room should remember. Say where it comes from.</aside>
</section>
`, s.id, stageStyle(t), headingStyle(t, 56, "1.1"), s.title, t.Display, t.DisplayWeight,
				t.Accent, t.Muted)
		}},
	{ID: "quote", Name: "Quote", Description: "A quote, large, and who said it",
		Heading: "A quote from a customer, in their own words",
		draw: func(t Theme, s slideText) string {
			return fmt.Sprintf(`<section id="%s" style="%s;justify-content:center">
<div style="display:flex;flex-direction:column;gap:40px;padding:0 0 0 64px;border-left:8px solid %s">
<p style="%s">%s</p>
<p style="font-size:32px;color:%s">Name, role and company</p>
</div>
<aside>Quote people in their own words, and say who they are.</aside>
</section>
`, s.id, stageStyle(t), t.Accent, headingStyle(t, 72, "1.2"), s.title, t.Muted)
		}},
	{ID: "closing", Name: "Closing", Description: "The one decision or ask to end on",
		Heading: "What we need",
		draw: func(t Theme, s slideText) string {
			return fmt.Sprintf(`<section id="%s" style="%s">
<h2 style="%s">%s</h2>
<p style="font-size:40px;color:%s">The one decision or the one ask, said plainly</p>
<aside>End on what the room should do next.</aside>
</section>
`, s.id, featureStyle(t), headingStyle(t, 88, "1.1"), s.title, t.FeatureMuted)
		}},
}

// featureStyle is a cover, a section and a closing slide's own style.
func featureStyle(t Theme) string {
	return fmt.Sprintf("background:%s;color:%s;font-family:%s;padding:128px;display:flex;"+
		"flex-direction:column;justify-content:center;gap:32px", t.Feature, t.FeatureInk, t.Body)
}

// stageStyle is every other slide's style.
func stageStyle(t Theme) string {
	return fmt.Sprintf("background:%s;color:%s;font-family:%s;padding:128px;display:flex;"+
		"flex-direction:column;gap:48px", t.Stage, t.Ink, t.Body)
}

// headingStyle is a heading at size, in the theme's display face.
func headingStyle(t Theme, size int, lineHeight string) string {
	return fmt.Sprintf("font-family:%s;font-size:%dpx;font-weight:%d;line-height:%s;letter-spacing:%dpx",
		t.Display, size, t.DisplayWeight, lineHeight, t.Tracking)
}

// Layouts are the layouts a plan may name, in a picker's order.
func Layouts() []Layout { return append([]Layout(nil), layouts...) }

// layoutByID is the layout with that id, false where there is none.
func layoutByID(id string) (Layout, bool) {
	i := slices.IndexFunc(layouts, func(l Layout) bool { return l.ID == id })
	if i < 0 {
		return Layout{}, false
	}
	return layouts[i], true
}

// ---- the plan ------------------------------------------------------------- //

// Plan is what a starter deck is composed from: the theme it is drawn in and
// the slides it opens with. A plan that names no theme is drawn in
// [DefaultTheme]; one that names no slides opens with [DefaultPlan]'s.
type Plan struct {
	Theme  string      `json:"theme,omitempty"`
	Slides []PlanSlide `json:"slides,omitempty"`
}

// PlanSlide is one slide of a plan: its layout and its title. No title is the
// layout's own heading; a cover's is the deck's title.
type PlanSlide struct {
	Layout string `json:"layout"`
	Title  string `json:"title,omitempty"`
}

// MaxPlanSlides bounds the slides a plan opens with: a starter is somewhere
// to begin, and a person who wants sixty slides wants an agent to draft them.
const MaxPlanSlides = 60

// MaxSlideTitleBytes bounds one slide's title in a plan.
const MaxSlideTitleBytes = 200

// ErrPlan is every refusal of a plan: a theme or a layout the format does not
// have, too many slides, a title too long.
var ErrPlan = errors.New("slideformat: the starter's plan")

// DefaultPlan is the deck the starter has always opened with: a cover, an
// agenda, two columns and a closing slide, in [DefaultTheme].
func DefaultPlan() Plan {
	return Plan{Theme: DefaultTheme, Slides: []PlanSlide{
		{Layout: LayoutCover}, {Layout: "agenda"}, {Layout: "columns"}, {Layout: "closing"},
	}}
}

// Look is a deck's look as a caller that knows the company resolves it, for a
// plan drawn in [BrandTheme]: the theme itself, the logo the cover shows and
// the typefaces the index declares. The logo and every face's src are paths of
// files the caller puts in the same version, which the deck's reader holds the
// version to like any image or face.
type Look struct {
	// Theme is the resolved theme. Its ID is [BrandTheme].
	Theme Theme

	// Logo is the path of the image the cover shows above its title, and
	// LogoAlt its text (the company's name); empty for none.
	Logo    string
	LogoAlt string

	// Faces are the typefaces the index declares, by key, each naming a
	// font file of the version; at most [MaxFaces].
	Faces map[string]IndexFace
}

// Compose is the starter deck's files for a deck titled title, by path, its
// index first in [IndexPath]: plan's slides in order, drawn in its theme.
//
// Each slide's id is its layout's, and a layout used again is numbered from
// two (`points`, `points-2`), so an id says what a slide began as. A section
// slide starts a section of the outline under its title. The titles are
// written escaped, as JSON in the index and as text on the slides.
//
// A plan drawn in [BrandTheme] is the caller's to resolve, and is refused
// here: [ComposeBranded] takes the look it resolved.
func Compose(title string, plan Plan) (map[string][]byte, error) {
	theme, slides, err := plan.resolve()
	if err != nil {
		return nil, err
	}
	return compose(title, theme, slides, Look{}), nil
}

// ComposeBranded is [Compose] for a plan drawn in the company's adopted brand:
// look carries the theme the caller resolved for [BrandTheme], the logo its
// cover shows and the faces its index declares. A plan naming any other theme
// is refused, as is a look whose theme is not [BrandTheme]'s or that declares
// more faces than a deck may.
func ComposeBranded(title string, plan Plan, look Look) (map[string][]byte, error) {
	switch {
	case plan.Theme != "" && plan.Theme != BrandTheme:
		return nil, fmt.Errorf("%w: theme %q is the engine's own, and a branded deck is drawn in %q",
			ErrPlan, plan.Theme, BrandTheme)
	case look.Theme.ID != BrandTheme:
		return nil, fmt.Errorf("%w: the look resolved for %q names the theme %q", ErrPlan,
			BrandTheme, look.Theme.ID)
	case len(look.Faces) > MaxFaces:
		return nil, fmt.Errorf("%w: the look declares %d typefaces and a deck declares at most %d",
			ErrPlan, len(look.Faces), MaxFaces)
	}
	slides, err := plan.slides()
	if err != nil {
		return nil, err
	}
	return compose(title, look.Theme, slides, look), nil
}

// compose draws slides in theme with look's logo and faces: the plan already
// resolved and held to the format's bounds.
func compose(title string, theme Theme, slides []PlanSlide, look Look) map[string][]byte {
	files := make(map[string][]byte, len(slides)+1)
	idx := Index{V: 4, Title: title, Theme: theme.ID, Order: make([]string, 0, len(slides)),
		Faces: look.Faces}
	logo, logoAlt := html.EscapeString(look.Logo), html.EscapeString(look.LogoAlt)
	used := map[string]int{}
	for _, s := range slides {
		layout, _ := layoutByID(s.Layout)
		used[layout.ID]++
		id := layout.ID
		if n := used[layout.ID]; n > 1 {
			id += "-" + strconv.Itoa(n)
		}
		text := s.Title
		switch {
		case text != "":
		case layout.ID == LayoutCover:
			text = title
		default:
			text = layout.Heading
		}
		idx.Order = append(idx.Order, id)
		if layout.ID == LayoutCover && idx.Cover == "" {
			idx.Cover = id
		}
		if layout.ID == "section" {
			if idx.Sections == nil {
				idx.Sections = map[string]IndexSection{}
			}
			idx.Sections["s"+strconv.Itoa(len(idx.Sections)+1)] = IndexSection{Description: text, Start: id}
		}
		st := slideText{id: id, title: html.EscapeString(text)}
		if layout.ID == LayoutCover {
			st.logo, st.logoAlt = logo, logoAlt
		}
		files[SlidePath(id)] = []byte(layout.draw(theme, st))
	}
	index, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		// AN INDEX OF STRINGS ALWAYS ENCODES; a failure here is a broken
		// build, not an input.
		panic("slideformat: encode the starter index: " + err.Error())
	}
	files[IndexPath] = append(index, '\n')
	return files
}

// resolve is the plan's theme and slides with its defaults filled in, or the
// [ErrPlan] that refuses it.
func (p Plan) resolve() (Theme, []PlanSlide, error) {
	id := p.Theme
	if id == "" {
		id = DefaultPlan().Theme
	}
	if id == BrandTheme {
		return Theme{}, nil, fmt.Errorf("%w: theme %q is drawn from the company's adopted brand, "+
			"and none was resolved for this deck", ErrPlan, BrandTheme)
	}
	theme, ok := ThemeByID(id)
	if !ok {
		return Theme{}, nil, fmt.Errorf("%w: theme %q is not one of %s", ErrPlan, id,
			words.Or(themeIDs()))
	}
	slides, err := p.slides()
	if err != nil {
		return Theme{}, nil, err
	}
	return theme, slides, nil
}

// slides is the plan's slides with the default filled in, or the [ErrPlan]
// that refuses them.
func (p Plan) slides() ([]PlanSlide, error) {
	slides := p.Slides
	if len(slides) == 0 {
		slides = DefaultPlan().Slides
	}
	if len(slides) > MaxPlanSlides {
		return nil, fmt.Errorf("%w: it names %d slides and a starter opens with at most %d",
			ErrPlan, len(slides), MaxPlanSlides)
	}
	for i, s := range slides {
		if _, ok := layoutByID(s.Layout); !ok {
			return nil, fmt.Errorf("%w: slide %d's layout %q is not one of %s", ErrPlan, i+1,
				s.Layout, words.Or(layoutIDs()))
		}
		if len(s.Title) > MaxSlideTitleBytes || !utf8.ValidString(s.Title) {
			return nil, fmt.Errorf("%w: slide %d's title is longer than %d bytes", ErrPlan,
				i+1, MaxSlideTitleBytes)
		}
	}
	return slides, nil
}

// layoutIDs are the layouts' ids, for a refusal that names them.
func layoutIDs() []string {
	out := make([]string, len(layouts))
	for i, l := range layouts {
		out[i] = l.ID
	}
	return out
}

// ---- samples -------------------------------------------------------------- //

// Sample is a theme drawn as a deck of every layout once, read through this
// format as any deck is: what a picker shows a theme as, and the slides the
// deck editor adds a layout from. Any theme samples, a caller-resolved
// [BrandTheme] included; a sample declares no face, since it is no version's
// and holds no file but its slides. [SampleBranded] draws the brand's logo.
func Sample(t Theme) (Deck, error) {
	return Read(composed(compose(t.Name, t, sampleSlides(), Look{})))
}

// SampleBranded is [Sample] for the caller-resolved [BrandTheme], drawn with
// the logo a deck begun in it shows on its cover (look.Logo, whose file is of
// type logoType), so the picker promises what the deck delivers. The sample
// holds the logo by its path and type alone, as no version's file; a caller
// draws it from the adopted brand. Faces are not declared, as in any sample.
func SampleBranded(look Look, logoType string) (Deck, error) {
	if look.Theme.ID != BrandTheme {
		return Deck{}, fmt.Errorf("%w: the look resolved for %q names the theme %q", ErrPlan,
			BrandTheme, look.Theme.ID)
	}
	look.Faces = nil
	files := composed(compose(look.Theme.Name, look.Theme, sampleSlides(), look))
	if look.Logo == "" {
		return Read(files)
	}
	return Read(sampleFiles{composed: files, image: look.Logo, essence: logoType})
}

// sampleSlides are every layout once, in a picker's order.
func sampleSlides() []PlanSlide {
	slides := make([]PlanSlide, len(layouts))
	for i, l := range layouts {
		slides[i] = PlanSlide{Layout: l.ID}
	}
	return slides
}

// sampleFiles are a sample's files and the one image its cover shows, held by
// path and type only: a sample is no version's, so the image has no bytes.
type sampleFiles struct {
	composed
	image, essence string
}

func (s sampleFiles) Paths() []string {
	out := append(s.composed.Paths(), s.image)
	slices.Sort(out)
	return out
}

func (s sampleFiles) Essence(p string) (string, bool) {
	if p == s.image {
		return s.essence, true
	}
	return s.composed.Essence(p)
}

func (s sampleFiles) Read(p string) ([]byte, error) {
	if p == s.image {
		return nil, nil
	}
	return s.composed.Read(p)
}

// composed is a starter's files as a version to read.
type composed map[string][]byte

func (c composed) Paths() []string {
	out := make([]string, 0, len(c))
	for p := range c {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func (c composed) Essence(p string) (string, bool) {
	if _, ok := c[p]; !ok {
		return "", false
	}
	if p == IndexPath {
		return "application/json", true
	}
	return "text/html", true
}

func (c composed) Read(p string) ([]byte, error) {
	b, ok := c[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return b, nil
}
