package code

import (
	"bytes"
	"go/build/constraint"
	goscanner "go/scanner" // the package's own scanner reads a row
	"go/token"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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
// reviewer reads.
//
// WHAT A PATH AT THE BOUND COSTS follows its bytes, whatever they hold,
// because the walk keeps nothing of a version but the comments between two
// code tokens, as offsets, and drops them once the token after them is read.
// Two versions at the bound, classed to the end (BenchmarkClassifyGo, one
// bounded run of five iterations an arm on two E-cores of a Core Ultra 7
// 265U shared with other builds), took 34 ms and 9 MB of allocation as
// ordinary code, and 106 ms and 85 MB as nothing but comments, one to a
// line, the most lexemes the bound holds — 116 ms where a //go:noinline
// governs every one of them. Half the comments' allocation is the lexer's
// own, a line table and a text a comment; the walk's is one gap of 24-byte
// records, here the whole file. What a whole revision may cost is the bound
// of its classification, which takes the comment arms as its worst, not of
// one file.
const MaxClassedBlobBytes = 1 << 20

// MaxClassedBytes is the most Go a revision's classification lexes — both
// versions of every path it classes, counted together — in the touched
// list's order: the path whose two versions would take it past this is not
// read, nor is any after it, and the revision's classes are INCOMPLETE
// ([RevisionClasses]). How MANY paths it lexes is bounded by the touched list
// itself ([MaxTouchedPaths]): a pair of small files costs a lookup and a
// lexer's setup, nothing a thousand of them make long.
//
// 16 MIB, A TENTH OF A READ'S GIT: lexing costs what the bytes cost, at most
// 58 ms a MiB — two versions at MaxClassedBlobBytes of nothing but comments
// under a //go:noinline, the arm costing most of BenchmarkClassifyGo
// (MaxClassedBlobBytes has the figures) — so the bound is some 0.93 s of
// lexing, a tenth of the ten seconds a read's own git is given
// (gitproc.ReadDeadline), which the lexing runs inside of, since it reads
// the blobs off their cat-file as git writes them. The rest of the budget is
// the facts' own: their walk, their patch and its id. The paths' bound is
// the one reached first by ordinary code — a thousand files at the median of
// Go's own tree, 2.7 KB, are some 5 MiB of both versions — so this cuts a
// revision of large files alone: generated code, or a vendored tree.
const MaxClassedBytes = 16 << 20

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
// comment_only two whose tokens are the same once the comments are set aside.
// So a raw string's spaces are its value's and count as code, a `;` swapped
// for a newline is not whitespace though it compiles alike, and a line
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
//     [a-z0-9]+:[a-z0-9], //lint:ignore and //nolint:errcheck among them,
//     read past the blanks before it as revive reads // revive:disable;
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
// EACH IS READ AS ITS TOOL READS IT, which is not always as the lexer does.
// go generate reads LINES, not tokens: every line of the file that starts
// //go:generate and a blank is a command it runs, whatever lexeme the line
// sits in — a block comment or a raw string as much as a comment of its own
// (cmd/go: "lines that look like directives in comments or multiline strings
// will be treated as directives"). And the compiler reads a directive with
// every carriage return the lexer drops from a comment's text, but for the
// one a Windows line ending leaves at its end — so a carriage return can
// split a pragma, or sit in an embed's pattern or a //line directive's file
// name, as one in front of a //go:generate hides its line from go generate.
// So the lines go generate runs are compared as it reads them
// (sameGenerateLines), and a directive's text as the compiler reads it: a
// command planted in a comment, or one a carriage return's removal arms, is
// code. Documentation keeps the lexer's reading, under which a carriage
// return is whitespace.
//
// AND A LINTER'S DIRECTIVE HOLDS ITS FILE'S LINES, because its reach is lines
// (sameHeldLines' line record says whose): in a file holding one, a token
// moved onto or off a line is code wherever it is, and so is a comment that,
// grown or shrunk by a line, moves a directive a line nearer to or further
// from the statement go/ast attaches it to. A reindent, a blank line between
// statements, and documentation that keeps its lines keep their class. Go's
// own directives attach by order — a pragma, an embed or an export to what
// follows it — and hold no lines.
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
	var j judgement
	if !j.lexInStep(lexGo(before), lexGo(after)) {
		return "", false
	}
	if !slices.Equal(buildConstraints(before), buildConstraints(after)) || !sameGenerateLines(before, after) {
		return "", false
	}
	if j.sameText {
		return ChangeClassWhitespaceOnly, true
	}
	return ChangeClassCommentOnly, true
}

