// Package toolmarkup is the grammar of the tool-call formats a model writes as
// TEXT: the markup its own vendor's native tool channel uses, typed out in a
// reply that was asked for something else.
//
// A coding CLI in text mode is asked for the engine's envelope (a fenced JSON
// block naming its calls), and a model trained on its vendor's own channel
// sometimes answers in that channel's form instead: Anthropic's
// `<invoke name="..."><parameter name="...">` (inside `<function_calls>` or
// not, the tags namespaced or not), Qwen and Hermes's
// `<tool_call>{"name": ...}</tool_call>`, the coder models'
// `<function=name><parameter=key>`. Measured on Claude Code under
// `--tools ""`: the doctor's isolation probe and a Dashboard Assistant turn
// both got a proposal written out as `<invoke>` markup, and the turn showed
// it to the person as its answer.
//
// ONE GRAMMAR, two readers. The envelope parser
// ([github.com/crewlet/crewlet/internal/providers/llm/cliagent.ParseEnvelope])
// reads the blocks [Read] finds and turns those naming an offered tool into the
// engine's calls; the tool loop asks [Index] whether prose it is about to show
// still carries any, and refuses to show it. A second copy of the tag list in
// either place would be the drift this package exists to prevent: a format one
// side converts and the other lets through as prose.
//
// CODE IS NOT A CALL. Markup inside a fenced block or an inline code span is a
// model showing what a call looks like (an answer about a vendor's API, a
// worked example), so [Read] and [Index] do not see it: it is prose a person
// may read, and it never runs. The envelope parser holds one exception, on
// the side of refusing: it reads each fenced block on its own with [Read],
// and a WHOLE call in one that names a tool the request offered, in a reply
// whose envelope does not make that call, is a protocol fault that runs
// nothing and asks again. Executing an example costs a change nobody asked
// for; showing a fenced call as the answer ended a turn on a promise nothing
// kept.
//
// Standard library only, so the provider contract's packages can share it.
package toolmarkup

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Block is one tool call written as markup.
type Block struct {
	// Start and End are the block's span in the text it was read from,
	// End exclusive, wrapper included: what [Prose] removes.
	Start, End int
	// Name is the tool an `<invoke>` or a `<function=...>` names, and
	// empty for a JSON-bodied tag, whose name is inside JSON.
	Name string
	// Params are an `<invoke>`'s parameters in the order written, each
	// value as written. Empty for a JSON-bodied tag.
	Params []Param
	// JSON is a `<tool_call>` or `<tool_use>` tag's body, trimmed, for the
	// caller to decode with the same rules as the envelope's own calls.
	JSON string
}

// Param is one parameter of a [Block], its value verbatim.
type Param struct {
	Name  string
	Value string
}

// tagPattern is every tag this package knows, opening or closing, under an
// optional namespace prefix (`<ns:invoke>`). Group 1 is the slash, 2 the
// prefix, 3 the tag name, 4 everything up to the `>`.
//
// `function` and `parameter` are tags only in their attribute forms
// (`<function=name>`, `<parameter name=...>`, `<parameter=key>`) or closing:
// a bare `<function>` or `<parameter>` is ordinary XML a model may well be
// writing about (a Maven pom, a WSDL), and is not taken for a call.
var tagPattern = regexp.MustCompile(
	`<(/?)((?:[A-Za-z][\w-]*:)?)(function_calls|invoke|function|parameter|tool_call|tool_use)\b([^<>]*)>`)

