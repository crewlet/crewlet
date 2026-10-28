package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/codeindex"
)

// A FILE is `git cat-file blob <id>` streamed through a page cutter that keeps
// only the page being cut — every Write of it bounded, gitproc's contract — and
// counts the rest of the file's lines as they pass, so Lines is the whole
// file's while no more than a page of it is ever held. A page is whole lines
// from where it starts while their encoded text fits; a line longer than a page
// on its own is served in pieces at rune boundaries (Continues), each page's
// cursor naming the byte the next resumes at, so the pieces concatenate to the
// line byte for byte. git cannot seek inside an object, so a page past the
// first reads everything before it — which is why, where the memo takes them,
// the run cuts every page from the asked one to its range's end (blobPages),
// each begun where the one before it ends: one stream of a file serves every
// page a read of it asks for after (memo.go).
//
// BINARY is git's own test (buffer_is_binary): a NUL in the first 8 000
// bytes, which codeindex.Binary holds — the one copy of the rule, by which the
// code index skips the same files `git grep -I` does. Such a file is answered
// by its size, with no text, and its stream is ended there.

// blobRead answers a file read: a blob by its own id, or the file a path names
// in a commit — or, where the path names a directory or none is given of a
// commit, the directory's listing.
func (r *read) blobRead(ctx context.Context, objs found, at cursor, m *memoRun) (code.GitAnswer, error) {
	rev := objs.revs[r.q.Blob.Rev]
	switch rev.peeledKind {
	case "blob":
		if objs.path != "" {
			return code.GitAnswer{}, fmt.Errorf("%w: blob.path: %s names a file already; a path names a "+
				"file in a commit", code.ErrInvalid, r.q.Blob.Rev)
		}
		return r.readBlob(ctx, rev.peeled, rev.peeled, "", rev.peeledSize, at, m)
	case "commit", "tree":
	default:
		return code.GitAnswer{}, fmt.Errorf("%w: blob.rev: %s names a %s; a file read names a commit or a "+
			"blob", code.ErrInvalid, r.q.Blob.Rev, rev.peeledKind)
	}
	switch objs.target.kind {
	case "blob":
		return r.readBlob(ctx, rev.peeled, objs.target.id, objs.path, objs.target.size, at, m)
	case "tree":
		// ONE READ FOR A FILE AND A DIRECTORY: a seat that names a
		// directory where it guessed a file is answered the listing it
		// needs next, not a refusal it must turn into a second read.
		if at.letter != 0 && at.letter != 'e' {
			return code.GitAnswer{}, errBadCursor(r.q.Cursor, code.GitTree, widestCursor('e', 1))
		}
		return r.list(ctx, rev.peeled, objs.path, objs.target.id, false, at, m)
	}
	return code.GitAnswer{}, fmt.Errorf("%w: %q names nothing in %s that holds lines (a submodule's commit is "+
		"another repository's)", code.ErrNotABlob, objs.path, rev.peeled)
}

