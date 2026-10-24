package lint

import (
	"bytes"
	"strings"
)

// rawAttr is one attribute of a start tag as it is SPELLED in the source: its
// name, lower case, and where its value sits in the tag's raw bytes, so a
// rewrite replaces exactly that value and leaves every other byte of the page
// as the author wrote it.
type rawAttr struct {
	name string

	// start and end bound the value inside the raw tag, quotes excluded;
	// both zero for an attribute with no value.
	start, end int
	valued     bool
}

// rawAttrs reads a start tag's attributes from its raw bytes (`<name a="b" c>`)
// by the HTML tokenizer's own attribute rules: whitespace and a lone `/`
// separate attributes, a name runs to whitespace, `/`, `>` or `=`, and a value
// is double-quoted, single-quoted or runs to whitespace or `>`.
//
// IT LOCATES, IT DOES NOT DECIDE. Every decision about a tag is taken on the
// standard tokenizer's decoded attributes, and every page this package rewrites
// is tokenized again and checked before it is answered ([verify]): a page on
// which this reading and the tokenizer's ever disagreed is refused rather than
// answered with a script the linter did not account for.
func rawAttrs(raw []byte) []rawAttr {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r' }
	i := 1
	for i < len(raw) && !isSpace(raw[i]) && raw[i] != '/' && raw[i] != '>' {
		i++
	}
	var out []rawAttr
	for i < len(raw) {
		for i < len(raw) && (isSpace(raw[i]) || raw[i] == '/') {
			i++
		}
		if i >= len(raw) || raw[i] == '>' {
			break
		}
		start := i
		i++ // a leading '=' belongs to the name
		for i < len(raw) && !isSpace(raw[i]) && raw[i] != '/' && raw[i] != '>' && raw[i] != '=' {
			i++
		}
		a := rawAttr{name: strings.ToLower(string(raw[start:i]))}
		j := i
		for j < len(raw) && isSpace(raw[j]) {
			j++
		}
		if j < len(raw) && raw[j] == '=' {
			j++
			for j < len(raw) && isSpace(raw[j]) {
				j++
			}
			a.valued = true
			switch {
			case j < len(raw) && (raw[j] == '"' || raw[j] == '\''):
				q := raw[j]
				end := bytes.IndexByte(raw[j+1:], q)
				if end < 0 {
					a.start, a.end = j+1, len(raw)
					i = len(raw)
				} else {
					a.start, a.end = j+1, j+1+end
					i = j + 2 + end
				}
			default:
				a.start = j
				for j < len(raw) && !isSpace(raw[j]) && raw[j] != '>' {
					j++
				}
				a.end = j
				i = j
			}
		}
		out = append(out, a)
	}
	return out
}

// attrValue renders a value for a double-quoted attribute.
func attrValue(v string) string {
	return strings.NewReplacer(`&`, "&amp;", `"`, "&quot;").Replace(v)
}

// rewriteTag is raw with the named attributes' values replaced, each written
// double-quoted, and insert placed straight after the tag name, so the first
// attribute a browser reads is the one inserted (a repeated attribute's first
// spelling wins).
func rewriteTag(raw []byte, attrs []rawAttr, values map[int]string, insert string) []byte {
	nameEnd := 1
	for nameEnd < len(raw) && !strings.ContainsRune(" \t\n\f\r/>", rune(raw[nameEnd])) {
		nameEnd++
	}
	var b bytes.Buffer
	b.Grow(len(raw) + len(insert) + 64)
	b.Write(raw[:nameEnd])
	b.WriteString(insert)
	at := nameEnd
	for i, a := range attrs {
		v, ok := values[i]
		if !ok || !a.valued {
			continue
		}
		// THE QUOTES ARE REPLACED WITH THE VALUE, so a value written
		// unquoted or single-quoted comes out double-quoted and escaped.
		start, end := a.start, a.end
		if start > 0 && (raw[start-1] == '"' || raw[start-1] == '\'') {
			start--
			if end < len(raw) && raw[end] == raw[start] {
				end++
			}
		}
		b.Write(raw[at:start])
		b.WriteString(`"` + attrValue(v) + `"`)
		at = end
	}
	b.Write(raw[at:])
	return b.Bytes()
}
