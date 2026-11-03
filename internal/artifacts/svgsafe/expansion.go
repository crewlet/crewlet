package svgsafe

import (
	"regexp"
	"sync"
)

// ---- what a drawing draws -------------------------------------------------- //
//
// A DRAWING THAT EXPANDS IS BOUNDED BY WHAT IT EXPANDS TO, and every reader
// asks this one bound, the inline slide drawing as the standalone file. A
// `<use>` draws its target again; a marker is drawn at its path's start, its
// end and, for `marker-mid`, at every vertex between, so a marker of a hundred
// elements on a path of a thousand vertices draws a hundred thousand; a
// pattern, a mask, a clip or a filter draws its subtree for the element that
// names it. Each is counted by the subtree it draws, multiplied as it is
// drawn, and a reference that reaches back into what refers to it is a cycle.

// refKind is how a reference draws its target.
type refKind int

const (
	// refUse draws its target once, in place.
	refUse refKind = iota
	// refMarkerEnd draws a marker once, at the path's start or its end.
	refMarkerEnd
	// refMarkerMid draws a marker at every vertex but the first and last.
	refMarkerMid
	// refPaint draws a pattern, a mask, a clip or a filter, or fills with a
	// gradient, once for the element that names it.
	refPaint
)

// refKindOf is how a `url(#id)` reference of the attribute name draws.
func refKindOf(name string) refKind {
	switch name {
	case "marker-start", "marker-end":
		return refMarkerEnd
	case "marker-mid":
		return refMarkerMid
	}
	return refPaint
}

// element is one element of a drawing as the expansion reads it.
type element struct {
	name     string
	line     int
	children []int
	refs     []elementRef

	// vertices is how many points its `d` or `points` names, at most: what a
	// `marker-mid` is drawn at.
	vertices int
}

// elementRef is one reference an element makes.
type elementRef struct {
	id   string
	kind refKind
	line int
}

// coordinate is one number of path data or a points list.
var coordinate = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`[-+]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][-+]?[0-9]+)?`)
})

// Open begins an element (SVG's spelling) of the drawing at line, inside the
// element opened last and not yet closed. Its attributes follow ([Drawing.Attr]),
// then its children, then [Drawing.Close].
func (d *Drawing) Open(name string, line int) {
	at := len(d.elems)
	d.elems = append(d.elems, element{name: name, line: line})
	if len(d.open) > 0 {
		parent := d.open[len(d.open)-1]
		d.elems[parent].children = append(d.elems[parent].children, at)
	}
	if name == "line" {
		d.elems[at].vertices = 2
	}
	d.open = append(d.open, at)
}

// Close ends the element opened last.
func (d *Drawing) Close() {
	if len(d.open) > 0 {
		d.open = d.open[:len(d.open)-1]
	}
}

// current is the element whose attributes are being read, or -1 before any.
func (d *Drawing) current() int {
	if len(d.open) == 0 {
		return -1
	}
	return d.open[len(d.open)-1]
}

// declare files id under the element being read.
func (d *Drawing) declare(id string) {
	if at := d.current(); at >= 0 {
		d.byID[id] = at
	}
}

// refer files a reference of the element being read.
func (d *Drawing) refer(id string, kind refKind, line int) {
	if at := d.current(); at >= 0 {
		d.elems[at].refs = append(d.elems[at].refs, elementRef{id: id, kind: kind, line: line})
	}
}

// vertices counts the points a `d` or `points` value names, at most: half its
// numbers, which over-counts an arc's (seven numbers, one point) and so errs
// toward refusing.
func (d *Drawing) vertices(val string) {
	if at := d.current(); at >= 0 {
		n := len(coordinate().FindAllStringIndex(val, -1))
		d.elems[at].vertices = max(d.elems[at].vertices, (n+1)/2)
	}
}

// expansion refuses a reference cycle and a drawing that draws more than
// [MaxExpandedElements] once every reference is expanded. Sizes saturate just
// past the bound, so no count overflows however the references multiply.
func (d *Drawing) expansion(bad func(line int, format string, args ...any)) {
	if len(d.elems) == 0 {
		return
	}
	const unvisited, visiting = 0, -1
	size := make([]int, len(d.elems))
	var walk func(at int) (int, bool)
	walk = func(at int) (int, bool) {
		switch s := size[at]; s {
		case visiting:
			return 0, false
		case unvisited:
		default:
			return s, true
		}
		size[at] = visiting
		e := d.elems[at]
		total := 1
		for _, r := range e.refs {
			target, ok := d.byID[r.id]
			if !ok {
				continue
			}
			if size[target] == visiting {
				bad(r.line, "<%s> refers to #%s, which draws this element again: a reference "+
					"reaches an element outside itself", e.name, r.id)
				return 0, false
			}
			s, ok := walk(target)
			if !ok {
				return 0, false
			}
			total = saturate(total + saturate(copies(r.kind, e.vertices)*s))
		}
		for _, c := range e.children {
			s, ok := walk(c)
			if !ok {
				return 0, false
			}
			total = saturate(total + s)
		}
		size[at] = total
		return total, true
	}
	for at := range d.elems {
		if size[at] != unvisited {
			continue
		}
		total, ok := walk(at)
		if !ok {
			return
		}
		if total > MaxExpandedElements {
			bad(0, "the drawing draws more than %d elements once its uses, markers and paint "+
				"servers are expanded: draw the repeated shapes once", MaxExpandedElements)
			return
		}
	}
}

// copies is how many times a reference of kind draws its target from an
// element of vertices points.
func copies(kind refKind, vertices int) int {
	switch kind {
	case refMarkerMid:
		return max(vertices-2, 0)
	default:
		return 1
	}
}

func saturate(n int) int { return min(n, MaxExpandedElements+1) }
