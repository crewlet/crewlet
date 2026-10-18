package lint

import (
	"bytes"
	"strings"
)

// Import is one module specifier script text names by a literal: a static
// import or export-from, or a dynamic import() of a string.
type Import struct {
	Specifier string
	Line      int
}

// Imports is every module specifier src names by a literal, in source order.
//
// A SCAN, NOT A PARSE: comments, strings, template literals and regular
// expression literals are skipped by the lexer's own rules for telling them
// apart, and an `import` or `export` keyword outside them is read for the
// string it loads. A specifier the script BUILDS at run time (`import(base +
// name)`) is not a literal and is not found: that is code deciding at view
// time what to load, which no publish-time reading can know, and the artifact
// origin's strict policy deliberately lets an author's own scripts load
// further scripts.
func Imports(src []byte) []Import {
	s := jsScanner{src: src, line: 1}
	s.scan(false)
	return s.found
}

// scan reads tokens to the end of the text or, inside a template's
// substitution, to the brace that closes it.
func (s *jsScanner) scan(substitution bool) {
	depth := 0
	for s.i < len(s.src) {
		start := s.i
		if !s.skipTrivia() {
			return
		}
		c := s.src[s.i]
		switch {
		case identStart(c):
			word := s.ident()
			if !s.prevDot && (word == "import" || word == "export") {
				if spec, line, ok := s.specifierAfter(word); ok {
					s.found = append(s.found, Import{Specifier: spec, Line: line})
				}
			}
			s.last = 'a'
		case c == '"' || c == '\'':
			s.str()
			s.last = 'a'
		case c == '`':
			s.template()
			s.last = 'a'
		case c == '/':
			if s.regexAllowed() {
				s.regex()
				s.last = 'a'
			} else {
				s.i++
				s.last = '/'
			}
		case c == '{':
			depth++
			s.i++
			s.last = c
		case c == '}':
			if substitution && depth == 0 {
				s.i++
				return
			}
			depth--
			s.i++
			s.last = c
		default:
			s.i++
			s.last = c
		}
		s.prevDot = c == '.'
		if s.i == start {
			s.i++
		}
	}
}

// jsScanner walks script text one token at a time.
type jsScanner struct {
	src  []byte
	i    int
	line int

	// last is the last significant character, read as 'a' after a word,
	// a string or a literal: what tells a regular expression from a
	// division.
	last    byte
	prevDot bool

	// found is every specifier read so far.
	found []Import
}

// skipTrivia skips whitespace and comments, reporting whether a token
// follows at s.i.
func (s *jsScanner) skipTrivia() bool {
	for s.i < len(s.src) {
		c := s.src[s.i]
		switch {
		case c == '\n':
			s.line++
			s.i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f':
			s.i++
		case c == '/' && s.i+1 < len(s.src) && s.src[s.i+1] == '/':
			end := bytes.IndexByte(s.src[s.i:], '\n')
			if end < 0 {
				s.i = len(s.src)
				return false
			}
			s.i += end
		case c == '/' && s.i+1 < len(s.src) && s.src[s.i+1] == '*':
			end := bytes.Index(s.src[s.i+2:], []byte("*/"))
			if end < 0 {
				s.i = len(s.src)
				return false
			}
			s.line += bytes.Count(s.src[s.i:s.i+2+end], []byte("\n"))
			s.i += end + 4
		default:
			return true
		}
	}
	return false
}

func identStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func identPart(c byte) bool { return identStart(c) || c >= '0' && c <= '9' }

func (s *jsScanner) ident() string {
	start := s.i
	for s.i < len(s.src) && identPart(s.src[s.i]) {
		s.i++
	}
	return string(s.src[start:s.i])
}

