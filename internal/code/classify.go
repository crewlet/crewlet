package code

import (
	"bytes"
	"go/build/constraint"
	goscanner "go/scanner" // the package's own scanner reads a row
	"go/token"
	"slices"
	"strings"
)

// A CHANGE'S CLASSES — what a review of a change need not read — and the one
// lexer that judges them for Go source. Why a lexer, and why a directive and
// the layout around one are code, is the package doc's "A change's class is
// lexed, and a directive is code".

// ChangeClass names what a change does NOT touch. A class only ever RELAXES —
// a rule may exempt a change of a class from an approval it would otherwise
// ask — so a class is given only where it is true of the program and of every
// tool that reads the file, and whatever cannot be proved so gets none.
type ChangeClass string

const (
	// ChangeClassNoCodeChange is a REVISION whose base and head trees are
	// those of the revision before it: it moved nothing, so what held of the
	// one before holds of it. It is carried from revision to revision, and is
	// never a path's.
	ChangeClassNoCodeChange ChangeClass = "no_code_change"
	// ChangeClassWhitespaceOnly is a path whose two versions lex to one token
	// stream, comments included: only the space between tokens moved, none of
	// it a newline that ends a statement, and none of it around a directive.
	ChangeClassWhitespaceOnly ChangeClass = "whitespace_only"
	// ChangeClassCommentOnly is a path whose two versions lex to one token
	// stream once their comments are set aside, where no directive — a
	// comment the compiler, the build, a test or a linter reads — and nothing
	// around one changed.
	ChangeClassCommentOnly ChangeClass = "comment_only"
)

// ChangeClasses is the closed set, in this order: the revision's class, then
// the paths' classes narrowest first.
func ChangeClasses() []ChangeClass {
	return []ChangeClass{ChangeClassNoCodeChange, ChangeClassWhitespaceOnly, ChangeClassCommentOnly}
}

// PathClasses is the classes a lexer gives one path, narrowest first: every
// whitespace_only change is also a comment_only one and never the reverse, and
// [ClassifyGo] answers the narrowest that holds.
func PathClasses() []ChangeClass {
	return []ChangeClass{ChangeClassWhitespaceOnly, ChangeClassCommentOnly}
}

// Valid reports a class this build knows.
func (c ChangeClass) Valid() bool { return slices.Contains(ChangeClasses(), c) }

// MaxClassedBlobBytes is the largest version of a file [ClassifyGo] lexes; a
// change with a version past it gets no class.
//
// 1 MiB, PAST EVERY HAND-WRITTEN FILE OF A GO TREE: of the 8 088 .go files in
// Go 1.27.2's own src/, the median is 2.7 KB, the 99.9th percentile 523 KB,
// and the three past 1 MiB (cmd/compile's opGen.go and rewriteAMD64.go,
// time/tzdata's zzipdata.go) are all generated — files a generator rewrites
// and no person reads line by line, so a class on one would exempt nothing a
// reviewer reads. And the lexer is linear, so the bound caps what one path
// costs at two lexes of 1 MiB; what a whole revision may cost is the bound of
// its classification, not of one file.
const MaxClassedBlobBytes = 1 << 20

// byteOrderMark is the UTF-8 byte-order mark, which go/scanner skips at a
// file's start: no whitespace, so a change adding or removing one has no
// class.
var byteOrderMark = []byte("\uFEFF")