// judgement is what the walk has found of a change so far, and what it holds
// of a gap a later token decides.
//
// THE WALK KEEPS ONE GAP. Both versions are lexed in step, a code token at a
// time; the comments each reads before code token k — gap k — are compared
// with the other's the moment both have read token k, and dropped. A gap's
// comments are code whole where a LATER token says so — an import's path
// that turns out to be "C", the closing brace of a body that turns out to
// hold an example's output — so such a gap is compared both ways when it is
// read, and the walk keeps the two answers, never the comments, until that
// token. A version's comments are offsets into its source, the text the
// lexer returned for one left behind: so what a classification holds is two
// gaps of offsets, and the rest of its cost is the lexer's own. Keeping every
// comment of both versions to the end instead — its text, its layout, and the
// entries a directive's context made of them — cost two versions at
// [MaxClassedBlobBytes] of nothing but comments 247 ms and 374 MB, and 321 ms
// and 577 MB where a directive governed them all: nine times the time and
// thirty times the allocation of code as long (BenchmarkClassifyGo, measured
// as MaxClassedBlobBytes' doc says).
type judgement struct {
	same      bool // every token, and every directive's context, alike: comment_only so far
	sameText  bool // every comment alike too: whitespace_only so far
	sameLines bool // every lexeme the line record holds as many lines from the one before it
	depth     int  // braces open
	imports   cImports
	body      pendingBody
	// text is scratch for two comments' texts where a carriage return the
	// lexer drops sits in their bytes.
	text [2][]byte
}

// goFile is one version of the file as the walk reads it: the comments of the
// gap it is in, and what the walk to there knows.
type goFile struct {
	src     []byte
	file    *token.File
	sc      goscanner.Scanner
	gap     []lexComment // the comments before the code token being read
	open    bool         // gap's last comment awaits the lexeme after it
	prevEnd int          // where the last lexeme other than an inserted semicolon ended
	line    int          // where the line holding seen starts
	seen    int          // how far line has been brought (lineStart)
	lower   []byte       // scratch for a header comment's text in lower case
	// tokOff and tokEnd are where the code token just read starts and ends,
	// one the lexer did not insert.
	tokOff, tokEnd int
	// heldEnd is where the last lexeme the line record holds ended, and
	// heldCode whether it was a code token (sameHeldLines).
	heldEnd  int
	heldCode bool
	// linter is whether a comment read so far is a linter's directive.
	linter bool
}

// lexComment is one comment of a version: where its bytes are, where it sits,
// and what reads it — its text is src[off:end] itself, but for the carriage
// returns the lexer drops from it.
type lexComment struct {
	off, end int32
	key      layout
	kind     commentKind
	cr       bool // its bytes hold a carriage return the lexer drops from its text
	output   bool // an example's Output: or Unordered output:
}

// layout is where a comment sits, as far as a directive's meaning reads it.
type layout struct {
	col    int32 // bytes before it on its line
	next   int32 // the column the lexeme after it starts at, or -1 at the end of the file
	before uint8 // line breaks since the lexeme before it: 0, 1, or 2 for a blank line or more
	after  uint8 // line breaks until the lexeme after it, likewise
	lead   bool  // nothing but blanks before it on its line
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
	f := &goFile{src: src}
	f.file = token.NewFileSet().AddFile("", -1, len(src))
	f.sc.Init(f.file, src, nil, goscanner.ScanComments)
	return f
}

// lexInStep walks both versions' code tokens side by side, judging each gap
// as its token is read, and answers whether every token matched, with
// neither version reporting a lexical error, and every directive's context
// compared alike. It stops at the first that does not.
func (j *judgement) lexInStep(a, b *goFile) bool {
	j.same, j.sameText, j.sameLines = true, true, true
	a.heldCode, b.heldCode = true, true
	for k := 0; ; k++ {
		ta, la := a.code(k)
		tb, lb := b.code(k)
		if ta != tb || la != lb || a.sc.ErrorCount > 0 || b.sc.ErrorCount > 0 {
			return false
		}
		j.judgeGap(a, b, ta, la)
		if ta == token.EOF {
			j.endFile()
			return j.same && j.linesHold(a, b)
		}
		j.seeImport(ta, la)
		j.brace(ta)
		if !j.same || !j.linesHold(a, b) {
			return false
		}
	}
}

