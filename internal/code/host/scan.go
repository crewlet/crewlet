package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// THE RECEIVE PIPELINE'S THREE SCANS of a push — and a coding box's fourth,
// its pack's committers (committedAs) — each one git process in the push's
// scratch, in sequence — never one pool process piped into another, which
// would hold two slots of one lane at once and queue against itself.
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
// skewed tip, 110–150 ms over 200 000 commits against 5 ms with it
// (git 2.47.3 and 2.48.0 alike). The fourth is receive-pack's
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
func (s *Server) largestBlob(ctx context.Context, sc *cache.Scratch, idxPath string, packBytes, limit int64) (
	oid string, size int64, found bool, err error) {

	_, ids, err := idxOIDs(idxPath)
	if err != nil {
		return "", 0, false, err
	}
	defer func() { _ = ids.Close() }()
	lines := gitproc.NewLines(func(line []byte) error {
		fields := bytes.Fields(line)
		if len(fields) != 3 || string(fields[1]) != "blob" {
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
	return "", 0, false, nil
}

// committedAs is the commits the push's own pack holds whose committer's
// address is committer — the first maxEngineCommits of them, in the order the
// pack's index lists them — and whether it holds more (pastBound).
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
// ONE `rev-list --no-walk=unsorted --stdin` over the pack's ids, streamed from
// its index as the blob scan's are, printing each commit's id and committer
// (`--no-commit-header --format=%H%x00%ce`, git 2.33, under every git node's
// floor). rev-list walks no history here: handed without --objects, a tree or
// a blob is passed over once its header says what it is, and a tag is peeled
// to the commit it names, which the pack or the repository holds. `%ce` is the
// address as the commit wrote it, never `%cE`, which a .mailmap would rewrite,
// and the match FOLDS CASE over the whole address (bytes.EqualFold): a reader
// takes Engine@Seats.Invalid for the engine, and refusing more than the exact
// spelling admits no forgery. Past maxEngineCommits matches the scan ENDS —
// the push is refused whatever the rest are (forged) — so what one push makes
// the engine hold and the repository be asked is the bound's, never what a
// box sent. The rest is the pack's own cost: rev-list keeps an entry for every
// object the pack holds, and every commit's text until it is printed, as
// receive-pack's index-pack kept one an object for the same pack, and it is
// bounded by the blob scan's deadline.
func (s *Server) committedAs(ctx context.Context, sc *cache.Scratch, idxPath string, packBytes int64,
	committer string) (found []string, pastBound bool, err error) {

	_, ids, err := idxOIDs(idxPath)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = ids.Close() }()
	judged := []byte(committer)
	lines := gitproc.NewLines(func(line []byte) error {
		id, address, ok := bytes.Cut(line, []byte{0})
		if !ok || !gitref.ValidOID(id) {
			return fmt.Errorf("host: rev-list printed %q, which is no commit and its committer", line)
		}
		if !bytes.EqualFold(address, judged) {
			return nil
		}
		if len(found) == maxEngineCommits {
			pastBound = true
			return errFound
		}
		found = append(found, string(id))
		return nil
	})
	_, err = s.runGit(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate,
		Args:  []string{"rev-list", "--no-walk=unsorted", "--stdin", "--no-commit-header", "--format=%H%x00%ce"},
		Stdin: ids, Stdout: lines, Class: gitproc.ClassRead, Deadline: gitproc.PackWorkDeadline(packBytes),
	})
	if err == nil {
		err = lines.Close()
	}
	switch {
	case errors.Is(err, errFound):
		return found, true, nil
	case err != nil:
		return nil, false, fmt.Errorf("host: read the committers of the push's commits: %w", err)
	}
	return found, false, nil
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
