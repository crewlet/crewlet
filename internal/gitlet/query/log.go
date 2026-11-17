package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A LOG is ONE `git log` over the whole id of the commit its history is read
// from — `-z --topo-order --no-mailmap --no-color --no-notes`, a format whose
// every field ends in a NUL, the window its cursor, Since and Until name, the
// commit — and, where the read names a path, that path as the one argument
// after `--`, a LITERAL pathspec (`:(literal)<path>`): git reads every argument
// after `--` as a path and never as an option, and the literal magic makes
// every byte of it the path's own, so a path that begins with `-` or `:`, or
// spells `*`, is read as itself. NEVER ON `log --stdin`, whose paths are read
// a line at a time with a carriage return taken off a line's end: a path
// holding a line feed could not be named there, and one ending in a carriage
// return would be answered another path's history. The copy's changed-path
// filters serve a path given either way.
//
// --topo-order walks by generation where the copy holds a commit-graph — every
// copy its maintainer has written one for, and a copy serving an installed
// published chain always — every child before its parents, the order a
// history is read in. Every author and committer is named as its commit
// states it: `%an`, `%ae`, `%cn` and `%ce` are no `.mailmap`'s (git maps only
// `%aN`, `%aE`, `%cN` and `%cE`, measured on 2.56.0 against a `.mailmap` at
// HEAD, which a bare repository reads), and --no-mailmap holds that whatever
// log.mailmap or a later git makes of them —
// a `.mailmap` at some branch would make a page a function of a file at a ref
// rather than of the commits it names alone.
//
// # The framing: a record no message can forge
//
// Every field of the format ends in a NUL — `%H %P %an %ae %cn %ce %at %ct %s`,
// and the whole message (`%B`) where a Grep is set — and `-z` ends each record
// with one more, so a record is exactly its fields, each read to its NUL, and
// one empty field after them. A message may hold any byte but NUL, so a
// separator of any other byte let a pushed message forge a record git never
// listed: the `%x01` the first draft named would have read a message holding
// `\x01` and a record's worth of fields as a commit of its making. NUL is no
// byte a field can hold: git keeps a message and an ident as C strings, and a
// copy holds no commit Gitlet's own check did not admit — a push's
// pack is indexed by receive-pack under receive.fsckObjects and an engine
// write's by `index-pack --strict`, each the relay's fsck, which refuses a NUL
// in a commit's header (NUL_IN_HEADER) and, strict, in its message
// (NUL_IN_COMMIT). So the parser reads EXACTLY each record's declared fields,
// each held to its grammar — a whole id, whole ids between spaces, digits —
// and one more empty field, and a record that does not parse fails the page,
// logged `code_git_unparsed`: never answered as a commit
// (TestALogCannotBeForgedByAMessage).
//
// # The page
//
// A page is the commits of the walk from its cursor — `s:<n>`, the n commits
// of the walk before it, which git skips (`--skip=<n>`) — as many as the
// read's Limit and as many as the answer's bytes hold: each commit costed as
// a code tool sends it, its subject fenced and every string neutralised
// (page.go), with the push that introduced it, so the page ends BEFORE the
// commit that would take it past the bound. The walk says where a page ends,
// and the run is ended there (errStop), so a page costs the commits it covers.
// With no Grep that is the commit past its last, the Limit-and-first record —
// one record of look-ahead, which costs one record and keeps Complete exact
// (a page that holds the history's last commit says so itself). With a
// Grep it is the Limit-th match itself, and never a look for one more: past
// it every later message is streamed through the matcher only to learn
// whether a next page holds anything, and where the literal is rare that is
// the history's whole remainder or the read's whole budget of git — which
// then cuts the page, so it is neither memoized nor spared to the next ask,
// for the same commits the walk had in hand at once. Such a page is never
// Complete; the page after it answers the rest, an empty complete one where
// nothing more matches. A field git leaves unbounded is described past its
// bound, never cut (gitlet.MaxLogSubjectBytes and its kin), so the widest
// commit is a few KiB and always fits a page.
//
// A PAGE THE READ'S BUDGET OF GIT ENDS — or its answer falling due — carries
// what it found, its Scanned and its Next: never an error, and never a claim
// that nothing more matches, since a Grep that matches little in a long
// history scans on until the budget ends the page. Only a page that found
// nothing — whose walk did not reach the end of a commit in the time it had —
// is no answer: too large where it ran the read's whole budget (narrow it),
// unready where its answer fell due, which another Gitlet node answers.
//
// GREP is a literal matched here, never by git, over each message as it
// streams by (literal.go), so a message of any length is searched whole in
// memory bounded by the literal, and nothing grep-shaped reaches git.
//
// UNTIL is git's --min-age, the last whole second before it: a filter of each
// commit, not a stop. --skip counts only the commits git outputs, so the
// `s:<n>` cursor stays the walk's.
//
// PUSHED BY keeps a commit the Grep kept whose introducing push, in the copy's
// index of pushes, holds that seat; one the index cannot judge is counted in
// Unjudged. Such a page is the index's, so it is never memoized.
//
// THE MEMO keeps a page the walk itself ended — at its Limit, its bytes or
// the history's end — and never one the budget or its due instant cut short,
// which is a function of the machine and the moment rather than of the ids
// and the arguments its key names.

