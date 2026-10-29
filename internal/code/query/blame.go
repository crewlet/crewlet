package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A BLAME is ONE `git blame --porcelain -L <from>,<to> <commit> -- <path>`
// over the whole id of the commit the file is read at, its range given as
// lines the server formats, and the PATH THE LAST ARGUMENT, after `--`: git
// reads every argument after `--` as a path and never as an option, and blame
// takes a PATH, not a pathspec, so no magic applies — `blame <id> --
// --output=x` blames the file `--output=x`, or answers that there is none.
// It is the one read whose path reaches git as an argument rather than on a
// standard input, and that only because git blame reads none there: the one
// other form, a file named for `--contents`, reads a file's TEXT, not its
// history. The batch check before it (objects.go) found the path naming a
// file at the commit, so a blame of a directory or of nothing is refused
// naming the path before any blame runs.
//
// THE AUTHORS ARE THE COMMITS' OWN. A blame in a bare repository reads the
// `.mailmap` at HEAD — the default branch's, whatever commit is blamed — and
// git blame has no --no-mailmap, so its process is given the one key that
// names where it would read one, `mailmap.blob`, set to nothing: an answer is
// a function of the commits it names, as a log's is (--no-mailmap there).
// Read at HEAD, it would be a function of the moment too: a copy that holds a
// push's adopted pack before a round writes its ref has a HEAD naming the
// branch's earlier commit, or none, and reads that commit's mailmap or no
// mailmap where the same blame a round later reads the new one — and the memo
// keeps whichever page was cut first (measured on 2.56.0: "Mapped Name" over
// the pool's own run without the key, the commit's author with it).
//
// # The porcelain
//
// Each line is a group header — `<id> <line in the commit> <line in the file>`,
// and the group's length on its first line — then, the first time a commit is
// named, its details (author, author-mail, author-time, summary, …,
// filename), and the line itself after a TAB; a commit blamed under more than
// one path names its `filename` on every group. So the parser keeps each
// commit's details for the run and the path it was last named under, which is
// every later line's of that commit. A filename is C-quoted as git quotes a
// path that needs it, and read back exactly (unquoteC); a summary is the
// message's first LINE, git's own "(<id>)" for an empty message answered as an
// empty subject. Every field git leaves unbounded is held no further than the
// most an answer carries, and described past its bound
// (code.MaxBlameLineBytes, code.MaxLogSubjectBytes, code.MaxLogPersonBytes).
//
// # Pages, and one run for the range
//
// A page ends before the line whose encoding — its text fenced, and the first
// time the page names its commit that commit's details, subject fenced, with
// the push that introduced it — would take the answer past its bound, every
// string neutralised as a code tool sends it (page.go); its Next is that
// line, `l:<n>`. git cannot blame a range in pieces any cheaper than whole —
// its cost is the walk of the file's history for the lines named — so where
// the memo takes them the run cuts EVERY page of the range from the one
// asked, each memoized, and a range read page by page is blamed once; where
// it does not, the run is ended once the asked page is cut. A line too wide
// for an empty page — a path its origin had, longer than any read names — is
// refused naming it, never cut.
//
// PAST THE FILE'S END a range is git's own refusal, read by its FORMAT (a
// trace2 error event's, never its words, which name the path the request
// gave) and answered code.ErrInvalid naming how many lines the file has,
// counted by streaming the file once. An empty file has none, and is answered
// so with no blame run.

// blamePastEnd is git blame's refusal of a range that begins past the file's
// last line, by the format it prints it in: its singular and plural.
var blamePastEnd = []string{"file %s has only %lu line", "file %s has only %lu lines"}

// noMailmap is the configuration a blame runs with beside the copy's serve
// configuration: no `.mailmap` read from HEAD.
var noMailmap = gitproc.KV{Key: "mailmap.blob", Value: ""}

