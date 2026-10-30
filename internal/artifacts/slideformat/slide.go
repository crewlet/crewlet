package slideformat

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/svgsafe"
	"github.com/crewlet/crewlet/internal/words"
)

// ---- the stage and the bounds --------------------------------------------- //

// The stage every slide is laid out on, in CSS pixels: 16:9, scaled to fit
// wherever it is drawn.
const (
	StageWidth  = 1920
	StageHeight = 1080
)

// The bounds a slide is held to, Claude's (format.md, "Errors"), so a deck
// either format accepts the other accepts too.
const (
	MaxElements         = 200   // elements on one slide, an inline drawing counted once
	MaxDivDepth         = 15    // nested <div>s
	MaxPinnedPerBox     = 24    // position:absolute children of one <div>
	MaxTextRunes        = 20000 // characters of text on one slide, notes aside
	MaxInlineMarks      = 100   // <b>, <i>, <u>, <a> and <span> on one slide
	MaxTableRows        = 100   // rows of one table
	MaxTableColumns     = 24    // cells of one row
	MaxTableCells       = 4000  // cells of every table on one slide
	MaxNotesRunes       = 4000  // characters of speaker notes
	MaxDataTextIndentPx = 200   // the list indent a <div data-text> may name
)

// Transitions are how a slide may leave the stage in Present: `fade` or
// `push`. Claude's `magic` move pairs elements across slides by id, which this
// renderer does not draw, so it is refused rather than shown as a fade.
var Transitions = []string{"fade", "push"}

// ShapeKinds are the shapes an `<x-shape>` draws, each filling its box.
var ShapeKinds = []string{"rect", "rounded", "ellipse", "diamond", "arrow-right", "arrow-left",
	"arrow-up", "arrow-down", "line"}

// Icons are the names an `<x-icon>` may draw: Claude's icon names that
// Crewlet's glyph set (`@crewlethq/icons`) has a glyph for. The dashboard maps
// each to its glyph, and a gate holds the two lists equal.
var Icons = []string{"Activity", "Book", "Chart", "Chat", "Check", "CheckCircle", "Clock", "Code",
	"Database", "Globe", "Home", "Key", "Lightning", "Link", "Lock", "PaperPlane", "Search",
	"Settings", "Star", "Tool", "Trust", "Users", "Warning", "Wrench"}

// IconGlyphPercent is how much of an `<x-icon>`'s box its glyph fills, in
// percent of the box's width and height. The glyph sits centred in the box,
// and the rest is padding. This is Claude's rule, so an icon a deck sized for
// Claude's format draws the same size here: a 64px box draws a 32px glyph.
const IconGlyphPercent = 50

// ---- the canonical tree --------------------------------------------------- //