// ClassifyGo answers the narrowest class of the change from before to after,
// two versions of one Go source file, and false where no class holds.
//
// IT LEXES, WITH THE LANGUAGE'S OWN LEXER. Both versions go through
// go/scanner with comments returned, and their tokens are compared one for
// one, literals by their text — the semicolons the lexer inserts where a line
// ends a statement included, because in Go a newline there IS a token:
// `return` and x on two lines return nothing, and on one line return x.
// whitespace_only is two versions whose tokens and comments are the same,
// comment_only two whose tokens are the same once the comments are set aside. So a raw string's spaces are its value's and count as code, a `;`
// swapped for a newline is not whitespace though it compiles alike, and a line
// ending's carriage return, which the language discards even inside a raw
// string, is. Never `git diff -w`, which calls a newline that ends a statement
// a space, and never a pattern over the text, which cannot tell a comment
// from a string holding one.
//
// A DIRECTIVE IS CODE. A comment something reads as an instruction changes
// what the file builds, runs or reports, so no class holds where one was
// added, removed, edited or moved:
//   - every comment go/ast calls a directive — //go:… (pragmas, //go:embed,
//     //go:generate, //go:linkname, //go:build), //line and its /*line …*/
//     form, //export, //extern, and every //name:value line of the form
//     [a-z0-9]+:[a-z0-9], //lint:ignore and //nolint:errcheck among them;
//   - the build's // +build lines, and every // +word marker a generator
//     reads (Kubernetes' +k8s:…, controller-gen's +kubebuilder:…);
//   - a linter's suppression: //nolint as golangci-lint reads it, so
//     // nolint and a bare //nolint too, gosec's #nosec, and — in the
//     file's header — a generated-code marker, Go's "Code generated …
//     DO NOT EDIT." or a looser phrase golangci-lint takes for one, under
//     which it lints none of the file;
//   - an example's Output: or Unordered output:, which go test compares
//     the example's output with — and, because that is the LAST comment
//     group of the example's body, every comment of a top-level body holding
//     one: a comment added below it is a test that compares nothing;
//   - a cgo preamble — every comment before an import of "C", where cgo
//     reads the declaration's doc and the spec's — which is C source.
//
// A directive governs what FOLLOWS it, so every comment after the first one in
// a run of comments, up to the next token, is the directive's: a comment
// added between //go:embed and its variable is a change to the embed. The
// rest of a file's comments — its documentation — may say anything: the doc
// comment above a //go:noinline may be rewritten, and a license header edited
// above a //go:build line.
//
// AND SO IS THE LAYOUT AROUND ONE, because whitespace alone changes what a
// directive does: a //line or //go:generate indented off its line's start is
// ignored, a //go: pragma after code on its line is an error, a blank line
// between //export and its function detaches it, and one inside an example's
// Output splits off what go test compares. So each comment of a directive's
// run keeps, beside its text, where it starts on its line, whether anything
// precedes it there, how many lines part it from what comes before it and
// after it (one, or a blank line between), and the column of what follows
// it; a comment in the directive's own group above it keeps that layout and
// may change its text. And the build constraints a file states are compared
// as go/build reads them from its header (buildConstraints), the one place a
// blank line or a block comment far from a constraint decides whether it
// holds.
//
// NO CLASS for a version that is empty — a file added or deleted, which a
// caller judges before it asks — or past [MaxClassedBlobBytes], for one with a
// lexical error, or where one version has a byte-order mark and the other
// none.
func ClassifyGo(before, after []byte) (ChangeClass, bool) {
	if len(before) == 0 || len(after) == 0 || len(before) > MaxClassedBlobBytes || len(after) > MaxClassedBlobBytes {
		return "", false
	}
	if bytes.HasPrefix(before, byteOrderMark) != bytes.HasPrefix(after, byteOrderMark) {
		return "", false
	}
	a, b := lexGo(before), lexGo(after)
	imports, ok := lexInStep(a, b)
	if !ok {
		return "", false
	}
	rule := contextRule{preamble: imports.gaps, examples: exampleBodies(a, b)}
	if !slices.Equal(a.context(rule), b.context(rule)) {
		return "", false
	}
	if !slices.Equal(buildConstraints(before), buildConstraints(after)) {
		return "", false
	}
	if slices.EqualFunc(a.comments, b.comments, sameComment) {
		return ChangeClassWhitespaceOnly, true
	}
	return ChangeClassCommentOnly, true
}

// goFile is one version of the file as the lexer reads it: its comments, each
// placed by the code token it precedes, and what the walk to there knows.
// Code tokens are compared as both versions are read and never kept, so a
// classification holds the comments of the two versions and nothing else.
type goFile struct {
	src      []byte
	file     *token.File
	sc       goscanner.Scanner
	comments []goComment
	prevEnd  int // where the last lexeme other than an inserted semicolon ended
	open     int // the comment the next lexeme gives its after and next, or -1
	groups   int // comment groups begun
	depth    int // braces open
	region   int // the code token opening the outermost brace still open
	line     int // where the line holding seen starts
	seen     int // how far line has been brought (lineStart)
}