// linesHold reports whether the line record lets the class stand: it held,
// or neither version has read a linter's directive, whose reach is what the
// record is for.
func (j *judgement) linesHold(a, b *goFile) bool {
	return j.sameLines || !a.linter && !b.linter
}

// code scans to the next code token — an inserted semicolon is one — and
// answers it, gathering the comments it passes as gap k, every one of which
// knows what follows it by the time it returns.
func (f *goFile) code(k int) (token.Token, string) {
	f.gap = f.gap[:0]
	for {
		pos, tok, lit := f.sc.Scan()
		off := f.file.Offset(pos)
		switch {
		case tok == token.COMMENT:
			f.comment(k, off, f.file.Offset(f.sc.End()), lit)
			continue
		case tok == token.EOF:
			f.closeOpen(len(f.src))
		case tok == token.SEMICOLON && lit == "\n":
			// An inserted semicolon sits at the newline that ended its line,
			// or inside the comment that held one: no lexeme a layout is
			// measured from. What follows the gap's last comment is the
			// lexeme the lexer reaches next, past the blanks it skips, which
			// is read now rather than when the next gap begins, so that the
			// gap is whole when the walk compares it.
			if f.open {
				f.closeOpen(f.nextLexeme(int(f.gap[len(f.gap)-1].end)))
			}
		default:
			f.closeOpen(off)
			f.prevEnd = f.file.Offset(f.sc.End())
			f.tokOff, f.tokEnd = off, f.prevEnd
		}
		return tok, lit
	}
}

// comment records the comment text at [off, end) as one of gap k. Its text is
// read here, for what reads it, and left behind: the comment is its offsets.
func (f *goFile) comment(k, off, end int, text string) {
	start := f.lineStart(off)
	f.closeOpen(off)
	if len(f.gap) == cap(f.gap) {
		// A gap may hold every comment of the file, so it doubles as it
		// grows, allocating under four records a comment in all; append,
		// which grows a long slice by a quarter, allocates five times the
		// records it ends with, and slices.Grow overshoots a doubling.
		grown := make([]lexComment, len(f.gap), max(2*len(f.gap), 16))
		copy(grown, f.gap)
		f.gap = grown
	}
	kind, linter := f.kindOf(text, k <= headerGap)
	f.linter = f.linter || linter
	f.gap = append(f.gap, lexComment{
		off: int32(off),
		end: int32(end),
		key: layout{
			col: int32(off - start),
			// Every byte that is not whitespace belongs to a lexeme (a
			// leading byte-order mark aside, which no version adds alone),
			// so only blanks precede a comment on its line exactly when the
			// lexeme before it ended on an earlier line.
			lead:   f.prevEnd <= start,
			before: lineBreaks(f.src[f.prevEnd:off]),
		},
		kind:   kind,
		cr:     len(text) != end-off,
		output: outputMarker(text),
	})
	f.open = true
	f.prevEnd = end
}

// closeOpen gives the comment awaiting it what follows it: the lexeme at off,
// or the end of the file at len(src).
func (f *goFile) closeOpen(off int) {
	if !f.open {
		return
	}
	f.open = false
	c := &f.gap[len(f.gap)-1]
	c.key.after = lineBreaks(f.src[c.end:off])
	c.key.next = -1
	if off < len(f.src) {
		c.key.next = int32(off - f.lineStart(off))
	}
}

// nextLexeme is where the lexeme at or after off starts: past the blanks,
// carriage returns and newlines go/scanner skips, every other byte belonging
// to a lexeme — or the end of the file.
func (f *goFile) nextLexeme(off int) int {
	for off < len(f.src) {
		switch f.src[off] {
		case ' ', '\t', '\r', '\n':
			off++
		default:
			return off
		}
	}
	return off
}