// Node is one element or one run of text of a slide, as the dashboard draws
// it: an element has a Tag, a run of text has Text alone.
type Node struct {
	Tag      string            `json:"tag,omitempty"`
	Text     string            `json:"text,omitempty"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	Style    []Decl            `json:"style,omitempty"`
	Children []Node            `json:"children,omitempty"`
}

// Slide is one slide of a deck, read.
type Slide struct {
	// ID is the slide's id: its file's name and its section's id.
	ID string `json:"id"`

	// Path is the slide's file in the version.
	Path string `json:"path"`

	// Title is the slide's first heading's text (else its first paragraph's),
	// what the outline, the filmstrip and a screen reader name it by.
	Title string `json:"title"`

	// Hidden is a slide Present skips.
	Hidden bool `json:"hidden,omitempty"`

	// Transition is how the slide leaves the stage in Present, empty for a
	// cut.
	Transition string `json:"transition,omitempty"`

	// Notes are the speaker notes: plain text, readable by everyone who can
	// open the deck.
	Notes string `json:"notes,omitempty"`

	// Root is the `<section>`, its notes taken out.
	Root Node `json:"root"`

	text []string
}

// ---- findings ------------------------------------------------------------- //

// Rule names what a finding is about, from a closed set.
type Rule string

// The rules a slide or an index can break.
const (
	RuleMarkup    Rule = "markup"    // markup the parser cannot read as one closed tree
	RuleElement   Rule = "element"   // an element outside the subset, or where it may not sit
	RuleAttribute Rule = "attribute" // an attribute outside the subset or its grammar
	RuleStyle     Rule = "style"     // a style property or value outside the subset
	RuleBound     Rule = "bound"     // a count or size past its bound
	RuleReference Rule = "reference" // a file, a slide or an id that is not there
	RuleIndex     Rule = "index"     // the deck index itself
)

// Finding is one thing a deck breaks, in one file, at one line where it has
// one.
type Finding struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Rule    Rule   `json:"rule"`
	Message string `json:"message"`
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: %s: %s", f.Path, f.Line, f.Rule, f.Message)
	}
	return fmt.Sprintf("%s: %s: %s", f.Path, f.Rule, f.Message)
}

// ---- the parser ----------------------------------------------------------- //

// Files answers what the version holds at a path: its media type essence, and
// false for no file there.
type Files func(path string) (essence string, ok bool)

var (
	inlineTags = []string{"b", "i", "u", "a", "span", "br"}
	blockTags  = []string{"h1", "h2", "h3", "p", "ul", "ol", "div", "img", "table", "hr", "svg",
		"x-shape", "x-icon"}
	voidTags   = []string{"br", "img", "hr"}
	textHolder = []string{"h1", "h2", "h3", "p", "li", "th", "td", "a", "b", "i", "u", "span"}
	markTags   = []string{"b", "i", "u", "a", "span"}
	headings   = []string{"h1", "h2", "h3"}
	idRE       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// imageEssences are the image types an `<img>` may show.
var imageEssences = []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/svg+xml"}

// children answers what an element may hold.
func children(tag string) []string {
	switch tag {
	case "section":
		return append(slices.Clone(blockTags), "aside")
	case "div":
		return blockTags
	case "ul", "ol":
		return []string{"li"}
	case "table":
		return []string{"tr"}
	case "tr":
		return []string{"th", "td"}
	case "a":
		return []string{"b", "i", "u", "span", "br"}
	}
	if slices.Contains(textHolder, tag) {
		return inlineTags
	}
	return nil
}

// frame is one open element.
type frame struct {
	node   *Node
	line   int
	pinned int // position:absolute children
	rows   int

	// skip is an element already refused: read through to its end so the
	// rest of the file is checked too, and never drawn.
	skip bool
}

type parser struct {
	path   string
	id     string
	files  Files
	src    []byte
	line   int
	found  []Finding
	slide  Slide
	stack  []*frame
	count  int
	depth  int
	marks  int
	cells  int
	runes  int
	svg    *svgDrawing
	svgTop int
	notes  *strings.Builder
	done   bool
}

func (p *parser) bad(rule Rule, line int, format string, args ...any) {
	p.found = append(p.found, Finding{Path: p.path, Line: line, Rule: rule,
		Message: fmt.Sprintf(format, args...)})
}

// ParseSlide reads one slide file, whose id is id, against the version's
// other files.
func ParseSlide(path, id string, src []byte, files Files) (Slide, []Finding) {
	p := &parser{path: path, id: id, files: files, src: src, line: 1}
	p.parse()
	return p.slide, p.found
}

func (p *parser) top() *frame {
	if len(p.stack) == 0 {
		return nil
	}
	return p.stack[len(p.stack)-1]
}

func (p *parser) parse() {
	if !utf8.Valid(p.src) {
		p.bad(RuleMarkup, 0, "the file is not UTF-8 text")
		return
	}
	// THE ENGINE'S NONCE PLACEHOLDER is in no source, a slide's included: the
	// read paths strip it from every HTML file to show its author's text, so
	// a slide holding it would read back as something it is not.
	if i := bytes.Index(p.src, []byte(artifacts.NoncePlaceholder)); i >= 0 {
		p.bad(RuleMarkup, 1+bytes.Count(p.src[:i], []byte("\n")), "the slide holds the engine's "+
			"nonce placeholder, which no file may")
		return
	}
	z := html.NewTokenizer(bytes.NewReader(p.src))
	z.SetMaxBuf(len(p.src) + 1)
	offset := 0
	for {
		tt := z.Next()
		raw := z.Raw()
		line := p.line
		p.line += bytes.Count(raw, []byte("\n"))
		start := offset
		offset += len(raw)
		if tt == html.ErrorToken {
			if !errors.Is(z.Err(), io.EOF) {
				p.bad(RuleMarkup, line, "the markup cannot be read: %v", z.Err())
			}
			break
		}
		if !p.step(z, tt, line, start, offset) {
			return
		}
	}
	switch {
	case len(p.stack) > 0:
		f := p.top()
		p.bad(RuleMarkup, f.line, "<%s> is never closed", f.node.Tag)
	case !p.done:
		p.bad(RuleMarkup, 0, "the file holds no <section>: a slide file is exactly one "+
			"<section id=%q> and nothing else", p.id)
	}
}

// step takes one token, and answers false once the file cannot be read on.
func (p *parser) step(z *html.Tokenizer, tt html.TokenType, line, start, end int) bool {
	switch tt {
	case html.CommentToken:
		return true
	case html.DoctypeToken:
		p.bad(RuleMarkup, line, "a slide file holds no doctype, <html>, <head> or <body>: "+
			"only its <section>")
		return false
	case html.TextToken:
		p.text(string(z.Text()), line)
		return true
	case html.StartTagToken, html.SelfClosingTagToken:
		tok := z.Token()
		return p.open(tok, tt == html.SelfClosingTagToken, line, start)
	case html.EndTagToken:
		tok := z.Token()
		return p.close(tok.Data, line, end)
	}
	return true
}

func (p *parser) text(s string, line int) {
	f := p.top()
	blank := strings.TrimSpace(s) == ""
	switch {
	case f != nil && f.skip:
	case f == nil:
		if !blank {
			p.bad(RuleMarkup, line, "text outside the <section>: a slide file is its section alone")
		}
	case f.node.Tag == "aside":
		p.notes.WriteString(s)
	case p.svg != nil:
		if svgsafe.IsTextElement(f.node.Tag) {
			f.node.Children = append(f.node.Children, Node{Text: s})
		} else if !blank {
			p.bad(RuleElement, line, "text in an svg's <%s>: text in a drawing sits in <text>", f.node.Tag)
		}
	case slices.Contains(textHolder, f.node.Tag):
		p.runes += utf8.RuneCountInString(s)
		f.node.Children = append(f.node.Children, Node{Text: s})
		p.slide.text = append(p.slide.text, s)
	case !blank:
		p.bad(RuleElement, line, "text directly in <%s>: put it in a <p>, a heading or a list", f.node.Tag)
	}
}

func (p *parser) open(tok html.Token, selfClosing bool, line, start int) bool {
	tag := tok.Data
	parent := p.top()
	if parent != nil && parent.skip {
		p.skipOver(tok, selfClosing, line)
		return true
	}
	if p.svg != nil {
		return p.openSVG(tok, selfClosing, line)
	}
	switch {
	case parent == nil && p.done:
		p.bad(RuleMarkup, line, "<%s> after the <section>: a slide file holds one section and "+
			"nothing after it", tag)
		return false
	case parent == nil && tag != "section":
		p.bad(RuleMarkup, line, "<%s> outside a <section>: a slide file is exactly one "+
			"<section id=%q>", tag, p.id)
		return false
	case parent != nil && parent.node.Tag == "aside":
		p.bad(RuleElement, line, "<%s> in the notes: speaker notes are plain text", tag)
		p.skipOver(tok, selfClosing, line)
		return true
	}
	if hint, refused := refusedTags[tag]; refused && parent != nil || refused && tag != "section" {
		p.bad(RuleElement, line, "<%s> is not in the slide subset: %s", tag, hint)
		p.skipOver(tok, selfClosing, line)
		return true
	}
	if parent != nil && !slices.Contains(children(parent.node.Tag), tag) {
		p.bad(RuleElement, line, "<%s> may not sit in <%s>; <%s> holds %s", tag, parent.node.Tag,
			parent.node.Tag, describeTags(children(parent.node.Tag)))
		p.skipOver(tok, selfClosing, line)
		return true
	}
	if parent != nil && parent.node.Tag == "section" && p.notes != nil {
		p.bad(RuleElement, line, "<%s> after the notes: <aside> is the section's last child", tag)
	}
	node := Node{Tag: tag}
	if tag == "svg" {
		p.svg = &svgDrawing{start: start, Drawing: svgsafe.NewDrawing()}
		p.svgTop = len(p.stack)
		p.svg.Open("svg", line)
	}
	p.attrs(&node, tok.Attr, line)
	if tag != "section" && tag != "aside" {
		p.count++
		if p.count > MaxElements {
			// READ NO FURTHER: past the bound the slide is refused whatever
			// else it holds, and a file of tens of thousands of elements
			// costs nothing more to refuse than one just past it.
			p.bad(RuleBound, line, "the slide holds more than %d elements: split it", MaxElements)
			return false
		}
	}
	if slices.Contains(markTags, tag) {
		p.marks++
		if p.marks == MaxInlineMarks+1 {
			p.bad(RuleBound, line, "the slide holds more than %d inline marks (b, i, u, a, span)",
				MaxInlineMarks)
		}
	}
	p.bounds(parent, &node, line)
	if tag == "aside" {
		if p.notes != nil {
			p.bad(RuleElement, line, "a second <aside>: a slide has one set of notes")
		}
		p.notes = &strings.Builder{}
	}
	if slices.Contains(voidTags, tag) || (selfClosing && (tag == "x-shape" || tag == "x-icon")) {
		p.attach(node)
		return true
	}
	if selfClosing {
		p.bad(RuleMarkup, line, "<%s/> is not self-closing in HTML: write <%s></%s>", tag, tag, tag)
		return false
	}
	if tag == "div" {
		p.depth++
		if p.depth == MaxDivDepth+1 {
			p.bad(RuleBound, line, "<div>s nest more than %d deep", MaxDivDepth)
		}
	}
	p.stack = append(p.stack, &frame{node: &node, line: line})
	return true
}

// refusedTags are elements Claude's format or HTML has that a Crewlet slide
// does not, each with what to write instead.
var refusedTags = map[string]string{
	"script":        "a slide runs no code",
	"style":         "every style is inline, in the element's style attribute",
	"link":          "a typeface is a face in deck.json",
	"iframe":        "a slide embeds no page",
	"x-embed":       "a slide embeds no live page in this release; show a picture of it as an <img>",
	"x-connector":   "draw the line in an inline <svg>",
	"object":        "show a picture as an <img> of a file in the version",
	"embed":         "show a picture as an <img> of a file in the version",
	"canvas":        "draw it in an inline <svg>",
	"video":         "a slide shows no video in this release; show a still as an <img>",
	"audio":         "a slide plays no sound",
	"form":          "a slide takes no input",
	"input":         "a slide takes no input",
	"button":        "a slide takes no input",
	"textarea":      "a slide takes no input",
	"select":        "a slide takes no input",
	"foreignobject": "a drawing holds no HTML",
	"html":          "a slide file is its <section> alone",
	"head":          "a slide file is its <section> alone",
	"body":          "a slide file is its <section> alone",
	"title":         "the deck's title is deck.json's title",
	"meta":          "a slide file is its <section> alone",
	"thead":         "a table is <tr>s directly; a first row of <th> is the header",
	"tbody":         "a table is <tr>s directly",
	"tfoot":         "a table is <tr>s directly",
	"h4":            "use <h3> with a smaller font-size",
	"h5":            "use <h3> with a smaller font-size",
	"h6":            "use <h3> with a smaller font-size",
	"em":            "use <i>",
	"strong":        "use <b>",
	"blockquote":    "use a <p> or a painted <div>",
	"pre":           "use a <p> with font-family:monospace",
	"code":          "use a <span> in a <p> with font-family:monospace",
	"section":       "a slide is one <section>; group with a <div>",
}

// skipOver steps over a refused element: its content is read to its end, so
// the rest of the file is checked, and none of it is drawn.
func (p *parser) skipOver(tok html.Token, selfClosing bool, line int) {
	if selfClosing || slices.Contains(voidTags, tok.Data) {
		return
	}
	p.stack = append(p.stack, &frame{node: &Node{Tag: tok.Data}, line: line, skip: true})
}

// bounds holds a new element to the counts its parent and the slide keep.
func (p *parser) bounds(parent *frame, node *Node, line int) {
	if parent == nil {
		return
	}
	if pos, _ := declared(node.Style, "position"); pos == "absolute" && parent.node.Tag == "div" {
		parent.pinned++
		if parent.pinned == MaxPinnedPerBox+1 {
			p.bad(RuleBound, line, "a <div> pins more than %d children: split them across sibling "+
				"boxes", MaxPinnedPerBox)
		}
	}
	switch node.Tag {
	case "tr":
		parent.rows++
		if parent.rows == MaxTableRows+1 {
			p.bad(RuleBound, line, "a table holds more than %d rows", MaxTableRows)
		}
	case "th", "td":
		parent.rows++ // a row counts its cells
		if parent.rows == MaxTableColumns+1 {
			p.bad(RuleBound, line, "a table row holds more than %d cells", MaxTableColumns)
		}
		p.cells++
		if p.cells == MaxTableCells+1 {
			p.bad(RuleBound, line, "the slide's tables hold more than %d cells", MaxTableCells)
		}
	}
}

// attrs reads an element's attributes into node.
func (p *parser) attrs(node *Node, attrs []html.Attribute, line int) {
	bad := func(format string, args ...any) { p.bad(RuleAttribute, line, format, args...) }
	seen := map[string]bool{}
	for _, a := range attrs {
		key := a.Key
		if seen[key] {
			bad("<%s> names %s twice", node.Tag, key)
			continue
		}
		seen[key] = true
		switch {
		case strings.HasPrefix(key, "on"):
			bad("<%s %s>: an event handler never runs in a slide", node.Tag, key)
			continue
		case key == "style":
			node.Style = parseStyle(node.Tag, a.Val, func(format string, args ...any) {
				p.bad(RuleStyle, line, "<%s> %s", node.Tag, fmt.Sprintf(format, args...))
			})
			continue
		case key == "class":
			bad("<%s class>: a slide has no stylesheet, so style the element inline", node.Tag)
			continue
		}
		p.attr(node, key, a.Val, bad)
	}
	checkPinned(node.Style, func(format string, args ...any) {
		p.bad(RuleStyle, line, "<%s> %s", node.Tag, fmt.Sprintf(format, args...))
	})
	p.required(node, seen, bad)
}

// attr reads one attribute the element's row names.
func (p *parser) attr(node *Node, key, val string, bad func(string, ...any)) {
	set := func() {
		if node.Attrs == nil {
			node.Attrs = map[string]string{}
		}
		node.Attrs[key] = val
	}
	switch node.Tag + " " + key {
	case "section id":
		if val != p.id {
			bad("<section id=%q> is in the file of slide %q: a slide's section id is its "+
				"file's name", val, p.id)
		}
	case "section hidden":
		p.slide.Hidden = true
	case "section data-transition":
		if !slices.Contains(Transitions, val) {
			bad("data-transition=%q: a slide leaves by %s", val, words.Or(Transitions))
			return
		}
		p.slide.Transition = val
	case "section data-section":
		bad("data-section: a deck's sections are deck.json's sections, each naming the slide " +
			"it starts at")
	case "img src":
		p.imageSrc(val, bad)
		set()
	case "img alt":
		set()
	case "a href":
		if !strings.HasPrefix(val, "https://") || strings.ContainsAny(val, " \t\n\r<>\"'`") {
			bad("<a href=%q>: a link is an https:// address", val)
			return
		}
		set()
	case "x-shape kind":
		if !slices.Contains(ShapeKinds, val) {
			bad("<x-shape kind=%q>: a shape is one of %s", val, words.Or(ShapeKinds))
			return
		}
		set()
	case "x-icon name":
		if !slices.Contains(Icons, val) {
			bad("<x-icon name=%q>: an icon is one of %s", val, words.Or(Icons))
			return
		}
		set()
	case "div data-text":
		if val != "" {
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || n > MaxDataTextIndentPx {
				bad("data-text=%q: a text box's list indent is a whole number of px from 0 to %d",
					val, MaxDataTextIndentPx)
				return
			}
		}
		set()
	default:
		if node.Tag == "svg" {
			if name, v, ok := p.svg.Attr("svg", key, val, 0, bad); ok {
				if node.Attrs == nil {
					node.Attrs = map[string]string{}
				}
				node.Attrs[name] = v
			}
			return
		}
		switch {
		case strings.HasPrefix(key, "data-build"):
			bad("<%s %s>: builds are not drawn in this release; show the element from the "+
				"start, or split the slide", node.Tag, key)
		case key == "id":
			bad("<%s id>: only a <section> carries an id (magic moves are not drawn in this "+
				"release)", node.Tag)
		default:
			bad("<%s %s> is not an attribute the slide subset takes", node.Tag, key)
		}
	}
}

