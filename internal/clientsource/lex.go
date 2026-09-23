package clientsource

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The scanner: TypeScript and TSX source as a stream of tokens, with every
// comment, every run of whitespace and every piece of JSX prose gone.
//
// A SCANNER, NOT A PARSER, and the line between the two is the one this
// package needs. Every question a gate asks is about a declaration's TOKENS —
// which strings a literal holds, which members an interface names, which
// string a call is handed first — and the only thing that makes those hard to
// answer with a pattern is what a pattern cannot tell apart: a `]` that closes
// the list from one inside a string, a quote that opens a string from one in a
// comment or in a sentence of JSX, a `/` that divides from one that opens a
// regular expression whose character class holds a `{`. Those are LEXICAL
// facts, and a lexer settles all of them without a grammar.
//
// THE THREE PLACES A LEXER NEEDS CONTEXT are the three this one tracks, and
// each was a real failure mode in this tree rather than a theoretical one:
//
//   - A `/` is division after a value and a regular expression anywhere an
//     expression can start. The dashboard has regexes whose classes hold
//     braces and quotes (`/^\}/`, `/-{2,}/`), and read as division each one
//     unbalances everything after it.
//   - A `<` in a `.tsx` file opens an ELEMENT anywhere an expression can
//     start, and JSX text is prose: "the company's sealed credentials" holds
//     an apostrophe that a quote-counting scan reads as the start of a string
//     running to the next one, which can be a hundred lines away.
//   - A `}` closes a block, a template literal's `${…}` or a JSX expression
//     container, and which one decides whether the scan resumes as code, as a
//     template or as markup. A stack of what each open brace returns to is the
//     whole of that.
//
// Where it cannot tell, it FAILS rather than guesses: an unterminated string,
// template, comment or regular expression, an element that never closes, and
// a bracket that closes nothing or closes the wrong kind of bracket are errors
// naming the file and line. A scanner that lost its place and kept going would
// read the rest of the file as the wrong thing, and a gate built on it would
// report whatever it happened to see. And every read scans every source file
// in the tree, so a construct this scanner cannot follow fails every gate the
// day it lands, naming the line, rather than the one gate that happened to
// read past it.

// kind is what a token is.
type kind uint8

const (
	kIdent kind = iota + 1 // identifiers and keywords alike
	kString
	kTemplate // a whole template literal, or one chunk of one with substitutions
	kNumber
	kRegex
	kPunct
	kJSX // one whole element, opened from code: a value, like a string
)

// token is one lexical unit, located in the source it came from.
type token struct {
	kind       kind
	nl         bool // a line terminator separates it from the token before
	start, end int  // byte offsets into the file
	// value is a string literal's COOKED value (escapes decoded) and a
	// complete template's; every other kind reads its source text.
	value string
	src   *string
}

// text is the token's source text.
func (t token) text() string { return (*t.src)[t.start:t.end] }

// is reports whether the token is punctuation spelled as one of `spellings`.
func (t token) is(spellings ...string) bool {
	if t.kind != kPunct {
		return false
	}
	text := t.text()
	for _, s := range spellings {
		if text == s {
			return true
		}
	}
	return false
}

// word reports whether the token is the identifier or keyword `w`.
func (t token) word(w string) bool { return t.kind == kIdent && t.text() == w }

// literal is the value a string or a complete template carries, and whether
// the token is one. A template CHUNK — a template with a substitution in it —
// is not a constant, so it answers its raw text: a gate comparing it against a
// set of names fails on it loudly rather than skipping it quietly.
func (t token) literal() (string, bool) {
	switch t.kind {
	case kString:
		return t.value, true
	case kTemplate:
		if complete(t.text()) {
			return t.value, true
		}
		return t.text(), true
	}
	return "", false
}

// complete reports whether a template token is a whole template with no
// substitution: it opens and closes with a backtick.
func complete(raw string) bool {
	return len(raw) >= 2 && raw[0] == '`' && raw[len(raw)-1] == '`'
}

// frame is an open bracket, and for a `{` what it returns the scan to when it
// closes.
type frame uint8

const (
	fParen    frame = iota + 1 // `(`
	fBracket                   // `[`
	fBlock                     // `{` in code: an object, a block, a type
	fTemplate                  // `${`: the rest of a template literal
	fJSXExpr                   // `{` in markup: the element it sits in
)