// readBlob is a page of the file blob — at path under rev, where a path named
// it — of size bytes, from at, and, where m takes them, every page after it to
// the end of the read's range.
func (r *read) readBlob(ctx context.Context, rev, blob, path string, size int64, at cursor,
	m *memoRun) (code.GitAnswer, error) {

	from, fromByte := max(r.q.Blob.StartLine, 1), 0
	switch at.letter {
	case 0:
	case 'l', 'b':
		from, fromByte = at.a, at.b
		if from < max(r.q.Blob.StartLine, 1) || (r.q.Blob.EndLine > 0 && from > r.q.Blob.EndLine) {
			return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, "a line inside the read's range")
		}
	default:
		return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, widestCursor('l', 1)+" or "+
			widestCursor('b', 2))
	}
	p, pB64 := code.WirePath(path)
	page := func(pg *blobPage) code.GitAnswer {
		a := code.GitAnswer{Kind: code.GitBlob, Rev: rev, Blob: &code.BlobAnswer{Path: p, PathB64: pB64, OID: blob,
			Size: size}}
		if pg.started {
			b := a.Blob
			b.StartLine, b.EndLine, b.StartByte = pg.startLine, pg.endLine, pg.fromByte
			b.Text, b.Continues = string(pg.text), pg.cont
			a.Next = formatCursor(pg.next)
		}
		return a
	}
	// THE ENVELOPE at its longest, and the text's own key: what the page's
	// text may take is the rest.
	envelope := code.GitAnswer{Kind: code.GitBlob, Rev: rev,
		Next: widestCursor('b', 2), Blob: &code.BlobAnswer{Path: p, PathB64: pB64, OID: blob, Size: widest,
			Binary: true, Lines: widest, StartLine: widest, EndLine: widest, StartByte: widest, Text: "x",
			Continues: true}}
	room := r.room(envelope) + 1
	pages := newBlobPages(&blobPage{from: from, fromByte: fromByte, to: r.q.Blob.EndLine, left: room, line: 1},
		room, m != nil, func(pg *blobPage) { m.put(pg.begun, page(pg), pg.left) })

	deadline, err := r.budget()
	if err != nil {
		return code.GitAnswer{}, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"cat-file", "blob", blob}, Config: r.s.o.Cache.ServeConfig(),
		Stdout: pages, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if errors.Is(err, errStop) {
		err = nil // a binary file, decided from its opening
	}
	if err == nil {
		pages.finish()
	}
	narrow := fmt.Errorf("%w: reading %s did not finish in the read's %s of git: read fewer of its lines",
		code.ErrSliceTooLarge, blob, r.s.gitBudget)
	if err := r.failed(ctx, "read of a file", res, err, narrow); err != nil {
		return code.GitAnswer{}, err
	}
	pg := pages.asked
	a := page(pg)
	b := a.Blob
	lines := pages.lines()
	switch {
	case pages.binary:
		b.Binary = true
	case !pg.started:
		switch {
		case lines == 0 && from == 1 && fromByte == 0:
			// an empty file: nothing to read
		case at.letter != 0:
			return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, "a position inside the file")
		default:
			return code.GitAnswer{}, fmt.Errorf("%w: blob.start_line: %d is past the file's %d lines",
				code.ErrInvalid, from, lines)
		}
	}
	if pages.binary {
		// A binary file is answered by its size alone: what of its lines the
		// opening held is no count of the file's.
		lines = 0
	}
	m.put(at, a, pg.left)
	m.place(code.GitBlob, same(memoTail{lines: lines}))
	b.Lines = lines
	return a, nil
}

// blobPages is a Cmd.Stdout cutting a file read's pages out of the stream:
// the page asked for, and — where more is set — every page after it to the
// end of the read's range, each a blobPage of its own, begun where the last
// one's next cursor names and fed the bytes of the line it begins in from
// there (lineBuf), so every page is exactly the page a read from its cursor
// would cut, and each is handed to page as it is cut. It decides binary
// itself, git's test on the file's opening, and counts the whole file's lines
// whatever the pages do.
type blobPages struct {
	room  int
	more  bool
	page  func(pg *blobPage)
	asked *blobPage
	// cur is the page being cut — the asked one first — and nil once none is.
	cur *blobPage

	// sniff holds the file's opening until binary is decided.
	sniff   []byte
	decided bool
	binary  bool

	// line and col are where the next byte is; newlines, bytes and last
	// count the file.
	line, col int
	newlines  int
	bytes     int64
	last      byte

	// lineBuf is the current line's bytes from bufCol on — never any before
	// the asked page begins, nor before the page being cut began in it — kept
	// while a page past the asked one may begin inside it.
	lineBuf []byte
	bufCol  int
}

// newBlobPages is the pages of a read whose asked page is asked, each cut
// against room; more, every page after it too, each handed to page.
func newBlobPages(asked *blobPage, room int, more bool, page func(pg *blobPage)) *blobPages {
	return &blobPages{room: room, more: more, page: page, asked: asked, cur: asked, line: 1}
}

func (w *blobPages) Write(p []byte) (int, error) {
	n := len(p)
	w.bytes += int64(n)
	if n > 0 {
		w.last = p[n-1]
	}
	if !w.decided {
		take := min(len(p), codeindex.BinaryProbeBytes-len(w.sniff))
		w.sniff = append(w.sniff, p[:take]...)
		p = p[take:]
		if len(w.sniff) < codeindex.BinaryProbeBytes {
			return n, nil
		}
		if w.decide() {
			return n, errStop
		}
	}
	w.feed(p)
	return n, nil
}

