package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/provenance"
)

// EVERY FILE'S PATCH is the stat's walk for the files and totals, then one diff-pairs fed the
// admitted records from the page's file on, each followed by a lone NUL that makes git end the
// file's patch with one: a file's patch is the bytes between two NULs, whatever headers it holds.

// maxPatchesRunPages is how many pages past the asked one a run cuts and memoizes: about
// 0.9 MiB, what a person reading a stacked diff scrolls through next; a later page is a run
// of its own from its cursor, which feeds git only the files from it on.
const maxPatchesRunPages = 16

// patchFeed is what a patches run hands diff-pairs: each admitted record from the from-th on,
// each followed by the lone NUL that makes git end its patch with one, and each one's place.
type patchFeed struct {
	from  int
	in    bytes.Buffer
	files []gitlet.FilePatch
}

// take feeds e, the m-th file the diff admits, where it is at or past the page's file.
func (f *patchFeed) take(m int, e rawEntry) {
	if m < f.from {
		return
	}
	f.in.Write(e.raw())
	f.in.WriteByte(0)
	path, b64 := gitlet.WirePath(e.paths[len(e.paths)-1])
	f.files = append(f.files, gitlet.FilePatch{File: m, Path: path, PathB64: b64, Status: string(e.status)})
}

// patchesRead answers a page of every file's patch: the stat's walk for the files and totals,
// then ONE diff-pairs fed the admitted records from the cursor's file on, each flushed so git
// ends its patch with a NUL, the pages cut at lines as they stream and the run stopped once
// maxPatchesRunPages past the asked one are cut.
func (r *read) patchesRead(ctx context.Context, d diffAsk, at cursor, m *memoRun) (gitlet.GitAnswer, error) {
	from, fromByte := 0, 0
	if at.letter != 0 {
		from, fromByte = at.a-1, at.b-1
	}
	feed := &patchFeed{from: from}
	st, err := r.stat(ctx, d.base, d.head, d.filter, d.renames, 0, 0, d.narrowing, nil, feed)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	if at.letter != 0 && from >= st.total {
		return gitlet.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, fmt.Sprintf("a file of the %d the diff "+
			"admits", st.total))
	}
	tail := memoTail{total: st.total, added: st.added, deleted: st.deleted}
	page := func(pg *patchesPage) gitlet.GitAnswer {
		return gitlet.GitAnswer{Kind: d.kind, Next: formatCursor(pg.next), Diff: &gitlet.DiffAnswer{Base: d.base,
			Head: d.head, Files: []gitlet.DiffFile{}, Patches: pg.patches}}
	}
	answer := func(a gitlet.GitAnswer) gitlet.GitAnswer {
		a.Diff.Total, a.Diff.Added, a.Diff.Deleted = tail.total, tail.added, tail.deleted
		return a
	}
	// The envelope holds one empty patch, given back to the room, so the list's brackets are in it.
	envelope := gitlet.GitAnswer{Kind: d.kind, Next: widestCursor('w', 2), Diff: &gitlet.DiffAnswer{Base: d.base,
		Head: d.head, Files: []gitlet.DiffFile{}, Total: widest, Added: widest, Deleted: widest,
		Patches: []gitlet.FilePatch{{}}}}
	room := r.room(envelope) + jsonLen(gitlet.FilePatch{})
	asked := &patchesPage{begun: at, left: room}
	if len(feed.files) == 0 {
		// A diff of no file: a page of no patch.
		a := page(asked)
		m.put(at, a, asked.left)
		m.place(d.kind, same(tail))
		return answer(a), nil
	}
	cut := &patchesCut{files: feed.files, room: room, fromByte: fromByte, started: fromByte == 0,
		more: m != nil, asked: asked, cur: asked, headK: -1,
		stage: func(pg *patchesPage) { m.put(pg.begun, page(pg), pg.left) }}
	deadline, err := r.budget()
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"diff-pairs", "-z", "-p", "--full-index", "--no-color", "--no-ext-diff",
			"--no-textconv", "-U3"},
		Config: append(r.s.o.Cache.ServeConfig(), patchConfig), Stdin: &feed.in, Stdout: cut,
		Class: gitproc.ClassRead, Deadline: deadline,
	})
	if res.Damage != nil {
		// git may exit 0 over damage it read around: no page of such a run is an answer.
		return gitlet.GitAnswer{}, r.failed(ctx, "patches of a diff", res, err, nil)
	}
	switch {
	case cut.badFrom:
		r.charge(res)
		return gitlet.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, "the start of a line of a file's patch")
	case cut.err != nil:
		r.charge(res)
		return gitlet.GitAnswer{}, cut.err
	case errors.Is(err, errStop):
		r.charge(res)
		err = nil
	case err == nil:
		err = cut.end()
	}
	short := false
	if err != nil && (gitlet.AnswerDue(ctx) || (errors.Is(err, gitproc.ErrDeadline) &&
		r.metered)) {
		// THE BUDGET ENDED GIT: the asked page as it stands, kept only where it was whole.
		switch {
		case asked.done:
			r.charge(res)
			err = nil
		case len(asked.patches) > 0:
			r.charge(res)
			err, short = nil, true
			cut.shortened()
		}
	}
	if err != nil {
		narrow := fmt.Errorf("%w: the patches of the diff of %s and %s did not finish in the read's %s of git: "+
			"name paths, or read the files one at a time with file", gitlet.ErrSliceTooLarge, d.base, d.head,
			r.s.gitBudget)
		if d.kind == gitlet.GitInterdiff {
			narrow = fmt.Errorf("%w: the patches of the interdiff of %s and %s did not finish in the read's %s "+
				"of git: read its files one at a time with file", gitlet.ErrSliceTooLarge, d.base, d.head,
				r.s.gitBudget)
		}
		if err = r.failed(ctx, "patches of a diff", res, err, narrow); err != nil {
			return gitlet.GitAnswer{}, err
		}
	}
	a := page(asked)
	if !short {
		m.put(at, a, asked.left)
		m.place(d.kind, same(tail))
	}
	return answer(a), nil
}