// judgeGap compares the comments both versions read before code token tok —
// a gap, every comment of which knows what follows it — and files what it
// found where the token says the gap belongs: before an import declaration
// or one of its specs, where the import's path says whether it is a cgo
// preamble; in a top-level body, whose closing brace says whether it holds
// an example; or nowhere a later token decides, where only a directive's
// context counts. And it moves both versions' line records on over the gap
// and the token, lit the token's literal.
func (j *judgement) judgeGap(a, b *goFile, tok token.Token, lit string) {
	empty := len(a.gap) == 0 && len(b.gap) == 0
	fa, ga := directiveStart(a.gap)
	fb, gb := directiveStart(b.gap)
	context, whole, output := true, true, false
	if !empty {
		if j.sameText && !j.sameTexts(a, b) {
			j.sameText = false
		}
		context = j.sameContexts(a, fa, ga, b, fb, gb)
		output = slices.ContainsFunc(a.gap, isOutput) || slices.ContainsFunc(b.gap, isOutput)
	}
	if j.sameLines {
		atToken := tok != token.EOF && (tok != token.SEMICOLON || lit != "\n")
		j.sameLines = sameHeldLines(a, a.gap[ga:], b, b.gap[gb:], atToken)
	}
	pending := j.imports.pendingFor(tok, j.depth)
	inBody := j.depth > 0
	if !empty && (pending != nil || inBody && j.body.whole) {
		whole = j.sameWholes(a, b)
	}
	if pending != nil {
		*pending = pendingGap{open: true, whole: whole, context: context}
	}
	if inBody {
		j.body.output = j.body.output || output
		j.body.context = j.body.context && context
		j.body.whole = j.body.whole && whole
	}
	if pending == nil && !inBody {
		j.same = j.same && context
	}
}

// sameTexts reports whether the gap's comments are the same in both
// versions, comment for comment: whitespace_only's own test.
func (j *judgement) sameTexts(a, b *goFile) bool {
	if len(a.gap) != len(b.gap) {
		return false
	}
	for i := range a.gap {
		if !j.sameCommentText(a, a.gap[i], b, b.gap[i]) {
			return false
		}
	}
	return true
}

// sameWholes reports whether the gap's comments are the same in both
// versions, text and layout, comment for comment: a gap that is code whole.
func (j *judgement) sameWholes(a, b *goFile) bool {
	if len(a.gap) != len(b.gap) {
		return false
	}
	for i := range a.gap {
		if a.gap[i].key != b.gap[i].key || !j.sameEntryText(a, a.gap[i], b, b.gap[i]) {
			return false
		}
	}
	return true
}

// sameContexts reports whether the two versions' comments of the gap hold
// the same entries a directive's context is, in order: from the gap's first
// directive on, every comment whole, text and layout; ahead of it, a header
// comment's text, and the layout of each other comment of the directive's
// own group — free to say anything, not to move. The entries are walked in
// step and never gathered; fa and ga, fb and gb are each version's
// directiveStart.
func (j *judgement) sameContexts(a *goFile, fa, ga int, b *goFile, fb, gb int) bool {
	x, y := 0, 0
	for {
		px, okx := nextEntry(a.gap, &x, fa, ga)
		py, oky := nextEntry(b.gap, &y, fb, gb)
		switch {
		case okx != oky || px != py:
			return false
		case !okx:
			return true
		}
		cx, cy := a.gap[x-1], b.gap[y-1]
		if px != layoutEntry && !j.sameEntryText(a, cx, b, cy) || px != textEntry && cx.key != cy.key {
			return false
		}
	}
}

// entryPart is what of a comment a directive's context holds.
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

// nextEntry moves *i past the next comment of gap that a directive's context
// holds, and answers what of it the context holds; first is the gap's first
// directive and group the first comment of its group, both len(gap) where it
// holds none.
func nextEntry(gap []lexComment, i *int, first, group int) (entryPart, bool) {
	for ; *i < len(gap); *i++ {
		var part entryPart
		switch {
		case *i >= first:
			part = wholeEntry
		case gap[*i].kind == headerComment:
			part = textEntry
		case *i >= group:
			part = layoutEntry
		default:
			continue
		}
		*i++
		return part, true
	}
	return 0, false
}

// directiveStart is the index of the gap's first directive and that of the
// first comment of its comment group — comments on adjacent lines, with no
// token between — or len(gap) for both where it holds none.
func directiveStart(gap []lexComment) (first, group int) {
	for i, c := range gap {
		if c.key.before > 1 {
			group = i
		}
		if c.kind == directiveComment {
			return i, group
		}
	}
	return len(gap), len(gap)
}