// decide calls the file binary or feeds its opening: true for binary, whose
// stream is ended there.
func (w *blobPages) decide() bool {
	w.decided = true
	if codeindex.Binary(w.sniff) {
		w.binary = true
		return true
	}
	w.feed(w.sniff)
	w.sniff = nil
	return false
}

// finish ends the stream: a file shorter than the probe is decided now, and
// the last line with no newline settled — on every page it ends, since a
// last line too long for one is cut into pieces as any is — and the page past
// the asked one that the file's end ended, with room to spare, handed on.
func (w *blobPages) finish() {
	if !w.decided && w.decide() {
		return
	}
	if w.cur == nil {
		return
	}
	w.cur.finish()
	w.chain(false, true)
	if p := w.cur; p != nil && p != w.asked && p.started {
		w.page(p)
	}
}

// lines is how many lines the file has: its newlines, and one more for a
// last line without one.
func (w *blobPages) lines() int {
	if w.bytes > 0 && w.last != '\n' {
		return w.newlines + 1
	}
	return w.newlines
}

// feed takes p's bytes, a line's segment and its newline at a time, so the
// page being cut is told of each line's end as the line it ends is the one
// in hand.
func (w *blobPages) feed(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.segment(p)
			return
		}
		w.segment(p[:i])
		w.newline()
		p = p[i+1:]
	}
}

// newlineByte is a line's end, as a page is fed one.
var newlineByte = []byte{'\n'}

// segment takes seg, bytes of the current line from col on, none a newline.
func (w *blobPages) segment(seg []byte) {
	if len(seg) == 0 {
		return
	}
	if w.cur != nil {
		if w.more {
			w.buffer(seg)
		}
		w.cur.feed(seg)
	}
	w.col += len(seg)
	w.chain(false, false)
}

// newline takes the current line's end.
func (w *blobPages) newline() {
	w.newlines++
	if w.cur != nil {
		w.cur.feed(newlineByte)
		w.chain(true, false)
	}
	w.line, w.col = w.line+1, 0
	w.lineBuf, w.bufCol = w.lineBuf[:0], 0
}

// buffer keeps seg, bytes of the current line at col, from where the asked
// page begins on.
func (w *blobPages) buffer(seg []byte) {
	at := w.col
	if a := w.asked; !a.started {
		if w.line != a.from {
			return
		}
		if skip := a.fromByte - at; skip > 0 {
			if skip >= len(seg) {
				return
			}
			seg, at = seg[skip:], at+skip
		}
	}
	if len(w.lineBuf) == 0 {
		w.bufCol = at
	}
	w.lineBuf = append(w.lineBuf, seg...)
}

// chain hands on every page the stream has just ended — at a line it could
// not take whole, at a piece of one too long for any page, or at its range's
// end — and begins the next where its cursor names, while pages past the
// asked one are cut: fed the current line's bytes from where it begins, the
// newline that just ended the line where one did (nl), and the stream's end
// where it has come (eof) — each of which may end it too.
func (w *blobPages) chain(nl, eof bool) {
	for w.cur != nil && w.cur.done {
		done := w.cur
		if done != w.asked {
			w.page(done)
		}
		if !w.more || done.next.letter == 0 {
			w.cur = nil // the range's end, or no page past the asked one wanted
			return
		}
		start := done.next.b // a whole line's page begins at its byte 0
		if len(w.lineBuf) > 0 && start > w.bufCol {
			w.lineBuf, w.bufCol = w.lineBuf[start-w.bufCol:], start
		}
		col := w.bufCol
		if len(w.lineBuf) == 0 {
			col = w.col
		}
		next := &blobPage{from: done.next.a, fromByte: start, to: w.asked.to, left: w.room, line: w.line,
			col: col, begun: done.next}
		w.cur = next
		if len(w.lineBuf) > 0 {
			next.feed(w.lineBuf)
		}
		if nl {
			next.feed(newlineByte)
		}
		if eof {
			next.finish()
		}
	}
}