// goComment is one comment of a version, and where it sits.
type goComment struct {
	text   string // as the lexer returns it: its markers, no carriage return
	gap    int    // the code token it precedes: the comments of one gap lie between two tokens
	end    int    // the offset just past it
	group  int    // its comment group: comments on adjacent lines, with no token between
	region int    // the code token opening the top-level brace it sits in, or -1
	kind   commentKind
	output bool // an example's Output: or Unordered output:
	key    layout
}

// layout is where a comment sits, as far as a directive's meaning reads it.
type layout struct {
	col    int   // bytes before it on its line
	lead   bool  // nothing but blanks before it on its line
	before uint8 // line breaks since the lexeme before it: 0, 1, or 2 for a blank line or more
	after  uint8 // line breaks until the lexeme after it, likewise
	next   int   // the column the lexeme after it starts at, or -1 at the end of the file
}

// commentKind is what reads a comment besides a person.
type commentKind uint8

const (
	// plainComment is documentation: nothing but a reader reads it.
	plainComment commentKind = iota
	// headerComment is read from the file's header wherever it sits in it:
	// a //go:build or // +build line, whose place there buildConstraints
	// judges, or a generated-code marker.
	headerComment
	// directiveComment is an instruction to whatever reads what follows it.
	directiveComment
)

// headerGap is the last gap of a file's header: the comments before its
// package clause (gap 0), inside it (1), beside it (2), and below it up to
// the first declaration (3) — every comment golangci-lint reads in looking
// for a generated-code marker, since it parses a file to its package clause
// and the token after it.
const headerGap = 3

// lexGo readies one version for lexInStep.
func lexGo(src []byte) *goFile {
	f := &goFile{src: src, open: -1, region: -1}
	f.file = token.NewFileSet().AddFile("", -1, len(src))
	f.sc.Init(f.file, src, nil, goscanner.ScanComments)
	return f
}

// lexInStep walks both versions' code tokens side by side, stopping at the
// first that differs, and answers whether every token matched with neither
// version reporting a lexical error, and where a cgo preamble may sit.
func lexInStep(a, b *goFile) (cImports, bool) {
	var imports cImports
	for k := 0; ; k++ {
		ta, la := a.code(k)
		tb, lb := b.code(k)
		if ta != tb || la != lb || a.sc.ErrorCount > 0 || b.sc.ErrorCount > 0 {
			return imports, false
		}
		if ta == token.EOF {
			return imports, true
		}
		imports.see(k, ta, la, a.depth)
		a.brace(k, ta)
		b.brace(k, tb)
	}
}

// code scans to the next code token — an inserted semicolon is one — and
// answers it, recording each comment it passes as one of gap k.
func (f *goFile) code(k int) (token.Token, string) {
	for {
		pos, tok, lit := f.sc.Scan()
		off := f.file.Offset(pos)
		switch {
		case tok == token.COMMENT:
			f.comment(k, off, f.file.Offset(f.sc.End()), lit)
			continue
		case tok == token.EOF:
			f.closeOpen(len(f.src), -1)
		case tok != token.SEMICOLON || lit != "\n":
			// An inserted semicolon sits at the newline that ended its line,
			// or inside the comment that held one: no lexeme a layout is
			// measured from.
			f.closeOpen(off, off-f.lineStart(off))
			f.prevEnd = f.file.Offset(f.sc.End())
		}
		return tok, lit
	}
}

// comment records the comment text at [off, end) as one of gap k.
func (f *goFile) comment(k, off, end int, text string) {
	start := f.lineStart(off)
	f.closeOpen(off, off-start)
	c := goComment{
		text:   text,
		gap:    k,
		end:    end,
		region: -1,
		kind:   kindOf(text, k <= headerGap),
		output: outputMarker(text),
		key: layout{
			col: off - start,
			// Every byte that is not whitespace belongs to a lexeme (a
			// leading byte-order mark aside, which no version adds alone),
			// so only blanks precede a comment on its line exactly when the
			// lexeme before it ended on an earlier line.
			lead:   f.prevEnd <= start,
			before: lineBreaks(f.src[f.prevEnd:off]),
		},
	}
	if f.depth > 0 {
		c.region = f.region
	}
	if n := len(f.comments); n == 0 || f.comments[n-1].gap != k || c.key.before > 1 {
		f.groups++
	}
	c.group = f.groups
	f.comments = append(f.comments, c)
	f.open = len(f.comments) - 1
	f.prevEnd = end
}

