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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A push's scans — three, and a coding box's fourth, its pack's committers
// (committedAs) — each run as one git process in the push's scratch, in
// sequence: never one pool process piped into another, which would hold two
// slots of one lane at once and queue against itself. Every commit walk asked
// here goes by generation where there is a commit-graph (merge-base
// --is-ancestor by itself, the listings by --topo-order) and is bounded by the
// history as well as the push (gitproc.HistoryWalk), so a skewed push is slow,
// never refused (package doc, "A push's walks go by generation, and are
// bounded by the history").

// forceChecks is every branch update a push makes and whether each is forced:
// one `merge-base --is-ancestor <old> <new>` per update whose old and new are
// both commits (a creation or deletion asks nothing). stale marks an old this
// company holds no object for: the cache is synced at or past the
// advertisement the client was shown, so the client was not shown it here.
//
// Its width is the read lane's free slots when it starts (gitproc.Pool.Free),
// at least one: idle cores answer many updates at once, and a push never
// queues the node's other readers, or itself, behind its own checks. A check
// is about 2 ms a process (M2-gitproc), so thousands in series cost seconds.
func (s *Server) forceChecks(ctx context.Context, sc *cache.Scratch, objects int64, cmds []gitlet.Command,
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
// objects live objects: exit 0 is not forced, exit 1 forced, and an old this
// company holds no object for is stale. Bounded by forceDeadline: with no
// commit-graph, a commit dated before its parent walks every commit.
//
// STALE ONLY WHERE THE MISSING OBJECT IS ALL GIT MET: git says "Not a valid
// commit name" too for an old in a pack it cannot read, after a damage report.
// Stale would tell the client the company never held it and hide the damage,
// so a run naming damage to a non-derived file fails the check
// (code_push_unchecked); damage to a derived file (multi-pack-index,
// commit-graph, read around) says nothing about the old, which stays stale.
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
// objects: one local read (gitproc.ReadDeadline), and a walk of the history.
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
// `cat-file --batch-check` over the ids streamed from the pack's index, so the
// scratch's alternate (the whole repository) is never listed.
//
// Where commits is non-nil (a coding box's push), the same pass spools the
// pack's commits for committedAs, sparing a second pass over every id. It
// seals them only once the whole index is read: a scan that ended at an
// offender refuses the push, whose committers are then never asked about.
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
			// An object of the pack git could not read (`<id> missing`) may be a
			// commit, which would then go unjudged.
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

// packCommits spools the ids of a coding box's pack's commits, one "<hex>\n"
// line each in index order, written by the blob scan (largestBlob) and read
// back as committedAs's standard input, so the pack's trees and blobs are
// asked about once.
//
// ON DISK, NEVER IN MEMORY: a pack of gitlet.MaxPackBytes may hold millions of
// commits; on disk they take at most twice what the pack's index spends on
// them. It is cache.Spool's file, never this package's: only the cache's own
// steps write the cache's volume (cache's TestNothingButSyncWritesACache). It
// has no name, so the kernel frees it on close or process exit, and it is
// never fsynced: nothing outlives the request.
type packCommits struct {
	f *os.File
	w *bufio.Writer
	// n is how many commits are spooled; listed, that the blob scan read the
	// whole index, so they are every commit of the pack.
	n      int64
	listed bool
}

// newPackCommits is an empty spool of a pack's commits, a spool of c's.
func newPackCommits(c *cache.Cache) (*packCommits, error) {
	f, err := c.Spool()
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

// close closes the spool, which frees it: it has no name to remove. A failed
// close is not reported, since the spool is only read and the descriptor is
// released anyway.
func (p *packCommits) close() { _ = p.f.Close() }

// committedAs is the first maxEngineCommits commits of a coding box's pack
// (commits, in index order) whose committer address is committer, and whether
// there are more (pastBound, which ends the scan: the push is refused anyway).
// It judges the whole PACK, never what the updates introduce: the pack also
// lands a refused update's objects and unreachable ones fsck admits, which no
// later push would introduce. The address is git's `%ce` (no .mailmap), every
// committer line counts, and the match folds case as a reader does.
// ONE COMMIT AT A TIME: `cat-file --batch` over the spooled ids, each header
// read as it arrives, its message skipped (committers); `rev-list --format`
// holds every commit's text at once, and a box can send megabytes of message
// as a kilobyte of pack (TestABoxPushsCommitsAreReadOneAtATime). Bounded by
// the pack's work (gitproc.PackWorkDeadline): outrun, unchecked, never landed.
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

// committers is the Stdout of committedAs's `cat-file --batch`: per commit, a
// "<id> commit <size>" line, size bytes and a newline. It reads each header as
// it arrives, hands match the commit's id once a committer line's address
// folds to want, and skips everything past the header's blank line. It keeps
// only the batch line in hand, the commit's id and the address being judged,
// capped at utf8.UTFMax bytes per byte of want plus one: past the longest
// address that can fold to want, so a longer one folds to nothing.
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

// introduced is the commits the push introduces (reachable from its new
// values and from no ref the repository holds), the first
// gitlet.MaxIntroduced of them, and how many there are.
//
// TOPOLOGICAL ORDER decides which ids the push row keeps past
// gitlet.MaxIntroduced: no commit before its children, and one line of history
// whole before another, so the row keeps the commits NEAREST THE PUSHED TIPS.
// Date order keeps the newest by stated date, which a skewed clock decides,
// cutting a tip dated before the commits under it.
func (s *Server) introduced(ctx context.Context, sc *cache.Scratch, objects int64, news []string,
	packBytes int64) ([]string, int, error) {

	var (
		ids   []string
		count int
	)
	lines := gitproc.NewLines(func(line []byte) error {
		if count < gitlet.MaxIntroduced {
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
