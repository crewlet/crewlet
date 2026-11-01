package slideformat

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/words"
)

// ---- the value grammar ---------------------------------------------------- //
//
// Every inline style value is read by the grammar its property names, and the
// form a renderer is handed is the one the grammar wrote back: a bare number
// that means pixels gains its `px`, a point size becomes pixels, keywords are
// lowercased. What no grammar reads is refused, never passed through, so a
// value the dashboard applies is always one this file accepted.

// errValue is a value outside its property's grammar; the message names what
// the grammar takes.
type errValue struct{ want string }

func (e errValue) Error() string { return "takes " + e.want }

func want(format string, args ...any) error { return errValue{want: fmt.Sprintf(format, args...)} }

var numberRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$`)
})

// number reads a plain number.
func number(s string) (float64, bool) {
	if !numberRE().MatchString(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// fmtNum writes a number in its shortest exact form.
func fmtNum(f float64) string {
	if f == 0 {
		return "0"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// unit reads a number followed by one of units ("" for a bare number).
func unit(s string, units ...string) (float64, string, bool) {
	s = strings.ToLower(s)
	for _, u := range units {
		if u == "" {
			continue
		}
		if rest, ok := strings.CutSuffix(s, u); ok {
			if f, ok := number(rest); ok {
				return f, u, true
			}
		}
	}
	if slices.Contains(units, "") {
		if f, ok := number(s); ok {
			return f, "", true
		}
	}
	return 0, "", false
}

// length reads LEN: `Npx`, a bare N meaning pixels, or `Npt` converted to
// pixels, and answers its pixels.
func length(s string) (float64, bool) {
	f, u, ok := unit(s, "px", "pt", "")
	if !ok {
		return 0, false
	}
	if u == "pt" {
		f = f * 4 / 3
	}
	return f, true
}

func px(f float64) string {
	if f == 0 {
		return "0"
	}
	return fmtNum(math.Round(f*1000)/1000) + "px"
}

// lengthIn reads LEN within [lo, hi] pixels.
func lengthIn(s string, lo, hi float64) (string, error) {
	f, ok := length(s)
	if !ok {
		return "", want("a length in px (no em, rem, vw, vh or %%)")
	}
	if f < lo || f > hi {
		return "", want("a length from %spx to %spx", fmtNum(lo), fmtNum(hi))
	}
	return px(f), nil
}

func percent(s string) (float64, bool) {
	f, _, ok := unit(s, "%")
	return f, ok
}

func pct(f float64) string { return fmtNum(f) + "%" }

// angle reads ANGLE.
func angle(s string) (string, bool) {
	if s == "0" {
		return "0deg", true
	}
	f, u, ok := unit(s, "deg", "turn", "rad")
	if !ok {
		return "", false
	}
	return fmtNum(f) + u, true
}

// numberIn reads N within [lo, hi].
func numberIn(s string, lo, hi float64) (string, error) {
	f, ok := number(s)
	if !ok || f < lo || f > hi {
		return "", want("a number from %s to %s", fmtNum(lo), fmtNum(hi))
	}
	return fmtNum(f), nil
}

// ---- splitting ------------------------------------------------------------ //

// split cuts s at every top-level rune isSep matches (parentheses nest), each
// part trimmed, and reports false for unbalanced parentheses.
func split(s string, isSep func(rune) bool) ([]string, bool) {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
			if depth < 0 {
				return nil, false
			}
		case depth == 0 && isSep(r):
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + utf8.RuneLen(r)
		}
	}
	if depth != 0 {
		return nil, false
	}
	return append(out, strings.TrimSpace(s[start:])), true
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' }

// terms is s split at top-level whitespace.
func terms(s string) []string {
	parts, ok := split(s, isSpace)
	if !ok {
		return nil
	}
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// commas is s split at top-level commas, each part trimmed; false for an
// empty part or unbalanced parentheses.
func commas(s string) ([]string, bool) {
	out, ok := split(s, func(r rune) bool { return r == ',' })
	if !ok {
		return nil, false
	}
	for _, p := range out {
		if p == "" {
			return nil, false
		}
	}
	return out, true
}

// call reads `name(args)` and answers the lowercased name and the arguments.
func call(s string) (string, string, bool) {
	open := strings.IndexByte(s, '(')
	if open <= 0 || !strings.HasSuffix(s, ")") {
		return "", "", false
	}
	name := strings.ToLower(s[:open])
	for _, r := range name {
		if (r < 'a' || r > 'z') && r != '-' {
			return "", "", false
		}
	}
	return name, strings.TrimSpace(s[open+1 : len(s)-1]), true
}

// ---- colours -------------------------------------------------------------- //

var hexRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
})

// color reads COLOR: a hex colour, `rgb[a]()`, `hsl[a]()`, a CSS named colour
// or `transparent`. Never `currentcolor` or `var()`.
func color(s string) (string, error) {
	low := strings.ToLower(strings.TrimSpace(s))
	switch {
	case hexRE().MatchString(low):
		return low, nil
	case low == "transparent" || namedColors()[low]:
		return low, nil
	}
	name, args, ok := call(low)
	if !ok || !slices.Contains([]string{"rgb", "rgba", "hsl", "hsla"}, name) {
		return "", want("a colour: #hex, rgb(), rgba(), hsl(), hsla(), a named colour or transparent")
	}
	var parts []string
	if strings.Contains(args, ",") {
		parts, ok = commas(args)
	} else {
		var alpha string
		main, a, slash := strings.Cut(args, "/")
		parts = terms(main)
		if slash {
			alpha = strings.TrimSpace(a)
			parts = append(parts, alpha)
		}
		ok = len(parts) > 0
	}
	if !ok || len(parts) < 3 || len(parts) > 4 {
		return "", want("%s() with three channels and an optional alpha", name)
	}
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if i == 0 && strings.HasPrefix(name, "hsl") {
			if _, ok := angle(p); ok {
				continue
			}
		}
		if _, ok := number(p); ok {
			continue
		}
		if _, ok := percent(p); ok {
			continue
		}
		return "", want("%s() whose channels are numbers or percentages", name)
	}
	return name + "(" + strings.Join(trimAll(parts), ", ") + ")", nil
}

func trimAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

// ---- keyword sets --------------------------------------------------------- //

func keyword(set ...string) valueFunc {
	return func(v string, _ *styleCtx) (string, error) {
		low := strings.ToLower(v)
		if slices.Contains(set, low) {
			return low, nil
		}
		return "", want("one of %s", words.Or(set))
	}
}

var alignWords = []string{"start", "center", "end", "stretch", "baseline", "flex-start",
	"flex-end", "self-start", "self-end"}

// ---- composite grammars --------------------------------------------------- //

// lengthPctAuto reads LEN, PCT or `auto`.
func lengthPctAuto(v string, _ *styleCtx) (string, error) {
	low := strings.ToLower(v)
	if low == "auto" {
		return low, nil
	}
	if f, ok := percent(low); ok {
		return pct(f), nil
	}
	if f, ok := length(low); ok {
		return px(f), nil
	}
	return "", want("a length in px, a percentage or auto")
}

// offset reads a pinned element's offset: LEN, PCT, `auto`, or for `left` and
// `top` `calc(PCT ± LEN)`, which centres a box of known size.
func offset(calcOK bool) valueFunc {
	return func(v string, c *styleCtx) (string, error) {
		if name, args, ok := call(v); ok && name == "calc" && calcOK {
			w := terms(args)
			if len(w) == 3 && (w[1] == "+" || w[1] == "-") {
				p, okP := percent(w[0])
				l, okL := length(w[2])
				if okP && okL {
					return "calc(" + pct(p) + " " + w[1] + " " + px(l) + ")", nil
				}
			}
			return "", want("calc(N%% + Npx) or calc(N%% - Npx)")
		}
		return lengthPctAuto(v, c)
	}
}

// lengthUpTo reads LEN from 0 to hi pixels.
func lengthUpTo(hi float64) valueFunc {
	return func(v string, _ *styleCtx) (string, error) { return lengthIn(v, 0, hi) }
}

func numberOnly(lo, hi float64) valueFunc {
	return func(v string, _ *styleCtx) (string, error) { return numberIn(v, lo, hi) }
}

// boxSides reads one to four LEN (or PCT where pctOK) within [lo, hi].
func boxSides(lo, hi float64, pctOK bool) valueFunc {
	return func(v string, _ *styleCtx) (string, error) {
		w := terms(v)
		if len(w) < 1 || len(w) > 4 {
			return "", want("one to four lengths")
		}
		out := make([]string, len(w))
		for i, s := range w {
			if p, ok := percent(s); ok && pctOK {
				out[i] = pct(p)
				continue
			}
			l, err := lengthIn(s, lo, hi)
			if err != nil {
				return "", err
			}
			out[i] = l
		}
		return strings.Join(out, " "), nil
	}
}

func minWidth(v string, _ *styleCtx) (string, error) {
	low := strings.ToLower(v)
	if low == "min-content" || low == "max-content" {
		return low, nil
	}
	return lengthIn(v, 0, 1920)
}

// flex reads `none`, `auto`, or `N [N] [LEN | 0% | auto]`.
func flex(v string, _ *styleCtx) (string, error) {
	low := strings.ToLower(v)
	if low == "none" || low == "auto" {
		return low, nil
	}
	w := terms(low)
	if len(w) < 1 || len(w) > 3 {
		return "", want("none, auto, or a grow factor with an optional shrink factor and basis")
	}
	out := []string{}
	for i, s := range w {
		if i < 2 {
			if f, ok := number(s); ok && f >= 0 && f <= 1000 {
				out = append(out, fmtNum(f))
				continue
			}
			if i == 0 {
				return "", want("a grow factor first")
			}
		}
		switch {
		case s == "auto" || s == "0%":
			out = append(out, s)
		default:
			l, ok := length(s)
			if !ok || i == 0 {
				return "", want("a basis in px, 0%% or auto")
			}
			out = append(out, px(l))
		}
	}
	return strings.Join(out, " "), nil
}

func flexBasis(v string, _ *styleCtx) (string, error) {
	if strings.EqualFold(v, "auto") {
		return "auto", nil
	}
	return lengthIn(v, 0, 1920)
}

// tracks reads a grid template: up to 24 tracks of LEN, `Nfr`, `auto` or
// `repeat(INT, tracks)`.
func tracks(v string, _ *styleCtx) (string, error) {
	n := 0
	var read func(string, bool) (string, error)
	read = func(s string, nested bool) (string, error) {
		var out []string
		for _, t := range terms(s) {
			low := strings.ToLower(t)
			if name, args, ok := call(low); ok && name == "repeat" && !nested {
				parts, ok := split(args, func(r rune) bool { return r == ',' })
				if !ok || len(parts) != 2 {
					return "", want("repeat(N, tracks)")
				}
				count, err := strconv.Atoi(strings.TrimSpace(parts[0]))
				if err != nil || count < 1 || count > 24 {
					return "", want("repeat with a count from 1 to 24")
				}
				before := n
				inner, err := read(parts[1], true)
				if err != nil {
					return "", err
				}
				n = before + (n-before)*count
				out = append(out, "repeat("+strconv.Itoa(count)+", "+inner+")")
				continue
			}
			n++
			switch {
			case low == "auto":
				out = append(out, low)
			case strings.HasSuffix(low, "fr"):
				f, ok := number(strings.TrimSuffix(low, "fr"))
				if !ok || f < 0 || f > 1000 {
					return "", want("tracks in px, fr or auto")
				}
				out = append(out, fmtNum(f)+"fr")
			default:
				l, err := lengthIn(low, 0, 1920)
				if err != nil {
					return "", want("tracks in px, fr, auto or repeat() (no minmax, auto-fill, names or areas)")
				}
				out = append(out, l)
			}
		}
		if len(out) == 0 {
			return "", want("at least one track")
		}
		return strings.Join(out, " "), nil
	}
	out, err := read(v, false)
	if err != nil {
		return "", err
	}
	if n > 24 {
		return "", want("at most 24 tracks")
	}
	return out, nil
}

func gridSpan(v string, _ *styleCtx) (string, error) {
	w := terms(strings.ToLower(v))
	if len(w) == 2 && w[0] == "span" {
		if n, err := strconv.Atoi(w[1]); err == nil && n >= 1 && n <= 24 {
			return "span " + strconv.Itoa(n), nil
		}
	}
	return "", want("span N, from 1 to 24 (cells fill in source order)")
}

func aspectRatio(v string, _ *styleCtx) (string, error) {
	a, b, slash := strings.Cut(v, "/")
	x, err := numberIn(strings.TrimSpace(a), 0.01, 100)
	if err != nil {
		return "", want("N or N / N")
	}
	if !slash {
		return x, nil
	}
	y, err := numberIn(strings.TrimSpace(b), 0.01, 100)
	if err != nil {
		return "", want("N or N / N")
	}
	return x + " / " + y, nil
}

// ---- type ----------------------------------------------------------------- //

var familyRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 _-]{0,39}$`)
})