// required reports the attributes an element must carry.
func (p *parser) required(node *Node, seen map[string]bool, bad func(string, ...any)) {
	need := map[string][]string{"section": {"id"}, "img": {"src", "alt"}, "a": {"href"},
		"x-shape": {"kind"}, "x-icon": {"name"}, "svg": {"aria-label"}}[node.Tag]
	for _, key := range need {
		if !seen[key] {
			hint := ""
			switch key {
			case "alt":
				hint = ` (what it shows, or alt="" when it is decoration)`
			case "aria-label":
				hint = " (what the drawing shows, its alt text)"
			}
			bad("<%s> needs %s%s", node.Tag, key, hint)
		}
	}
}

// imageSrc holds an image to a raster or SVG file of the version.
func (p *parser) imageSrc(src string, bad func(string, ...any)) {
	low := strings.ToLower(src)
	switch {
	case strings.HasPrefix(low, "http:") || strings.HasPrefix(low, "https:") ||
		strings.HasPrefix(low, "data:") || strings.HasPrefix(low, "//"):
		bad("<img src=%q>: an image is a file of the version, named by its path from the "+
			"deck's root (images/team.png), never a URL", src)
		return
	case strings.HasPrefix(src, "/") || strings.HasPrefix(src, "./") || strings.Contains(src, ".."):
		bad("<img src=%q>: name the image by its path from the deck's root, such as "+
			"images/team.png", src)
		return
	}
	essence, ok := p.files(src)
	switch {
	case !ok:
		p.found = append(p.found, Finding{Path: p.path, Rule: RuleReference,
			Message: fmt.Sprintf("<img src=%q>: the version holds no file at %s", src, src)})
	case !slices.Contains(imageEssences, essence):
		bad("<img src=%q> is %s, and an image is PNG, JPEG, GIF, WebP or SVG", src, essence)
	}
}