// nameAttr reads a `name="..."` or `name='...'` attribute.
var nameAttr = regexp.MustCompile(`\bname\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// tag is one match of [tagPattern] outside code.
type tag struct {
	start, end int
	closing    bool
	prefix     string
	name       string
	attrs      string
}

// isMarkup reports whether a matched tag is one of the call forms, rather than
// a bare `<function>` or `<parameter>` element.
func (t tag) isMarkup() bool {
	if t.closing {
		return strings.TrimSpace(t.attrs) == ""
	}
	switch t.name {
	case "function":
		return strings.HasPrefix(strings.TrimSpace(t.attrs), "=")
	case "parameter":
		a := strings.TrimSpace(t.attrs)
		return strings.HasPrefix(a, "=") || nameAttr.MatchString(a)
	default:
		return true
	}
}

// tags is every markup tag in text outside code, in order.
//
// A closing `</function>` or `</parameter>` is markup only where a call form
// of that tag is open: the close of a bare element (a pom's `<parameter>`) is
// the element's, as its opening was.
func tags(text string) []tag {
	code := codeSpans(text)
	var out []tag
	open := map[string]int{}
	for _, m := range tagPattern.FindAllStringSubmatchIndex(text, -1) {
		if inSpans(code, m[0]) {
			continue
		}
		t := tag{start: m[0], end: m[1], closing: m[3] > m[2], prefix: text[m[4]:m[5]],
			name: text[m[6]:m[7]], attrs: text[m[8]:m[9]]}
		if !t.isMarkup() {
			continue
		}
		if t.name == "function" || t.name == "parameter" {
			key := t.prefix + t.name
			switch {
			case !t.closing:
				open[key]++
			case open[key] == 0:
				continue
			default:
				open[key]--
			}
		}
		out = append(out, t)
	}
	return out
}

// Index is the offset of the first tool-call markup in text outside code, or
// -1 when it carries none.
func Index(text string) int {
	if t := tags(text); len(t) > 0 {
		return t[0].start
	}
	return -1
}

// Visible is the part of text, written so far, that may be shown: everything
// before its first markup, and short of a trailing `<` that may yet become a
// tag (`<inv` with `oke name=` still to arrive). A live view that showed the
// prefix of a tag would draw it before the rest arrived to be recognised.
func Visible(text string) string {
	if i := Index(text); i >= 0 {
		text = text[:i]
	}
	lt := strings.LastIndexByte(text, '<')
	if lt < 0 || inSpans(codeSpans(text), lt) || strings.IndexByte(text[lt:], '>') >= 0 {
		return text
	}
	if couldOpen(text[lt+1:]) {
		return text[:lt]
	}
	return text
}

// tagNames are the tags [tagPattern] knows, for [couldOpen].
var tagNames = []string{"function_calls", "invoke", "function", "parameter", "tool_call", "tool_use"}

// couldOpen reports whether rest, what follows a `<` with no `>` after it yet,
// may still become a tag this package knows: nothing at all, or a tag name (or
// the start of one) under an optional slash and namespace, followed by nothing
// or by the start of its attributes. `a < b` is not one, so a comparison in
// prose does not hold the view back.
func couldOpen(rest string) bool {
	if rest == "" {
		return true
	}
	rest = strings.TrimPrefix(rest, "/")
	n := 0
	for n < len(rest) && (isNameByte(rest[n]) || rest[n] == ':') {
		n++
	}
	if n == 0 && rest != "" {
		return false
	}
	name, after := rest[:n], rest[n:]
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}
	for _, known := range tagNames {
		switch {
		case after == "" && strings.HasPrefix(known, name):
			return true
		case after != "" && name == known && (after[0] == ' ' || after[0] == '=' ||
			after[0] == '\t' || after[0] == '\n'):
			return true
		}
	}
	return false
}

// isNameByte reports whether c may appear in a tag or namespace name.
func isNameByte(c byte) bool {
	return c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// ErrMalformed is markup that does not read as a whole call: a tag cut off, a
// closing tag nothing opened, an `<invoke>` with no name.
var ErrMalformed = errors.New("the tool-call markup is malformed")

// Read finds every tool call written as markup in text outside code.
//
// It returns the blocks in order, and an error wrapping [ErrMalformed] when the
// markup does not read as whole calls; the blocks read before the fault are
// still returned, for a caller that reports them, but a caller deciding whether
// to RUN anything treats the error as the answer.
func Read(text string) ([]Block, error) {
	all := tags(text)
	var blocks []Block
	for i := 0; i < len(all); {
		t := all[i]
		switch {
		case t.closing:
			return blocks, fmt.Errorf("%w: a closing </%s> that nothing opened", ErrMalformed, t.name)
		case t.name == "function_calls":
			end, inner, next, err := wrapper(text, all, i)
			if err != nil {
				return blocks, err
			}
			if len(inner) == 0 {
				return blocks, fmt.Errorf("%w: a <%s> block that holds no call", ErrMalformed, t.name)
			}
			// The wrapper's span is the block's, so its tags leave the
			// prose with the calls inside it.
			inner[0].Start, inner[len(inner)-1].End = t.start, end
			blocks = append(blocks, inner...)
			i = next
		default:
			b, next, err := call(text, all, i)
			if err != nil {
				return blocks, err
			}
			blocks = append(blocks, b)
			i = next
		}
	}
	return blocks, nil
}

// wrapper reads a `<function_calls>` block from all[i]: its end, the calls
// inside it, and the index of the first tag after it. A wrapper with no close
// is read to the end of the text, the shape a reply cut off mid-call takes;
// the cut call inside it is what fails.
func wrapper(text string, all []tag, i int) (end int, inner []Block, next int, err error) {
	open := all[i]
	j := i + 1
	for j < len(all) {
		t := all[j]
		if t.closing && t.name == open.name {
			return t.end, inner, j + 1, nil
		}
		if t.name == open.name || t.closing {
			return 0, nil, 0, fmt.Errorf("%w: a <%s> block that does not close where it should",
				ErrMalformed, open.name)
		}
		b, after, callErr := call(text, all, j)
		if callErr != nil {
			return 0, nil, 0, callErr
		}
		inner = append(inner, b)
		j = after
	}
	return len(text), inner, j, nil
}

// call reads one call from the opening tag all[i]: the block and the index of
// the first tag after it.
func call(text string, all []tag, i int) (Block, int, error) {
	open := all[i]
	switch open.name {
	case "tool_call", "tool_use":
		closeAt := closer(all, i+1, open)
		if closeAt < 0 {
			return Block{}, 0, fmt.Errorf("%w: a <%s> that is cut off", ErrMalformed, open.name)
		}
		c := all[closeAt]
		return Block{Start: open.start, End: c.end,
			JSON: strings.TrimSpace(text[open.end:c.start])}, closeAt + 1, nil
	case "invoke", "function":
		name := tagName(open)
		if name == "" {
			return Block{}, 0, fmt.Errorf("%w: an <%s> that names no tool", ErrMalformed, open.name)
		}
		b := Block{Start: open.start, Name: name}
		seen := map[string]bool{}
		for j := i + 1; j < len(all); {
			t := all[j]
			switch {
			case t.closing && t.name == open.name:
				b.End = t.end
				return b, j + 1, nil
			case t.name == "parameter" && !t.closing:
				p, after, err := param(text, all, j)
				if err != nil {
					return Block{}, 0, err
				}
				if seen[p.Name] {
					return Block{}, 0, fmt.Errorf("%w: %q sets the parameter %q twice",
						ErrMalformed, name, p.Name)
				}
				seen[p.Name] = true
				b.Params = append(b.Params, p)
				j = after
			default:
				return Block{}, 0, fmt.Errorf("%w: a <%s> inside the call to %q",
					ErrMalformed, tagLabel(t), name)
			}
		}
		return Block{}, 0, fmt.Errorf("%w: the call to %q is cut off", ErrMalformed, name)
	default:
		return Block{}, 0, fmt.Errorf("%w: a <%s> outside any call", ErrMalformed, open.name)
	}
}

// param reads one parameter from its opening tag all[i]: its value is the text
// up to its closing tag VERBATIM, tags and all, since a value (a page body, an
// HTML snippet) may itself hold markup of any kind.
func param(text string, all []tag, i int) (Param, int, error) {
	open := all[i]
	name := tagName(open)
	if name == "" {
		return Param{}, 0, fmt.Errorf("%w: a parameter with no name", ErrMalformed)
	}
	closeAt := closer(all, i+1, open)
	if closeAt < 0 {
		return Param{}, 0, fmt.Errorf("%w: the parameter %q is cut off", ErrMalformed, name)
	}
	return Param{Name: name, Value: text[open.end:all[closeAt].start]}, closeAt + 1, nil
}

// closer is the index of the first tag from j that closes open, under the same
// prefix, or -1.
func closer(all []tag, j int, open tag) int {
	for ; j < len(all); j++ {
		if t := all[j]; t.closing && t.name == open.name && t.prefix == open.prefix {
			return j
		}
	}
	return -1
}

// tagName is the name an opening tag carries: `name="x"`, or `=x` in the
// coder models' form.
func tagName(t tag) string {
	attrs := strings.TrimSpace(t.attrs)
	if rest, ok := strings.CutPrefix(attrs, "="); ok {
		return strings.Trim(strings.TrimSpace(rest), `"'`)
	}
	if m := nameAttr.FindStringSubmatch(attrs); m != nil {
		return m[1] + m[2]
	}
	return ""
}