var genericFamilies = []string{"serif", "sans-serif", "monospace", "cursive", "system-ui"}

// family reads FAMILY: a comma-separated list of family names, quoted or not,
// each a letter then letters, digits, spaces, `_` and `-`, at most 40, or a
// generic family. A family the deck does not declare and the reader lacks
// falls back to the next, as in any stylesheet.
func family(v string, _ *styleCtx) (string, error) {
	parts, ok := commas(v)
	if !ok || len(parts) > 8 {
		return "", want("a list of up to eight family names")
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		quoted := len(p) >= 2 && (p[0] == '\'' || p[0] == '"') && p[len(p)-1] == p[0]
		name := p
		if quoted {
			name = p[1 : len(p)-1]
		}
		name = strings.Join(strings.Fields(name), " ")
		if !familyRE().MatchString(name) {
			return "", want("family names of letters, digits, spaces, _ and -, a letter first, " +
				"at most 40 characters")
		}
		if !quoted && slices.Contains(genericFamilies, strings.ToLower(name)) {
			out[i] = strings.ToLower(name)
			continue
		}
		out[i] = "'" + name + "'"
	}
	return strings.Join(out, ", "), nil
}

func fontSize(v string, _ *styleCtx) (string, error) { return lengthIn(v, 8, 400) }

