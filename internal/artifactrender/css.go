package artifactrender

import (
	"slices"
	"sort"
	"strings"
)

// THE CUSTOM PROPERTY SCAN: which `var(--name)` a page's stylesheets reference
// that no stylesheet, style attribute or `@property` rule it loaded defines.
// Such a reference makes its declaration invalid at computed-value time, so a
// `background: var(--missing)` paints nothing, which the eye misses on a white
// page and a reader sees as a see-through bar.
//
// A SCAN OF THE TEXT, NOT THE CSSOM: in the shared origin mode a page's own
// stylesheets are cross-origin to its opaque document, so their rules cannot
// be read from the page, while the engine served every byte of them. The scan
// keeps strings and comments out, follows blocks to find each declaration's
// selector, and is never a validator: what it cannot read as a declaration it
// passes over.

// sheet is one stylesheet's text and where it came from: a served file's
// display path, `<style>` for an inline block, or `style=""` for an
// attribute.
type sheet struct {
	source string
	text   string
	// inline is a style attribute's declarations, which have no block.
	inline bool
}

// use is one reference to a custom property: where it is, the selector of the
// rule holding it and the property it sets.
type use struct {
	name     string
	source   string
	line     int
	selector string
	property string
	fallback bool
}

// scan is what a set of sheets defines and references.
type scan struct {
	defined map[string]bool
	uses    []use
}

// scanSheets reads every sheet.
func scanSheets(sheets []sheet) scan {
	s := scan{defined: map[string]bool{}}
	for _, sh := range sheets {
		s.read(sh)
	}
	return s
}

// unresolvedUse is a referenced name no sheet defines, with each of its uses.
type unresolvedUse struct {
	name     string
	fallback bool // every use names a fallback
	uses     []use
}

// unresolved is every name referenced and defined nowhere, by name.
func (s scan) unresolved() []unresolvedUse {
	byName := map[string]*unresolvedUse{}
	var names []string
	for _, u := range s.uses {
		if s.defined[u.name] {
			continue
		}
		got := byName[u.name]
		if got == nil {
			got = &unresolvedUse{name: u.name, fallback: true}
			byName[u.name] = got
			names = append(names, u.name)
		}
		got.uses = append(got.uses, u)
		got.fallback = got.fallback && u.fallback
	}
	sort.Strings(names)
	out := make([]unresolvedUse, 0, len(names))
	for _, n := range names {
		out = append(out, *byName[n])
	}
	return out
}

// near is up to three defined names closest to name: the longest shared
// prefix first, then the smallest edit distance, as a reader mistyping a
// token's last word would want them.
func (s scan) near(name string) []string {
	type cand struct {
		name   string
		prefix int
		dist   int
	}
	var cands []cand
	for d := range s.defined {
		p := commonPrefix(name, d)
		dist := editDistance(name, d)
		if p < len("--color-") && dist > len(name)/2 {
			continue
		}
		cands = append(cands, cand{d, p, dist})
	}
	slices.SortFunc(cands, func(a, b cand) int {
		switch {
		case a.prefix != b.prefix:
			return b.prefix - a.prefix
		case a.dist != b.dist:
			return a.dist - b.dist
		}
		return strings.Compare(a.name, b.name)
	})
	out := []string{}
	for _, c := range cands {
		if len(out) == 3 {
			break
		}
		out = append(out, c.name)
	}
	return out
}

// read scans one sheet.
func (s *scan) read(sh sheet) {
	r := reader{src: stripComments(sh.text), line: 1}
	if sh.inline {
		s.declarations(&r, sh.source, "", len(r.src))
		return
	}
	// A STRAY `}` ends no sheet: the top level is read again past it.
	for r.pos < len(r.src) {
		s.block(&r, sh.source, "")
	}
}