func (p *parser) close(tag string, line, end int) bool {
	f := p.top()
	if name, ok := svgsafe.Element(tag); ok && p.svg != nil && (f == nil || !f.skip) {
		tag = name
	}
	if f == nil || f.node.Tag != tag {
		if slices.Contains(voidTags, tag) {
			return true // </br> and the like close nothing
		}
		open := "nothing"
		if f != nil {
			open = "<" + f.node.Tag + ">"
		}
		p.bad(RuleMarkup, line, "</%s> closes %s: close every element in the order it opened",
			tag, open)
		return false
	}
	p.stack = p.stack[:len(p.stack)-1]
	if f.skip {
		return true
	}
	node := *f.node
	if p.svg != nil && len(p.stack) > p.svgTop {
		// AN ELEMENT INSIDE THE DRAWING CLOSES its place in the drawing's tree.
		p.svg.Close()
	}
	if p.svg != nil && len(p.stack) == p.svgTop {
		if end-p.svg.start > MaxSVGBytes {
			p.bad(RuleBound, f.line, "the drawing is %d bytes and an inline svg may be at most "+
				"%d: make it an image file", end-p.svg.start, MaxSVGBytes)
		}
		p.svg.Close()
		p.svg.Resolve(func(l int, format string, args ...any) { p.bad(RuleReference, l, format, args...) })
		p.svg = nil
	}
	switch tag {
	case "div":
		p.depth--
	case "aside":
		notes := strings.TrimSpace(p.notes.String())
		if n := utf8.RuneCountInString(notes); n > MaxNotesRunes {
			p.bad(RuleBound, f.line, "the notes are %d characters and may be at most %d", n,
				MaxNotesRunes)
		}
		p.slide.Notes = notes
	case "section":
		p.finish(node, line)
		return true
	}
	p.attach(node)
	return true
}