// logFormat and logFormatBody are the two formats a log runs: every field
// NUL-terminated, the whole message last where a Grep needs it.
const (
	logFormat     = "--format=%H%x00%P%x00%an%x00%ae%x00%cn%x00%ce%x00%at%x00%ct%x00%s%x00"
	logFormatBody = "--format=%H%x00%P%x00%an%x00%ae%x00%cn%x00%ce%x00%at%x00%ct%x00%s%x00%B%x00"
)

// The fields of a record, in their order; logBody is carried where a Grep is
// set.
const (
	logOID = iota
	logParents
	logAuthor
	logEmail
	logCommitter
	logCommitterEmail
	logAuthored
	logCommitted
	logSubject
	logBody
)

// logLiteral is the pathspec magic a log's path is given: every byte after it
// is the path's own.
const logLiteral = ":(literal)"

// logLimitOf is how many commits a page of q lists at most.
func logLimitOf(q *gitlet.LogQuery) int {
	if q.Limit == 0 {
		return gitlet.DefaultLogLimit
	}
	return q.Limit
}

// logWindow is the arguments that window a log: since as git's --max-age (the
// unix second the walk stops before) and until as its --min-age (the last
// second a commit kept is dated at), each formatted by the server from a time
// the query validated, and the commits a cursor's page skips.
func logWindow(since, until time.Time, skip int) []string {
	var out []string
	if !since.IsZero() {
		out = append(out, "--max-age="+strconv.FormatInt(sinceSecond(since), 10))
	}
	if !until.IsZero() {
		out = append(out, "--min-age="+strconv.FormatInt(untilSecond(until), 10))
	}
	if skip > 0 {
		out = append(out, "--skip="+strconv.Itoa(skip))
	}
	return out
}

// sinceSecond is the first whole second at or after since: git keeps a commit
// whose date is at or after its --max-age, and a commit's date is whole
// seconds, so one dated in the second since falls inside of was committed
// before since and is not kept. The memo keys a log by it, as two Sinces
// inside one second window the same walk.
func sinceSecond(since time.Time) int64 {
	s := since.Unix()
	if since.Nanosecond() > 0 {
		s++
	}
	return s
}

// untilSecond is the last whole second before until: git's --min-age is
// inclusive and Until is not. Validate holds until after the epoch, so this
// is never the -1 git reads as no bound.
func untilSecond(until time.Time) int64 { return sinceSecond(until) - 1 }

// errUnparsed is git output that does not parse as the format asked: a page
// that meets one fails, and is never answered.
var errUnparsed = errors.New("query: git answered output that does not parse as asked")