// patchesPage is one page of a diff's patches as it is cut: the cursor it begins at, its
// patches, the last one's text while it grows, what they left of the room, and its Next.
type patchesPage struct {
	begun   cursor
	patches []gitlet.FilePatch
	text    []byte
	left    int
	next    cursor
	done    bool
}

// flush settles the last patch's text.
func (pg *patchesPage) flush() {
	if n := len(pg.patches); n > 0 {
		pg.patches[n-1].Patch = string(pg.text)
	}
}

// patchesCut cuts diff-pairs' patches into pages as they stream (a Cmd.Stdout): a NUL ends
// the file being read and moves to the next of files, and bytes are held a line at a time,
// never more than a page's room of one. The asked page begins at byte fromByte of files[0];
// each page after it begins at the line the one before could not take.
type patchesCut struct {
	files    []gitlet.FilePatch
	room     int
	fromByte int
	more     bool
	stage    func(pg *patchesPage)

	// The stream: the file it is in, its patch's bytes so far, where the line in hand began,
	// whether the asked page has begun, whether the rest of the file's patch is passed over,
	// and the line while it may join a page, costed so far.
	k       int
	off     int
	lineAt  int
	started bool
	omitted bool
	held    bool
	line    []byte
	cost    int
	costed  int
	// headK is the file whose head cost headCost is.
	headK    int
	headCost int

	asked   *patchesPage
	cur     *patchesPage
	staged  int
	stopped bool
	badFrom bool
	err     error
}

// Write takes p, bytes of diff-pairs' output.
func (c *patchesCut) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !c.stopped {
		if p[0] == 0 {
			c.endFile()
			p = p[1:]
			continue
		}
		if c.k >= len(c.files) {
			c.err, c.stopped = fmt.Errorf("query: diff-pairs answered bytes past the %d files it was fed",
				len(c.files)), true
			break
		}
		seg := p
		if i := bytes.IndexAny(p, "\n\x00"); i >= 0 {
			seg = p[:i]
			if p[i] == '\n' {
				seg = p[:i+1]
			}
		}
		c.take(seg)
		p = p[len(seg):]
		if !c.stopped && seg[len(seg)-1] == '\n' {
			if c.held {
				c.place(c.line, c.cost+textCost(c.line[c.costed:]))
			}
			c.nextLine()
		}
	}
	if c.stopped {
		return n, errStop
	}
	return n, nil
}

// take adds seg, part of one line, to the line in hand.
func (c *patchesCut) take(seg []byte) {
	if c.off == c.lineAt {
		c.begin()
	}
	c.off += len(seg)
	if !c.held {
		return
	}
	c.line = append(c.line, seg...)
	for c.costed < len(c.line) {
		cost, size, ok := runeCost(c.line[c.costed:])
		if !ok {
			break
		}
		c.cost, c.costed = c.cost+cost, c.costed+size
	}
	if c.cost > c.room-c.head() {
		c.tooLong()
	}
}

// begin opens a line at lineAt: the asked page's first where it begins at fromByte.
func (c *patchesCut) begin() {
	if !c.started {
		switch {
		case c.lineAt == c.fromByte:
			c.started = true
		case c.lineAt > c.fromByte:
			c.badFrom, c.stopped = true, true
			return
		}
	}
	c.held = c.started && c.cur != nil && !c.omitted
}

// nextLine ends the line in hand.
func (c *patchesCut) nextLine() {
	c.lineAt, c.held, c.line, c.cost, c.costed = c.off, false, c.line[:0], 0, 0
}

