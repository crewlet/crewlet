package host

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// THE RECEIVE PIPELINE'S THREE SCANS of a push — and a coding box's fourth,
// its pack's committers (committedAs), over the commits the blob scan's own
// pass told apart from the pack's other objects (packCommits), a commit at a
// time — each one git process in the push's scratch, in sequence — never one
// pool process piped into another, which would hold two slots of one lane at
// once and queue against itself.
//
// EVERY COMMIT WALK GOES BY GENERATION. A walk git makes newest first BY DATE
// stops only once every commit it still holds is older than what it is
// looking for, so a commit dated before its parent — a person's skewed
// clock, a GIT_COMMITTER_DATE — walks the whole history. Where the scratch's
// alternate has a commit-graph (the maintainer writes one first, before any
// index), a walk can stop on generation numbers instead, and three of the
// four take them: merge-base --is-ancestor does by itself, and the listings
// of what a push introduces and where a blob is are asked for in
// TOPOLOGICAL ORDER (--topo-order), which is what makes rev-list walk by
// generation — without it rev-list read every commit of the graph for a
// skewed tip, 50 ms over 120 000 commits against 2.4 ms with it on the node
// pin, git 2.56.0 (TestAPushsWalksGoByGeneration's fixture; git 2.47.3 and
// 2.48.0 walked it alike). The fourth is receive-pack's
// own connectivity check, which is git's and takes no flag of ours
// (gitproc.ReceivePackDeadline). Where there is no commit-graph yet, every
// walk is by date whatever its flags, and each is bounded by what the
// repository holds as well as by what the push sends (gitproc.HistoryWalk),
// so a skewed push into a large history is slow, never refused.