// closeOpen gives the comment awaiting it what follows it: the lexeme at off,
// starting at column col (-1: the end of the file).
func (f *goFile) closeOpen(off, col int) {
	if f.open < 0 {
		return
	}
	c := &f.comments[f.open]
	c.key.after = lineBreaks(f.src[c.end:off])
	c.key.next = col
	f.open = -1
}

// brace follows code token k into and out of top-level braces — a function's
// body, a composite's elements — the regions an example's output is judged in.
func (f *goFile) brace(k int, tok token.Token) {
	switch tok {
	case token.LBRACE:
		if f.depth == 0 {
			f.region = k
		}
		f.depth++
	case token.RBRACE:
		if f.depth > 0 {
			f.depth--
		}
	}
}

// contextRule names the gaps all of whose comments are code: a cgo preamble's,
// and those of an example's body.
type contextRule struct {
	preamble map[int]bool // gaps, by the code token they precede
	examples map[int]bool // top-level brace regions, by the token opening them
}

// whole reports whether c's gap is one all of whose comments are code.
func (r contextRule) whole(c goComment) bool {
	return r.preamble[c.gap] || c.region >= 0 && r.examples[c.region]
}

// exampleBodies is the top-level brace regions holding an example's output
// marker in either version.
func exampleBodies(files ...*goFile) map[int]bool {
	var bodies map[int]bool
	for _, f := range files {
		for _, c := range f.comments {
			if c.output && c.region >= 0 {
				if bodies == nil {
					bodies = map[int]bool{}
				}
				bodies[c.region] = true
			}
		}
	}
	return bodies
}

// contextEntry is what a version holds of one comment a directive governs: its
// text, its layout, or both, beside the gap it sits in.
type contextEntry struct {
	gap  int
	text string
	key  layout
	part entryPart
}

// entryPart is what of a comment its entry holds.
type entryPart uint8

const (
	// wholeEntry is a directive, or what one governs: its text and its layout.
	wholeEntry entryPart = iota
	// textEntry is a header comment ahead of any directive of its gap: its
	// text, since where a build constraint sits in the header is
	// buildConstraints' to judge, and a generated-code marker counts wherever
	// it sits there.
	textEntry
	// layoutEntry is documentation in a directive's own comment group, above
	// it: free to say anything, not to move.
	layoutEntry
)

// context is every entry of this version's comments that a directive governs,
// in order: the two versions' lists equal is "no directive changed".
func (f *goFile) context(rule contextRule) []contextEntry {
	var out []contextEntry
	for cs := f.comments; len(cs) > 0; {
		n := 1
		for n < len(cs) && cs[n].gap == cs[0].gap {
			n++
		}
		out = appendContext(out, cs[:n], rule)
		cs = cs[n:]
	}
	return out
}

// appendContext appends the entries of one gap's comments: every one where the
// gap is code whole; else, from its first directive on, every comment whole;
// ahead of it, a header comment's text and the layout of the directive's own
// group.
func appendContext(out []contextEntry, gap []goComment, rule contextRule) []contextEntry {
	if rule.whole(gap[0]) {
		for _, c := range gap {
			out = append(out, c.entry(wholeEntry))
		}
		return out
	}
	first := slices.IndexFunc(gap, func(c goComment) bool { return c.kind == directiveComment })
	for i, c := range gap {
		switch {
		case first >= 0 && i >= first:
			out = append(out, c.entry(wholeEntry))
		case c.kind == headerComment:
			out = append(out, c.entry(textEntry))
		case first >= 0 && c.group == gap[first].group:
			out = append(out, c.entry(layoutEntry))
		}
	}
	return out
}

// entry is c's entry holding part.
func (c goComment) entry(part entryPart) contextEntry {
	e := contextEntry{gap: c.gap, part: part}
	if part != layoutEntry {
		e.text = c.text
	}
	if part != textEntry {
		e.key = c.key
	}
	return e
}

// sameComment reports two versions' comments that are one comment, in one
// place among the code.
func sameComment(x, y goComment) bool { return x.gap == y.gap && x.text == y.text }

