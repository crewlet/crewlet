package query

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A REVISION'S FACTS are what one revision of a change IS against its target,
// a function of two whole commit ids alone — its head, and its target's tip
// as the asker read it — so any git node answers them alike, and whoever
// records them reads them once a revision and target. Up to four processes of
// the read lane, one after another:
//
//  1. THE BASE — `git merge-base <target> <head>`: the change is measured
//     from where its history meets its target's, as a merge of it would be.
//     Where that is the head itself the target holds the change already
//     (HeadInTarget), and there is nothing left to measure. Histories that
//     share no commit have no merge base: the change is measured from the
//     empty tree, which every git knows without holding it — every file of
//     the head is the change's.
//  2. THE TOUCHED PATHS — `git diff-tree -r --name-only -z --no-renames <base>
//     <head>`: every file the change's merge would change, read until the
//     list passes code.MaxTouchedPaths, its JSON passes code.MaxTouchedBytes
//     (or the room its answer leaves), or a path is no UTF-8, which JSON
//     cannot carry exactly. There the read stops git and the facts say the
//     revision TOUCHES ALL, with no list: what every consumer reads as every
//     path, where a list cut at its bound would leave the files past the cut
//     out of everything that matches against it. Renames off, so a moved
//     file is its two paths, as the merge would touch them.
//  3. THE PATCH — `git diff-tree -r -p --full-index --no-renames <base>
//     <head>`, the cumulative diff of the change, every file in git's order,
//     with three lines of context, written into a SPOOL (the cache's: a file
//     with no name on its volume, gone when closed) and bounded there at
//     MaxPatchIDDiffBytes.
//  4. ITS ID — `git patch-id --verbatim`, reading the spool back: the hash of
//     the patch's lines with their line numbers left out, so the same change
//     rebased onto a target that moved nothing near its lines keeps its id.
//
// --VERBATIM, NEVER --STABLE: --stable strips all whitespace before it hashes,
// and whitespace is meaningful — Python's and YAML's indentation, a
// Makefile's tab, a string literal's spaces — so a change to it alone must not
// read as the change a reviewer approved. --verbatim hashes the lines as they
// are, and orders files as --stable does (each file's hash summed), where git
// 2.39 and later carry it: every git node's floor.
//
// --FULL-INDEX because patch-id hashes a binary file by the two blob ids its
// `index` line names, and git abbreviates them to however many digits make
// them unique in the repository at hand — which grows with its objects — so
// one change would hash apart on two copies holding different counts of
// objects. Whole ids are the same on every node.
//
// A SPOOL, because the two processes run one after the other in the read
// lane, never nested: piped together they would hold two of its slots at
// once, and a lane filled with first halves would wait on itself for the
// second. A patch held in memory instead would be MaxPatchIDDiffBytes a read,
// and a spool is no file of the cache's: the kernel removes it however the
// read ends.
//
// AN ID IS OPTIONAL, AND ITS OMISSION THE SAFE SIDE: past MaxPatchIDDiffBytes,
// and where the patch or its id ran out what was left of the read's budget of
// git, the facts answer with PatchIDOmitted — an id that equals nothing, so no
// approval carries across it — rather than fail: the base and the touched
// paths are what a change cannot do without, and they cost little. Those two
// fail the read as any read's git does (server.go): a walk that ran its whole
// budget is too large, a busy or damaged node unready.

// MaxPatchIDDiffBytes is the largest cumulative diff whose patch-id a
// revision's facts take: past it the facts answer PatchIDOmitted.
//
// 256 MIB, MEASURED TO FIT: BenchmarkRevisionFacts read the facts of a change
// whose patch is 249 MiB — 781 files of 256 KiB, every fourth line rewritten,
// a formatter's run — in 2.30 s at p50 and 2.38 s at the slowest of three
// reads (-benchtime 3x, one count, git 2.56.0 on two E-cores of a Core Ultra
// 7 265U), some 114 MB of patch a second, as its 1, 16 and 64 MiB arms (18,
// 170 and 621 ms) are: under a quarter of the one budget a read's own git
// has, gitproc.ReadDeadline, so a node at its share, four times slower, still
// takes the id. So the facts need NO DEADLINE OF THEIR OWN: a longer one would
// outlast the attempt a git read's asker waits (estate.GitReadAttempt, which
// that budget is a term of), answered late rather than omitted. A spool at
// the bound is a quarter of a gigabyte on the cache's volume for the second
// or two its read runs. A change whose diff passes it — a vendored tree, data
// files — carries no approval from one revision to the next.
const MaxPatchIDDiffBytes = 256 << 20