// blameRead answers a blame read: the page asked and, where m takes them,
// every page after it to the range's end.
func (r *read) blameRead(ctx context.Context, objs found, at cursor, m *memoRun) (code.GitAnswer, error) {
	q := r.q.Blame
	rev := objs.revs[q.Rev]
	if rev.peeledKind != "commit" {
		return code.GitAnswer{}, fmt.Errorf("%w: blame.rev: %s names a %s; a blame reads a file at a commit",
			code.ErrInvalid, q.Rev, rev.peeledKind)
	}
	switch objs.target.kind {
	case "blob":
	case "tree":
		return code.GitAnswer{}, fmt.Errorf("%w: %q names a directory in %s: blame a file in it",
			code.ErrNotABlob, objs.path, rev.peeled)
	default:
		return code.GitAnswer{}, fmt.Errorf("%w: %q names no file in %s that holds lines (a submodule's "+
			"commit is another repository's)", code.ErrNotABlob, objs.path, rev.peeled)
	}
	start, end := q.Range()
	from := start
	switch at.letter {
	case 0:
	case 'l':
		if from = at.a; from <= start || from > end {
			return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, "a line inside the blame's range")
		}
	}
	if objs.target.size == 0 {
		// AN EMPTY FILE HAS NO LINES, which git blame refuses to name a
		// range of: answered so, with no run.
		switch {
		case at.letter != 0:
			return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, "a line of the file")
		case from > 1:
			return code.GitAnswer{}, r.pastEnd(from, objs.path, 0)
		}
		return *newBlameCut(r, nil, rev.peeled, objs.path, at, commitsOf(r.q, objs)).asked, nil
	}
	c := newBlameCut(r, m, rev.peeled, objs.path, at, commitsOf(r.q, objs))
	stream := newBlameStream(c.take)
	deadline, err := r.budget()
	if err != nil {
		return code.GitAnswer{}, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"blame", "--porcelain", "-L", strconv.Itoa(from) + "," + strconv.Itoa(end), rev.peeled,
			"--", objs.path},
		Config: append(r.s.o.Cache.ServeConfig(), noMailmap), Stdout: stream, Class: gitproc.ClassRead,
		Deadline: deadline,
	})
	switch {
	case res.Damage != nil:
	case c.tooLarge != nil:
		r.charge(res)
		return code.GitAnswer{}, c.tooLarge
	case errors.Is(err, errUnparsed):
		r.charge(res)
		return code.GitAnswer{}, r.unparsed(ctx, "blame", err)
	case c.stopped:
		err = nil // the asked page is cut, and no page after it is wanted
	case err == nil:
		if cerr := stream.close(); cerr != nil {
			r.charge(res)
			return code.GitAnswer{}, r.unparsed(ctx, "blame", cerr)
		}
		c.finish()
	case res.Exit == 128 && res.Trace.Reported(blamePastEnd...):
		r.charge(res)
		if at.letter != 0 {
			return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, "a line of the file")
		}
		lines, err := r.countLines(ctx, objs.target.id)
		if err != nil {
			return code.GitAnswer{}, err
		}
		return code.GitAnswer{}, r.pastEnd(from, objs.path, lines)
	}
	narrow := fmt.Errorf("%w: blaming lines %d to %d of %q did not finish in the read's %s of git: blame "+
		"fewer of its lines", code.ErrSliceTooLarge, from, end, objs.path, r.s.gitBudget)
	if err := r.failed(ctx, "blame", res, err, narrow); err != nil {
		return code.GitAnswer{}, err
	}
	c.restore()
	m.place(code.GitBlame, same(memoTail{}))
	return *c.asked, nil
}

// pastEnd is a blame whose range begins at line from of path, which has lines
// lines.
func (r *read) pastEnd(from int, path string, lines int) error {
	return fmt.Errorf("%w: blame.start_line: %d is past the end of %q, which has %d lines", code.ErrInvalid, from,
		path, lines)
}

// countLines is how many lines blob has, streamed once through a counter: its
// newlines, and one more for a last line without one — git blame's count.
func (r *read) countLines(ctx context.Context, blob string) (int, error) {
	deadline, err := r.budget()
	if err != nil {
		return 0, err
	}
	var n lineCount
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"cat-file", "blob", blob}, Config: r.s.o.Cache.ServeConfig(),
		Stdout: &n, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if err := r.failed(ctx, "count of a file's lines", res, err, nil); err != nil {
		return 0, err
	}
	return n.lines(), nil
}

// lineCount counts a stream's lines as it passes.
type lineCount struct {
	newlines, bytes int
	last            byte
}

func (c *lineCount) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.newlines += bytes.Count(p, newlineByte)
		c.bytes += len(p)
		c.last = p[len(p)-1]
	}
	return len(p), nil
}

func (c *lineCount) lines() int {
	if c.bytes > 0 && c.last != '\n' {
		return c.newlines + 1
	}
	return c.newlines
}

