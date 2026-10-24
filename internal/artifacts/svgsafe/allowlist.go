package svgsafe

import (
	"regexp"
	"slices"
	"strings"
)

// ---- the allowlist -------------------------------------------------------- //

// elements are a drawing's elements, by the lowercased name the HTML tokenizer
// reads to the name SVG spells.
var elements = map[string]string{
	"svg": "svg", "g": "g", "defs": "defs", "path": "path", "rect": "rect", "circle": "circle",
	"ellipse": "ellipse", "line": "line", "polyline": "polyline", "polygon": "polygon",
	"lineargradient": "linearGradient", "radialgradient": "radialGradient", "stop": "stop",
	"clippath": "clipPath", "mask": "mask", "pattern": "pattern", "title": "title", "desc": "desc",
	"text": "text", "tspan": "tspan", "use": "use", "symbol": "symbol", "marker": "marker",
	"filter": "filter", "fegaussianblur": "feGaussianBlur", "feoffset": "feOffset",
	"feblend": "feBlend", "fecolormatrix": "feColorMatrix", "feflood": "feFlood",
	"fecomposite": "feComposite", "femerge": "feMerge", "femergenode": "feMergeNode",
	"fedropshadow": "feDropShadow",
}

// textElements are a drawing's elements that hold text.
var textElements = map[string]bool{"text": true, "tspan": true, "title": true, "desc": true}

// attributes are a drawing's attributes, lowercased to as SVG spells them.
var attributes = map[string]string{
	"id": "id", "viewbox": "viewBox", "width": "width", "height": "height", "x": "x", "y": "y",
	"x1": "x1", "y1": "y1", "x2": "x2", "y2": "y2", "cx": "cx", "cy": "cy", "r": "r", "rx": "rx",
	"ry": "ry", "fx": "fx", "fy": "fy", "d": "d", "points": "points", "transform": "transform",
	"fill": "fill", "stroke": "stroke", "stroke-width": "stroke-width",
	"stroke-linecap": "stroke-linecap", "stroke-linejoin": "stroke-linejoin",
	"stroke-miterlimit": "stroke-miterlimit", "stroke-dasharray": "stroke-dasharray",
	"stroke-dashoffset": "stroke-dashoffset", "stroke-opacity": "stroke-opacity",
	"fill-opacity": "fill-opacity", "fill-rule": "fill-rule", "clip-rule": "clip-rule",
	"opacity": "opacity", "offset": "offset", "stop-color": "stop-color",
	"stop-opacity": "stop-opacity", "gradientunits": "gradientUnits",
	"gradienttransform": "gradientTransform", "spreadmethod": "spreadMethod",
	"clip-path": "clip-path", "mask": "mask", "preserveaspectratio": "preserveAspectRatio",
	"aria-label": "aria-label", "aria-hidden": "aria-hidden", "role": "role",
	"font-size": "font-size", "font-family": "font-family", "font-weight": "font-weight",
	"font-style": "font-style", "text-anchor": "text-anchor",
	"dominant-baseline": "dominant-baseline", "letter-spacing": "letter-spacing", "dx": "dx",
	"dy": "dy", "patternunits": "patternUnits", "patterncontentunits": "patternContentUnits",
	"patterntransform": "patternTransform", "maskunits": "maskUnits",
	"maskcontentunits": "maskContentUnits", "clippathunits": "clipPathUnits", "href": "href",
	"xlink:href": "href", "filter": "filter", "stddeviation": "stdDeviation", "in": "in",
	"in2": "in2", "result": "result", "mode": "mode", "values": "values", "type": "type",
	"operator": "operator", "k1": "k1", "k2": "k2", "k3": "k3", "k4": "k4",
	"flood-color": "flood-color", "flood-opacity": "flood-opacity",
	"markerwidth": "markerWidth", "markerheight": "markerHeight", "refx": "refX", "refy": "refY",
	"orient": "orient", "markerunits": "markerUnits", "marker-start": "marker-start",
	"marker-mid": "marker-mid", "marker-end": "marker-end", "filterunits": "filterUnits",
	"primitiveunits": "primitiveUnits", "visibility": "visibility", "display": "display",
	"paint-order": "paint-order", "vector-effect": "vector-effect",
	"shape-rendering": "shape-rendering", "pathlength": "pathLength", "textlength": "textLength",
	"lengthadjust": "lengthAdjust", "color": "color", "rotate": "rotate",
}

// dropped are attributes a drawing may carry that change nothing once it is
// read: its namespace declarations and its version. Nothing else is dropped.
var dropped = map[string]bool{"xmlns": true, "xmlns:xlink": true, "version": true}

// refAttributes may name a paint server, clip, mask, filter or marker of the
// same drawing, as `url(#id)`.
var refAttributes = map[string]bool{"fill": true, "stroke": true, "clip-path": true, "mask": true,
	"filter": true, "marker-start": true, "marker-mid": true, "marker-end": true}

// forbidden are the substrings no attribute value may carry, compared
// lowercased: each is how a value becomes a request or script.
var forbidden = []string{"javascript:", "data:", "<", "expression", "@import"}