// forceChecks is every branch update a push makes, and whether each is forced:
// one `merge-base --is-ancestor <old> <new>` a branch whose old and new are
// both commits — a creation or a deletion asks nothing. stale is set for an
// old value this company holds no object for: the cache is synced at or past
// the advertisement the client was shown, so an old id it lacks is not one the
// client was shown here.
//
// RUN WITH BOUNDED PARALLELISM: up to the read lane's FREE slots at the moment
// the check starts (gitproc.Pool.Free), and at least one, so a node with idle
// cores answers a push of many branch updates at once while a loaded one asks
// them one after another, exactly as before, and a push never queues the
// node's other readers — or itself — behind its own checks. Each is about 2 ms
// a process (M2-gitproc's measured section), so a thousand updates in series
// were 2–5 s of a person's push.
func (s *Server) forceChecks(ctx context.Context, sc *cache.Scratch, objects int64, cmds []code.Command,
	todo []int) (
	forced, stale map[int]bool, err error) {

	forced, stale = map[int]bool{}, map[int]bool{}
	if len(todo) == 0 {
		return forced, stale, nil
	}
	width := max(min(s.o.Git.Free(gitproc.ClassRead), len(todo)), 1)
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		first error
		next  int
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for range width {
		wg.Go(func() {
			for {
				mu.Lock()
				if next == len(todo) || first != nil {
					mu.Unlock()
					return
				}
				i := todo[next]
				next++
				mu.Unlock()
				isForced, isStale, cerr := s.isForced(ctx, sc, objects, cmds[i].Old, cmds[i].New)
				mu.Lock()
				switch {
				case cerr != nil && first == nil:
					first = cerr
					cancel()
				case isStale:
					stale[i] = true
				case isForced:
					forced[i] = true
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if first != nil {
		return nil, nil, first
	}
	return forced, stale, nil
}

// isForced asks whether old is an ancestor of new in sc, over a repository of
// objects live objects: exit 0 is not forced, exit 1 is forced, and an old
// this company holds no object for is stale. Bounded by one read attempt and
// a walk of the history (gitproc.HistoryWalk): where the repository has no
// commit-graph yet, a new commit dated before its parent walks every commit,
// and a read's ten seconds alone refused the push into a repository of more
// than about two million.
//
// STALE ONLY WHERE THE MISSING OBJECT IS ALL GIT MET. git names an old it
// cannot parse "Not a valid commit name" whether the copy never held it or
// holds it in a pack it cannot read — there it says "inflate: …" (or another
// pack report) first, and the missing object is that damage's consequence.
// Stale tells the client the company never held its old, which is false
// there, and would hide the damage from the operator, so a run that named
// damage to anything but a derived file is the check's failure
// (code_push_unchecked, naming it). Damage to a DERIVED file — a
// multi-pack-index or commit-graph git reads around, from the scratch's
// alternate — tells nothing about the old, and leaves the update stale.
func (s *Server) isForced(ctx context.Context, sc *cache.Scratch, objects int64, old, new string) (
	forced, stale bool, err error) {

	res, err := s.runGit(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Args: []string{"merge-base", "--is-ancestor", old, new},
		Class: gitproc.ClassRead, Answers: []int{1}, Deadline: forceDeadline(objects),
	})
	switch {
	case errors.Is(err, gitproc.ErrMissingObject) && !namedStoredDamage(err):
		return false, true, nil
	case err != nil:
		return false, false, fmt.Errorf("host: is %s an ancestor of %s: %w", old, new, err)
	}
	return res.Exit == 1, false, nil
}

// namedStoredDamage reports whether a git run's failure named damage to a
// file that is not derived — a pack, its index or its reverse index — beside
// whatever else it named.
func namedStoredDamage(err error) bool {
	var d *gitproc.Damage
	return errors.As(err, &d) && errors.Is(d.Kind, gitproc.ErrCorrupt) && !d.Derived()
}

// forceDeadline bounds one force check over a repository of objects live
// objects: one read attempt, and a walk of the history.
func forceDeadline(objects int64) time.Duration {
	return gitproc.ReadDeadline + gitproc.HistoryWalk(objects)
}

// listingDeadline bounds one listing from a push's tips (introduced, pathOf)
// over a repository of objects live objects: the pack's own work, and a walk
// of the history.
func listingDeadline(packBytes, objects int64) time.Duration {
	return gitproc.PackWorkDeadline(packBytes) + gitproc.HistoryWalk(objects)
}

// errFound ends a scan at its answer: not a failure.
var errFound = errors.New("host: found")

// largestBlob is the first blob in the push's own pack over limit bytes: ONE
// `cat-file --batch-check` over the pack's ids, streamed from its index, so the
// scratch's alternate — the whole repository — is never listed, ending at the
// first offender.
//
// AND THE PACK'S COMMITS, where commits is a spool — a coding box's push,
// whose commits' committers are read next (committedAs): each line names its
// object's type, so the commits are told apart in this same pass, and
// spooled as it meets them, rather than in a second pass over every id of
// the pack. Sealed only where the pass read the whole index: one that ended
// at an offender refuses every command it leaves, so nothing of the push is
// stored and its committers are never asked about.
func (s *Server) largestBlob(ctx context.Context, sc *cache.Scratch, idxPath string, packBytes, limit int64,
	commits *packCommits) (oid string, size int64, found bool, err error) {

	_, ids, err := idxOIDs(idxPath)
	if err != nil {
		return "", 0, false, err
	}
	defer func() { _ = ids.Close() }()
	lines := gitproc.NewLines(func(line []byte) error {
		fields := bytes.Fields(line)
		switch {
		case len(fields) != 3 && commits != nil:
			// An object of the pack git could not read (`<id> missing`) may
			// be a commit, which would then go unjudged.
			return fmt.Errorf("host: git answered %q for an object of the push's own pack", line)
		case len(fields) != 3:
			return nil
		}
		if string(fields[1]) == "commit" && commits != nil {
			return commits.add(fields[0])
		}
		if string(fields[1]) != "blob" {
			return nil
		}
		n, perr := strconv.ParseInt(string(fields[2]), 10, 64)
		if perr != nil {
			return fmt.Errorf("host: git printed %q as the size of %s: %w", fields[2], fields[0], perr)
		}
		if n <= limit {
			return nil
		}
		oid, size = string(fields[0]), n
		return errFound
	})
	_, err = s.runGit(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Args: []string{"cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)", "--buffer"},
		Stdin: ids, Stdout: lines, Class: gitproc.ClassRead, Deadline: gitproc.PackWorkDeadline(packBytes),
	})
	if errors.Is(err, errFound) {
		return oid, size, true, nil
	}
	if err == nil {
		err = lines.Close()
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("host: scan the push's blobs: %w", err)
	}
	if commits != nil {
		if err := commits.seal(); err != nil {
			return "", 0, false, err
		}
	}
	return "", 0, false, nil
}

// packCommits is the ids of the commits a coding box's pack holds, in the
// order its index lists them, as the blob scan meets them (largestBlob): a
// spool in the push's scratch, one "<hex>\n" line a commit, which the
// committer scan (committedAs) reads back as its standard input — so the
// pack's trees and blobs, nearly all of its objects, are asked about once.
//
// ON DISK, NEVER IN MEMORY: a pack of code.MaxPackBytes may hold millions of
// commits, whose ids alone would be hundreds of mebibytes of a push's heap;
// on disk they are at most twice the bytes the pack's own index spends on
// them, beside the pack, and removed with the landing (remove), or with the
// scratch should that fail. Written without an fsync: nothing outlives the
// request, as nothing of the scratch does.
type packCommits struct {
	f *os.File
	w *bufio.Writer
	// n is how many commits are spooled; listed is that the blob scan read
	// the whole index, so they are every commit of the pack — a scan that
	// ended at a blob past the limit spooled only those before it.
	n      int64
	listed bool
}

// newPackCommits is an empty spool of a pack's commits, in dir, the push's
// scratch.
func newPackCommits(dir string) (*packCommits, error) {
	f, err := os.CreateTemp(dir, "commits-*")
	if err != nil {
		return nil, fmt.Errorf("host: make the spool of the push's commits: %w", err)
	}
	return &packCommits{f: f, w: bufio.NewWriterSize(f, 64<<10)}, nil
}

// add spools the commit id, forty hex digits as git printed them.
func (p *packCommits) add(id []byte) error {
	if !gitref.ValidOID(id) {
		return fmt.Errorf("host: git printed %q as the id of a commit of the push", id)
	}
	if _, err := p.w.Write(id); err != nil {
		return fmt.Errorf("host: spool the push's commits: %w", err)
	}
	if err := p.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("host: spool the push's commits: %w", err)
	}
	p.n++
	return nil
}

// seal records that every commit of the pack is spooled, and writes out the
// last of them.
func (p *packCommits) seal() error {
	if err := p.w.Flush(); err != nil {
		return fmt.Errorf("host: spool the push's commits: %w", err)
	}
	p.listed = true
	return nil
}

// every reports whether p holds every commit of the pack: false for nil, and
// for a spool whose scan did not read the whole index.
func (p *packCommits) every() bool { return p != nil && p.listed }

// input is the spool from its first line, as a git process's standard input.
func (p *packCommits) input() io.Reader {
	return io.NewSectionReader(p.f, 0, p.n*(gitref.OIDHexLen+1))
}

// remove closes and removes the spool. A failure leaves a file the scratch's
// own removal takes with it, so nothing reports one.
func (p *packCommits) remove() {
	_ = p.f.Close()
	_ = os.Remove(p.f.Name())
}

// committedAs is the commits of a coding box's pack (commits, as the blob scan
// spooled them) whose committer's address is committer — the first
// maxEngineCommits of them, in the order the pack's index lists them — and
// whether it holds more (pastBound).
//
// THE PACK, NEVER WHAT ITS UPDATES INTRODUCE. What a push stores is its pack,
// whole, and a pack carries more than the commits its updates still standing
// introduce: the objects of an update the landing refused — a force its run
// may not make, sent beside an update it may — and any object a client sent
// that no ref reaches, which receive-pack's fsck admits. Once the push lands,
// each is in the repository, held, and a later push naming it introduces
// nothing the repository lacks. So it is every commit of the pack whose
// committer is read, never the listing's (introduced).
//
// ONE COMMIT AT A TIME, IN GIT AND HERE. One `cat-file --batch` over the
// pack's commits alone — the ids the blob scan's one pass over the index told
// apart from its trees and blobs — and each commit's header read as it
// arrives (committers): the address of its committer line judged, and the
// rest of it, the message however long, passed over unread. cat-file holds
// the one commit it is printing — beside git's delta base cache
// (core.deltaBaseCacheLimit), as every reader of a pack does — and frees it
// before the next, and this holds one header line's first ten bytes and the
// address it is judging: the bound receive-pack's index-pack and fsck already
// hold the same pack to, an object at a time. A listing of commits holds the
// pack's sum: `rev-list --no-walk=unsorted --stdin --format=%H%x00%ce`, as
// this scan first was, parses every commit it is handed before it prints one
// and keeps each one's text until it has, and a box sends a commit's
// megabytes as a kilobyte of pack. MEASURED (git 2.53.0, the host's; the
// pin's rev-list keeps the same): over a pack of 1.2 MB holding 8 208
// commits, sixteen of them of 8 MiB messages
// (TestABoxPushsCommitsAreReadOneAtATime's), the rev-list peaked at 142.6 MB
// in 0.11 s and the cat-file at 14.8 MB in 0.06 s; over this repository's own
// history, 166 681 objects of which 8 570 commits, the rev-list took 0.53 s
// at 341 MB and the cat-file 0.15 s at 27.7 MB, beside the blob scan's 0.34 s
// it no longer repeats. What the text costs here is time, which the deadline
// bounds: the pack's own work (gitproc.PackWorkDeadline), which a pack whose
// commits inflate far past its bytes outruns, and is then unchecked, never
// landed.
//
// The address is git's own `%ce`: between the committer line's first `<` and
// the `>` after it, as the commit wrote it, never what a .mailmap would
// rewrite. Every committer line of a header is judged — receive-pack's fsck
// admits one — and the match FOLDS CASE over the whole address
// (bytes.EqualFold): a reader takes Engine@Seats.Invalid for the engine, and
// refusing more than the exact spelling admits no forgery. Past
// maxEngineCommits matches the scan ENDS — the push is refused whatever the
// rest are (forged) — so what one push makes the engine hold and the
// repository be asked is the bound's, never what a box sent.
func (s *Server) committedAs(ctx context.Context, sc *cache.Scratch, commits *packCommits, packBytes int64,
	committer string) (found []string, pastBound bool, err error) {

	if !commits.every() {
		return nil, false, errors.New("host: the push's commits were never listed whole, so none can be " +
			"judged: the blob scan ended before the pack's index did")
	}
	if commits.n == 0 {
		return nil, false, nil
	}
	read := &committers{want: []byte(committer), match: func(id []byte) error {
		if len(found) == maxEngineCommits {
			pastBound = true
			return errFound
		}
		found = append(found, string(id))
		return nil
	}}
	_, err = s.runGit(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Args: []string{"cat-file", "--batch", "--buffer"},
		Stdin: commits.input(), Stdout: read, Class: gitproc.ClassRead, Deadline: gitproc.PackWorkDeadline(packBytes),
	})
	if err == nil {
		err = read.close(commits.n)
	}
	switch {
	case errors.Is(err, errFound):
		return found, true, nil
	case err != nil:
		return nil, false, fmt.Errorf("host: read the committers of the push's commits: %w", err)
	}
	return found, false, nil
}

// committerField is the name a commit's header line names its committer by.
const committerField = "committer "

// maxBatchHeader bounds the line `cat-file --batch` writes before an object's
// bytes: an id, a type and a size, some sixty bytes — anything past a few
// times that is no such line.
const maxBatchHeader = 256

// committers is the Stdout of committedAs's `cat-file --batch`: a commit's
// "<id> commit <size>" line, its size in bytes and a newline, one commit after
// another. It reads each commit's header lines as they arrive, hands match the
// commit's id once a committer line's address folds to want, and passes over
// everything after the header's blank line as it arrives, holding nothing of
// it: what it keeps is the batch line in hand, the commit's id and the
// address it is judging, at most utf8.UTFMax bytes for each of want's and one
// more — past the longest address that can fold to want, so a longer one
// folds to nothing.
type committers struct {
	want  []byte
	match func(id []byte) error

	head    []byte // the batch line in hand
	id      []byte // the commit in hand
	left    int64  // its bytes still to come
	in, end bool   // inside its bytes; past them, its newline next
	read    int64  // the commits read to their newline

	// What is read of the commit in hand's header: whether the header is
	// still being read, whether the line in hand spells committerField so
	// far (lead) and in whole (field), whether the commit matched, how far
	// into its field name the line is (col), where on a committer line the
	// address is (mail: 0 before its '<', 1 inside it, 2 past its '>'), and
	// the address so far.
	header, lead, field, matched bool
	col, mail                    int
	addr                         []byte
}

func (c *committers) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		switch {
		case c.end:
			if p[0] != '\n' {
				return n, fmt.Errorf("host: cat-file wrote %q after commit %s, where its newline goes", p[0], c.id)
			}
			p, c.end = p[1:], false
			c.read++
		case c.in:
			take := min(c.left, int64(len(p)))
			if c.header {
				if err := c.scan(p[:take]); err != nil {
					return n, err
				}
			}
			p, c.left = p[take:], c.left-take
			if c.left == 0 {
				c.in, c.end = false, true
			}
		default:
			i := bytes.IndexByte(p, '\n')
			line := p
			if i >= 0 {
				line = p[:i]
			}
			if len(c.head)+len(line) > maxBatchHeader {
				return n, fmt.Errorf("host: cat-file wrote a line of more than %d bytes where a commit's id, type "+
					"and size go", maxBatchHeader)
			}
			c.head = append(c.head, line...)
			if i < 0 {
				return n, nil
			}
			p = p[i+1:]
			if err := c.begin(); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

// begin takes the batch line in hand as the commit whose bytes follow it.
func (c *committers) begin() error {
	fields := bytes.Fields(c.head)
	if len(fields) != 3 || !gitref.ValidOID(fields[0]) || string(fields[1]) != "commit" {
		return fmt.Errorf("host: cat-file wrote %q where a commit's id, type and size go", c.head)
	}
	size, err := strconv.ParseInt(string(fields[2]), 10, 64)
	switch {
	case err != nil:
		return fmt.Errorf("host: cat-file wrote %q as the size of commit %s: %w", fields[2], fields[0], err)
	case size < 0:
		return fmt.Errorf("host: cat-file wrote %d as the size of commit %s", size, fields[0])
	}
	c.id = append(c.id[:0], fields[0]...)
	c.head = c.head[:0]
	c.left, c.in, c.end = size, size > 0, size == 0
	c.header, c.lead, c.field, c.matched, c.col, c.mail, c.addr = true, false, false, false, 0, 0, c.addr[:0]
	return nil
}

// scan reads b, the next bytes of the commit in hand, while they are its
// header: a line at a time, a committer line's address judged, until the
// blank line its message follows.
func (c *committers) scan(b []byte) error {
	for _, x := range b {
		switch {
		case !c.header:
			return nil
		case x == '\n' && c.col == 0:
			c.header = false
		case x == '\n':
			c.lead, c.field, c.col, c.mail, c.addr = false, false, 0, 0, c.addr[:0]
		case c.col < len(committerField):
			c.lead = (c.col == 0 || c.lead) && x == committerField[c.col]
			c.field = c.lead && c.col == len(committerField)-1
			c.col++
		case c.field:
			if err := c.address(x); err != nil {
				return err
			}
		}
	}
	return nil
}

// address reads x, the next byte of a committer line past its field name.
func (c *committers) address(x byte) error {
	switch {
	case c.mail == 0 && x == '<':
		c.mail = 1
	case c.mail != 1:
	case x != '>':
		if len(c.addr) <= utf8.UTFMax*len(c.want) {
			c.addr = append(c.addr, x)
		}
	default:
		c.mail = 2
		if !c.matched && bytes.EqualFold(c.addr, c.want) {
			c.matched = true
			return c.match(c.id)
		}
	}
	return nil
}

// close reports output that ended inside a commit, or that held fewer
// commits than want, the number the scan was handed: an answer for part of
// the pack is no answer.
func (c *committers) close(want int64) error {
	switch {
	case c.in || c.end || len(c.head) > 0:
		return errors.New("host: cat-file's output ended inside a commit")
	case c.read != want:
		return fmt.Errorf("host: cat-file printed %d of the push's %d commits", c.read, want)
	}
	return nil
}

// pathOf is the path a blob of the push is reached by — the first `rev-list
// --objects` names it at from the ids the push sent (news: every command's new
// id, refused or not, since the pack carries the objects of all of them), past
// what the repository already holds — or "(unreachable in this push)" for a
// blob no new tree names. Walked in topological order, as introduced is.
func (s *Server) pathOf(ctx context.Context, sc *cache.Scratch, objects int64, news []string, oid string,
	packBytes int64) string {

	path := "(unreachable in this push)"
	lines := gitproc.NewLines(func(line []byte) error {
		id, p, ok := bytes.Cut(line, []byte{' '})
		if ok && string(id) == oid {
			path = string(p)
			return errFound
		}
		return nil
	})
	_, err := s.runGit(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Args: append(append([]string{"rev-list", "--objects", "--topo-order"}, news...), "--not", "--alternate-refs"),
		Stdout: lines, Class: gitproc.ClassRead, Deadline: listingDeadline(packBytes, objects),
	})
	if err != nil && !errors.Is(err, errFound) {
		s.log.Warn("code_push_blob_path", "dir", sc.Dir, "blob", oid, "error", err)
	}
	return path
}

// introduced is the commits the push introduces — reachable from its new
// values and from no ref the repository holds — the first code.MaxIntroduced
// of them in TOPOLOGICAL order, and how many there are.
//
// The order is what decides which ids a push row keeps when a push brings
// more than code.MaxIntroduced (an import, a long-lived branch landing), and
// topological order keeps the right ones: no commit is listed before every
// child of it the push brings, and one line of history is listed whole before
// another, so the row keeps the commits NEAREST THE PUSHED TIPS, each tip
// first. Date order — what rev-list gives without the flag — kept the newest
// by their stated dates, which a skewed clock decides: a tip dated before
// the thousand commits under it was cut from its own row. Whoever reads the
// row — `crewlet code status` and the code reads list it, IntroducedCount
// says how many more there are — reads the commits a push brought nearest to
// where it put the refs.
func (s *Server) introduced(ctx context.Context, sc *cache.Scratch, objects int64, news []string,
	packBytes int64) ([]string, int, error) {

	var (
		ids   []string
		count int
	)
	lines := gitproc.NewLines(func(line []byte) error {
		if count < code.MaxIntroduced {
			ids = append(ids, string(line))
		}
		count++
		return nil
	})
	_, err := s.runGit(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Args: append(append([]string{"rev-list", "--topo-order"}, news...), "--not", "--alternate-refs"),
		Stdout: lines, Class: gitproc.ClassRead, Deadline: listingDeadline(packBytes, objects),
	})
	if err == nil {
		err = lines.Close()
	}
	if err != nil {
		return nil, 0, fmt.Errorf("host: list the commits the push introduces: %w", err)
	}
	return ids, count, nil
}