// THE LINE RECORD is what a linter's directive reads of where the code sits.
// A suppression's reach is lines: golangci-lint's nolint covers the lines of
// its comment group and of the statement under it, staticcheck's lint:ignore
// the first line of the node go/ast attaches it to, gosec's #nosec that
// node, revive's the next line or every line to its enable — and which node
// go/ast attaches a comment to is its own reading of the lines around it. So
// each version keeps, for every code token but an inserted semicolon and
// every comment of a directive's context from its group on — the lexemes the
// record holds — the line breaks since the one it held before, and for a
// comment those inside it too, and a file holding a linter's directive gets
// no class unless the two versions' records agree. Between them the two
// counts reach every byte from one held lexeme to the next, so a token moved
// onto or off a line moves one of them, wherever the line break was: a
// directive's text and a code token's are compared whole, but the
// documentation in a directive's group may say anything, and a line it grows
// inside itself moves what follows it a line on as surely as a line break
// after it would. The record runs through the whole file, since a reach may:
// revive's //revive:disable runs to its enable or the file's end, and a
// nolint above the package clause covers the file; a window drawn round each
// directive instead would be each linter's own rule for the node it attaches
// to, restated, and any rule it missed a reach left open.
//
// sameHeldLines moves both versions' line records on over a gap's held
// comments, ha of version a and hb of b, and over the code token after them
// where atToken says the walk is at one the lexer did not insert, and reports
// whether each sits as many lines from the lexeme held before it in both,
// and each comment spans as many.
func sameHeldLines(a *goFile, ha []lexComment, b *goFile, hb []lexComment, atToken bool) bool {
	if len(ha) != len(hb) {
		return false
	}
	for i := range ha {
		if a.heldComment(ha[i]) != b.heldComment(hb[i]) {
			return false
		}
	}
	return !atToken || a.held(a.tokOff, a.tokEnd, true) == b.held(b.tokOff, b.tokEnd, true)
}

// heldComment moves the version's line record on over comment c, one the
// record holds, and answers its place there: the line breaks since the lexeme
// held before it (held), and those inside it, up to two as lineBreaks counts
// them.
func (f *goFile) heldComment(c lexComment) [2]uint8 {
	return [2]uint8{f.held(int(c.off), int(c.end), false), lineBreaks(f.src[c.off:c.end])}
}

// held moves the version's line record on to the lexeme at [off, end) — a
// code token, or a comment the record holds — and answers the line breaks
// since the lexeme it held before: between two code tokens whether there is
// one at all, since all a linter asks of two tokens is whether they share a
// line; beside a comment 0, 1, or 2 for a blank line or more, which go/ast
// reads in attaching a comment to the statement above it or the one below.
func (f *goFile) held(off, end int, code bool) uint8 {
	n := lineBreaks(f.src[f.heldEnd:off])
	if code && f.heldCode {
		n = min(n, 1)
	}
	f.heldEnd, f.heldCode = end, code
	return n
}

// isOutput reports an example's output marker.
func isOutput(c lexComment) bool { return c.output }

// sameCommentText reports whether two comments, x of version a and y of b, have
// the same text as the lexer returns it.
func (j *judgement) sameCommentText(a *goFile, x lexComment, b *goFile, y lexComment) bool {
	return bytes.Equal(a.text(x, &j.text[0]), b.text(y, &j.text[1]))
}

// sameEntryText reports whether two comments a directive's context holds
// read alike: a directive as the compiler reads it (directiveText), anything
// else as the lexer returns it. Where only one is a directive their texts
// differ, since what the lexer returns of the two would.
func (j *judgement) sameEntryText(a *goFile, x lexComment, b *goFile, y lexComment) bool {
	if x.kind == directiveComment || y.kind == directiveComment {
		return bytes.Equal(a.directiveText(x), b.directiveText(y))
	}
	return j.sameCommentText(a, x, b, y)
}

// directiveText is directive c's text as the compiler reads one: its bytes,
// every carriage return the lexer drops from its text kept — one splitting a
// pragma leaves none, and one in an embed's pattern or a //line directive's
// file name names another — but for the one a Windows line ending leaves at
// a line comment's end, which the compiler and the lexer both drop.
func (f *goFile) directiveText(c lexComment) []byte {
	raw := f.src[c.off:c.end]
	if raw[1] == '/' && raw[len(raw)-1] == '\r' {
		return raw[:len(raw)-1]
	}
	return raw
}

// text is c's text as the lexer returns it: its bytes, but for the carriage
// returns it drops, which only where its bytes hold one are stripped, into
// buf.
func (f *goFile) text(c lexComment, buf *[]byte) []byte {
	raw := f.src[c.off:c.end]
	if !c.cr {
		return raw
	}
	*buf = stripCR((*buf)[:0], raw, raw[1] == '*')
	return *buf
}