// open is one bracket not yet closed: which, and where it is, so a file that
// never closes it can say where it opened.
//
// EVERY BRACKET, not only the braces the modes need. A scanner that lost its
// place — a regular expression read as a division, a `<` read as the wrong
// thing — keeps producing tokens, and the one symptom that is nearly certain
// is a bracket closing the wrong kind of bracket. Checked here, that is an
// error naming a line rather than a gate reading the wrong tokens.
type open struct {
	frame frame
	at    int
}

// element is one open JSX element.
type element struct {
	children bool // past the opening tag's `>`, reading children
	fromCode bool // opened where an expression starts, not as another's child
	at       int
}

type mode uint8

const (
	mCode mode = iota
	mTag
	mChildren
)

type lexer struct {
	file string
	src  *string
	pos  int
	jsx  bool
	nl   bool
	toks []token

	mode   mode
	braces []open
	elems  []element
}

// lex scans one file. `jsx` is whether it is a `.tsx` file: in a `.ts` file a
// `<` never opens an element.
func lex(file, src string, jsx bool) ([]token, error) {
	l := &lexer{file: file, src: &src, jsx: jsx}
	for l.pos < len(src) {
		var err error
		switch l.mode {
		case mCode:
			err = l.code()
		case mTag:
			err = l.tag()
		case mChildren:
			err = l.children()
		}
		if err != nil {
			return nil, err
		}
	}
	// WHERE IT OPENED, not the end of the file: the innermost thing left
	// open is the one a reader has to go and find.
	switch {
	case len(l.elems) > 0:
		return nil, l.errorf(l.elems[len(l.elems)-1].at, "a JSX element is never closed")
	case len(l.braces) > 0:
		top := l.braces[len(l.braces)-1]
		return nil, l.errorf(top.at, "a %s is never closed", top.frame)
	}
	return l.toks, nil
}

func (l *lexer) errorf(at int, format string, args ...any) error {
	line := strings.Count((*l.src)[:min(at, len(*l.src))], "\n") + 1
	return fmt.Errorf("%s:%d: %s", l.file, line, fmt.Sprintf(format, args...))
}

func (l *lexer) emit(k kind, start int, value string) {
	l.toks = append(l.toks, token{
		kind: k, nl: l.nl, start: start, end: l.pos, value: value, src: l.src,
	})
	l.nl = false
}

func (l *lexer) peek(off int) byte {
	if l.pos+off < len(*l.src) {
		return (*l.src)[l.pos+off]
	}
	return 0
}

// code scans one token of ordinary TypeScript, or skips one run of space or
// one comment.
func (l *lexer) code() error {
	src := *l.src
	c := src[l.pos]
	switch {
	case c == '\n' || c == '\r':
		l.nl = true
		l.pos++
	case c == ' ' || c == '\t' || c == '\v' || c == '\f':
		l.pos++
	case c == '/' && l.peek(1) == '/':
		l.lineComment()
	case c == '/' && l.peek(1) == '*':
		return l.blockComment()
	case c == '"' || c == '\'':
		return l.str(c)
	case c == '`':
		start := l.pos
		l.pos++
		return l.template(start)
	case c == '#' || c == '$' || c == '_' || isASCIILetter(c):
		l.ident()
	case isDigit(c) || (c == '.' && isDigit(l.peek(1))):
		l.number()
	case c == '/':
		if l.expressionStarts() {
			return l.regex()
		}
		l.punct()
	case c == '<' && l.jsx && l.expressionStarts() && l.opensElement():
		return l.openElement(true)
	case c == '{':
		l.braces = append(l.braces, open{frame: fBlock, at: l.pos})
		l.punct()
	case c == '(':
		l.braces = append(l.braces, open{frame: fParen, at: l.pos})
		l.punct()
	case c == '[':
		l.braces = append(l.braces, open{frame: fBracket, at: l.pos})
		l.punct()
	case c == ')':
		return l.close(fParen)
	case c == ']':
		return l.close(fBracket)
	case c == '}':
		return l.closeBrace()
	case c >= utf8.RuneSelf:
		r, size := utf8.DecodeRuneInString(src[l.pos:])
		switch {
		case r == '\u2028' || r == '\u2029':
			l.nl = true
			l.pos += size
		case unicode.IsSpace(r) || r == '\uFEFF':
			l.pos += size
		case unicode.IsLetter(r):
			l.ident()
		default:
			return l.errorf(l.pos, "unexpected %q", r)
		}
	default:
		l.punct()
	}
	return nil
}