var (
	idPattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
	refPattern = regexp.MustCompile(`^url\(\s*#([A-Za-z][A-Za-z0-9_.-]{0,63})\s*\)$`)
)

// Element is the SVG spelling of an element the HTML tokenizer reads as lower,
// and whether a drawing may hold it.
func Element(lower string) (string, bool) {
	name, ok := elements[lower]
	return name, ok
}

// IsElement reports whether name, spelled exactly as SVG spells it, is an
// element a drawing may hold: the question an XML reader asks, since XML
// names are case-sensitive.
func IsElement(name string) bool {
	canonical, ok := elements[strings.ToLower(name)]
	return ok && canonical == name
}

// IsTextElement reports whether an element (SVG's spelling) holds text.
func IsTextElement(name string) bool { return textElements[name] }

// NestHint is why child may not sit in parent, and false when it may: a
// title, a description or a span holds text alone.
func NestHint(parent, child string) (string, bool) {
	if textElements[parent] && parent != "text" {
		return "<" + child + "> in <" + parent + ">: a title, a description or a span is text alone", true
	}
	return "", false
}

// ElementHint is what to say about an element a drawing may not hold.
func ElementHint(name string) string {
	switch strings.ToLower(name) {
	case "script", "style", "foreignobject":
		return "a drawing runs no code, carries no stylesheet and holds no HTML"
	case "a", "image", "feimage":
		return "a drawing links to nothing and shows no other file: draw it with shapes"
	case "animate", "animatemotion", "animatetransform", "animatecolor", "set":
		return "a drawing does not animate: an animation can change an attribute after it is checked"
	}
	return "a drawing holds shapes, paths, text, gradients, clips, masks, markers and filters"
}

// Elements are every element a drawing may hold, as SVG spells them, sorted.
func Elements() []string {
	out := make([]string, 0, len(elements))
	for _, name := range elements {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Attributes are every attribute a drawing may carry, as SVG spells them,
// sorted.
func Attributes() []string {
	var out []string
	for _, name := range attributes {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// ---- one drawing ---------------------------------------------------------- //

// Drawing is one drawing as it is read: the ids it declares and the ids it
// refers to, each reference with the line it is on, so every reference is
// resolved once the drawing is closed ([Drawing.Resolve]).
type Drawing struct {
	ids  map[string]bool
	refs []reference
}

type reference struct {
	id   string
	line int
}

// NewDrawing begins a drawing.
func NewDrawing() *Drawing { return &Drawing{ids: map[string]bool{}} }

// Attr checks one attribute of an element (SVG's spelling) of the drawing, its
// key lowercased as an HTML tokenizer reads it (`xlink:href` for the XLink
// reference), and answers its canonical name and value, or false: refused
// through bad, or dropped (a namespace declaration, `version`).
func (d *Drawing) Attr(tag, key, val string, line int, bad func(string, ...any)) (string, string, bool) {
	if strings.HasPrefix(key, "on") {
		bad("<%s %s>: an event handler never runs in a drawing", tag, key)
		return "", "", false
	}
	if key == "style" {
		bad("<%s style>: inside an svg, use presentation attributes such as fill and stroke", tag)
		return "", "", false
	}
	if dropped[key] {
		return "", "", false
	}
	name, ok := attributes[key]
	if !ok {
		bad("<%s %s> is not an attribute a drawing may carry", tag, key)
		return "", "", false
	}
	low := strings.ToLower(val)
	for _, f := range forbidden {
		if strings.Contains(low, f) {
			bad("<%s %s>: %q is not allowed in a drawing", tag, key, f)
			return "", "", false
		}
	}
	switch {
	case name == "id":
		if !idPattern.MatchString(val) {
			bad("<%s id=%q>: an id is a letter, then letters, digits, _, . and -, at most 64", tag, val)
			return "", "", false
		}
		if d.ids[val] {
			bad("<%s id=%q> repeats an id of the same drawing", tag, val)
			return "", "", false
		}
		d.ids[val] = true
	case name == "href":
		id, ok := strings.CutPrefix(val, "#")
		if !ok || !idPattern.MatchString(id) {
			bad("<%s href=%q>: a drawing refers only to its own shapes, as #id", tag, val)
			return "", "", false
		}
		d.refs = append(d.refs, reference{id: id, line: line})
	case strings.Contains(low, "url("):
		m := refPattern.FindStringSubmatch(strings.TrimSpace(val))
		if m == nil || !refAttributes[name] {
			bad("<%s %s=%q>: a drawing refers only to its own gradients, clips, masks, filters "+
				"and markers, as url(#id)", tag, key, val)
			return "", "", false
		}
		d.refs = append(d.refs, reference{id: m[1], line: line})
	}
	return name, val, true
}

// Resolve reports every reference to an id the drawing does not declare.
func (d *Drawing) Resolve(bad func(line int, format string, args ...any)) {
	for _, r := range d.refs {
		if !d.ids[r.id] {
			bad(r.line, "the drawing refers to #%s, which none of its own elements declares", r.id)
		}
	}
}