// stripCR appends b, a comment's bytes, without the carriage returns
// go/scanner drops from its text: every one, but for one in a block
// comment's `*\r/`, which would close the comment early without it (its own
// stripCR, Go issue 11151).
func stripCR(dst, b []byte, block bool) []byte {
	start := len(dst)
	for i, ch := range b {
		if ch != '\r' || block && len(dst)-start > len("/*") && dst[len(dst)-1] == '*' && i+1 < len(b) && b[i+1] == '/' {
			dst = append(dst, ch)
		}
	}
	return dst
}

// pendingBody is the open top-level body's gaps — a function's, a
// composite's — compared both ways until its closing brace says whether it
// holds an example's output, which makes every comment in it code.
type pendingBody struct {
	output  bool // an example's output marker in either version
	whole   bool // every gap so far alike, text and layout
	context bool // every gap so far alike as a directive's context
}

// brace follows code token tok into and out of top-level braces — a
// function's body, a composite's elements — the bodies an example's output
// is judged in, and gives a body's gaps their verdict at its closing brace.
func (j *judgement) brace(tok token.Token) {
	switch tok {
	case token.LBRACE:
		if j.depth == 0 {
			j.body = pendingBody{whole: true, context: true}
		}
		j.depth++
	case token.RBRACE:
		if j.depth > 0 {
			j.depth--
			if j.depth == 0 {
				j.closeBody()
			}
		}
	}
}

// closeBody files the body's gaps: whole where it holds an example's output,
// else as a directive's context.
func (j *judgement) closeBody() {
	if j.body.output {
		j.same = j.same && j.body.whole
	} else {
		j.same = j.same && j.body.context
	}
}

// endFile files what the end of the file leaves undecided: an import
// declaration or a body it cuts off.
func (j *judgement) endFile() {
	j.closeSpec()
	j.closeDecl()
	if j.depth > 0 {
		j.closeBody()
	}
}