// kindOf is what reads the comment text, as the lexer returns it; header is
// whether it sits in the file's header.
func kindOf(text string, header bool) commentKind {
	if strings.HasPrefix(text, "/*") {
		switch body := strings.TrimSuffix(text[2:], "*/"); {
		case strings.HasPrefix(body, "line "), suppression(body):
			return directiveComment
		case header && generatedMarker(body):
			return headerComment
		}
		return plainComment
	}
	if constraint.IsGoBuild(text) || constraint.IsPlusBuild(text) {
		return headerComment
	}
	switch body := text[2:]; {
	case goDirective(body), generatorMarker(body), suppression(body):
		return directiveComment
	case header && generatedMarker(body):
		return headerComment
	}
	return plainComment
}

// goDirective reports a line comment's body, past its //, that Go's own
// tools read as a directive: go/ast's list — "line ", "extern ", "export ",
// and the form [a-z0-9]+:[a-z0-9]…, which every tool that follows Go's
// convention writes (lint:ignore, nolint:errcheck, revive:disable) — and any
// go: or lint: at all, which the compiler and staticcheck take whatever
// follows the colon.
func goDirective(body string) bool {
	for _, prefix := range []string{"line ", "extern ", "export ", "go:", "lint:"} {
		if strings.HasPrefix(body, prefix) {
			return true
		}
	}
	name, rest, ok := strings.Cut(body, ":")
	return ok && name != "" && rest != "" && lowerAlnum(name) && lowerAlnum(rest[:1])
}

// generatorMarker reports a // +word line: go/build's +build form, which code
// generators took for their markers.
func generatorMarker(body string) bool {
	t := strings.TrimLeft(body, " \t")
	return len(t) > 1 && t[0] == '+' && ('a' <= t[1] && t[1] <= 'z' || 'A' <= t[1] && t[1] <= 'Z')
}

// suppression reports a comment body that silences a linter: nolint as
// golangci-lint reads it, past any slashes and spaces and followed by no more
// of a word, and gosec's #nosec anywhere — in either case, so a spelling a
// linter may not honour still errs toward code.
func suppression(body string) bool {
	t := strings.TrimLeft(body, "/ \t*")
	if len(t) >= len("nolint") && strings.EqualFold(t[:len("nolint")], "nolint") {
		if rest := t[len("nolint"):]; rest == "" || !wordByte(rest[0]) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(body), "#nosec")
}