// emptyTree is git's id of the tree with nothing in it, which every git holds
// without an object: the base a change whose history shares none with its
// target's is measured from.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// errPatchPastBound ends a patch's run at MaxPatchIDDiffBytes: its spool's
// writer's error, which the pool hands back as gitproc.ErrOutput.
var errPatchPastBound = errors.New("query: the patch is past the bound on a diff whose patch-id is taken")

// factsRead answers a revision's facts.
func (r *read) factsRead(ctx context.Context, objs found) (code.GitAnswer, error) {
	q := r.q.RevisionFacts
	head, target := objs.revs[q.Head], objs.revs[q.Target]
	for _, side := range []struct {
		field, rev string
		o          object
	}{{"revision_facts.head", q.Head, head}, {"revision_facts.target", q.Target, target}} {
		if side.o.peeledKind != "commit" {
			return code.GitAnswer{}, fmt.Errorf("%w: %s: %s names a %s; a revision's facts are of two commits",
				code.ErrInvalid, side.field, side.rev, side.o.peeledKind)
		}
	}
	mb, err := r.mergeBase(ctx, target.peeled, head.peeled, fmt.Errorf("%w: the merge base of %s and %s did "+
		"not finish in the read's %s of git: a copy walks a long history by date until its maintenance writes "+
		"its commit-graph, and answers the same facts once it has", code.ErrSliceTooLarge, target.peeled,
		head.peeled, r.s.gitBudget))
	if err != nil {
		return code.GitAnswer{}, err
	}
	f := &code.RevisionFactsAnswer{Head: head.peeled, Target: target.peeled, MergeBase: mb, Touched: []string{},
		HeadInTarget: mb == head.peeled}
	a := code.GitAnswer{Kind: code.GitRevisionFacts, RevisionFacts: f}
	if f.HeadInTarget {
		return a, nil
	}
	base := mb
	if base == "" {
		base = emptyTree
	}
	// THE ROOM THE LIST HAS: the answer with every other field at its
	// widest — an id where the id goes, every flag set — and the list's own
	// brackets, which the envelope holds already.
	envelope := code.GitAnswer{Kind: code.GitRevisionFacts, RevisionFacts: &code.RevisionFactsAnswer{
		Head: f.Head, Target: f.Target, MergeBase: mb, PatchID: f.Head, PatchIDOmitted: true, Touched: []string{},
		TouchesAll: true}}
	limit := min(r.s.touchedBytes, r.room(envelope)+code.TouchedCost(nil))
	if f.Touched, f.TouchesAll, err = r.touched(ctx, base, head.peeled, limit); err != nil {
		return code.GitAnswer{}, err
	}
	if f.PatchID, f.PatchIDOmitted, err = r.patchID(ctx, base, head.peeled); err != nil {
		return code.GitAnswer{}, err
	}
	return a, nil
}