// attach adds a finished element to its parent.
func (p *parser) attach(node Node) {
	parent := p.top()
	parent.node.Children = append(parent.node.Children, node)
}

// finish completes the slide from its closed section.
func (p *parser) finish(root Node, line int) {
	p.done = true
	root.Attrs = nil
	root.Children = slices.DeleteFunc(root.Children, func(n Node) bool { return n.Tag == "aside" })
	root.Children = trimBlank(root.Children)
	p.slide.ID, p.slide.Path, p.slide.Root = p.id, p.path, root
	p.slide.Title = titleOf(root)
	if p.runes > MaxTextRunes {
		p.bad(RuleBound, line, "the slide holds %d characters of text and may hold at most %d",
			p.runes, MaxTextRunes)
	}
	if _, ok := declared(root.Style, "background"); !ok {
		if _, ok := declared(root.Style, "background-color"); !ok {
			p.bad(RuleStyle, line, "<section> has no background: set one on every slide, so it "+
				"reads the same in every theme")
		}
	}
}

// trimBlank drops whitespace-only text between blocks, which draws nothing.
func trimBlank(nodes []Node) []Node {
	return slices.DeleteFunc(nodes, func(n Node) bool { return n.Tag == "" && strings.TrimSpace(n.Text) == "" })
}

