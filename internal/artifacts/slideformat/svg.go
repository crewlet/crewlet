package slideformat

import (
	"regexp"
	"strings"
	"sync"
)

// ---- inline SVG ----------------------------------------------------------- //
//
// A chart or a diagram is one `<svg>`: shapes, paths, gradients, clips, masks,
// markers and filter primitives, drawn as one opaque graphic. It reaches
// nothing outside itself: no script, no event attribute, no `foreignObject`,
// no `<style>` (an inline stylesheet would style the whole dashboard), no
// `style` attribute, and every reference (`href`, `url(#…)`) is to an id of
// the same drawing. The dashboard gives each drawing's ids a prefix of its own
// when it draws them, so two slides, or one slide drawn twice as a thumbnail,
// never resolve each other's gradients.

// MaxSVGBytes bounds one inline drawing's source, as Claude's format does
// (52 KB): a drawing past it is a picture, which belongs in an image file.
const MaxSVGBytes = 52 << 10

// svgElements are the drawing's elements, by the lowercased name the HTML
// tokenizer reads to the name SVG spells. Every table and pattern of this file
// is built at first use (the package doc says why).
var svgElements = sync.OnceValue(func() map[string]string {
	return map[string]string{
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
})

// svgTextElements are the drawing's elements that hold text.
var svgTextElements = sync.OnceValue(func() map[string]bool {
	return map[string]bool{"text": true, "tspan": true, "title": true, "desc": true}
})

// svgAttrs are the drawing's attributes, lowercased to as SVG spells them.
var svgAttrs = sync.OnceValue(func() map[string]string {
	return map[string]string{
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
})

// svgDropped are attributes a drawing may carry that change nothing once it
// is inline in a page: its namespace declarations.
var svgDropped = sync.OnceValue(func() map[string]bool {
	return map[string]bool{"xmlns": true, "xmlns:xlink": true, "version": true}
})

// svgRefAttrs may name a paint server, clip, mask, filter or marker of the
// same drawing, as `url(#id)`.
var svgRefAttrs = sync.OnceValue(func() map[string]bool {
	return map[string]bool{"fill": true, "stroke": true, "clip-path": true, "mask": true,
		"filter": true, "marker-start": true, "marker-mid": true, "marker-end": true}
})

var (
	svgIDRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
	})
	svgRefRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^url\(\s*#([A-Za-z][A-Za-z0-9_.-]{0,63})\s*\)$`)
	})
)

// svgDrawing is one drawing as it is read: the ids it declares and the ids it
// refers to, each reference with the line it is on.
type svgDrawing struct {
	start int // the source offset of `<svg`
	ids   map[string]bool
	refs  []svgRef
}

type svgRef struct {
	id   string
	line int
}

// svgAttr checks one attribute of a drawing's element and answers its
// canonical name and value, or a problem.
func (d *svgDrawing) svgAttr(tag, key, val string, line int, bad func(string, ...any)) (string, string, bool) {
	if strings.HasPrefix(key, "on") {
		bad("<%s %s>: an event handler never runs in a slide", tag, key)
		return "", "", false
	}
	if key == "style" {
		bad("<%s style>: inside an svg, use presentation attributes such as fill and stroke", tag)
		return "", "", false
	}
	if svgDropped()[key] {
		return "", "", false
	}
	name, ok := svgAttrs()[key]
	if !ok {
		bad("<%s %s> is not an attribute an inline svg may carry", tag, key)
		return "", "", false
	}
	low := strings.ToLower(val)
	for _, f := range []string{"javascript:", "data:", "<", "expression", "@import"} {
		if strings.Contains(low, f) {
			bad("<%s %s>: %q is not allowed in a drawing", tag, key, f)
			return "", "", false
		}
	}
	switch {
	case name == "id":
		if !svgIDRE().MatchString(val) {
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
		if !ok || !svgIDRE().MatchString(id) {
			bad("<%s href=%q>: a drawing refers only to its own shapes, as #id", tag, val)
			return "", "", false
		}
		d.refs = append(d.refs, svgRef{id: id, line: line})
	case strings.Contains(low, "url("):
		m := svgRefRE().FindStringSubmatch(strings.TrimSpace(val))
		if m == nil || !svgRefAttrs()[name] {
			bad("<%s %s=%q>: a drawing refers only to its own gradients, clips, masks, filters "+
				"and markers, as url(#id)", tag, key, val)
			return "", "", false
		}
		d.refs = append(d.refs, svgRef{id: m[1], line: line})
	}
	return name, val, true
}

// resolve reports every reference to an id the drawing does not declare.
func (d *svgDrawing) resolve(bad func(line int, format string, args ...any)) {
	for _, r := range d.refs {
		if !d.ids[r.id] {
			bad(r.line, "the drawing refers to #%s, which none of its own elements declares", r.id)
		}
	}
}