func fontWeight(v string, _ *styleCtx) (string, error) {
	low := strings.ToLower(v)
	if low == "normal" || low == "bold" {
		return low, nil
	}
	if n, err := strconv.Atoi(low); err == nil && n >= 100 && n <= 900 && n%100 == 0 {
		return low, nil
	}
	return "", want("normal, bold or a whole hundred from 100 to 900")
}

// lineHeight reads N, PCT or LEN; the 0.5 to 4 times bound is held where the
// size is known, so a bare multiplier is held to it here.
func lineHeight(v string, _ *styleCtx) (string, error) {
	low := strings.ToLower(v)
	if f, ok := number(low); ok {
		if f < 0.5 || f > 4 {
			return "", want("a multiplier from 0.5 to 4")
		}
		return fmtNum(f), nil
	}
	if f, ok := percent(low); ok {
		if f < 50 || f > 400 {
			return "", want("a percentage from 50%% to 400%%")
		}
		return pct(f), nil
	}
	return lengthIn(low, 4, 1600)
}

func letterSpacing(v string, _ *styleCtx) (string, error) {
	low := strings.ToLower(v)
	if low == "normal" {
		return low, nil
	}
	if f, _, ok := unit(low, "em"); ok {
		if f < -0.5 || f > 1 {
			return "", want("an em spacing from -0.5em to 1em")
		}
		return fmtNum(f) + "em", nil
	}
	return lengthIn(low, -24, 32)
}