// generatedMarker reports a header comment's body that marks the file
// generated: Go's "Code generated … DO NOT EDIT." line (go/ast's
// IsGenerated), and the looser phrases golangci-lint takes for one, in any
// case — under any of which it lints none of the file.
func generatedMarker(body string) bool {
	lower := strings.ToLower(body)
	for _, phrase := range []string{"code generated", "do not edit", "autogenerated", "automatically generated", "generated by"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// outputMarker reports a comment go/doc reads as an example's output, once it
// opens its group: Output: or Unordered output:, in any case, after any
// space (go/doc's outputPrefix).
func outputMarker(text string) bool {
	body := text[2:]
	if strings.HasPrefix(text, "/*") {
		body = strings.TrimSuffix(body, "*/")
	}
	body = strings.TrimLeft(body, " \t\n\v\f\r")
	for _, marker := range []string{"output:", "unordered output:"} {
		if len(body) >= len(marker) && strings.EqualFold(body[:len(marker)], marker) {
			return true
		}
	}
	return false
}

// cImports finds where a cgo preamble may sit: before an import declaration
// that imports "C", whose doc cgo reads where "C" is its only spec, and before
// the spec itself, whose doc it reads otherwise. Both are code. It is fed the
// code tokens in order, with the brace depth each is read at.
type cImports struct {
	state   cState
	declGap int          // the gap before the declaration's import keyword
	specGap int          // the gap before the spec being read, in a parenthesized declaration
	gaps    map[int]bool // the gaps a preamble may sit in
}

// cState is where cImports is in an import declaration.
type cState uint8

const (
	cOutside cState = iota // in no import declaration
	cImport                // just past `import`
	cNamed                 // past `import name`
	cBlock                 // in `import ( … )`, where a spec begins
	cSpec                  // in `import ( … )`, past a spec's first token
)

// see reads code token k.
func (c *cImports) see(k int, tok token.Token, lit string, depth int) {
	switch c.state {
	case cOutside:
		if tok == token.IMPORT && depth == 0 {
			c.declGap, c.state = k, cImport
		}
	case cImport:
		c.state = cOutside
		switch tok {
		case token.LPAREN:
			c.state = cBlock
		case token.IDENT, token.PERIOD:
			c.state = cNamed
		case token.STRING:
			c.path(lit, -1)
		}
	case cNamed:
		if tok == token.STRING {
			c.path(lit, -1)
		}
		c.state = cOutside
	case cBlock, cSpec:
		switch {
		case tok == token.RPAREN:
			c.state = cOutside
		case tok == token.SEMICOLON:
			c.state = cBlock
		case c.state == cBlock:
			c.specGap, c.state = k, cSpec
			if tok == token.STRING {
				c.path(lit, k)
			}
		case tok == token.STRING:
			c.path(lit, c.specGap)
		}
	}
}

// path reads an import path's literal; specGap is the gap before its spec in a
// parenthesized declaration, or -1. Both spellings of "C" count: go/build
// unquotes a raw one to C, and cgo reads only the quoted one, so the raw one
// errs toward code.
func (c *cImports) path(lit string, specGap int) {
	if lit != `"C"` && lit != "`C`" {
		return
	}
	if c.gaps == nil {
		c.gaps = map[int]bool{}
	}
	c.gaps[c.declGap] = true
	if specGap >= 0 {
		c.gaps[specGap] = true
	}
}

// buildConstraints is what go/build reads of a file's build constraints: every
// //go:build line of its header, and the // +build lines it honours — those
// above the last blank line before the header's first line that is not a //
// comment — following go/build's reading of a header line for line (its
// parseFileHeader). It is why a blank line, or a block comment above a +build
// line, is code there, while a //go:build line may sit anywhere in the
// header. A header ends at the first line holding anything but comments.
func buildConstraints(src []byte) []string {
	src = bytes.TrimPrefix(src, byteOrderMark)
	var lines []string
	end, ended, inBlock := 0, false, false
	rest := src
header:
	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, rest = line[:i], rest[i+1:]
		} else {
			rest = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 && !ended {
			end = len(src) - len(rest)
			continue
		}
		if !bytes.HasPrefix(line, []byte("//")) {
			ended = true
		}
		if !inBlock && constraint.IsGoBuild(string(line)) {
			lines = append(lines, string(line))
		}
		for len(line) > 0 {
			switch {
			case inBlock:
				i := bytes.Index(line, []byte("*/"))
				if i < 0 {
					continue header
				}
				inBlock = false
				line = bytes.TrimSpace(line[i+len("*/"):])
			case bytes.HasPrefix(line, []byte("//")):
				continue header
			case bytes.HasPrefix(line, []byte("/*")):
				inBlock = true
				line = bytes.TrimSpace(line[len("/*"):])
			default:
				break header
			}
		}
	}
	for line := range bytes.SplitSeq(src[:end], []byte("\n")) {
		if text := string(bytes.TrimSpace(line)); constraint.IsPlusBuild(text) {
			lines = append(lines, text)
		}
	}
	return lines
}

// lineStart is the offset at which the line holding off starts. The walk asks
// it of offsets that never move back — each lexeme's start, in order — so it
// reads every byte of the file once: searching back from each lexeme to its
// line's start instead would cost a file holding one long line of n comments
// n² reads, for a file anybody may push.
func (f *goFile) lineStart(off int) int {
	if i := bytes.LastIndexByte(f.src[f.seen:off], '\n'); i >= 0 {
		f.line = f.seen + i + 1
	}
	f.seen = off
	return f.line
}

// lineBreaks is how many newlines b holds, up to two: what a layout tells
// apart is the same line, the next one, and a blank line between.
func lineBreaks(b []byte) uint8 { return uint8(min(bytes.Count(b, []byte("\n")), 2)) }

// lowerAlnum reports whether s is all lowercase ASCII letters and digits.
func lowerAlnum(s string) bool {
	for i := range len(s) {
		if b := s[i]; !('a' <= b && b <= 'z' || '0' <= b && b <= '9') {
			return false
		}
	}
	return true
}

// wordByte reports a byte that continues a word: a letter, a digit or _.
func wordByte(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || b == '_'
}