// touched is every path the diff from base to head changes, in git's order —
// or all, with no list, once the list would pass the read's bound on its paths
// or on its JSON (limit), or meets a path that is no UTF-8.
func (r *read) touched(ctx context.Context, base, head string, limit int) ([]string, bool, error) {
	paths, all, cost := []string{}, false, code.TouchedCost(nil)
	recs := newRecords(0, gitproc.MaxLine+code.MaxGitPathBytes, func(rec []byte, long bool) error {
		c := textCost(rec) + 2 // the path, quoted
		if len(paths) > 0 {
			c++ // the comma before it
		}
		// A PATH THE FACTS' RECORD COULD NOT CARRY TOUCHES ALL, as one past
		// the list's bounds does: bytes that are not UTF-8, which JSON cannot
		// carry exactly, and one no read by oid could name back (code.GitPath:
		// past code.MaxGitPathBytes, or not clean), which the revise that
		// records the facts refuses — a list holding it would leave its change
		// due to its home for ever, its every revise refused.
		if long || !utf8.Valid(rec) || len(paths) == r.s.touchedPaths || cost+c > limit ||
			!nameable(rec) {
			all = true
			return errStop
		}
		paths, cost = append(paths, string(rec)), cost+c
		return nil
	})
	deadline, err := r.budget()
	if err != nil {
		return nil, false, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   append([]string{"diff-tree", "-r", "--name-only", "-z", "--no-renames"}, base, head),
		Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	switch {
	case all && res.Damage == nil:
		r.charge(res)
		return []string{}, true, nil
	case err == nil:
		err = recs.close()
	}
	narrow := fmt.Errorf("%w: the walk of the paths %s changes since %s did not finish in the read's %s of git",
		code.ErrSliceTooLarge, head, base, r.s.gitBudget)
	if err = r.failed(ctx, "walk of the paths a change touches", res, err, narrow); err != nil {
		return nil, false, err
	}
	return paths, false, nil
}

// nameable reports a touched path a read by oid could name back, and so one a
// revision's facts may list (code.GitPath).
func nameable(rec []byte) bool {
	_, err := code.GitPath("touched", string(rec), "", false)
	return err == nil
}

// patchID is `git patch-id --verbatim` of the diff from base to head: "" for
// a diff of nothing, and omitted past the read's bound on the diff, or where
// what was left of its budget of git ran out.
func (r *read) patchID(ctx context.Context, base, head string) (string, bool, error) {
	deadline, err := r.budget()
	if err != nil {
		return "", true, nil // the base and the paths spent the read's whole budget
	}
	spool, err := r.s.o.Cache.Spool()
	if err != nil {
		return "", false, r.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not make a spool for the "+
			"patch of %s: %v; another git node answers", r.s.o.NodeID, head, err))
	}
	defer func() { _ = spool.Close() }()
	w := &spooled{f: spool, limit: r.s.patchBytes}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: append([]string{"diff-tree", "-r", "-p", "--full-index", "--no-renames",
			"--no-color", "--no-ext-diff", "--no-textconv", "-U3"}, base, head),
		Config: append(r.s.o.Cache.ServeConfig(), patchConfig), Stdout: w, Class: gitproc.ClassRead,
		Deadline: deadline,
	})
	switch {
	case w.failed != nil && res.Damage == nil:
		r.charge(res)
		return "", false, r.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not write the patch of %s to "+
			"its spool: %v; another git node answers", r.s.o.NodeID, head, w.failed))
	case r.omits(ctx, res, err):
		return "", true, nil
	}
	if err = r.failed(ctx, "patch of a change", res, err, nil); err != nil {
		return "", false, err
	}
	if w.wrote == 0 {
		// A DIFF OF NOTHING has no patch and no id: a revision that changes
		// nothing carries nothing.
		return "", false, nil
	}
	if _, err = spool.Seek(0, io.SeekStart); err != nil {
		return "", false, r.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not read back the spool of "+
			"the patch of %s: %v; another git node answers", r.s.o.NodeID, head, err))
	}
	if deadline, err = r.budget(); err != nil {
		return "", true, nil // the patch spent the read's whole budget
	}
	res, err = r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"patch-id", "--verbatim"}, Config: r.s.o.Cache.ServeConfig(),
		Stdin: spool, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if r.omits(ctx, res, err) {
		return "", true, nil
	}
	if err = r.failed(ctx, "patch-id of a change", res, err, nil); err != nil {
		return "", false, err
	}
	// ONE PATCH, ONE LINE: "<patch-id> <commit id>", the commit id git's
	// zero id, since a diff of two trees names no commit.
	fields := strings.Fields(string(res.Stdout))
	if len(fields) != 2 || !gitref.ValidOID(fields[0]) || fields[1] != gitref.ZeroOID {
		return "", false, fmt.Errorf("query: patch-id answered %q for a patch of %d bytes, which is no one patch's "+
			"id", truncated(res.Stdout), w.wrote)
	}
	return fields[0], false, nil
}

// omits reports whether a run of the patch or its id ended where the facts
// omit the id — at the bound on the patch, or at its deadline, what was left
// of the read's budget of git — charging its run. A run that reported damage,
// one the read's answer falling due ended, and one its caller ended are none:
// failed judges those as it judges every run.
func (r *read) omits(ctx context.Context, res gitproc.Result, err error) bool {
	switch {
	case err == nil, res.Damage != nil, code.AnswerDue(ctx), ctx.Err() != nil:
		return false
	case errors.Is(err, errPatchPastBound), errors.Is(err, gitproc.ErrDeadline):
		r.charge(res)
		return true
	}
	return false
}

// spooled is the Stdout of a patch's run: git's patch written into its spool,
// refused at the first byte past limit (errPatchPastBound), which stops the
// run. failed is a write the spool itself refused — the node's disk — which
// the read answers as the node's own failure, never as a patch past its
// bound.
type spooled struct {
	f      *os.File
	limit  int64
	wrote  int64
	failed error
}

func (s *spooled) Write(p []byte) (int, error) {
	if s.wrote+int64(len(p)) > s.limit {
		return 0, errPatchPastBound
	}
	n, err := s.f.Write(p)
	s.wrote += int64(n)
	if err != nil {
		s.failed = err
		return n, fmt.Errorf("query: write a patch to its spool: %w", err)
	}
	return n, nil
}
