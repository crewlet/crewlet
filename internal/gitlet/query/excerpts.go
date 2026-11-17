package query

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// AN EXCERPT is the lines of an anchor's blob around its range, marked as its revision's diff
// marks them. Each anchor is paired with its file's other side, the diff of every distinct
// pair is ONE diff-pairs at whole-file context, and each window is cut from it as it streams.

// excerptPair is the two blobs one diff of an excerpts read compares, "" for a side that is
// not there (an added or a deleted file).
type excerptPair struct{ old, new string }

// excerptsRead answers an excerpts read: its anchors' pairs found, ONE diff-pairs over every
// distinct pair, each window cut from its pair's whole-file hunk as it streams, and the
// excerpts paged whole from the cursor's anchor, every page staged for the memo.
func (r *read) excerptsRead(ctx context.Context, objs found, at cursor, m *memoRun) (gitlet.GitAnswer, error) {
	q := r.q.Excerpts
	for i, a := range q.Anchors {
		for _, c := range []struct{ field, rev string }{{"head", a.Head}, {"base", a.Base}} {
			if o := objs.revs[c.rev]; c.rev != "" && o.peeledKind != "commit" {
				return gitlet.GitAnswer{}, fmt.Errorf("%w: excerpts.anchors[%d].%s: %s names a %s; an excerpt "+
					"is read in a revision's commits", gitlet.ErrInvalid, i, c.field, c.rev, o.peeledKind)
			}
		}
		if o := objs.revs[a.BlobOID]; o.kind != "blob" {
			return gitlet.GitAnswer{}, fmt.Errorf("%w: excerpts.anchors[%d].blob_oid: %s names a %s; an "+
				"excerpt's lines are a file's", gitlet.ErrInvalid, i, a.BlobOID, o.kind)
		}
	}
	cut := newExcerptCutter(q)
	// Each anchor's pair: its blob and the file at its path on the other side, or the rename
	// a walk finds where that side has none.
	type candidate struct {
		anchor   int
		old      bool
		base, hd string
	}
	var candidates []candidate
	for i, a := range q.Anchors {
		other, blob := objs.placed[i], a.BlobOID
		old := a.Side == gitlet.SideOld
		switch {
		case other.kind == "blob" && other.id == blob:
			cut.pairWith(i, "", excerptPair{new: blob}, true)
		case other.kind == "blob" && old:
			cut.pairWith(i, "M", excerptPair{old: blob, new: other.id}, false)
		case other.kind == "blob":
			cut.pairWith(i, "M", excerptPair{old: other.id, new: blob}, false)
		case a.Base != "":
			candidates = append(candidates, candidate{anchor: i, old: old, base: objs.revs[a.Base].peeled,
				hd: objs.revs[a.Head].peeled})
		default:
			cut.pairWith(i, "A", excerptPair{new: blob}, false)
		}
	}
	var walks [][2]string
	for _, c := range candidates {
		if w := [2]string{c.base, c.hd}; !slices.Contains(walks, w) {
			walks = append(walks, w)
		}
	}
	for _, w := range walks {
		news, olds := map[string][]int{}, map[string][]int{}
		for _, c := range candidates {
			if c.base != w[0] || c.hd != w[1] {
				continue
			}
			a := q.Anchors[c.anchor]
			p, _ := gitlet.GitPath("path", a.Path, a.PathB64, false)
			if c.old {
				olds[p] = append(olds[p], c.anchor)
			} else {
				news[p] = append(news[p], c.anchor)
			}
		}
		if err := r.renames(ctx, w[0], w[1], func(oldPath, newPath, oldID, newID string) {
			for _, i := range news[newPath] {
				cut.renamed(i, oldID, q.Anchors[i].BlobOID)
			}
			for _, i := range olds[oldPath] {
				cut.renamed(i, q.Anchors[i].BlobOID, newID)
			}
			delete(news, newPath)
			delete(olds, oldPath)
		}, func() bool { return len(news) == 0 && len(olds) == 0 }); err != nil {
			return gitlet.GitAnswer{}, err
		}
		// A candidate no rename placed is added, or on the old side deleted.
		for _, c := range candidates {
			a := q.Anchors[c.anchor]
			p, _ := gitlet.GitPath("path", a.Path, a.PathB64, false)
			switch {
			case c.base != w[0] || c.hd != w[1]:
			case !c.old && slices.Contains(news[p], c.anchor):
				cut.pairWith(c.anchor, "A", excerptPair{new: a.BlobOID}, false)
			case c.old && slices.Contains(olds[p], c.anchor):
				cut.pairWith(c.anchor, "D", excerptPair{old: a.BlobOID}, false)
			}
		}
	}
	if err := r.diffExcerpts(ctx, cut); err != nil {
		return gitlet.GitAnswer{}, err
	}
	return r.excerptPages(cut.excerpts(), at, m)
}