// blameCut is the pages one blame run cuts from its lines as they arrive: the
// page asked, and — where the memo takes them — every page after it.
type blameCut struct {
	r    *read
	m    *memoRun
	rev  string
	path string
	wire string
	b64  string
	// base is the commits every page names before its lines — the read's
	// revision — which each page's decoration begins from.
	base []string

	// asked is the page the read asked for, and askedNamed and askedPushed
	// its decoration as it was cut, given back to the read once the run has
	// cut the pages after it (restore).
	asked       *code.GitAnswer
	askedNamed  []string
	askedPushed map[string]code.PushedBy
	askedDone   bool
	// cur is the page being cut, from the cursor curAt, its items leaving
	// left of the room; inPage the commits it names.
	cur    *code.GitAnswer
	curAt  cursor
	left   int
	inPage map[string]bool
	// stopped: the run's lines are wanted no more — the asked page is cut
	// and the memo takes none, or a page past it met a line wider than a
	// page. tooLarge is the asked page's own such line, its refusal.
	stopped  bool
	tooLarge error
}

func newBlameCut(r *read, m *memoRun, rev, path string, at cursor, base []string) *blameCut {
	c := &blameCut{r: r, m: m, rev: rev, path: path, base: base}
	c.wire, c.b64 = code.WirePath(path)
	c.begin(at)
	c.asked = c.cur
	return c
}

// page is an empty page of the blame.
func (c *blameCut) page() *code.GitAnswer {
	return &code.GitAnswer{Kind: code.GitBlame, Rev: c.rev, Blame: &code.BlameAnswer{Path: c.wire, PathB64: c.b64,
		Lines: []code.BlameLine{}, Commits: map[string]code.BlameCommit{}}}
}

// begin starts a page at the cursor at: its decoration the read's revision
// alone, and its room what the read's answer leaves once its envelope —
// every number and its cursor at their widest, its path neutralised — is
// spent.
func (c *blameCut) begin(at cursor) {
	c.r.index(c.base)
	envelope := c.page()
	envelope.Next = widestCursor('l', 1)
	envelope.Blame.StartLine, envelope.Blame.EndLine = widest, widest
	c.cur, c.curAt, c.inPage = c.page(), at, map[string]bool{}
	c.left = c.r.room(*envelope) - neutralGrowth(c.wire)
}

// take places one blamed line on the page being cut, ending the page before it
// where it does not fit — and, where the memo takes them, beginning the next
// at it.
func (c *blameCut) take(l *blamedLine) error {
	if c.stopped {
		return errStop
	}
	line, lineCost := l.line(c.path)
	if c.place(l, line, lineCost) {
		return nil
	}
	b := c.cur.Blame
	if len(b.Lines) == 0 {
		// WIDER THAN AN EMPTY PAGE: refused on the page asked, and where a
		// page past it, no page from it is cut — a read asking from it is
		// refused it.
		if c.cur == c.asked {
			c.tooLarge = fmt.Errorf("%w: line %d of %q is wider, with its commit, than one answer holds: read "+
				"the file's line itself", code.ErrSliceTooLarge, l.final, c.path)
		}
		c.stopped = true
		return errStop
	}
	c.end(cursor{letter: 'l', a: l.final})
	if c.m == nil {
		c.stopped = true
		return errStop
	}
	c.begin(cursor{letter: 'l', a: l.final})
	if !c.place(l, line, lineCost) {
		c.stopped = true
		return errStop
	}
	return nil
}

// place adds the line to the page being cut where it fits — beside its commit's
// details, the first time the page names the commit — and reports whether it
// did.
func (c *blameCut) place(l *blamedLine, line code.BlameLine, lineCost int) bool {
	b := c.cur.Blame
	cost := lineCost
	if len(b.Lines) > 0 {
		cost++ // the comma before it
	}
	var name func()
	if !c.inPage[l.commit] {
		commit, growth := l.detail.commit()
		entry := map[string]code.BlameCommit{l.commit: commit}
		cost += jsonLen(entry) - len("{}") + growth
		if len(b.Commits) > 0 {
			cost++
		}
		pushed, n := c.r.pushCost(l.commit)
		cost, name = cost+pushed, n
		if cost > c.left {
			return false
		}
		b.Commits[l.commit] = commit
		c.inPage[l.commit] = true
		name()
	} else if cost > c.left {
		return false
	}
	if len(b.Lines) == 0 {
		b.StartLine = l.final
	}
	b.Lines, b.EndLine = append(b.Lines, line), l.final
	c.left -= cost
	return true
}