// block reads rules until the end of the text or the `}` closing the block
// it was called in; selector is the nearest enclosing style rule's.
func (s *scan) block(r *reader, source, selector string) {
	for {
		prelude, end := r.until("{;}")
		switch end {
		case 0:
			return
		case '}':
			// A declaration list ending at the block's close: the text
			// before it belongs to the enclosing rule's declarations,
			// which the caller reads.
			return
		case ';':
			// An at-rule statement (`@import`, `@charset`) or a stray
			// declaration at the top level, which defines nothing.
			continue
		}
		head := strings.TrimSpace(prelude)
		switch {
		case strings.HasPrefix(head, "@property"):
			name := strings.TrimSpace(strings.TrimPrefix(head, "@property"))
			if strings.HasPrefix(name, "--") {
				s.defined[name] = true
			}
			r.skipBlock()
		case strings.HasPrefix(head, "@media"), strings.HasPrefix(head, "@supports"),
			strings.HasPrefix(head, "@layer"), strings.HasPrefix(head, "@container"),
			strings.HasPrefix(head, "@scope"), strings.HasPrefix(head, "@document"),
			strings.HasPrefix(head, "@starting-style"):
			s.block(r, source, selector)
		case strings.HasPrefix(head, "@"):
			// @font-face, @keyframes, @page and the rest hold declarations
			// that are not a style rule's; a keyframe's references still
			// count as uses under the at-rule's name.
			s.rule(r, source, head)
		default:
			sel := head
			if selector != "" && strings.Contains(sel, "&") {
				sel = strings.ReplaceAll(sel, "&", selector)
			}
			s.rule(r, source, collapse(sel))
		}
	}
}

// rule reads one rule's body, its declarations and any rule nested in it,
// up to and including its closing brace.
func (s *scan) rule(r *reader, source, selector string) {
	for {
		start := r.pos
		text, end := r.until("{;}")
		switch end {
		case 0:
			s.declaration(text, r.lineAt(start), source, selector)
			return
		case ';':
			s.declaration(text, r.lineAt(start), source, selector)
		case '}':
			s.declaration(text, r.lineAt(start), source, selector)
			return
		case '{':
			// A nested rule (CSS nesting) or a nested at-rule: its prelude
			// is the text since the last declaration.
			head := strings.TrimSpace(text)
			nested := collapse(head)
			if strings.Contains(nested, "&") {
				nested = strings.ReplaceAll(nested, "&", selector)
			} else if !strings.HasPrefix(nested, "@") && selector != "" {
				nested = selector + " " + nested
			}
			if strings.HasPrefix(nested, "@") {
				nested = selector
			}
			s.rule(r, source, nested)
		}
	}
}

// declarations reads a style attribute's declarations up to end.
func (s *scan) declarations(r *reader, source, selector string, end int) {
	for r.pos < end {
		start := r.pos
		text, stop := r.until(";")
		s.declaration(text, r.lineAt(start), source, selector)
		if stop == 0 {
			return
		}
	}
}

// declaration reads one `property: value`, recording a definition of a
// custom property and every `var()` in the value.
func (s *scan) declaration(text string, line int, source, selector string) {
	name, value, ok := strings.Cut(text, ":")
	if !ok {
		return
	}
	// The declaration's own line is where its text starts past blank lines.
	line += strings.Count(name[:len(name)-len(strings.TrimLeft(name, " \t\r\n"))], "\n")
	property := strings.TrimSpace(name)
	if property == "" || strings.ContainsAny(property, " \t\n{}") {
		return
	}
	if strings.HasPrefix(property, "--") {
		s.defined[property] = true
	}
	for _, ref := range varRefs(value) {
		s.uses = append(s.uses, use{name: ref.name, source: source, line: line,
			selector: selector, property: property, fallback: ref.fallback})
	}
}

// ref is one `var()` in a value.
type ref struct {
	name     string
	fallback bool
}