// logRead answers a log read: one page, memoized where m takes it and the walk
// ended it.
func (r *read) logRead(ctx context.Context, objs found, at cursor, m *memoRun) (gitlet.GitAnswer, error) {
	q := r.q.Log
	rev := objs.revs[q.Rev]
	if rev.peeledKind != "commit" {
		return gitlet.GitAnswer{}, fmt.Errorf("%w: log.rev: %s names a %s; a log walks a commit's history",
			gitlet.ErrInvalid, q.Rev, rev.peeledKind)
	}
	path, _ := gitlet.GitPath("log.path", q.Path, q.PathB64, true)
	wire, wireB64 := gitlet.WirePath(path)
	a := gitlet.GitAnswer{Kind: gitlet.GitLog, Rev: rev.peeled,
		Log: &gitlet.LogAnswer{Path: wire, PathB64: wireB64, Commits: []gitlet.LogCommit{}}}
	envelope := gitlet.GitAnswer{Kind: gitlet.GitLog, Rev: rev.peeled, Next: widestCursor('s', 1),
		Log: &gitlet.LogAnswer{Path: wire, PathB64: wireB64, Commits: []gitlet.LogCommit{}, Scanned: widest,
			Complete: true}}
	if q.PushedBy != "" {
		most := widest
		envelope.Log.Unjudged = &most
	}
	p := &logPage{r: r, a: a.Log, limit: logLimitOf(q), left: r.room(envelope) - neutralGrowth(wire),
		filtered: q.Grep != "" || q.PushedBy != "", seat: q.PushedBy}
	format := logFormat
	var grep *literal
	if q.Grep != "" {
		format, grep = logFormatBody, newLiteral(q.Grep)
	}
	stream := newLogStream(grep, p.take)

	skip := at.a // the first page's cursor is the zero one: skip none
	tail := append(logWindow(q.Since, q.Until, skip), rev.peeled)
	if path != "" {
		tail = append(tail, "--", logLiteral+path)
	}
	deadline, err := r.budget()
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   append([]string{"log", "-z", "--topo-order", "--no-mailmap", "--no-color", "--no-notes", format}, tail...),
		Config: r.s.o.Cache.ServeConfig(), Stdout: stream, Class: gitproc.ClassRead, Deadline: deadline,
	})
	cut := false
	switch {
	case res.Damage != nil:
	case errors.Is(err, errUnparsed):
		r.charge(res)
		return gitlet.GitAnswer{}, r.unparsed(ctx, "log", err)
	case p.ended:
		err = nil // the page ended itself, and ended the walk there
	case err == nil:
		if cerr := stream.close(); cerr != nil {
			r.charge(res)
			return gitlet.GitAnswer{}, r.unparsed(ctx, "log", cerr)
		}
		a.Log.Complete = true
	case p.scanned > 0 && (gitlet.AnswerDue(ctx) || (errors.Is(err, gitproc.ErrDeadline) && r.metered)):
		// CUT SHORT, with commits read: a page of what it found, whose
		// Next goes on from the last of them.
		r.charge(res)
		cut, err = true, nil
	}
	narrow := fmt.Errorf("%w: the history of %s did not reach its first commit in the read's %s of git: "+
		"narrow it with a path or a later since", gitlet.ErrSliceTooLarge, rev.peeled, r.s.gitBudget)
	if !q.Until.IsZero() {
		// A window far back may spend the budget passing over newer commits,
		// which a later since does not help.
		narrow = fmt.Errorf("%w: the history of %s before %s did not reach a commit in the read's %s of git: "+
			"narrow it with a path or a since", gitlet.ErrSliceTooLarge, rev.peeled,
			q.Until.UTC().Format(time.RFC3339), r.s.gitBudget)
	}
	if !cut {
		if err = r.failed(ctx, "history walk", res, err, narrow); err != nil {
			return gitlet.GitAnswer{}, err
		}
	}
	a.Log.Scanned = p.scanned
	if q.PushedBy != "" {
		n := p.unjudged
		a.Log.Unjudged = &n
	}
	if !a.Log.Complete {
		a.Next = formatCursor(cursor{letter: 's', a: skip + p.scanned})
	}
	if !cut {
		// A PushedBy read reaches neither: memoizable said no, so m is nil.
		m.put(at, a, p.left)
		m.place(gitlet.GitLog, same(memoTail{}))
	}
	return a, nil
}

// unparsed is what a read whose git answered output that does not parse is
// answered, logged for the operator: a bug in the reader or a git that writes
// what this build does not read, never an answer about the repository.
func (r *read) unparsed(ctx context.Context, kind string, err error) error {
	log.ErrorContext(ctx, "code_git_unparsed", "node", r.s.o.NodeID, "repo", r.q.RepoID, "kind", kind,
		"error", err)
	return fmt.Errorf("query: the %s of repository %s: %w", kind, r.q.RepoID, err)
}

// logPage is the page a log cuts from its walk's records, as they arrive.
type logPage struct {
	r     *read
	a     *gitlet.LogAnswer
	limit int
	left  int
	// filtered is that a Grep or a PushedBy keeps the page's records, so the
	// page ends at its Limit-th kept record rather than at a record past it
	// (the doc's "# The page").
	filtered bool
	// seat is the PushedBy, "" for none, and unjudged how many records the
	// Grep kept whose push the index could not name.
	seat     string
	unjudged int
	// scanned is how many records of the walk the page covers so far, and
	// ended that the page found its own end (the record past its last, or a
	// filtered page's Limit-th kept record) and ended the walk there.
	scanned int
	ended   bool
}