// str skips a string literal, answering its value with escapes removed.
func (s *jsScanner) str() string {
	q := s.src[s.i]
	s.i++
	var b strings.Builder
	for s.i < len(s.src) {
		c := s.src[s.i]
		switch {
		case c == '\\' && s.i+1 < len(s.src):
			b.WriteByte(s.src[s.i+1])
			if s.src[s.i+1] == '\n' {
				s.line++
			}
			s.i += 2
			continue
		case c == q:
			s.i++
			return b.String()
		case c == '\n':
			// An unterminated string ends at the line.
			return b.String()
		}
		b.WriteByte(c)
		s.i++
	}
	return b.String()
}

// template skips a template literal, reading its substitutions as the code
// they are.
func (s *jsScanner) template() {
	s.i++
	for s.i < len(s.src) {
		c := s.src[s.i]
		switch {
		case c == '\\':
			s.i += 2
			continue
		case c == '`':
			s.i++
			return
		case c == '\n':
			s.line++
		case c == '$' && s.i+1 < len(s.src) && s.src[s.i+1] == '{':
			s.i += 2
			last, dot := s.last, s.prevDot
			s.last, s.prevDot = 0, false
			s.scan(true)
			s.last, s.prevDot = last, dot
			continue
		}
		s.i++
	}
}

// regexAllowed reports whether a slash here begins a regular expression
// literal rather than a division: after an operator, an opening bracket, a
// separator, or at the start.
func (s *jsScanner) regexAllowed() bool {
	if s.i+1 < len(s.src) && (s.src[s.i+1] == '/' || s.src[s.i+1] == '*') {
		return false
	}
	return s.last == 0 || strings.IndexByte("(,=:[!&|?{};+-*%<>~^", s.last) >= 0
}

// regex skips a regular expression literal, its character classes included.
func (s *jsScanner) regex() {
	s.i++
	inClass := false
	for s.i < len(s.src) {
		c := s.src[s.i]
		switch {
		case c == '\\':
			s.i += 2
			continue
		case c == '\n':
			return
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '/' && !inClass:
			s.i++
			for s.i < len(s.src) && identPart(s.src[s.i]) {
				s.i++
			}
			return
		}
		s.i++
	}
}

// specifierAfter reads the string an import or export keyword loads, if it
// loads one: `import "x"`, `import(...)` of a literal, `import a from "x"`,
// `export * from "x"`, `export { a } from "x"`.
func (s *jsScanner) specifierAfter(word string) (string, int, bool) {
	save := *s
	if !s.skipTrivia() {
		return "", 0, false
	}
	c := s.src[s.i]
	if word == "import" {
		switch {
		case c == '"' || c == '\'':
			line := s.line
			return s.str(), line, true
		case c == '(':
			s.i++
			if !s.skipTrivia() {
				return "", 0, false
			}
			if q := s.src[s.i]; q == '"' || q == '\'' {
				line := s.line
				return s.str(), line, true
			}
			*s = save
			return "", 0, false
		case c == '.':
			// import.meta
			*s = save
			return "", 0, false
		}
	} else if c != '*' && c != '{' {
		// export const, export default, export function: nothing loaded.
		*s = save
		return "", 0, false
	}
	// A clause up to `from "x"`, never past a statement's end.
	for s.i < len(s.src) {
		if !s.skipTrivia() {
			break
		}
		c := s.src[s.i]
		switch {
		case c == ';' || c == '(' || c == ')' || c == '=' || c == ':':
			*s = save
			return "", 0, false
		case c == '"' || c == '\'':
			*s = save
			return "", 0, false
		case identStart(c):
			if s.ident() == "from" {
				if !s.skipTrivia() {
					break
				}
				if q := s.src[s.i]; q == '"' || q == '\'' {
					line := s.line
					return s.str(), line, true
				}
				*s = save
				return "", 0, false
			}
		default:
			s.i++
		}
	}
	*s = save
	return "", 0, false
}

// RemoteImports is every specifier among imports that names another origin.
func RemoteImports(imports []Import) []Import {
	var out []Import
	for _, im := range imports {
		if IsRemote(im.Specifier) {
			out = append(out, im)
		}
	}
	return out
}