// blobPage cuts one page of a file's lines out of the stream blobPages feeds
// it: the window from byte fromByte of line from to the end of line to (0: the
// file's end), while the page's text costs at most left. A page past the one a
// read asked for is begun by blobPages at the cursor the page before it names
// (begun), fed from there. The file's binary test and its line count are the
// stream's, which blobPages keeps once for every page it cuts.
type blobPage struct {
	from, fromByte, to int
	left               int
	begun              cursor

	// line and col are where the next byte is: its line, 1-based, and how
	// many bytes into it.
	line, col int

	// The page: started once the window begins, done once it is cut.
	started, done      bool
	startLine, endLine int
	text               []byte
	cont               bool
	next               cursor

	// cur is the line in hand inside the window — from the window's start,
	// or its own — while it may still join the page: curCost what its first
	// costed bytes cost, cutAt the longest of its prefixes at a rune
	// boundary that fits the page on its own, and over that it has outgrown
	// the page, so it can join no more whole and no more of it is kept.
	cur                []byte
	curCost, costed    int
	cutAt, curStartCol int
	over               bool
}

// finish ends the stream: a last line with no newline is settled.
func (b *blobPage) finish() {
	if b.started && !b.done && b.col > 0 {
		b.settle(false)
	}
}

// feed takes p's bytes, line by line. A line's opening that p ends on is
// handed on only where it holds bytes: the window begins at a line only once
// the line is known to exist — by a byte of it, or by its newline — since the
// file's end can follow a newline at once, and the "line" after it is none.
func (b *blobPage) feed(p []byte) {
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(p) > 0 {
				b.segment(p)
			}
			return
		}
		b.segment(p[:i])
		if b.started && !b.done {
			b.settle(true)
		}
		b.line, b.col = b.line+1, 0
		p = p[i+1:]
	}
}

// segment takes seg, bytes of line b.line from b.col on, none a newline.
func (b *blobPage) segment(seg []byte) {
	if !b.started {
		skip := b.fromByte - b.col
		if b.line != b.from || skip > len(seg) {
			b.col += len(seg)
			return
		}
		b.col += skip
		seg = seg[skip:]
		b.started, b.startLine, b.endLine, b.curStartCol = true, b.line, b.line, b.col
	}
	if !b.done {
		b.take(seg)
	}
	b.col += len(seg)
}

// take adds seg to the line in hand while it may join the page, costing its
// runes as they complete; a line that outgrows the page alone is decided at
// once — the page ends before it, or it is cut into pieces — rather than held.
func (b *blobPage) take(seg []byte) {
	if b.over {
		return
	}
	b.cur = append(b.cur, seg...)
	for b.costed < len(b.cur) {
		c, n, ok := runeCost(b.cur[b.costed:])
		if !ok {
			return // the rest of a rune comes with the next bytes
		}
		if b.curCost+c > b.left {
			b.over = true
			b.cur = b.cur[:b.costed]
			b.outgrown()
			return
		}
		b.curCost, b.costed, b.cutAt = b.curCost+c, b.costed+n, b.costed+n
	}
}

// outgrown cuts the page at a line in hand that cannot join it whole: before
// it, where the page holds text, or at the longest piece of it that fits, as
// the page's whole text.
func (b *blobPage) outgrown() {
	b.done = true
	if len(b.text) > 0 {
		b.next = cursor{letter: 'l', a: b.line}
		return
	}
	b.text = append(b.text, b.cur[:b.cutAt]...)
	b.cont, b.endLine = true, b.line
	b.next = cursor{letter: 'b', a: b.line, b: b.curStartCol + b.cutAt}
}

// settle ends the line in hand: at its newline (nl), or at the file's end.
func (b *blobPage) settle(nl bool) {
	if !b.over {
		// What is left uncosted is a rune the line ended inside: bytes that
		// are not UTF-8, each costing as the encoder writes one.
		tail := b.cur[b.costed:]
		cost := b.curCost + textCost(tail)
		if nl {
			cost += asciiCost['\n']
		}
		switch {
		case cost <= b.left:
			b.text = append(b.text, b.cur...)
			if nl {
				b.text = append(b.text, '\n')
			}
			b.left -= cost
			b.endLine = b.line
			if b.to > 0 && b.line >= b.to {
				b.done = true
			}
		case len(b.text) > 0:
			b.done = true
			b.next = cursor{letter: 'l', a: b.line}
		default:
			// The line alone is a page and a newline, or a page and an
			// incomplete rune: its piece is what fits, as when it outgrew
			// the page midway.
			b.costed = len(b.cur) - len(tail)
			b.over = true
			b.outgrown()
		}
	}
	b.cur, b.curCost, b.costed, b.cutAt, b.over, b.curStartCol = b.cur[:0], 0, 0, 0, false, 0
}