// take places one record of the walk on the page, passes it (unmatched, pushed by
// another seat, or counted as unjudged), or ends the page before it with errStop;
// a filtered page ends at its Limit-th kept record.
func (p *logPage) take(rec *logRecord) error {
	// The seat after the Grep, so Unjudged counts only commits every other
	// narrowing kept.
	if rec.matched && p.seat != "" {
		switch by, judged := p.r.introducedBy(rec.oid); {
		case !judged:
			p.unjudged++
			rec.matched = false
		case by.Principal.Seat != p.seat:
			rec.matched = false
		}
	}
	if !rec.matched {
		p.scanned++
		return nil
	}
	if len(p.a.Commits) == p.limit {
		p.ended = true
		return errStop
	}
	c, growth := rec.commit()
	cost := jsonLen(c) + growth
	if len(p.a.Commits) > 0 {
		cost++ // the comma before it
	}
	pushed, name := p.r.pushCost(c.OID)
	pushed += p.r.tagCost(c.OID)
	if cost+pushed > p.left {
		if len(p.a.Commits) == 0 {
			// The widest commit is a few KiB, its every field bounded:
			// one past an empty page is the bounds' or the page's bug.
			return fmt.Errorf("%w: commit %s encodes to %d bytes, past the %d an empty page leaves",
				errUnparsed, c.OID, cost+pushed, p.left)
		}
		p.ended = true
		return errStop
	}
	name()
	p.a.Commits = append(p.a.Commits, c)
	p.left -= cost + pushed
	p.scanned++
	if p.filtered && len(p.a.Commits) == p.limit {
		p.ended = true
		return errStop
	}
	return nil
}

// pushCost is what naming commit id on a page adds to its answer's stamps — the
// push that introduced it, where the copy's index holds one the read has not
// named yet — and name, which names it once the page takes the commit: a
// commit a page leaves off adds nothing to the stamps of the page that does
// not hold it.
func (r *read) pushCost(id string) (int, func()) {
	for _, named := range r.named {
		if named == id {
			return 0, func() {}
		}
	}
	name := func() { r.decorate(id) }
	if !r.prov.Indexed() {
		return 0, name
	}
	by, ok := r.prov.Commit(id)
	if !ok {
		return 0, name
	}
	entry := map[string]gitlet.PushedBy{id: by}
	if len(r.pushedBy) == 0 {
		// The first entry brings the field itself, and its comma.
		return jsonLen(gitlet.GitAnswer{PushedBy: entry}) - jsonLen(gitlet.GitAnswer{}), name
	}
	return jsonLen(entry) - len("{}") + len(","), name
}

// introducedBy is the push that introduced commit id as the copy's index names
// it, and whether it names one. A node with no readable index judges nothing
// rather than refusing: the index decorates, and no read waits on it.
func (r *read) introducedBy(id string) (gitlet.PushedBy, bool) {
	if !r.prov.Indexed() {
		return gitlet.PushedBy{}, false
	}
	return r.prov.Commit(id)
}

// logRecord is one commit as the walk wrote it, its fields read and held to
// their bounds.
type logRecord struct {
	oid            string
	parents        parentList
	author         heldText
	email          heldText
	committer      heldText
	committerEmail heldText
	authored       time.Time
	// committed is the commit's own date; subject its message's first
	// paragraph as %s joins it; matched whether the Grep kept it — every
	// record, where there is none.
	committed time.Time
	subject   heldText
	matched   bool
}

