package commentanchor

import (
	"fmt"
	"html"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
)

// Read is the index of a stored version of an artifact of kind, read through src
// by what its comments may point at: pages and scripts, a deck or a canvas. Nil,
// with no read, for a kind whose comments take the whole version alone.
func Read(kind artifacts.Kind, src artifacts.FileSource, m artifacts.Manifest) (*Index, error) {
	accepted, ok := accepts[kind]
	if !ok {
		return nil, refuse("artifact kind %q takes no comments in this build", kind)
	}
	switch {
	case slices.Contains(accepted, KindHTML):
		var files []File
		for _, p := range src.Paths() {
			if !indexed(p) {
				continue
			}
			data, err := src.Read(p)
			if err != nil {
				return nil, fmt.Errorf("commentanchor: read %s: %w", p, err)
			}
			files = append(files, File{Path: p, Data: data})
		}
		return IndexHTML(files), nil
	case slices.Contains(accepted, KindSlides):
		deck, err := slideformat.Read(src)
		if err != nil {
			return nil, err
		}
		return IndexSlides(deck), nil
	case slices.Contains(accepted, KindDesign):
		canvas, err := canvasformat.Read(src, m)
		if err != nil {
			return nil, err
		}
		return IndexCanvas(canvas), nil
	}
	return nil, nil
}

// indexed reports a file the html index reads: a page, or a script a page may load.
func indexed(p string) bool {
	lower := strings.ToLower(p)
	return isPage(p) || strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".mjs")
}

// Size is roughly the bytes the index holds, what a cache of indexes weighs it by.
func (x *Index) Size() int64 {
	if x == nil {
		return 0
	}
	var n int
	for page, names := range x.pages {
		n += len(page)
		for name := range names {
			n += len(name)
		}
	}
	for name := range x.shared {
		n += len(name)
	}
	for id, els := range x.slides {
		n += len(id)
		for _, e := range els {
			n += len(e.path) + len(e.tag) + len(e.text)
		}
	}
	for p := range x.boards {
		n += len(p) + 32
	}
	return int64(n)
}

// Element is the node a slides anchor names on deck: the slide's section for
// an empty path, else the element at the path; false where the deck holds none.
func Element(d slideformat.Deck, a Anchor) (slideformat.Node, bool) {
	if a.Kind != KindSlides {
		return slideformat.Node{}, false
	}
	i := slices.IndexFunc(d.Slides, func(s slideformat.Slide) bool { return s.ID == a.Slide })
	if i < 0 {
		return slideformat.Node{}, false
	}
	n := d.Slides[i].Root
	if a.Path == "" {
		return n, true
	}
	for _, step := range strings.Split(a.Path, "/") {
		at, err := strconv.Atoi(step)
		if err != nil || at < 0 || at >= len(n.Children) {
			return slideformat.Node{}, false
		}
		n = n.Children[at]
	}
	return n, n.Tag != ""
}

// Markup is a slide's node written as markup: its tag, its attributes in name
// order, its style and its children, a run of text escaped.
func Markup(n slideformat.Node) string {
	var b strings.Builder
	writeMarkup(&b, n)
	return b.String()
}

// voidTags are the elements written with no closing tag.
var voidTags = []string{"br", "img", "hr"}

func writeMarkup(b *strings.Builder, n slideformat.Node) {
	if n.Tag == "" {
		b.WriteString(html.EscapeString(n.Text))
		return
	}
	b.WriteString("<" + n.Tag)
	for _, k := range slices.Sorted(maps.Keys(n.Attrs)) {
		b.WriteString(" " + k + `="` + html.EscapeString(n.Attrs[k]) + `"`)
	}
	if len(n.Style) > 0 {
		decls := make([]string, len(n.Style))
		for i, d := range n.Style {
			decls[i] = d.Property + ": " + d.Value
		}
		b.WriteString(` style="` + html.EscapeString(strings.Join(decls, "; ")) + `"`)
	}
	b.WriteString(">")
	if slices.Contains(voidTags, n.Tag) {
		return
	}
	for _, c := range n.Children {
		writeMarkup(b, c)
	}
	b.WriteString("</" + n.Tag + ">")
}