// end ends the page being cut before the line next (none: the range's end) and
// stages it, keeping the asked page's decoration to give back to the read.
func (c *blameCut) end(next cursor) {
	c.cur.Next = formatCursor(next)
	c.m.put(c.curAt, *c.cur, c.left)
	if c.cur == c.asked {
		c.askedDone = true
		c.askedNamed, c.askedPushed = c.r.named, c.r.pushedBy
	}
}

// finish ends the run's last page at the range's end.
func (c *blameCut) finish() {
	if !c.stopped && len(c.cur.Blame.Lines) > 0 {
		c.end(cursor{})
	}
}

// restore gives the read the asked page's decoration back, where pages after
// it were cut with their own.
func (c *blameCut) restore() {
	if c.askedDone {
		c.r.named, c.r.pushedBy = c.askedNamed, c.askedPushed
	}
}

// blamedLine is one line as the porcelain named it: where it is now and was
// then, the commit that last changed it with that commit's details, the path
// it had there, and its text held to its bound.
type blamedLine struct {
	final, orig int
	commit      string
	detail      *blameDetail
	text        *heldText
}

// line is the line as a page answers it — its origin's path where it was
// another than path, its text described past its bound — and what it costs
// the page as a code tool sends it: its encoding, every string neutralised,
// and its text fenced.
func (l *blamedLine) line(path string) (code.BlameLine, int) {
	out := code.BlameLine{Line: l.final, Commit: l.commit, OrigLine: l.orig}
	growth := 0
	if l.detail.path != path {
		out.OrigPath, out.OrigPathB64 = code.WirePath(l.detail.path)
		growth += neutralGrowth(out.OrigPath)
	}
	text, size, g := l.text.described(code.MaxBlameLineBytes)
	out.Text, out.TextBytes = text, size
	growth += g
	if text != "" {
		growth += fenceCost[provenance.SourceRepoFile]
	}
	return out, jsonLen(out) + growth
}

// blameDetail is a commit's details as the porcelain named them the first
// time it named the commit, and the path it last named the commit under.
type blameDetail struct {
	author, email heldText
	authored      time.Time
	summary       heldText
	path          string
	// answered is the commit as a page answers it, made from the details
	// once their lines have all been read (commit), and growth what a code
	// tool's sending adds to its encoding.
	answered *code.BlameCommit
	growth   int
}

// commit is the commit as a page answers it, every field described past its
// bound, and what a code tool's sending adds to its encoding: every string
// neutralised, its subject fenced.
func (d *blameDetail) commit() (code.BlameCommit, int) {
	if d.answered != nil {
		return *d.answered, d.growth
	}
	c := code.BlameCommit{AuthoredAt: d.authored}
	var g int
	c.Author, c.AuthorBytes, g = d.author.described(code.MaxLogPersonBytes)
	d.growth += g
	c.AuthorEmail, c.AuthorEmailBytes, g = d.email.described(code.MaxLogPersonBytes)
	d.growth += g
	c.Subject, c.SubjectBytes, g = d.summary.described(code.MaxLogSubjectBytes)
	d.growth += g
	if c.Subject != "" {
		d.growth += fenceCost[provenance.SourceRepoCommit]
	}
	d.answered = &c
	// The held bytes are answered for now: only the path is read again.
	d.author, d.email, d.summary = heldText{}, heldText{}, heldText{}
	return c, d.growth
}

// The porcelain's lines, by what begins them.
const (
	blameUnknown = iota // nothing of the line read yet
	blameText           // a TAB: the line of the file
	blameToken          // its first word, until a space
	blameHeader         // a group header, past its commit's id
	blameMeta           // a commit's detail, past its key
)

// blameLineCap is the most of a header or a detail the parser holds that is
// not a field an answer may carry: a header's numbers, a date, a quoted path —
// four bytes a byte at the most, of a path no read names past
// code.MaxGitPathBytes. Past it the line does not parse.
const blameLineCap = 4*code.MaxGitPathBytes + 64

// blameStream is a blame's Stdout: its porcelain parsed line by line as it
// streams by, each blamed line handed to take whole with its commit's details.
// An error from take ends the run, as any Stdout's does; output that does not
// parse ends it with errUnparsed.
type blameStream struct {
	take func(*blamedLine) error

	state int
	// small is a line's first word, or a header's or a short detail's
	// whole; key a detail's key once its space is read, and value where its
	// value goes — nil for a detail no answer carries.
	small []byte
	key   string
	value *heldText
	text  heldText

	// pending is the line a header named, awaiting its text.
	pending  bool
	cur      blamedLine
	commits  map[string]*blameDetail
	lastSeen *blameDetail
}