// commit is rec as a page answers it, every field git leaves unbounded
// described past its bound, and what a code tool's sending adds to its
// encoding: every string neutralised, and its subject fenced.
func (rec *logRecord) commit() (gitlet.LogCommit, int) {
	c := gitlet.LogCommit{OID: rec.oid, AuthoredAt: rec.authored, CommittedAt: rec.committed}
	if rec.parents.count <= gitlet.MaxLogParents {
		c.Parents = rec.parents.ids
	} else {
		c.ParentCount = rec.parents.count
	}
	var growth, g int
	c.Author, c.AuthorBytes, g = rec.author.described(gitlet.MaxLogPersonBytes)
	growth += g
	c.AuthorEmail, c.AuthorEmailBytes, g = rec.email.described(gitlet.MaxLogPersonBytes)
	growth += g
	c.Committer, c.CommitterBytes, g = rec.committer.described(gitlet.MaxLogPersonBytes)
	growth += g
	c.CommitterEmail, c.CommitterEmailBytes, g = rec.committerEmail.described(gitlet.MaxLogPersonBytes)
	growth += g
	c.EngineCommitted = engineCommitted(&rec.committer, &rec.committerEmail)
	c.Subject, c.SubjectBytes, g = rec.subject.described(gitlet.MaxLogSubjectBytes)
	growth += g
	if c.Subject != "" {
		growth += fenceCost[provenance.SourceRepoCommit]
	}
	return c, growth
}

// engineCommitted is the engine's mark for a committer name at email; a field
// held past its cap is never the convention, its bytes not all here.
func engineCommitted(name, email *heldText) *bool {
	engine := !name.long() && !email.long() && gitlet.EngineCommitted(string(name.buf), string(email.buf))
	return &engine
}

// heldText is a field's bytes up to a cap, and its whole length past it: a
// field git leaves unbounded is held no further than the most of it any
// answer could carry.
type heldText struct {
	buf []byte
	n   int
	cap int
}

// add takes the field's next bytes.
func (h *heldText) add(b []byte) {
	h.n += len(b)
	if room := h.cap - len(h.buf); room > 0 {
		h.buf = append(h.buf, b[:min(room, len(b))]...)
	}
}

// long reports a field that ran past its cap: its bytes are not all held.
func (h *heldText) long() bool { return h.n > len(h.buf) }

// reset empties the field for the next record, keeping its cap.
func (h *heldText) reset() { h.buf, h.n = h.buf[:0], 0 }

// described is the field as an answer carries it — its text where the field
// is held whole and its encoding, neutralised, is within bound, and otherwise
// none and its size in bytes — and what neutralising the text adds to its
// encoding.
func (h *heldText) described(bound int) (text string, size, growth int) {
	if h.long() {
		return "", h.n, 0
	}
	s := string(h.buf)
	if cost := neutralCost(s); cost > bound {
		return "", h.n, 0
	} else if s != "" {
		return s, 0, cost - textCost(h.buf)
	}
	return "", 0, 0
}

// parentList reads %P as it streams by: whole ids between single spaces, the
// first MaxLogParents of them kept and every one counted, so an octopus merge
// of any width costs a bounded field.
type parentList struct {
	ids   []string
	count int
	cur   []byte
	bad   bool
}

func (p *parentList) add(b []byte) {
	for _, c := range b {
		switch {
		case p.bad:
			return
		case c == ' ':
			p.next()
		case len(p.cur) < gitref.OIDHexLen && (('0' <= c && c <= '9') || ('a' <= c && c <= 'f')):
			p.cur = append(p.cur, c)
		default:
			p.bad = true
		}
	}
}

// next ends the id in hand: one whole id, or the list is no list of ids.
func (p *parentList) next() {
	if len(p.cur) != gitref.OIDHexLen {
		p.bad = true
		return
	}
	p.count++
	if len(p.ids) < gitlet.MaxLogParents {
		p.ids = append(p.ids, string(p.cur))
	}
	p.cur = p.cur[:0]
}

// end ends the field: true where it was whole ids between single spaces, or
// none at all — a root commit's.
func (p *parentList) end() bool {
	if len(p.cur) > 0 || p.count > 0 {
		p.next()
	}
	return !p.bad
}

func (p *parentList) reset() { p.ids, p.count, p.cur, p.bad = nil, 0, p.cur[:0], false }

// logStream is a log's Stdout: its walk's records parsed as they stream by,
// each handed to take whole — never more of one held than its bounded fields.
// An error from take ends the run, as any Stdout's does; a record that does
// not parse ends it with errUnparsed.
type logStream struct {
	grep *literal
	// last is the record's last field — its subject, or its message where a
	// Grep reads one — after which one empty field ends the record.
	last int
	take func(*logRecord) error

	// field is which field the next byte is of — last+1 being the empty one
	// that ends a record — and fieldBytes how many of its bytes have
	// arrived, so a byte where none belongs, and the stream's end inside a
	// record, are both seen.
	field      int
	fieldBytes int
	rec        logRecord
	// small holds a field read whole — an id, a date — up to smallField
	// bytes, and one past it to tell that it ran on.
	small []byte
}