func (l *lexer) lineComment() {
	src := *l.src
	for l.pos < len(src) && src[l.pos] != '\n' && src[l.pos] != '\r' {
		l.pos++
	}
}

func (l *lexer) blockComment() error {
	src := *l.src
	end := strings.Index(src[l.pos+2:], "*/")
	if end < 0 {
		return l.errorf(l.pos, "a /* comment is never closed")
	}
	if strings.ContainsAny(src[l.pos:l.pos+2+end], "\n\r") {
		l.nl = true
	}
	l.pos += 2 + end + 2
	return nil
}

func (l *lexer) ident() {
	src := *l.src
	start := l.pos
	// The first character is already known to start one, and may be more
	// than a byte wide.
	_, size := utf8.DecodeRuneInString(src[l.pos:])
	l.pos += size
	for l.pos < len(src) {
		c := src[l.pos]
		if c == '$' || c == '_' || isASCIILetter(c) || isDigit(c) {
			l.pos++
			continue
		}
		if c < utf8.RuneSelf {
			break
		}
		r, size := utf8.DecodeRuneInString(src[l.pos:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.Is(unicode.Mn, r) {
			break
		}
		l.pos += size
	}
	l.emit(kIdent, start, "")
}

func (l *lexer) number() {
	src := *l.src
	start := l.pos
	for l.pos < len(src) {
		c := src[l.pos]
		switch {
		case isDigit(c) || isASCIILetter(c) || c == '_':
			l.pos++
		// ONE decimal point, and only one a digit follows or no name
		// does: `items[0].kind` is an index and a property, not the
		// number `0.kind`.
		case c == '.' && !strings.Contains(src[start:l.pos], ".") &&
			(isDigit(l.peek(1)) || !isNameStart(l.peek(1)) && l.peek(1) != '.'):
			l.pos++
		case (c == '+' || c == '-') && (src[l.pos-1] == 'e' || src[l.pos-1] == 'E') &&
			!strings.HasPrefix(src[start:], "0x") && !strings.HasPrefix(src[start:], "0X"):
			l.pos++
		default:
			l.emit(kNumber, start, "")
			return
		}
	}
	l.emit(kNumber, start, "")
}

// str scans a quoted string, decoding its escapes: `'task'` and `"task"` are
// the same value, and so is `"task"`.
func (l *lexer) str(quote byte) error {
	src := *l.src
	start := l.pos
	l.pos++
	var b strings.Builder
	for l.pos < len(src) {
		c := src[l.pos]
		switch {
		case c == quote:
			l.pos++
			l.emit(kString, start, b.String())
			return nil
		case c == '\\':
			if err := l.escape(&b, false); err != nil {
				return err
			}
		case c == '\n' || c == '\r':
			return l.errorf(start, "a string is never closed")
		default:
			b.WriteByte(c)
			l.pos++
		}
	}
	return l.errorf(start, "a string is never closed")
}

// escape decodes one backslash escape at l.pos into b.
//
// `raw` is a template literal's escape, which may be malformed: a TAGGED
// template (`String.raw` over `C:\users`) is handed its raw text and has no
// cooked value to get wrong, so a `\u` that is not hexadecimal there is text,
// not an error that would stop the whole file being read.
func (l *lexer) escape(b *strings.Builder, raw bool) error {
	src := *l.src
	at := l.pos
	l.pos++ // the backslash
	if l.pos >= len(src) {
		return l.errorf(at, "a backslash ends the file")
	}
	c := src[l.pos]
	l.pos++
	switch c {
	case 'n':
		b.WriteByte('\n')
	case 't':
		b.WriteByte('\t')
	case 'r':
		b.WriteByte('\r')
	case 'b':
		b.WriteByte('\b')
	case 'f':
		b.WriteByte('\f')
	case 'v':
		b.WriteByte('\v')
	case '0':
		b.WriteByte(0)
	case '\r':
		// A line continuation writes nothing, CRLF included.
		if l.pos < len(src) && src[l.pos] == '\n' {
			l.pos++
		}
	case '\n':
	case 'x':
		n, err := strconv.ParseUint(src[l.pos:min(l.pos+2, len(src))], 16, 8)
		switch {
		case err == nil && l.pos+2 <= len(src):
			b.WriteRune(rune(n))
			l.pos += 2
		case raw:
			b.WriteString(src[at:l.pos])
		default:
			return l.errorf(at, "a \\x escape is not two hexadecimal digits")
		}
	case 'u':
		digits := src[l.pos:min(l.pos+4, len(src))]
		width := 4
		if strings.HasPrefix(src[l.pos:], "{") {
			digits = ""
			if end := strings.IndexByte(src[l.pos:], '}'); end >= 0 {
				digits, width = src[l.pos+1:l.pos+end], end+1
			}
		}
		n, err := strconv.ParseUint(digits, 16, 32)
		switch {
		case err == nil && len(digits) > 0:
			b.WriteRune(rune(n))
			l.pos += width
		case raw:
			b.WriteString(src[at:l.pos])
		default:
			return l.errorf(at, "a \\u escape is not hexadecimal")
		}
	default:
		// Any other character stands for itself — `\"`, `\'`, `\\`, `` \` ``
		// and a backslash before a multi-byte rune alike.
		l.pos--
		r, size := utf8.DecodeRuneInString(src[l.pos:])
		b.WriteRune(r)
		l.pos += size
	}
	return nil
}

// template scans a template literal from l.pos to its closing backtick or its
// next `${`, whichever comes first. `start` is where the token began: the
// opening backtick, or the `}` that closed the previous substitution.
func (l *lexer) template(start int) error {
	src := *l.src
	var b strings.Builder
	for l.pos < len(src) {
		c := src[l.pos]
		switch {
		case c == '`':
			l.pos++
			l.emit(kTemplate, start, b.String())
			return nil
		case c == '$' && l.peek(1) == '{':
			l.braces = append(l.braces, open{frame: fTemplate, at: l.pos})
			l.pos += 2
			l.emit(kTemplate, start, "")
			return nil
		case c == '\\':
			if err := l.escape(&b, true); err != nil {
				return err
			}
		default:
			b.WriteByte(c)
			l.pos++
		}
	}
	return l.errorf(start, "a template literal is never closed")
}

func (l *lexer) regex() error {
	src := *l.src
	start := l.pos
	l.pos++
	class := false
	for l.pos < len(src) {
		c := src[l.pos]
		switch {
		case c == '\\':
			l.pos += 2
			continue
		case c == '\n' || c == '\r':
			return l.errorf(start, "a regular expression is never closed")
		case class:
			class = c != ']'
		case c == '[':
			class = true
		case c == '/':
			l.pos++
			for l.pos < len(src) && isASCIILetter(src[l.pos]) {
				l.pos++
			}
			l.emit(kRegex, start, "")
			return nil
		}
		l.pos++
	}
	return l.errorf(start, "a regular expression is never closed")
}

// puncts are the multi-character punctuators, longest first.
//
// `<` and `>` are NEVER joined to a neighbour — no `<<`, `>>`, `>=` — because
// in a type they are brackets, and `Array<Array<string>>` or
// `const x: Map<K, V>= …` written without a space must still balance. A
// comparison read as two tokens costs nothing: nothing here reads
// comparisons.
var puncts = []string{
	"...", "===", "!==", "**=", "&&=", "||=", "??=",
	"=>", "==", "!=", "<=", "&&", "||", "??", "?.", "++", "--", "**",
	"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=",
}

func (l *lexer) punct() {
	src := *l.src
	start := l.pos
	for _, p := range puncts {
		if !strings.HasPrefix(src[l.pos:], p) {
			continue
		}
		// `a?.5:b` is a conditional, not an optional chain.
		if p == "?." && isDigit(l.peek(2)) {
			continue
		}
		l.pos += len(p)
		l.emit(kPunct, start, "")
		return
	}
	l.pos++
	l.emit(kPunct, start, "")
}

// close scans a `)` or a `]`, which must close the innermost open bracket.
func (l *lexer) close(want frame) error {
	if _, err := l.pop(want); err != nil {
		return err
	}
	l.punct()
	return nil
}

// pop takes the innermost open bracket, which must be of one of the kinds
// `want` names.
func (l *lexer) pop(want ...frame) (frame, error) {
	closing := (*l.src)[l.pos]
	if len(l.braces) == 0 {
		return 0, l.errorf(l.pos, "a `%c` closes nothing", closing)
	}
	top := l.braces[len(l.braces)-1]
	if !slices.Contains(want, top.frame) {
		return 0, l.errorf(l.pos, "a `%c` closes the %s opened on line %d",
			closing, top.frame, strings.Count((*l.src)[:top.at], "\n")+1)
	}
	l.braces = l.braces[:len(l.braces)-1]
	return top.frame, nil
}

func (f frame) String() string {
	switch f {
	case fParen:
		return "`(`"
	case fBracket:
		return "`[`"
	case fTemplate:
		return "template substitution `${`"
	}
	return "`{`"
}

func (l *lexer) closeBrace() error {
	top, err := l.pop(fBlock, fTemplate, fJSXExpr)
	if err != nil {
		return err
	}
	switch top {
	case fTemplate:
		start := l.pos
		l.pos++
		return l.template(start)
	case fJSXExpr:
		l.punct()
		if len(l.elems) == 0 {
			return l.errorf(l.pos, "a JSX expression closes outside any element")
		}
		if l.elems[len(l.elems)-1].children {
			l.mode = mChildren
		} else {
			l.mode = mTag
		}
	default:
		l.punct()
	}
	return nil
}

// keywordsBeforeExpression are the keywords after which an expression starts,
// so a `/` opens a regular expression and a `<` opens an element.
var keywordsBeforeExpression = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true, "of": true,
	"new": true, "delete": true, "void": true, "throw": true, "case": true,
	"do": true, "else": true, "yield": true, "await": true,
}

// expressionStarts reports whether an expression may start here — the one
// question that tells a regular expression from a division and an element
// from a comparison.
//
// AFTER A VALUE IT MAY NOT: an identifier, a literal, a closing `)` `]` `}`,
// or a whole element. After anything else — an operator, an opening bracket, a
// comma, `=>`, a keyword like `return` — it may. A keyword used as a property
// name (`x.delete`) is a value, which is why the token before it is checked.
func (l *lexer) expressionStarts() bool {
	if len(l.toks) == 0 {
		return true
	}
	last := l.toks[len(l.toks)-1]
	switch last.kind {
	case kNumber, kString, kRegex, kJSX:
		return false
	case kTemplate:
		return strings.HasSuffix(last.text(), "${")
	case kIdent:
		if !keywordsBeforeExpression[last.text()] {
			return false
		}
		return len(l.toks) < 2 || !l.toks[len(l.toks)-2].is(".", "?.")
	}
	return !last.is(")", "]", "}")
}

// opensElement reports whether the `<` at l.pos opens a JSX element rather
// than a generic arrow function's type parameters.
//
// `<T,>(x: T) => x` and `<T extends U>(…)` are how a `.tsx` file spells a
// generic arrow, precisely because `<T>(…)` would be an element there. Every
// other `<` followed by a name or by `>` (a fragment) is an element.
func (l *lexer) opensElement() bool {
	src := *l.src
	i := l.pos + 1
	for i < len(src) && isSpace(src[i]) {
		i++
	}
	if i >= len(src) {
		return false
	}
	if src[i] == '>' {
		return true
	}
	if !isNameStart(src[i]) {
		return false
	}
	for i < len(src) && isNamePart(src[i]) {
		i++
	}
	for i < len(src) && isSpace(src[i]) {
		i++
	}
	rest := src[i:]
	extends := strings.HasPrefix(rest, "extends") &&
		(len(rest) == len("extends") || !isNamePart(rest[len("extends")]))
	return !strings.HasPrefix(rest, ",") && !extends
}

// openElement scans a `<` that opens an element — from code, or as a child of
// another element — through its tag name.
func (l *lexer) openElement(fromCode bool) error {
	src := *l.src
	at := l.pos
	l.pos++ // '<'
	for l.pos < len(src) && isSpace(src[l.pos]) {
		l.pos++
	}
	if l.pos < len(src) && src[l.pos] == '>' {
		l.pos++
		l.elems = append(l.elems, element{children: true, fromCode: fromCode, at: at})
		l.mode = mChildren
		return nil
	}
	for l.pos < len(src) && isNamePart(src[l.pos]) {
		l.pos++
	}
	l.elems = append(l.elems, element{fromCode: fromCode, at: at})
	l.mode = mTag
	// TYPE ARGUMENTS ON AN ELEMENT — `<Segmented<View> …>` — are legal TSX
	// and in this tree, so they are skipped as a balanced group rather than
	// read as a second tag.
	for l.pos < len(src) && isSpace(src[l.pos]) {
		l.pos++
	}
	if l.pos < len(src) && src[l.pos] == '<' {
		return l.skipTypeArguments()
	}
	return nil
}

func (l *lexer) skipTypeArguments() error {
	src := *l.src
	start := l.pos
	depth := 0
	for l.pos < len(src) {
		switch c := src[l.pos]; {
		case c == '=' && l.peek(1) == '>':
			l.pos += 2
			continue
		case c == '<':
			depth++
		case c == '>':
			depth--
			if depth == 0 {
				l.pos++
				return nil
			}
		case c == '"' || c == '\'':
			end := strings.IndexByte(src[l.pos+1:], c)
			if end < 0 {
				return l.errorf(l.pos, "a string is never closed")
			}
			l.pos += end + 1
		}
		l.pos++
	}
	return l.errorf(start, "an element's type arguments are never closed")
}

// tag scans one piece of an opening tag: an attribute name, an `=`, a value,
// a spread, or the `>` or `/>` that ends it.
func (l *lexer) tag() error {
	src := *l.src
	c := src[l.pos]
	switch {
	case isSpace(c):
		l.pos++
	case c == '/' && l.peek(1) == '/':
		l.lineComment()
	case c == '/' && l.peek(1) == '*':
		return l.blockComment()
	case c == '>':
		l.pos++
		l.elems[len(l.elems)-1].children = true
		l.mode = mChildren
	case c == '/' && l.peek(1) == '>':
		l.pos += 2
		l.closeElement()
	case c == '{':
		return l.openExpression()
	case c == '"' || c == '\'':
		// An attribute's string has no escapes in JSX, and is markup rather
		// than a value this package reads.
		end := strings.IndexByte(src[l.pos+1:], c)
		if end < 0 {
			return l.errorf(l.pos, "an attribute's string is never closed")
		}
		l.pos += end + 2
	case c == '=' || isNamePart(c):
		l.pos++
	default:
		return l.errorf(l.pos, "unexpected %q inside a JSX tag", c)
	}
	return nil
}

// children scans an element's content: prose up to the next `<` or `{`, a
// child element, an expression container, or the closing tag.
func (l *lexer) children() error {
	src := *l.src
	for l.pos < len(src) && src[l.pos] != '<' && src[l.pos] != '{' {
		l.pos++
	}
	switch {
	case l.pos >= len(src):
		return nil // lex reports the unclosed element
	case src[l.pos] == '{':
		return l.openExpression()
	}
	i := l.pos + 1
	for i < len(src) && isSpace(src[i]) {
		i++
	}
	if i < len(src) && src[i] == '/' {
		end := strings.IndexByte(src[i:], '>')
		if end < 0 {
			return l.errorf(l.pos, "a closing tag is never closed")
		}
		l.pos = i + end + 1
		l.closeElement()
		return nil
	}
	return l.openElement(false)
}

func (l *lexer) openExpression() error {
	l.braces = append(l.braces, open{frame: fJSXExpr, at: l.pos})
	l.punct()
	l.mode = mCode
	return nil
}

// closeElement ends the innermost element. One opened from code is a VALUE
// there, and is emitted as one token so what follows it reads as coming after
// a value.
func (l *lexer) closeElement() {
	closed := l.elems[len(l.elems)-1]
	l.elems = l.elems[:len(l.elems)-1]
	if closed.fromCode {
		l.mode = mCode
		l.emit(kJSX, l.pos, "")
		return
	}
	l.mode = mChildren
}

func isASCIILetter(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }
func isDigit(c byte) bool       { return c >= '0' && c <= '9' }
func isSpace(c byte) bool       { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isNameStart(c byte) bool   { return isASCIILetter(c) || c == '_' || c == '$' }

// isNamePart is a character of a JSX tag or attribute name, which unlike an
// identifier may hold `-` (`aria-label`), `:` (`xlink:href`) and `.`
// (`<Menu.Item>`).
func isNamePart(c byte) bool {
	return isNameStart(c) || isDigit(c) || c == '-' || c == ':' || c == '.' || c >= utf8.RuneSelf
}