// titleOf is a slide's first heading's text, else its first paragraph's.
func titleOf(root Node) string {
	for _, want := range [][]string{headings, {"p"}} {
		if t := firstText(root, want); t != "" {
			return t
		}
	}
	return ""
}

func firstText(n Node, tags []string) string {
	if slices.Contains(tags, n.Tag) {
		return clip(strings.Join(strings.Fields(textOf(n)), " "), 120)
	}
	for _, c := range n.Children {
		if t := firstText(c, tags); t != "" {
			return t
		}
	}
	return ""
}

func textOf(n Node) string {
	if n.Tag == "" {
		return n.Text
	}
	if n.Tag == "br" {
		return " "
	}
	var b strings.Builder
	for _, c := range n.Children {
		b.WriteString(textOf(c))
	}
	return b.String()
}

// clip cuts s to at most n bytes at a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ---- drawings ------------------------------------------------------------- //

func (p *parser) openSVG(tok html.Token, selfClosing bool, line int) bool {
	parent := p.top()
	name, ok := svgsafe.Element(tok.Data)
	if !ok {
		p.bad(RuleElement, line, "<%s> in an svg: %s", tok.Data, svgsafe.ElementHint(tok.Data))
		return false
	}
	if hint, refused := svgsafe.NestHint(parent.node.Tag, name); refused {
		p.bad(RuleElement, line, "%s", hint)
		return false
	}
	node := Node{Tag: name}
	p.svg.Open(name, line)
	for _, a := range tok.Attr {
		key := a.Key
		if a.Namespace != "" {
			key = a.Namespace + ":" + a.Key
		}
		if n, v, ok := p.svg.Attr(name, key, a.Val, line, func(format string, args ...any) {
			p.bad(RuleAttribute, line, format, args...)
		}); ok {
			if node.Attrs == nil {
				node.Attrs = map[string]string{}
			}
			node.Attrs[n] = v
		}
	}
	if selfClosing {
		p.svg.Close()
		p.attach(node)
		return true
	}
	p.stack = append(p.stack, &frame{node: &node, line: line})
	return true
}

// ---- the subset, as lists ------------------------------------------------- //
//
// The dashboard's renderer holds the same allowlists again (contract/
// artifacts.ts), gated equal to these, so a tree this package would not have
// produced is still never drawn as anything but text.

// Elements are every HTML element a slide may hold, sorted.
func Elements() []string {
	set := []string{"section", "aside"}
	for _, t := range []string{"section", "div", "ul", "ol", "table", "tr", "a", "p"} {
		set = union(set, children(t))
	}
	return slices.Sorted(slices.Values(set))
}

// StyleProperties are every inline style property a slide may declare,
// sorted.
func StyleProperties() []string {
	out := make([]string, 0, len(properties))
	for p := range properties {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// SVGElements are every element an inline drawing may hold, as SVG spells
// them, sorted.
func SVGElements() []string { return svgsafe.Elements() }

// SVGAttributes are every attribute an inline drawing may carry, as SVG
// spells them, sorted.
func SVGAttributes() []string { return svgsafe.Attributes() }
