package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// THE RECEIVE PIPELINE'S THREE SCANS of a push, each one git process in the
// push's scratch, in sequence — never one pool process piped into another, which
// would hold two slots of one lane at once and queue against itself.

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
func (s *Server) forceChecks(ctx context.Context, dir string, cmds []code.Command, todo []int) (
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
				isForced, isStale, cerr := s.isForced(ctx, dir, cmds[i].Old, cmds[i].New)
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

// isForced asks whether old is an ancestor of new in dir: exit 0 is not
// forced, exit 1 is forced, and an old this company holds no object for is
// stale.
func (s *Server) isForced(ctx context.Context, dir, old, new string) (forced, stale bool, err error) {
	res, err := s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: dir, Args: []string{"merge-base", "--is-ancestor", old, new},
		Class: gitproc.ClassRead, Answers: []int{1}, Deadline: gitproc.ReadDeadline,
	})
	switch {
	case errors.Is(err, gitproc.ErrMissingObject):
		return false, true, nil
	case err != nil:
		return false, false, fmt.Errorf("host: is %s an ancestor of %s: %w", old, new, err)
	}
	return res.Exit == 1, false, nil
}

// errFound ends a scan at its answer: not a failure.
var errFound = errors.New("host: found")

// largestBlob is the first blob in the push's own pack over limit bytes: ONE
// `cat-file --batch-check` over the pack's ids, streamed from its index, so the
// scratch's alternate — the whole repository — is never listed, ending at the
// first offender.
func (s *Server) largestBlob(ctx context.Context, dir, idxPath string, packBytes, limit int64) (
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
	_, err = s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: dir, Args: []string{"cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)", "--buffer"},
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

// pathOf is the path a blob of the push is reached by — the first `rev-list
// --objects` names it at from the ids the push sent (news: every command's new
// id, refused or not, since the pack carries the objects of all of them), past
// what the repository already holds — or "(unreachable in this push)" for a
// blob no new tree names.
func (s *Server) pathOf(ctx context.Context, dir string, news []string, oid string, packBytes int64) string {
	path := "(unreachable in this push)"
	lines := gitproc.NewLines(func(line []byte) error {
		id, p, ok := bytes.Cut(line, []byte{' '})
		if ok && string(id) == oid {
			path = string(p)
			return errFound
		}
		return nil
	})
	_, err := s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: dir, Args: append(append([]string{"rev-list", "--objects"}, news...), "--not", "--alternate-refs"),
		Stdout: lines, Class: gitproc.ClassRead, Deadline: gitproc.PackWorkDeadline(packBytes),
	})
	if err != nil && !errors.Is(err, errFound) {
		s.log.Warn("code_push_blob_path", "dir", dir, "blob", oid, "error", err)
	}
	return path
}

// introduced is the commits the push introduces — reachable from its new
// values and from no ref the repository holds — the first code.MaxIntroduced
// of them in rev-list's order, and how many there are.
func (s *Server) introduced(ctx context.Context, dir string, news []string, packBytes int64) ([]string, int, error) {
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
	_, err := s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: dir, Args: append(append([]string{"rev-list"}, news...), "--not", "--alternate-refs"),
		Stdout: lines, Class: gitproc.ClassRead, Deadline: gitproc.PackWorkDeadline(packBytes),
	})
	if err == nil {
		err = lines.Close()
	}
	if err != nil {
		return nil, 0, fmt.Errorf("host: list the commits the push introduces: %w", err)
	}
	return ids, count, nil
}