func newBlameStream(take func(*blamedLine) error) *blameStream {
	s := &blameStream{take: take, commits: map[string]*blameDetail{}}
	s.text.cap = code.MaxBlameLineBytes
	return s
}

func (s *blameStream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if err := s.add(p); err != nil {
				return n, err
			}
			break
		}
		if err := s.add(p[:i]); err != nil {
			return n, err
		}
		if err := s.end(); err != nil {
			return n, err
		}
		p = p[i+1:]
	}
	return n, nil
}

// add takes b, bytes of the line in hand.
func (s *blameStream) add(b []byte) error {
	for len(b) > 0 {
		switch s.state {
		case blameUnknown:
			if b[0] == '\t' {
				if !s.pending {
					return fmt.Errorf("%w: git blame named a line with no header before it", errUnparsed)
				}
				s.state, b = blameText, b[1:]
				continue
			}
			s.state = blameToken
		case blameText:
			s.text.add(b)
			return nil
		case blameToken:
			i := bytes.IndexByte(b, ' ')
			if i < 0 {
				return s.hold(b)
			}
			if err := s.hold(b[:i]); err != nil {
				return err
			}
			b = b[i+1:]
			if err := s.token(); err != nil {
				return err
			}
		case blameHeader:
			return s.hold(b)
		case blameMeta:
			if s.value != nil {
				s.value.add(b)
				return nil
			}
			if s.key == "filename" || s.key == "author-time" {
				return s.hold(b)
			}
			return nil // a detail no answer carries: read past
		}
	}
	return nil
}

// hold keeps b in small, up to blameLineCap.
func (s *blameStream) hold(b []byte) error {
	if len(s.small)+len(b) > blameLineCap {
		return fmt.Errorf("%w: git blame wrote a line of %d bytes and more, past the %d a header or a path "+
			"is read to", errUnparsed, len(s.small)+len(b), blameLineCap)
	}
	s.small = append(s.small, b...)
	return nil
}

// token reads a line's first word, its space just read: a commit's whole id
// begins a group header, any other word is a detail's key.
func (s *blameStream) token() error {
	word := string(s.small)
	s.small = s.small[:0]
	if gitref.ValidOID(word) {
		s.state, s.cur.commit = blameHeader, word
		return nil
	}
	if !s.pending || s.lastSeen == nil {
		return fmt.Errorf("%w: git blame wrote a detail (%q) before any header", errUnparsed, truncated([]byte(word)))
	}
	s.state, s.key, s.value = blameMeta, word, nil
	d := s.lastSeen
	switch word {
	case "author":
		s.value = &d.author
	case "author-mail":
		s.value = &d.email
	case "summary":
		s.value = &d.summary
	}
	if s.value != nil {
		if s.value.n > 0 {
			return fmt.Errorf("%w: git blame named a commit's %s twice", errUnparsed, word)
		}
		// The address is held with its brackets, two bytes past its bound.
		s.value.cap = code.MaxLogPersonBytes
		switch word {
		case "author-mail":
			s.value.cap += len("<>")
		case "summary":
			s.value.cap = code.MaxLogSubjectBytes
		}
	}
	return nil
}

// end ends the line in hand at its line feed.
func (s *blameStream) end() error {
	var err error
	switch s.state {
	case blameText:
		err = s.line()
	case blameToken:
		// A WORD ALONE ON ITS LINE: a detail with no value (`boundary`),
		// which no answer carries, or a header that is all id, which is none.
		if gitref.ValidOID(string(s.small)) || !s.pending {
			err = fmt.Errorf("%w: git blame wrote %q alone on a line", errUnparsed, truncated(s.small))
		}
	case blameHeader:
		err = s.header()
	case blameMeta:
		err = s.detail()
	case blameUnknown:
		err = fmt.Errorf("%w: git blame wrote an empty line", errUnparsed)
	}
	s.state, s.small, s.key, s.value = blameUnknown, s.small[:0], "", nil
	return err
}

