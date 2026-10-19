package slideformat

import (
	"slices"
	"strings"
)

// ---- the style table ------------------------------------------------------ //
//
// One row a property: the grammar its value is read by, and the elements it
// applies to. The table is Claude's slide subset (format.md, "Supported CSS")
// less what Crewlet's renderer does not draw; a property not here, or on an
// element its row does not name, is refused with the closest thing it may be.

// Decl is one style declaration of the canonical tree: a property and the
// value its grammar wrote back. Declarations keep the order they were
// written, since a later side of a border overrides the whole.
type Decl struct {
	Property string `json:"p"`
	Value    string `json:"v"`
}

// styleCtx is what a value's grammar may ask about where it sits.
type styleCtx struct {
	tag string
}

type valueFunc func(v string, c *styleCtx) (string, error)

type property struct {
	value valueFunc
	on    []string
}

// The element groups the table names.
var (
	textTags   = []string{"h1", "h2", "h3", "p", "ul", "ol", "li"}
	boxTags    = []string{"section", "div"}
	cellTags   = []string{"th", "td"}
	placedTags = []string{"div", "h1", "h2", "h3", "p", "ul", "ol", "img", "table", "hr", "x-shape",
		"x-icon", "svg"}
	typeTags  = union(boxTags, textTags, cellTags, []string{"table"})
	paintTags = union([]string{"div", "img", "table", "x-icon", "x-shape", "hr"}, textTags, cellTags)
)

