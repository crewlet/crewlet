package commentanchor

import (
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
)

// Index is what one version holds that an anchor can be found by: its pages
// and the names each declares, its slides and their elements, or its boards.
// Immutable once built, so one copy may be cached and shared by version.
type Index struct {
	// pages is every HTML page and the names declared in it; shared are
	// names declared in scripts, which any page may load.
	pages  map[string]map[string]bool
	shared map[string]bool

	// slides is every slide's elements in document order, and order the
	// slide ids as the deck shows them.
	slides map[string][]element
	order  []string

	// boards is every artboard by its page's path.
	boards map[string]canvasformat.Board
}

// element is one element of a slide as an anchor finds it.
type element struct{ path, tag, text string }

// File is one file of a version, as the html index reads it.
type File struct {
	Path string
	Data []byte
}

var (
	attrRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`data-crewlet-anchor\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	})
	callRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`comments\.anchor\(\s*(?:"([^"]*)"|'([^']*)')`)
	})
)

// IndexHTML is the index of an html version's files: every .html and .htm
// page, the `data-crewlet-anchor` names each declares, and the names each page
// or script passes to `comments.anchor`.
func IndexHTML(files []File) *Index {
	idx := &Index{pages: map[string]map[string]bool{}, shared: map[string]bool{}}
	for _, f := range files {
		lower := strings.ToLower(f.Path)
		switch {
		case isPage(f.Path):
			names := idx.pages[f.Path]
			if names == nil {
				names = map[string]bool{}
				idx.pages[f.Path] = names
			}
			declare(names, attrRE(), f.Data)
			declare(names, callRE(), f.Data)
		case strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".mjs"):
			// A script may be loaded by any page, so its names count on every page.
			declare(idx.shared, callRE(), f.Data)
		}
	}
	return idx
}

// declare adds every valid name re finds in data; a malformed one declares nothing.
func declare(into map[string]bool, re *regexp.Regexp, data []byte) {
	for _, m := range re.FindAllSubmatch(data, -1) {
		name := string(m[1])
		if len(m[2]) > 0 {
			name = string(m[2])
		}
		if ValidName(name) {
			into[name] = true
		}
	}
}

// IndexSlides is the index of a deck: every slide by id, in the deck's order,
// and every element on it with its path, tag and text.
func IndexSlides(d slideformat.Deck) *Index {
	idx := &Index{slides: make(map[string][]element, len(d.Slides))}
	for _, s := range d.Slides {
		idx.order = append(idx.order, s.ID)
		var els []element
		walk(s.Root.Children, "", &els)
		idx.slides[s.ID] = els
	}
	return idx
}

// walk lists every element beneath one node's children, parents first; a run
// of text holds an index too, as the drawn path counts it.
func walk(children []slideformat.Node, prefix string, into *[]element) {
	for i, c := range children {
		path := strconv.Itoa(i)
		if prefix != "" {
			path = prefix + "/" + path
		}
		if c.Tag == "" {
			continue
		}
		*into = append(*into, element{path: path, tag: c.Tag, text: ElementText(c)})
		walk(c.Children, path, into)
	}
}

// ElementText is an element's text as an anchor keeps it: every run beneath
// it joined, a <br> as a space, whitespace collapsed, the first [MaxTextRunes].
func ElementText(n slideformat.Node) string {
	var b strings.Builder
	var gather func(slideformat.Node)
	gather = func(n slideformat.Node) {
		switch n.Tag {
		case "":
			b.WriteString(n.Text)
		case "br":
			b.WriteString(" ")
		default:
			for _, c := range n.Children {
				gather(c)
			}
		}
	}
	gather(n)
	return clipRunes(strings.Join(strings.Fields(b.String()), " "), MaxTextRunes)
}

// clipRunes is s cut to at most n characters.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}

// IndexCanvas is the index of a design: every artboard by its page's path,
// with its size.
func IndexCanvas(c canvasformat.Canvas) *Index {
	return &Index{boards: maps.Clone(c.Boards)}
}

// Pages are the version's HTML pages, sorted.
func (x *Index) Pages() []string {
	if x == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(x.pages))
}

// Names are the names a page declares, its scripts' included, sorted; none
// for a page the version does not hold.
func (x *Index) Names(page string) []string {
	if x == nil {
		return nil
	}
	own, ok := x.pages[page]
	if !ok {
		return nil
	}
	set := maps.Clone(own)
	maps.Copy(set, x.shared)
	return slices.Sorted(maps.Keys(set))
}

// SlideOrder is the deck's slide ids in order, what [Label] numbers a slide by.
func (x *Index) SlideOrder() []string {
	if x == nil {
		return nil
	}
	return slices.Clone(x.order)
}

// declared reports whether page holds name, declared in it or in a script.
func (x *Index) declared(page, name string) bool {
	return x.pages[page][name] || x.shared[name]
}