// varRefs is every `var(--name)` and `var(--name, fallback)` in a value,
// nested ones included.
func varRefs(value string) []ref {
	var out []ref
	value = blankStrings(value)
	lower := strings.ToLower(value)
	for i := 0; ; {
		j := strings.Index(lower[i:], "var(")
		if j < 0 {
			return out
		}
		k := i + j + len("var(")
		rest := strings.TrimLeft(value[k:], " \t\r\n")
		n := 0
		for n < len(rest) && identByte(rest[n]) {
			n++
		}
		name := rest[:n]
		if strings.HasPrefix(name, "--") && len(name) > 2 {
			after := strings.TrimLeft(rest[n:], " \t\r\n")
			out = append(out, ref{name: name, fallback: strings.HasPrefix(after, ",")})
		}
		i = k
	}
}

// blankStrings is value with every quoted string's content made spaces, so
// a `var(` in generated content is no reference.
func blankStrings(value string) string {
	b := []byte(value)
	for i := 0; i < len(b); i++ {
		quote := b[i]
		if quote != '"' && quote != '\'' {
			continue
		}
		for i++; i < len(b) && b[i] != quote; i++ {
			if b[i] == '\\' && i+1 < len(b) {
				b[i] = ' '
				i++
			}
			b[i] = ' '
		}
	}
	return string(b)
}

// identByte reports a byte of a CSS identifier: ASCII letters, digits,
// hyphens and underscores, and every byte of a non-ASCII character.
func identByte(b byte) bool {
	return b == '-' || b == '_' || b >= 0x80 || ('a' <= b && b <= 'z') ||
		('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// reader walks a sheet's text outside strings, counting lines.
type reader struct {
	src string
	pos int
	// line is the line at counted, the offset lines were last counted to:
	// every offset asked is at or past the last one, so each byte is
	// counted once.
	line    int
	counted int
}

// until reads up to the first byte of stops outside a string or a bracket,
// answering the text before it and the byte (0 at the end of the text).
// Parentheses and brackets are followed so a `;` in `url(a;b)` or a
// `{` in an attribute selector's string does not end anything.
func (r *reader) until(stops string) (string, byte) {
	start := r.pos
	depth := 0
	for r.pos < len(r.src) {
		c := r.src[r.pos]
		switch {
		case c == '\\' && r.pos+1 < len(r.src):
			r.pos += 2
			continue
		case c == '"' || c == '\'':
			r.skipString(c)
			continue
		case c == '(' || c == '[':
			depth++
		case (c == ')' || c == ']') && depth > 0:
			depth--
		case depth == 0 && strings.IndexByte(stops, c) >= 0:
			text := r.src[start:r.pos]
			r.pos++
			return text, c
		}
		r.pos++
	}
	return r.src[start:], 0
}

// skipString passes a quoted string, its escapes included.
func (r *reader) skipString(quote byte) {
	r.pos++
	for r.pos < len(r.src) {
		c := r.src[r.pos]
		switch {
		case c == '\\':
			r.pos += 2
			continue
		case c == quote, c == '\n':
			r.pos++
			return
		}
		r.pos++
	}
}

// skipBlock passes the rest of a block whose `{` was read, nested blocks
// included.
func (r *reader) skipBlock() {
	for depth := 1; depth > 0; {
		_, c := r.until("{}")
		switch c {
		case 0:
			return
		case '{':
			depth++
		case '}':
			depth--
		}
	}
}

// lineAt is the 1-based line of offset pos, at or past every offset asked
// before it.
func (r *reader) lineAt(pos int) int {
	if pos > r.counted {
		r.line += strings.Count(r.src[r.counted:pos], "\n")
		r.counted = pos
	}
	return r.line
}

// stripComments blanks every comment outside a string, keeping its newlines
// so lines still count from the file's own.
func stripComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(src) && src[j] != c && src[j] != '\n' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, len(src))
			b.WriteString(src[i:j])
			i = j
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			stop := len(src)
			if end >= 0 {
				stop = i + 2 + end + 2
			}
			for _, ch := range src[i:stop] {
				if ch == '\n' {
					b.WriteByte('\n')
				}
			}
			b.WriteByte(' ')
			i = stop
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// collapse is text with every run of white space made one space.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// commonPrefix is the length of a's and b's shared prefix.
func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