// font reads the shorthand `[italic] [weight] LEN[/N] FAMILY`.
func font(v string, c *styleCtx) (string, error) {
	w := terms(v)
	var out []string
	i := 0
	if i < len(w) && strings.EqualFold(w[i], "italic") {
		out = append(out, "italic")
		i++
	}
	if i < len(w) {
		if fw, err := fontWeight(w[i], c); err == nil {
			out = append(out, fw)
			i++
		}
	}
	if i >= len(w) {
		return "", want("[italic] [weight] size[/line-height] family")
	}
	size, lh, hasLH := strings.Cut(w[i], "/")
	s, err := fontSize(size, c)
	if err != nil {
		return "", err
	}
	if hasLH {
		l, lhErr := lineHeight(lh, c)
		if lhErr != nil {
			return "", lhErr
		}
		s += "/" + l
	}
	out = append(out, s)
	i++
	if i >= len(w) {
		return "", want("a family after the size")
	}
	f, err := family(strings.Join(w[i:], " "), c)
	if err != nil {
		return "", err
	}
	return strings.Join(append(out, f), " "), nil
}

var decorationStyles = []string{"solid", "double", "dotted", "dashed", "wavy"}

func textDecoration(v string, c *styleCtx) (string, error) {
	w := terms(strings.ToLower(v))
	if len(w) == 0 || !slices.Contains([]string{"none", "underline", "line-through"}, w[0]) {
		return "", want("none, underline or line-through, then an optional style and colour")
	}
	if c.tag == "span" && w[0] == "line-through" {
		return "", want("underline or none on a span")
	}
	out := []string{w[0]}
	rest := w[1:]
	if len(rest) > 0 && slices.Contains(decorationStyles, rest[0]) {
		out = append(out, rest[0])
		rest = rest[1:]
	}
	if len(rest) == 1 {
		col, err := color(rest[0])
		if err != nil {
			return "", err
		}
		out = append(out, col)
		rest = nil
	}
	if len(rest) > 0 || (w[0] == "none" && len(out) > 1) {
		return "", want("none, underline or line-through, then an optional style and colour")
	}
	return strings.Join(out, " "), nil
}