// endFile ends the file being read at git's NUL: its last line where it had no newline, the
// head alone of a file whose patch is empty, and a cursor at its patch's end is the next file's.
func (c *patchesCut) endFile() {
	if c.k >= len(c.files) {
		c.err, c.stopped = fmt.Errorf("query: diff-pairs answered more patches than the %d files it was fed",
			len(c.files)), true
		return
	}
	if c.off > c.lineAt && c.held {
		c.place(c.line, c.cost+textCost(c.line[c.costed:]))
	}
	if !c.started {
		if c.off != c.fromByte {
			c.badFrom, c.stopped = true, true
			return
		}
		c.started = true
	}
	if c.off == 0 && c.cur != nil {
		c.place(nil, 0)
	}
	if c.stopped {
		return
	}
	c.k, c.off, c.omitted = c.k+1, 0, false
	c.nextLine()
}

// head is what the current file's patch costs a page before its text: its encoding with
// Continues and RestOmitted set and its fence.
func (c *patchesCut) head() int {
	if c.headK != c.k {
		f := c.files[c.k]
		f.Continues, f.RestOmitted = true, true
		c.headK, c.headCost = c.k, jsonLen(f)+fenceCost[provenance.SourceRepoPatch]
	}
	return c.headCost
}

// place puts line, costing cost, on the page being cut, or ends that page before it and
// begins the next with it.
func (c *patchesCut) place(line []byte, cost int) {
	f := c.files[c.k]
	for c.cur != nil {
		pg := c.cur
		last := len(pg.patches) - 1
		same := last >= 0 && pg.patches[last].File == f.File
		need := cost
		if !same {
			need += c.head()
			if last >= 0 {
				need++ // the comma before it
			}
		}
		if need <= pg.left {
			if !same {
				pg.flush()
				pg.patches, pg.text = append(pg.patches, f), pg.text[:0]
			}
			pg.text, pg.left = append(pg.text, line...), pg.left-need
			return
		}
		if last < 0 {
			c.tooLong()
			return
		}
		c.endPage(cursor{letter: 'w', a: f.File + 1, b: c.lineAt + 1}, same)
	}
}

// endPage ends the page being cut before next, its last patch going on there where continues,
// and begins the page after it, or stops the run once the pages it cuts are cut.
func (c *patchesCut) endPage(next cursor, continues bool) {
	pg := c.cur
	pg.flush()
	if continues {
		pg.patches[len(pg.patches)-1].Continues = true
	}
	pg.next, pg.done, c.cur = next, true, nil
	if pg != c.asked {
		c.stage(pg)
		c.staged++
	}
	if !c.more || c.staged >= maxPatchesRunPages {
		c.stopped = true
		return
	}
	c.cur = &patchesPage{begun: next, left: c.room}
}

// tooLong is the line in hand longer than any page holds: the file's patch stops before it,
// marked RestOmitted, and the stream passes over the rest of that patch to the next file's.
func (c *patchesCut) tooLong() {
	f := c.files[c.k]
	c.held, c.omitted = false, true
	for pg := c.cur; pg != nil; pg = c.cur {
		last := len(pg.patches) - 1
		if last >= 0 && pg.patches[last].File == f.File {
			pg.patches[last].RestOmitted = true
			return
		}
		need := c.head() // its head was costed with RestOmitted set
		if last >= 0 {
			need++
		}
		if need <= pg.left {
			pg.flush()
			f.RestOmitted = true
			pg.patches, pg.text, pg.left = append(pg.patches, f), pg.text[:0], pg.left-need
			return
		}
		if last < 0 {
			// Not even the file's head fits a page: the asked page is refused, a later one ends the run.
			if pg == c.asked {
				c.err = fmt.Errorf("%w: the patch of %q does not fit one answer: read the file's lines instead",
					gitlet.ErrSliceTooLarge, f.Path)
			}
			c.cur, c.stopped = nil, true
			return
		}
		c.endPage(cursor{letter: 'w', a: f.File + 1, b: c.lineAt + 1}, false)
	}
}

// end settles the patches git answered whole: every file fed ended by its NUL, and the last
// page, which git's end ends, staged or answered.
func (c *patchesCut) end() error {
	if c.k != len(c.files) || c.off > 0 {
		return fmt.Errorf("query: diff-pairs answered %d patches for the %d files it was fed", c.k, len(c.files))
	}
	if pg := c.cur; pg != nil {
		pg.flush()
		pg.done, c.cur = true, nil
		if pg != c.asked && len(pg.patches) > 0 {
			c.stage(pg)
		}
	}
	return nil
}

// shortened ends the asked page where the read's budget ended git: its Next the line it did
// not reach, and its last patch going on there where that line is its file's.
func (c *patchesCut) shortened() {
	pg := c.asked
	pg.flush()
	if c.k >= len(c.files) {
		return
	}
	f := c.files[c.k]
	if c.omitted {
		// The rest of this file's patch is left out: the page after begins at the next file.
		pg.next = cursor{}
		if c.k+1 < len(c.files) {
			pg.next = cursor{letter: 'w', a: c.files[c.k+1].File + 1, b: 1}
		}
		return
	}
	pg.next = cursor{letter: 'w', a: f.File + 1, b: c.lineAt + 1}
	if last := len(pg.patches) - 1; pg.patches[last].File == f.File {
		pg.patches[last].Continues = true
	}
}