// renames walks base..head's whole-tree diff with renames detected, handing each rename to
// found until settled reports every candidate placed.
func (r *read) renames(ctx context.Context, base, head string, found func(oldPath, newPath, oldID, newID string),
	settled func() bool) error {

	var (
		e    rawEntry
		need int
		hit  bool
	)
	recs := newRecords(0, gitproc.MaxLine+gitlet.MaxGitPathBytes, func(rec []byte, long bool) (err error) {
		if long {
			return fmt.Errorf("query: diff-tree answered a record of %d bytes or more, past any path", len(rec))
		}
		if need > 0 {
			e.paths = append(e.paths, string(rec))
			if need--; need > 0 || e.status != 'R' {
				return nil
			}
			found(e.paths[0], e.paths[1], e.oldID, e.newID)
			if settled() {
				hit = true
				return errStop
			}
			return nil
		}
		e, need, err = parseRaw(rec)
		return err
	})
	deadline, err := r.budget()
	if err != nil {
		return err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   []string{"diff-tree", "-z", "-r", "--raw", "--no-ext-diff", "--no-textconv", "-M", base, head},
		Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	switch {
	case hit && res.Damage == nil:
		r.charge(res)
		return nil
	case err == nil:
		err = recs.close()
	}
	narrow := fmt.Errorf("%w: the rename walk of %s and %s did not finish in the read's %s of git: read fewer "+
		"findings' excerpts at a time", gitlet.ErrSliceTooLarge, base, head, r.s.gitBudget)
	return r.failed(ctx, "rename walk of an excerpt's revision", res, err, narrow)
}

// diffExcerpts runs ONE diff-pairs over cut's pairs, numbered in the order they were paired,
// its patch fed to cut as it streams.
func (r *read) diffExcerpts(ctx context.Context, cut *excerptCutter) error {
	var in bytes.Buffer
	for n, p := range cut.pairs {
		switch {
		case p.old == "":
			fmt.Fprintf(&in, ":000000 100644 %s %s A\x00%d\x00", strings.Repeat("0", len(p.new)), p.new, n)
		case p.new == "":
			fmt.Fprintf(&in, ":100644 000000 %s %s D\x00%d\x00", p.old, strings.Repeat("0", len(p.old)), n)
		default:
			fmt.Fprintf(&in, ":100644 100644 %s %s M\x00%d\x00", p.old, p.new, n)
		}
	}
	deadline, err := r.budget()
	if err != nil {
		return err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		// -U is gitlet.MaxAnchorLine, the most lines a blob holds: each pair's diff is one hunk
		// holding every line of both sides.
		Args: []string{"diff-pairs", "-z", "-p", "-U1073741824", "--no-color", "--no-ext-diff",
			"--no-textconv"},
		Config: r.s.o.Cache.ServeConfig(), Stdin: &in, Stdout: cut, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if err == nil {
		err = cut.close()
	}
	narrow := fmt.Errorf("%w: the diffs of the %d files the excerpts were read in did not finish in the read's "+
		"%s of git: read fewer findings' excerpts at a time", gitlet.ErrSliceTooLarge, len(cut.pairs),
		r.s.gitBudget)
	return r.failed(ctx, "diff of excerpted files", res, err, narrow)
}

// excerptPages answers the page of excerpts at at, each page whole excerpts in the anchors'
// order, and stages every page from the first, or from at where no page begins there.
func (r *read) excerptPages(excerpts []gitlet.Excerpt, at cursor, m *memoRun) (gitlet.GitAnswer, error) {
	envelope := gitlet.GitAnswer{Kind: gitlet.GitExcerpts, Next: widestCursor('x', 1),
		Excerpts: &gitlet.ExcerptsAnswer{Excerpts: []gitlet.Excerpt{}}}
	room := r.room(envelope)
	costs := make([]int, len(excerpts))
	for i, e := range excerpts {
		costs[i] = jsonLen(e)
	}
	// page is the page from anchor i: its answer, where the next begins, and the room it left.
	page := func(i int) (gitlet.GitAnswer, int, int, error) {
		used, j := 0, i
		for ; j < len(excerpts); j++ {
			c := costs[j]
			if j > i {
				c++ // the comma before it
			}
			if used+c > room {
				break
			}
			used += c
		}
		if j == i {
			return gitlet.GitAnswer{}, 0, 0, fmt.Errorf("%w: the excerpt of anchor %s encodes to %d bytes, past "+
				"the %d one answer holds beside its envelope", gitlet.ErrSliceTooLarge, excerpts[i].ID, costs[i], room)
		}
		a := gitlet.GitAnswer{Kind: gitlet.GitExcerpts, Excerpts: &gitlet.ExcerptsAnswer{Excerpts: excerpts[i:j]}}
		if j < len(excerpts) {
			a.Next = formatCursor(cursor{letter: 'x', a: j})
		}
		return a, j, room - used, nil
	}
	from := 0
	for i := 0; i < at.a; {
		_, next, _, err := page(i)
		if err != nil {
			return gitlet.GitAnswer{}, err
		}
		i = next
		if i > at.a {
			from = at.a // the cursor is no page's start at this bound: its pages alone
		}
	}
	var asked gitlet.GitAnswer
	for i := from; i < len(excerpts); {
		a, next, left, err := page(i)
		if err != nil {
			return gitlet.GitAnswer{}, err
		}
		c := cursor{}
		if i > 0 {
			c = cursor{letter: 'x', a: i}
		}
		m.put(c, a, left)
		if i == at.a {
			asked = a
		}
		i = next
	}
	m.place(gitlet.GitExcerpts, same(memoTail{}))
	return asked, nil
}

// excerptCutter is a Cmd.Stdout cutting each anchor's window out of an excerpts diff as it
// streams, line by line, holding no line past a window.
type excerptCutter struct {
	windows []excerptWindow
	pairs   []excerptPair
	index   map[excerptPair]int
	// anchors is each pair's windows, and open how many of them are not settled.
	anchors [][]int
	open    []int
	// cur is the pair the lines are of (-1 before the first), and oldAt and newAt the
	// numbers its next line takes, counted once its hunk has begun (inHunk).
	cur          int
	inHunk       bool
	oldAt, newAt int
	// line is the line in hand, cut to lineCap bytes, and size its whole length.
	line []byte
	size int
	// held is the text the windows hold, and peak the most held at once with the line in hand.
	held, peak int
}

// lineCap is the most of a line the cutter keeps: its mark and the longest text an excerpt
// shows, which every header an excerpts diff writes fits in too.
const lineCap = 1 + gitlet.MaxExcerptLineBytes

// excerptWindow is one anchor's excerpt as it is cut: lines lo to hi on its side (asContext
// reads an unchanged file's added lines as kept ones), and the other side's lines pending
// since its last line, which join only when a later line is in the window.
type excerptWindow struct {
	ex        gitlet.Excerpt
	old       bool
	asContext bool
	lo, hi    int
	pending   []gitlet.ExcerptLine
	over      bool
	started   bool
	done      bool
}

func newExcerptCutter(q *gitlet.ExcerptsQuery) *excerptCutter {
	c := &excerptCutter{index: map[excerptPair]int{}, cur: -1}
	around := q.ContextOf()
	for _, a := range q.Anchors {
		c.windows = append(c.windows, excerptWindow{ex: gitlet.Excerpt{ID: a.ID, Lines: []gitlet.ExcerptLine{}},
			old: a.Side == gitlet.SideOld, lo: max(1, a.Start-around), hi: a.End + around})
	}
	return c
}

// pairWith pairs anchor i with p, its file's status in the revision's diff status.
func (c *excerptCutter) pairWith(i int, status string, p excerptPair, asContext bool) {
	n, ok := c.index[p]
	if !ok {
		n = len(c.pairs)
		c.index[p] = n
		c.pairs = append(c.pairs, p)
		c.anchors, c.open = append(c.anchors, nil), append(c.open, 0)
	}
	c.anchors[n] = append(c.anchors[n], i)
	c.open[n]++
	w := &c.windows[i]
	w.ex.Status, w.asContext = status, asContext
}

// renamed pairs anchor i with the two blobs of its file's rename; a rename that kept the file
// whole is read as its lines kept, since git prints nothing for two equal blobs.
func (c *excerptCutter) renamed(i int, old, new string) {
	if old == new {
		c.pairWith(i, "R", excerptPair{new: new}, true)
		return
	}
	c.pairWith(i, "R", excerptPair{old: old, new: new}, false)
}

// excerpts is every anchor's excerpt, in the anchors' order.
func (c *excerptCutter) excerpts() []gitlet.Excerpt {
	out := make([]gitlet.Excerpt, len(c.windows))
	for i, w := range c.windows {
		out[i] = w.ex
	}
	return out
}

func (c *excerptCutter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		seg := p
		if i >= 0 {
			seg = p[:i]
		}
		c.keep(seg)
		if i < 0 {
			break
		}
		if err := c.end(); err != nil {
			return n, err
		}
		c.line, c.size, p = c.line[:0], 0, p[i+1:]
	}
	return n, nil
}

// keep adds seg to the line in hand, up to lineCap bytes; a line of a settled pair keeps only
// what tells a pair's header.
func (c *excerptCutter) keep(seg []byte) {
	c.size += len(seg)
	var first byte
	switch {
	case len(c.line) > 0:
		first = c.line[0]
	case len(seg) > 0:
		first = seg[0]
	}
	limit := lineCap
	if c.cur >= 0 && c.open[c.cur] == 0 && first != 'd' {
		limit = 1
	}
	if left := limit - len(c.line); left > 0 {
		c.line = append(c.line, seg[:min(left, len(seg))]...)
	}
	c.peak = max(c.peak, c.held+len(c.line))
}

// close reports a patch that ended inside a line, as git's own failure to end it.
func (c *excerptCutter) close() error {
	if c.size > 0 {
		return fmt.Errorf("query: diff-pairs' patch ended inside a line")
	}
	return nil
}

// end reads the line in hand: a pair's header, a binary pair's line, its hunk's header, or
// one of its file's lines.
func (c *excerptCutter) end() error {
	rec := c.line
	if c.inHunk && len(rec) > 0 && (rec[0] == ' ' || rec[0] == '+' || rec[0] == '-' || rec[0] == '\\') {
		return c.fileLine()
	}
	switch {
	case bytes.HasPrefix(rec, pairHeader):
		at, ok := pairOf(rec[len(pairHeader):], len(c.pairs))
		if !ok {
			return fmt.Errorf("query: diff-pairs answered %q, which names no pair it was handed", truncated(rec))
		}
		c.cur, c.inHunk = at, false
	case c.cur < 0 || c.open[c.cur] == 0:
	case bytes.HasPrefix(rec, binaryHeader):
		for _, i := range c.anchors[c.cur] {
			c.windows[i].ex.Binary = true
			c.settle(i)
		}
	case bytes.HasPrefix(rec, hunkHeader):
		h, ok := gitlet.ParseHunk(rec)
		if !ok {
			return fmt.Errorf("query: diff-pairs answered %q, which is no hunk header", truncated(rec))
		}
		c.inHunk, c.oldAt, c.newAt = true, h.OldStart, h.NewStart
	}
	return nil
}

// fileLine reads one line of the current pair's hunk into the windows it falls in.
func (c *excerptCutter) fileLine() error {
	var l gitlet.ExcerptLine
	switch c.line[0] {
	case '\\':
		return nil // "\ No newline at end of file" is no line of the file
	case ' ':
		l = gitlet.ExcerptLine{Mark: " ", Old: c.oldAt, New: c.newAt}
		c.oldAt, c.newAt = c.oldAt+1, c.newAt+1
	case '+':
		l = gitlet.ExcerptLine{Mark: "+", New: c.newAt}
		c.newAt++
	default:
		l = gitlet.ExcerptLine{Mark: "-", Old: c.oldAt}
		c.oldAt++
	}
	if c.open[c.cur] == 0 {
		return nil
	}
	shown := false
	for _, i := range c.anchors[c.cur] {
		w := &c.windows[i]
		if w.done {
			continue
		}
		at := l.New
		if w.old && !w.asContext {
			at = l.Old
		}
		switch {
		case at == 0:
			if w.started {
				if len(w.ex.Lines)+len(w.pending) < gitlet.MaxExcerptLines {
					if !shown {
						l, shown = c.shown(l), true
					}
					w.pending = append(w.pending, l)
					c.held += len(l.Text)
				} else {
					w.over = true
				}
			}
			continue
		case at < w.lo:
			continue
		case at > w.hi:
			c.settle(i)
			continue
		}
		if !shown {
			l, shown = c.shown(l), true
		}
		w.started = true
		for _, p := range w.pending {
			c.take(i, p)
		}
		c.drop(w)
		if w.over {
			w.ex.More = true
		}
		in := l
		if w.asContext {
			in.Mark, in.Old = " ", l.New
		}
		c.take(i, in)
		if w.ex.More {
			c.settle(i)
		}
	}
	return nil
}

// shown is l with its text: the line in hand past its mark, or its size where the text runs
// past what an excerpt shows.
func (c *excerptCutter) shown(l gitlet.ExcerptLine) gitlet.ExcerptLine {
	text, size := c.line[1:], c.size-1
	switch {
	case size > gitlet.MaxExcerptLineBytes || textCost(text) > gitlet.MaxExcerptLineBytes:
		l.TextBytes = size
	default:
		l.Text = string(text)
	}
	return l
}

// take adds l to window i's excerpt, or marks it More where it is full.
func (c *excerptCutter) take(i int, l gitlet.ExcerptLine) {
	w := &c.windows[i]
	if len(w.ex.Lines) == gitlet.MaxExcerptLines {
		w.ex.More = true
		return
	}
	w.ex.Lines = append(w.ex.Lines, l)
	c.held += len(l.Text)
}

// drop lets go of w's pending lines.
func (c *excerptCutter) drop(w *excerptWindow) {
	for _, p := range w.pending {
		c.held -= len(p.Text)
	}
	w.pending, w.over = nil, false
}

// settle ends window i: nothing more joins it.
func (c *excerptCutter) settle(i int) {
	w := &c.windows[i]
	c.drop(w)
	if !w.done {
		w.done = true
		c.open[c.cur]--
	}
}