func textStroke(v string, _ *styleCtx) (string, error) {
	w := terms(v)
	if len(w) != 2 {
		return "", want("a width from 0 to 8px and a colour")
	}
	l, err := lengthIn(w[0], 0, 8)
	if err != nil {
		return "", err
	}
	col, err := color(w[1])
	if err != nil {
		return "", err
	}
	return l + " " + col, nil
}

func colorValue(v string, _ *styleCtx) (string, error) { return color(v) }

// ---- paint ---------------------------------------------------------------- //

var sides = []string{"top", "right", "bottom", "left"}

// gradient reads one linear, repeating-linear or radial gradient of two to
// eight stops.
func gradient(v string) (string, error) {
	name, args, ok := call(v)
	if !ok || !slices.Contains([]string{"linear-gradient", "repeating-linear-gradient",
		"radial-gradient"}, name) {
		return "", want("a colour or a linear-gradient(), repeating-linear-gradient() or radial-gradient()")
	}
	parts, ok := commas(args)
	if !ok {
		return "", want("%s() with comma-separated stops", name)
	}
	var out []string
	first := strings.ToLower(parts[0])
	switch name {
	case "radial-gradient":
		if head, err := radialHead(first); err == nil {
			out = append(out, head)
			parts = parts[1:]
		}
	default:
		if a, ok := angle(first); ok {
			out, parts = append(out, a), parts[1:]
		} else if to, ok := strings.CutPrefix(first, "to "); ok {
			for _, s := range terms(to) {
				if !slices.Contains(sides, s) {
					return "", want("a direction of to top, right, bottom or left")
				}
			}
			out, parts = append(out, "to "+strings.Join(terms(to), " ")), parts[1:]
		}
	}
	if len(parts) < 2 || len(parts) > 8 {
		return "", want("two to eight colour stops")
	}
	for _, p := range parts {
		w := terms(p)
		if len(w) < 1 || len(w) > 3 {
			return "", want("a stop of a colour and up to two positions")
		}
		col, err := color(w[0])
		if err != nil {
			return "", err
		}
		stop := []string{col}
		for _, pos := range w[1:] {
			if f, ok := percent(pos); ok {
				stop = append(stop, pct(f))
				continue
			}
			l, ok := length(pos)
			if !ok {
				return "", want("stop positions in %% or px")
			}
			stop = append(stop, px(l))
		}
		out = append(out, strings.Join(stop, " "))
	}
	return name + "(" + strings.Join(out, ", ") + ")", nil
}