// tagLabel is a tag as an error names it.
func tagLabel(t tag) string {
	if t.closing {
		return "/" + t.name
	}
	return t.name
}

// Prose is text with every block's span removed and the joins trimmed: what a
// reply said around its calls.
func Prose(text string, blocks []Block) string {
	var parts []string
	keep := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	at := 0
	for _, blk := range blocks {
		if blk.Start < at || blk.End > len(text) || blk.End < blk.Start {
			continue
		}
		keep(text[at:blk.Start])
		at = blk.End
	}
	keep(text[at:])
	return strings.Join(parts, "\n\n")
}

// codeSpans are the spans of text that are code: every ``` fence, an
// unterminated one running to the end, and every inline `...` span on one
// line outside them.
func codeSpans(text string) [][2]int {
	var spans [][2]int
	at := 0
	for {
		open := strings.Index(text[at:], "```")
		if open < 0 {
			break
		}
		open += at
		end := strings.Index(text[open+3:], "```")
		if end < 0 {
			spans = append(spans, inline(text[at:open], at)...)
			spans = append(spans, [2]int{open, len(text)})
			return spans
		}
		end += open + 3 + 3
		spans = append(spans, inline(text[at:open], at)...)
		spans = append(spans, [2]int{open, end})
		at = end
	}
	return append(spans, inline(text[at:], at)...)
}

// inline is the inline code spans of a stretch with no fence, offset by base.
func inline(s string, base int) [][2]int {
	var spans [][2]int
	lineStart := 0
	for lineStart <= len(s) {
		lineEnd := strings.IndexByte(s[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(s)
		} else {
			lineEnd += lineStart
		}
		line := s[lineStart:lineEnd]
		for i := 0; i < len(line); {
			open := strings.IndexByte(line[i:], '`')
			if open < 0 {
				break
			}
			open += i
			end := strings.IndexByte(line[open+1:], '`')
			if end < 0 {
				break
			}
			end += open + 1
			spans = append(spans, [2]int{base + lineStart + open, base + lineStart + end + 1})
			i = end + 1
		}
		lineStart = lineEnd + 1
	}
	return spans
}

// inSpans reports whether offset lies inside one of spans.
func inSpans(spans [][2]int, offset int) bool {
	for _, s := range spans {
		if offset >= s[0] && offset < s[1] {
			return true
		}
	}
	return false
}