// header reads a group header past its id: the line in the commit and in the
// file, and on a group's first line its length.
func (s *blameStream) header() error {
	if s.pending {
		return fmt.Errorf("%w: git blame named two headers with no line between", errUnparsed)
	}
	fields := strings.Fields(string(s.small))
	var nums []int
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || strconv.Itoa(n) != f {
			return fmt.Errorf("%w: git blame wrote the header %q", errUnparsed, truncated(s.small))
		}
		nums = append(nums, n)
	}
	if len(nums) != 2 && len(nums) != 3 {
		return fmt.Errorf("%w: git blame wrote the header %q", errUnparsed, truncated(s.small))
	}
	s.cur.orig, s.cur.final = nums[0], nums[1]
	d := s.commits[s.cur.commit]
	if d == nil {
		d = &blameDetail{}
		s.commits[s.cur.commit] = d
	}
	s.pending, s.lastSeen, s.cur.detail = true, d, d
	return nil
}

// detail ends a detail line: the ones an answer carries checked and kept.
func (s *blameStream) detail() error {
	d := s.lastSeen
	switch s.key {
	case "author-time":
		t, ok := unixSeconds(s.small)
		if !ok {
			return fmt.Errorf("%w: git blame wrote the author-time %q", errUnparsed, truncated(s.small))
		}
		d.authored = t
	case "filename":
		p, ok := unquoteC(s.small)
		if !ok {
			return fmt.Errorf("%w: git blame wrote the filename %q, which is no path git quotes", errUnparsed,
				truncated(s.small))
		}
		d.path = p
	case "author-mail":
		// Git writes the address between angle brackets, which are git's
		// and no part of it.
		if h := &d.email; h.n == len(h.buf) && h.n >= 2 && h.buf[0] == '<' && h.buf[h.n-1] == '>' {
			h.buf = append(h.buf[:0], h.buf[1:h.n-1]...)
			h.n -= 2
		} else if h.n == len(h.buf) {
			return fmt.Errorf("%w: git blame wrote an author-mail %q not between brackets", errUnparsed,
				truncated(h.buf))
		} else {
			// Too long to hold: its size, less the brackets, and none of
			// its bytes, so it is described by that size alone.
			h.buf, h.n = h.buf[:0], h.n-2
		}
	case "summary":
		// GIT'S STAND-IN FOR AN EMPTY MESSAGE is the commit's id in
		// parentheses — never a message's, which could not name its own id.
		if h := &d.summary; string(h.buf) == "("+s.cur.commit+")" {
			h.reset()
		}
	}
	return nil
}

// line ends a line of the file: handed on with its header's commit.
func (s *blameStream) line() error {
	if s.cur.detail.path == "" {
		return fmt.Errorf("%w: git blame named a line of commit %s with no filename", errUnparsed, s.cur.commit)
	}
	s.cur.text = &s.text
	err := s.take(&s.cur)
	s.pending = false
	s.text.reset()
	return err
}

// close reports a stream that ended inside a line, or a header with no line
// after it, as output that does not parse.
func (s *blameStream) close() error {
	if s.state != blameUnknown || s.pending {
		return fmt.Errorf("%w: git blame's output ended inside a line", errUnparsed)
	}
	return nil
}

// unquoteC is a path as git's quote_c_style writes it after a porcelain's key:
// itself where it needed no quoting, and otherwise between double quotes with
// C's escapes — \a \b \t \n \v \f \r \" \\ — and three octal digits for any
// other byte it quotes. False for text no such writing makes.
func unquoteC(b []byte) (string, bool) {
	if len(b) == 0 || b[0] != '"' {
		return string(b), true
	}
	if len(b) < 2 || b[len(b)-1] != '"' {
		return "", false
	}
	b = b[1 : len(b)-1]
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == '"' {
			return "", false
		}
		if c != '\\' {
			out = append(out, c)
			continue
		}
		if i++; i >= len(b) {
			return "", false
		}
		switch e := b[i]; e {
		case 'a':
			out = append(out, '\a')
		case 'b':
			out = append(out, '\b')
		case 't':
			out = append(out, '\t')
		case 'n':
			out = append(out, '\n')
		case 'v':
			out = append(out, '\v')
		case 'f':
			out = append(out, '\f')
		case 'r':
			out = append(out, '\r')
		case '"', '\\':
			out = append(out, e)
		case '0', '1', '2', '3':
			if i+2 >= len(b) || b[i+1] < '0' || b[i+1] > '7' || b[i+2] < '0' || b[i+2] > '7' {
				return "", false
			}
			out = append(out, (e-'0')<<6|(b[i+1]-'0')<<3|(b[i+2]-'0'))
			i += 2
		default:
			return "", false
		}
	}
	return string(out), true
}