func radialHead(s string) (string, error) {
	w := terms(s)
	var out []string
	if len(w) > 0 && (w[0] == "circle" || w[0] == "ellipse") {
		out, w = append(out, w[0]), w[1:]
	}
	if len(w) == 3 && w[0] == "at" {
		x, okX := percent(w[1])
		y, okY := percent(w[2])
		if !okX || !okY {
			return "", want("at X%% Y%%")
		}
		out, w = append(out, "at "+pct(x)+" "+pct(y)), nil
	}
	if len(out) == 0 || len(w) > 0 {
		return "", want("circle, ellipse or at X%% Y%%")
	}
	return strings.Join(out, " "), nil
}

// background reads a colour, or one gradient with an optional colour under
// it. A table row takes a colour alone; `url()` never reaches here.
func background(v string, c *styleCtx) (string, error) {
	if c.tag == "tr" || c.tag == "x-shape" || c.tag == "hr" {
		return color(v)
	}
	parts, ok := commas(v)
	if !ok {
		return "", want("a colour or a gradient")
	}
	// A gradient's own commas are inside its parentheses: the top level
	// holds the gradient and, at most, the colour under it.
	switch len(parts) {
	case 1:
		if _, _, isCall := call(parts[0]); isCall && strings.Contains(parts[0], "gradient") {
			return gradient(parts[0])
		}
		return color(parts[0])
	case 2:
		g, err := gradient(parts[0])
		if err != nil {
			return "", err
		}
		col, err := color(parts[1])
		if err != nil {
			return "", err
		}
		return g + ", " + col, nil
	}
	return "", want("one gradient, optionally with a colour under it")
}

var borderStyles = []string{"solid", "dashed", "dotted", "double", "none"}

// maxBorder is the widest stroke a border draws, Claude's format's bound.
const maxBorder = 16

// border reads `[LEN] style [COLOR]`, the style word required; a side also
// takes `none`.
func border(v string, _ *styleCtx) (string, error) {
	w := terms(strings.ToLower(v))
	if len(w) == 1 && w[0] == "none" {
		return "none", nil
	}
	var out []string
	i := 0
	if i < len(w) {
		if l, ok := length(w[i]); ok {
			if l < 0 || l > maxBorder {
				return "", want("a width from 0 to %dpx", maxBorder)
			}
			out = append(out, px(l))
			i++
		}
	}
	if i >= len(w) || !slices.Contains(borderStyles, w[i]) {
		return "", want("[width] solid, dashed, dotted, double or none [colour]: the style word is required")
	}
	out = append(out, w[i])
	i++
	if i < len(w) {
		col, err := color(strings.Join(w[i:], " "))
		if err != nil {
			return "", err
		}
		out = append(out, col)
	}
	return strings.Join(out, " "), nil
}

// shadows reads up to eight comma-separated shadows: `[inset] x y [blur
// [spread]] COLOR` for a box, `x y [blur] COLOR` for text.
func shadows(text bool) valueFunc {
	return func(v string, _ *styleCtx) (string, error) {
		if strings.EqualFold(v, "none") {
			return "none", nil
		}
		parts, ok := commas(v)
		if !ok || len(parts) > 8 {
			return "", want("up to eight shadows")
		}
		out := make([]string, len(parts))
		for i, p := range parts {
			w := terms(p)
			var s []string
			if !text && len(w) > 0 && strings.EqualFold(w[0], "inset") {
				s, w = append(s, "inset"), w[1:]
			}
			if len(w) < 3 {
				return "", want("x y [blur] colour")
			}
			col, err := color(w[len(w)-1])
			if err != nil {
				return "", err
			}
			lens := w[:len(w)-1]
			maxLens := 4
			if text {
				maxLens = 3
			}
			if len(lens) < 2 || len(lens) > maxLens {
				return "", want("x y [blur] colour")
			}
			bounds := [][2]float64{{-64, 64}, {-64, 64}, {0, 160}, {-32, 32}}
			if text {
				bounds[2] = [2]float64{0, 64}
			}
			for j, l := range lens {
				got, err := lengthIn(l, bounds[j][0], bounds[j][1])
				if err != nil {
					return "", err
				}
				s = append(s, got)
			}
			out[i] = strings.Join(append(s, col), " ")
		}
		return strings.Join(out, ", "), nil
	}
}