func union(sets ...[]string) []string {
	var out []string
	for _, s := range sets {
		for _, t := range s {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	return out
}

func on(sets ...[]string) []string { return union(sets...) }

func tags(t ...string) []string { return t }

// properties is the table.
var properties = map[string]property{
	"position":                {keyword("absolute", "relative"), placedTags},
	"left":                    {offset(true), placedTags},
	"top":                     {offset(true), placedTags},
	"right":                   {offset(false), placedTags},
	"bottom":                  {offset(false), placedTags},
	"width":                   {lengthPctAuto, on(placedTags, cellTags)},
	"height":                  {lengthPctAuto, placedTags},
	"min-width":               {minWidth, placedTags},
	"min-height":              {lengthUpTo(1080), placedTags},
	"max-width":               {lengthUpTo(1920), placedTags},
	"max-height":              {lengthUpTo(1080), placedTags},
	"display":                 {keyword("flex", "grid", "none"), boxTags},
	"flex-direction":          {keyword("row", "column", "row-reverse", "column-reverse"), boxTags},
	"flex-wrap":               {keyword("wrap", "nowrap"), tags("div")},
	"gap":                     {lengthUpTo(512), boxTags},
	"align-items":             {keyword(alignWords...), boxTags},
	"justify-content":         {keyword("start", "center", "end", "flex-start", "flex-end", "space-between", "space-around", "space-evenly"), boxTags},
	"justify-items":           {keyword("start", "center", "end", "stretch"), boxTags},
	"align-self":              {keyword(append([]string{"auto"}, alignWords...)...), placedTags},
	"justify-self":            {keyword("auto", "start", "center", "end", "stretch"), placedTags},
	"flex":                    {flex, placedTags},
	"flex-grow":               {numberOnly(0, 1000), placedTags},
	"flex-shrink":             {numberOnly(0, 1000), placedTags},
	"flex-basis":              {flexBasis, placedTags},
	"grid-template-columns":   {tracks, boxTags},
	"grid-template-rows":      {tracks, boxTags},
	"grid-column":             {gridSpan, placedTags},
	"grid-row":                {gridSpan, placedTags},
	"aspect-ratio":            {aspectRatio, placedTags},
	"padding":                 {boxSides(0, 256, false), on(boxTags, textTags, cellTags, tags("table"))},
	"overflow":                {keyword("hidden", "visible"), tags("div")},
	"font-family":             {family, typeTags},
	"font-size":               {fontSize, typeTags},
	"font-weight":             {fontWeight, on(typeTags, tags("span"))},
	"font-style":              {keyword("normal", "italic"), on(typeTags, tags("span"))},
	"font":                    {font, typeTags},
	"line-height":             {lineHeight, typeTags},
	"letter-spacing":          {letterSpacing, typeTags},
	"text-align":              {keyword("left", "center", "right", "justify"), typeTags},
	"text-transform":          {keyword("none", "uppercase", "lowercase", "capitalize"), typeTags},
	"white-space":             {keyword("normal", "nowrap"), typeTags},
	"text-decoration":         {textDecoration, on(typeTags, tags("span"))},
	"font-variant-numeric":    {keyword("normal", "tabular-nums"), typeTags},
	"-webkit-text-stroke":     {textStroke, typeTags},
	"color":                   {colorValue, on(typeTags, tags("span", "x-icon", "hr"))},
	"background":              {background, on(boxTags, paintTags, tags("tr"))},
	"background-color":        {colorValue, on(boxTags, paintTags, tags("tr"))},
	"background-clip":         {keyword("text"), textTags},
	"-webkit-background-clip": {keyword("text"), textTags},
	"-webkit-text-fill-color": {fillColor, on(textTags, tags("span"))},
	"border":                  {border, paintTags},
	"border-top":              {border, paintTags},
	"border-right":            {border, paintTags},
	"border-bottom":           {border, paintTags},
	"border-left":             {border, paintTags},
	"border-radius":           {boxSides(0, 1080, true), on(tags("div", "img", "table", "x-shape"), textTags)},
	"box-shadow":              {shadows(false), on(tags("div", "img", "table", "x-icon", "x-shape"), textTags)},
	"text-shadow":             {shadows(true), on(textTags, tags("table"))},
	"opacity":                 {numberOnly(0, 1), on(placedTags, tags("section"))},
	"transform":               {transform, placedTags},
	"filter":                  {filter, on(tags("div", "img", "x-shape"), textTags)},
	"backdrop-filter":         {filter, on(tags("div", "img", "x-shape"), textTags)},
	"mix-blend-mode":          {keyword("normal", "multiply", "screen", "overlay", "darken", "lighten"), on(tags("div", "img", "x-shape"), textTags)},
	"object-fit":              {keyword("cover", "contain"), tags("img")},
	"margin":                  {noop("0", "0px"), union(placedTags, textTags, cellTags, tags("section"))},
	"box-sizing":              {noop("border-box"), union(placedTags, textTags, cellTags, tags("section"))},
	"grid-template":           {noop("none"), boxTags},
}

// fillColor is the text fill under `background-clip:text`: transparent so the
// glyphs take the background, or a span's return to its own colour.
func fillColor(v string, c *styleCtx) (string, error) {
	low := strings.ToLower(v)
	if low == "transparent" || (low == "currentcolor" && c.tag == "span") {
		return low, nil
	}
	return "", want("transparent, or currentcolor on a span")
}

// forbidden are the substrings no value may carry, whatever its property:
// anything that names a URL or another declaration, escapes or comments.
var forbidden = []string{"url(", "image(", "image-set(", "var(", "env(", "attr(", "expression",
	"@", "\\", "/*", "*/", "!important", "<", ">", "{", "}"}

// parseStyle reads a `style` attribute of the element tag into its canonical
// declarations, each problem reported through bad.
func parseStyle(tag, raw string, bad func(format string, args ...any)) []Decl {
	var out []Decl
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, ":")
		if !ok {
			bad("style %q is not a property: value pair", part)
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		low := strings.ToLower(value)
		if i := slices.IndexFunc(forbidden, func(f string) bool { return strings.Contains(low, f) }); i >= 0 {
			bad("style %s: %q is not allowed in a slide (no URLs, variables, escapes or "+
				"comments; an image is an <img> of a file in the version)", name, forbidden[i])
			continue
		}
		prop, known := properties[name]
		if !known {
			bad("style %s is not in the slide subset%s", name, closest(name))
			continue
		}
		if !slices.Contains(prop.on, tag) {
			bad("style %s does not apply to <%s>; it applies to %s", name, tag, describeTags(prop.on))
			continue
		}
		if value == "" {
			bad("style %s has no value", name)
			continue
		}
		got, err := prop.value(value, &styleCtx{tag: tag})
		if err != nil {
			bad("style %s: %q %s", name, value, err.Error())
			continue
		}
		if got == "" {
			continue // a no-op, accepted and left out
		}
		out = slices.DeleteFunc(out, func(d Decl) bool { return d.Property == name })
		out = append(out, Decl{Property: name, Value: got})
	}
	return out
}

// closest is a hint for a property the subset does not take.
func closest(name string) string {
	switch {
	case name == "z-index":
		return ": paint order is source order, so list an element later to paint it on top"
	case strings.HasPrefix(name, "margin"):
		return ": space boxes with gap on their container, or padding"
	case name == "float":
		return ": lay boxes out in a flex row"
	case strings.HasPrefix(name, "background-"):
		return ": use background with a colour or a gradient"
	case strings.HasPrefix(name, "grid-"):
		return ": use grid-template-columns, grid-template-rows and span N"
	}
	return ""
}

func describeTags(set []string) string {
	out := make([]string, len(set))
	for i, t := range set {
		out[i] = "<" + t + ">"
	}
	return strings.Join(out, ", ")
}

// declared answers a property's canonical value in decls.
func declared(decls []Decl, name string) (string, bool) {
	for _, d := range decls {
		if d.Property == name {
			return d.Value, true
		}
	}
	return "", false
}

// checkPinned holds the offsets to a pinned element: `left`, `top`, `right`
// and `bottom` place a `position:absolute` box and nothing else.
func checkPinned(decls []Decl, bad func(format string, args ...any)) {
	pos, _ := declared(decls, "position")
	for _, side := range sides {
		if _, set := declared(decls, side); set && pos != "absolute" {
			bad("style %s places only a pinned element: add position:absolute, or lay the "+
				"element out in its container's flow", side)
		}
	}
}