// cImports reads the import declarations, a code token at a time, for where
// a cgo preamble may sit: before an import declaration that imports "C",
// whose doc cgo reads where "C" is its only spec, and before the spec
// itself, whose doc it reads otherwise. Both are code. A gap there is a
// preamble only once its import's path is read, so its two comparisons wait
// for that.
type cImports struct {
	state cState
	decl  pendingGap // the gap before the declaration's import keyword
	spec  pendingGap // the gap before the spec being read, in a parenthesized declaration
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

// pendingGap is a gap a cgo preamble may sit in, compared both ways until
// its import's path says which comparison counts.
type pendingGap struct {
	open    bool
	c       bool // its import is of "C": a preamble, code whole
	whole   bool // its comments alike, text and layout
	context bool // its comments alike as a directive's context
}

// pendingFor is the pending gap the gap before code token tok is, read at
// brace depth depth, or nil: the gap before an import keyword, or before a
// spec of a parenthesized declaration.
func (c *cImports) pendingFor(tok token.Token, depth int) *pendingGap {
	switch {
	case c.state == cOutside && tok == token.IMPORT && depth == 0:
		return &c.decl
	case c.state == cBlock && tok != token.RPAREN && tok != token.SEMICOLON && tok != token.EOF:
		return &c.spec
	}
	return nil
}

// seeImport reads code token tok, the gap before which judgeGap has filed.
func (j *judgement) seeImport(tok token.Token, lit string) {
	c := &j.imports
	switch c.state {
	case cOutside:
		if tok == token.IMPORT && j.depth == 0 {
			c.state = cImport
		}
	case cImport:
		switch tok {
		case token.LPAREN:
			c.state = cBlock
		case token.IDENT, token.PERIOD:
			c.state = cNamed
		case token.STRING:
			c.path(lit, false)
			j.closeDecl()
		default:
			j.closeDecl()
		}
	case cNamed:
		if tok == token.STRING {
			c.path(lit, false)
		}
		j.closeDecl()
	case cBlock, cSpec:
		switch {
		case tok == token.RPAREN:
			j.closeSpec()
			j.closeDecl()
		case tok == token.SEMICOLON:
			j.closeSpec()
			c.state = cBlock
		case c.state == cBlock:
			c.state = cSpec
			if tok == token.STRING {
				c.path(lit, true)
			}
		case tok == token.STRING:
			c.path(lit, true)
		}
	}
}

// path reads an import path's literal; inSpec is whether it is a spec's of a
// parenthesized declaration. Both spellings of "C" count: go/build unquotes a
// raw one to C, and cgo reads only the quoted one, so the raw one errs toward
// code.
func (c *cImports) path(lit string, inSpec bool) {
	if lit != `"C"` && lit != "`C`" {
		return
	}
	c.decl.c = true
	if inSpec {
		c.spec.c = true
	}
}

// closeDecl ends the import declaration, filing the gap before it.
func (j *judgement) closeDecl() {
	j.same = j.same && j.imports.decl.verdict()
	j.imports.decl, j.imports.state = pendingGap{}, cOutside
}

// closeSpec ends a parenthesized declaration's spec, filing the gap before it.
func (j *judgement) closeSpec() {
	j.same = j.same && j.imports.spec.verdict()
	j.imports.spec = pendingGap{}
}

// verdict is the comparison that counts for the gap: whole where it is a cgo
// preamble, else as a directive's context — and nothing to hold where no gap
// is pending.
func (p pendingGap) verdict() bool {
	switch {
	case !p.open:
		return true
	case p.c:
		return p.whole
	}
	return p.context
}

// kindOf is what reads the comment text, as the lexer returns it, and whether
// a linter reads it, whose reach is lines; header is whether it sits in the
// file's header.
func (f *goFile) kindOf(text string, header bool) (kind commentKind, linter bool) {
	if strings.HasPrefix(text, "/*") {
		switch body := strings.TrimSuffix(text[2:], "*/"); {
		case suppression(body):
			return directiveComment, true
		case strings.HasPrefix(body, "line "):
			return directiveComment, false
		case header && f.generatedMarker(body):
			return headerComment, false
		}
		return plainComment, false
	}
	if constraint.IsGoBuild(text) || constraint.IsPlusBuild(text) {
		return headerComment, false
	}
	switch body := text[2:]; {
	case suppression(body):
		return directiveComment, true
	case goDirective(body):
		return directiveComment, linterDirective(body)
	case generatorMarker(body):
		return directiveComment, false
	case header && f.generatedMarker(body):
		return headerComment, false
	}
	return plainComment, false
}

// linterDirective reports a directive's body that a linter reads rather than
// Go's own tools: staticcheck's lint:, and every //name:value but the
// compiler's go: — revive's, and any tool's that follows Go's convention, an
// unknown one's reach erring toward lines.
func linterDirective(body string) bool {
	if strings.HasPrefix(body, "lint:") {
		return true
	}
	name, ok := nameValue(body)
	return ok && name != "go"
}

// goDirective reports a line comment's body, past its //, that a tool reads
// as a directive: go/ast's list — "line ", "extern ", "export ", and the
// //name:value form (nameValue), which every tool that follows Go's
// convention writes (lint:ignore, nolint:errcheck, revive:disable) — and any
// go: or lint: at all, which the compiler and staticcheck take whatever
// follows the colon.
func goDirective(body string) bool {
	for _, prefix := range []string{"line ", "extern ", "export ", "go:", "lint:"} {
		if strings.HasPrefix(body, prefix) {
			return true
		}
	}
	_, ok := nameValue(body)
	return ok
}

// nameValue reports a directive body of the form [a-z0-9]+:[a-z0-9]…, and its
// name, read past any blanks before it as revive reads its own
// (`^//[\s]*revive:`): go/ast's form wants none, but // revive:disable, with
// the space most comments carry, turns revive's rules off to the file's end
// all the same — and a person's prose that takes the form erring toward code
// is the safe side.
func nameValue(body string) (string, bool) {
	name, rest, ok := strings.Cut(strings.TrimLeft(body, " \t\v\f"), ":")
	return name, ok && name != "" && rest != "" && lowerAlnum(name) && lowerAlnum(rest[:1])
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
	for i := strings.IndexByte(body, '#'); i >= 0; {
		if strings.EqualFold(body[i:min(i+len("#nosec"), len(body))], "#nosec") {
			return true
		}
		next := strings.IndexByte(body[i+1:], '#')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return false
}

// generatedPhrases are the phrases that mark a file generated, in lower case:
// Go's "Code generated … DO NOT EDIT." line (go/ast's IsGenerated), and the
// looser ones golangci-lint takes for one.
var generatedPhrases = [][]byte{[]byte("code generated"), []byte("do not edit"), []byte("autogenerated"), []byte("automatically generated"), []byte("generated by")}

// generatedMarker reports a header comment's body that marks the file
// generated, in any case — under any of which golangci-lint lints none of the
// file. The body is lowered into scratch the version keeps, so a header of a
// hundred thousand comments costs no allocation a comment.
func (f *goFile) generatedMarker(body string) bool {
	f.lower = appendLower(f.lower[:0], body)
	for _, phrase := range generatedPhrases {
		if bytes.Contains(f.lower, phrase) {
			return true
		}
	}
	return false
}

// appendLower appends s in lower case, rune by rune, as strings.ToLower maps
// it.
func appendLower(dst []byte, s string) []byte {
	for _, r := range s {
		dst = utf8.AppendRune(dst, unicode.ToLower(r))
	}
	return dst
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

// buildConstraints is what go/build reads of a file's build constraints: every
// //go:build line of its header, and the // +build lines it honours — those
// above the last blank line before the header's first line that is not a //
// comment — following go/build's reading of a header line for line (its
// parseFileHeader). It is why a blank line, or a block comment above a +build
// line, is code there, while a //go:build line may sit anywhere in the
// header. A header ends at the first line holding anything but comments, so
// a header may be a file of comments: a line is made a string only once it
// holds a constraint's word.
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
		if !inBlock && bytes.HasPrefix(line, []byte("//go:build")) && constraint.IsGoBuild(string(line)) {
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
		if !bytes.Contains(line, []byte("+build")) {
			continue
		}
		if text := string(bytes.TrimSpace(line)); constraint.IsPlusBuild(text) {
			lines = append(lines, text)
		}
	}
	return lines
}

// goGenerate opens a line go generate runs, when a blank follows it.
var goGenerate = []byte("//go:generate")

// generateLineBytes is the longest line go generate reads, its newline
// included: it reads a file through a bufio.Reader of the default size, 4 096
// bytes, and fails on a //go:generate line the reader cannot hold ("directive
// too long"), skipping any other.
const generateLineBytes = 4096

// sameGenerateLines reports whether two versions hold the same commands go
// generate runs, read as it reads them (cmd/go's generate.go): the file a line
// at a time, up to each newline, whatever lexeme a line sits in — a line that
// starts //go:generate and a blank, in a comment of its own, inside a block
// comment or a raw string, is a command — less the one carriage return a
// Windows line ending leaves before the newline. A command go generate fails
// on instead — one no newline ends, or one longer than its reader holds
// (generateLineBytes) — stops it with an error, and it runs nothing after it
// in the package; so whether it runs each line is compared beside the line,
// and a carriage return that takes a command past the reader's bound, or
// back within it, is code. A line's number, which go generate gives its
// command as GOLINE, is not held: a whitespace change above a command moves
// it as it moves every position a tool reports, and a command that reads its
// own file reads every byte of it, which no class can be true of.
func sameGenerateLines(a, b []byte) bool {
	at, bt := 0, 0
	for {
		la, runsA, okA := nextGenerateLine(a, &at)
		lb, runsB, okB := nextGenerateLine(b, &bt)
		if okA != okB || runsA != runsB || !bytes.Equal(la, lb) {
			return false
		}
		if !okA {
			return true
		}
	}
}

// nextGenerateLine is the next line of src from *at on that go generate
// reads as a command, without its newline and the carriage return before
// it; whether go generate runs it — a newline ends it within
// generateLineBytes — or fails on it; and whether there is one. *at moves
// past it.
func nextGenerateLine(src []byte, at *int) (line []byte, runs, ok bool) {
	for *at < len(src) {
		i := bytes.Index(src[*at:], goGenerate)
		if i < 0 {
			break
		}
		start := *at + i
		*at = start + 1
		if start > 0 && src[start-1] != '\n' {
			continue
		}
		if blank := start + len(goGenerate); blank >= len(src) || src[blank] != ' ' && src[blank] != '\t' {
			continue
		}
		end := bytes.IndexByte(src[start:], '\n')
		if end < 0 {
			*at = len(src)
			return src[start:], false, true
		}
		*at = start + end + 1
		return bytes.TrimSuffix(src[start:start+end], []byte("\r")), end < generateLineBytes, true
	}
	*at = len(src)
	return nil, false, false
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