// transform reads translate, rotate, scale and skew, any subset, in that order,
// each once.
func transform(v string, c *styleCtx) (string, error) {
	order := []string{"translate", "translatex", "translatey", "rotate", "scale", "skew", "skewx", "skewy"}
	last := -1
	var out []string
	for _, fn := range terms(v) {
		name, args, ok := call(fn)
		at := slices.Index(order, name)
		if !ok || at < 0 {
			return "", want("translate(), rotate(), scale() and skew() only")
		}
		if c.tag == "x-shape" && name != "rotate" {
			return "", want("rotate() only on a shape")
		}
		if at <= last {
			return "", want("each function once, in the order translate, rotate, scale, skew")
		}
		last = at
		var got []string
		switch name {
		case "translate", "translatex", "translatey":
			parts, ok := commas(args)
			if !ok || len(parts) > 2 || (name != "translate" && len(parts) != 1) {
				return "", want("%s(LEN)", name)
			}
			for _, p := range parts {
				if f, ok := percent(p); ok && f == -50 {
					got = append(got, "-50%")
					continue
				}
				l, err := lengthIn(p, -1920, 1920)
				if err != nil {
					return "", err
				}
				got = append(got, l)
			}
		case "rotate":
			a, ok := angle(args)
			if !ok {
				return "", want("rotate(ANGLE)")
			}
			got = []string{a}
		case "scale":
			n, err := numberIn(args, 0.5, 2)
			if err != nil {
				return "", err
			}
			got = []string{n}
		default:
			parts, ok := commas(args)
			if !ok || len(parts) > 2 {
				return "", want("%s(ANGLE)", name)
			}
			for _, p := range parts {
				a, ok := angle(p)
				f, _, _ := unit(a, "deg", "turn", "rad")
				if !ok || (strings.HasSuffix(a, "deg") && math.Abs(f) > 60) {
					return "", want("a skew of at most 60deg either way")
				}
				got = append(got, a)
			}
		}
		out = append(out, canonicalFn(name)+"("+strings.Join(got, ", ")+")")
	}
	if len(out) == 0 {
		return "", want("translate(), rotate(), scale() or skew()")
	}
	return strings.Join(out, " "), nil
}

func canonicalFn(name string) string {
	switch name {
	case "translatex":
		return "translateX"
	case "translatey":
		return "translateY"
	case "skewx":
		return "skewX"
	case "skewy":
		return "skewY"
	}
	return name
}

// filter reads blur, brightness, contrast, saturate, grayscale, sepia,
// invert and hue-rotate, any subset.
func filter(v string, _ *styleCtx) (string, error) {
	if strings.EqualFold(v, "none") {
		return "none", nil
	}
	bounds := map[string][2]float64{"brightness": {0, 2}, "contrast": {0, 2}, "saturate": {0, 3},
		"grayscale": {0, 1}, "sepia": {0, 1}, "invert": {0, 1}}
	var out []string
	for _, fn := range terms(v) {
		name, args, ok := call(fn)
		if !ok {
			return "", want("filter functions")
		}
		switch name {
		case "blur":
			l, err := lengthIn(args, 0, 20)
			if err != nil {
				return "", err
			}
			out = append(out, "blur("+l+")")
		case "hue-rotate":
			a, ok := angle(args)
			if !ok {
				return "", want("hue-rotate(ANGLE)")
			}
			out = append(out, "hue-rotate("+a+")")
		default:
			b, known := bounds[name]
			if !known {
				return "", want("blur, brightness, contrast, saturate, grayscale, sepia, invert or hue-rotate")
			}
			n, err := numberIn(args, b[0], b[1])
			if err != nil {
				return "", err
			}
			out = append(out, name+"("+n+")")
		}
	}
	if len(out) == 0 {
		return "", want("filter functions")
	}
	return strings.Join(out, " "), nil
}

// noop accepts a value that changes nothing on the stage (`margin:0`,
// `box-sizing:border-box`, `grid-template:none`), which is then left out of
// the canonical tree.
func noop(ok ...string) valueFunc {
	return func(v string, _ *styleCtx) (string, error) {
		if slices.Contains(ok, strings.ToLower(v)) {
			return "", nil
		}
		return "", want("only %s, which changes nothing on the stage", words.Or(ok))
	}
}