// smallField is the longest an id or a date may be: an id's forty digits,
// which no date's digits reach. A field past it does not parse.
const smallField = gitref.OIDHexLen

func newLogStream(grep *literal, take func(*logRecord) error) *logStream {
	s := &logStream{grep: grep, last: logSubject, take: take}
	if grep != nil {
		s.last = logBody
	}
	s.rec.author.cap, s.rec.email.cap = gitlet.MaxLogPersonBytes, gitlet.MaxLogPersonBytes
	s.rec.committer.cap, s.rec.committerEmail.cap = gitlet.MaxLogPersonBytes, gitlet.MaxLogPersonBytes
	s.rec.subject.cap = gitlet.MaxLogSubjectBytes
	return s
}

func (s *logStream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, 0)
		if i < 0 {
			s.add(p)
			break
		}
		s.add(p[:i])
		if err := s.end(); err != nil {
			return n, err
		}
		p = p[i+1:]
	}
	return n, nil
}

// add routes b, bytes of the field in hand, to where that field is read.
func (s *logStream) add(b []byte) {
	if len(b) == 0 {
		return
	}
	s.fieldBytes += len(b)
	if s.field > s.last {
		return // the record's end, which holds no byte: end refuses it
	}
	switch s.field {
	case logOID, logAuthored, logCommitted:
		if room := smallField + 1 - len(s.small); room > 0 {
			s.small = append(s.small, b[:min(len(b), room)]...)
		}
	case logParents:
		s.rec.parents.add(b)
	case logAuthor:
		s.rec.author.add(b)
	case logEmail:
		s.rec.email.add(b)
	case logCommitter:
		s.rec.committer.add(b)
	case logCommitterEmail:
		s.rec.committerEmail.add(b)
	case logSubject:
		s.rec.subject.add(b)
	case logBody:
		s.grep.write(b)
	}
}

// end ends the field in hand at its NUL: held to its grammar — and, at the
// record's empty last field, the record handed on.
func (s *logStream) end() error {
	if s.field > s.last {
		if s.fieldBytes > 0 {
			return fmt.Errorf("%w: a record of git log: its fields are followed by more than the record's end",
				errUnparsed)
		}
		if s.grep == nil {
			s.rec.matched = true
		}
		err := s.take(&s.rec)
		s.next()
		return err
	}
	var bad string
	switch s.field {
	case logOID:
		if s.rec.oid = string(s.small); !gitref.ValidOID(s.rec.oid) {
			bad = fmt.Sprintf("%q is no commit id", truncated(s.small))
		}
	case logParents:
		if !s.rec.parents.end() {
			bad = "its parents are no list of commit ids"
		}
	case logAuthored, logCommitted:
		t, ok := unixSeconds(s.small)
		if !ok {
			bad = fmt.Sprintf("%q is no date", truncated(s.small))
		}
		if s.field == logAuthored {
			s.rec.authored = t
		} else {
			s.rec.committed = t
		}
	case logBody:
		s.rec.matched = s.grep.end()
	}
	s.small, s.fieldBytes = s.small[:0], 0
	if bad != "" {
		return fmt.Errorf("%w: a record of git log: %s", errUnparsed, bad)
	}
	s.field++
	return nil
}

// next readies the stream for the next record.
func (s *logStream) next() {
	s.field, s.fieldBytes, s.small = 0, 0, s.small[:0]
	s.rec.oid, s.rec.matched = "", false
	s.rec.parents.reset()
	s.rec.author.reset()
	s.rec.email.reset()
	s.rec.committer.reset()
	s.rec.committerEmail.reset()
	s.rec.subject.reset()
}

// close reports a stream that ended inside a record — git ends every record
// it writes — as output that does not parse.
func (s *logStream) close() error {
	if s.field != 0 || s.fieldBytes != 0 {
		return fmt.Errorf("%w: git log's output ended inside a record", errUnparsed)
	}
	return nil
}

// unixSeconds is a date git wrote as its seconds since the epoch, in UTC.
func unixSeconds(digits []byte) (time.Time, bool) {
	if len(digits) == 0 || len(digits) > 19 {
		return time.Time{}, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return time.Time{}, false
		}
	}
	n, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0).UTC(), true
}
