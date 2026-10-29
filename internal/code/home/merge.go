package home

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// MergeabilityDeadline bounds the home's merge of one change — its one
// `merge-tree` — before it is recorded undecided: THIRTY SECONDS, three times
// a read's own git (gitproc.ReadDeadline), since a merge is a read's work at
// most three times over — the merge base found, both sides' trees diffed
// against it, their changes merged — and no client waits on it. A merge past
// it is recorded `unknown` and tried again at the next refresh, so a change
// whose merge outlasts it costs its home one deadline a refresh, never one a
// pass.
//
// MEASURED (BenchmarkMergeability, the whole step — sync, scratch, ancestry,
// merge, scratch removed — over a tree of 20 000 files, each side's files
// beside the other's in every directory either touched): 8.9 ms for a change
// of 10 files, 48 ms for one of 1 000, and 125 ms for 1 000 files changed both
// ways, each a content merge that conflicts (git 2.56.0 on two E-cores of a
// Core Ultra 7 265U, the machine loaded, -benchtime 3x and one count, the mean
// of three; 2026-10-29). The worst arm is some 240 times inside the deadline,
// which is kept at a read's own three-fold rather than cut to the figure: a
// repository several times that tree, or a change touching tens of thousands
// of files, merges in seconds, and a node at its maintenance lane's share
// still decides it. PROVISIONAL, as M4's spec marks it: what moves it is a
// production merge whose p99 nears it.
const MergeabilityDeadline = 3 * gitproc.ReadDeadline

// MergeabilityRefresh is how long a change's merge stands before a moved
// target — or an undecided merge — has its home merge it again: TEN MINUTES.
// A target moves on every push to it, and every change into it holds a merge
// against the tip it read; merged again at every move, a hot target would
// cost its home a merge per change per push. Ten minutes is how stale "as of
// <tip>" may grow on a change's page before it says so again — about the time
// a person takes to read a change — while a NEW head revision, which a
// person or a seat just pushed and waits on, is merged at once, whatever the
// refresh. PROVISIONAL (M4's spec): a hot target's cost is MergeabilityPerPass
// merges a pass at most, so only a change waiting longer than its target's
// readers tolerate moves it.
const MergeabilityRefresh = 10 * time.Minute

// MergeabilityPerPass is the most merges of a repository's moved targets one
// home runs in one pass — one [Backstop] — the changes whose merge is oldest
// first: SIXTY-FOUR, so a hot target's rechecks cost its home at most
// sixty-four merges a minute — and a repository with more changes behind a
// moved target than that is merged again in turn, oldest first, never the
// same first ones each refresh. A NEW head revision's merge takes none of it.
//
// At BenchmarkMergeability's figures (MergeabilityDeadline), sixty-four merges
// are 3.1 s of a core a minute for changes of a thousand files over a tree of
// twenty thousand, and 8 s where every one of them conflicts in a thousand —
// a twentieth to an eighth of one core — so a hot target's rechecks never take
// a home's core from its seats' reads, and its pushes' new revisions are
// merged at once beside them. PROVISIONAL, as M4's spec marks it: a pass's
// merges are bounded by the core a repository's sequential turn runs on in any
// case; this bounds them well below it, and a production merge time that
// grows past a second is what moves it.
const MergeabilityPerPass = 64

// MergeScratchID is the id of the scratch the home merges revision rev of
// change in, against its target's tip at position at: an UNNAMED derivation —
// a bare UUIDv7, which a scratch's directory takes as its name — of the three,
// minted at the change's own instant, so a merge's scratch says which merge it
// is. One merge of the three runs on a home at a time — a repository's work is
// one turn at a time — and its scratch is removed before the turn moves on, so
// a merge tried again at the same tip, an undecided one at its refresh, takes
// the name again and never meets the last one's scratch.
func MergeScratchID(change string, rev int64, at statelog.Position) string {
	minted, _ := statelog.OpMintedAt(change)
	return statelog.DeriveOpID(minted, "", "code_merge", change, strconv.FormatInt(rev, 10),
		strconv.FormatInt(at.Packed(), 10))
}

// merged is what one merge did: its state, and on a conflict the paths it
// left conflicted as a record holds them.
type merged struct {
	state     code.MergeState
	conflicts code.ConflictsHeld
}

// mergeIn is THE MERGE, the one function both of its callers run — a change's
// mergeability and a merge preview — so the two answer one merge: head merged
// into target, two whole commit ids, in sc — the scratch's own objects taking
// what the merge writes, its alternate reading the copy — in class, within
// deadline. CLEAN where git leaves no path conflicted, CONFLICT naming the
// paths it does, UNKNOWN where the merge ran out of deadline while ctx still
// lived. Every other end is an error: a merge that could not run, ctx's end,
// or an answer git does not give.
//
// ONE MERGE-TREE: the merged tree WRITTEN — the one way merge-tree merges
// without a working tree — NUL-separated, each conflicted path once, no
// messages, and a history that shares no commit with its target's merged
// from the empty tree, as a revision's facts measure it (code.Mergeability's
// doc). The merge base is git's own, every one it finds: what `git merge`
// would merge.
//
// IN A SCRATCH, NEVER A CACHE: `merge-tree --write-tree` writes the merged
// tree and every blob it merges into the repository it runs in, and a cache
// is a function of rows (internal/code/cache's writers gate). What the merge
// wrote leaves with the scratch; an object the copy already held is only
// freshened there, as an engine write's build freshens it.
func mergeIn(ctx context.Context, git Pool, sc *cache.Scratch, target, head string, class gitproc.Class,
	deadline time.Duration) (merged, error) {

	out := &mergeOutput{}
	res, err := git.Run(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate,
		Args: []string{"merge-tree", "--write-tree", "-z", "--name-only", "--no-messages",
			"--allow-unrelated-histories", target, head},
		Stdout: out, Class: class, Deadline: deadline, Answers: []int{1},
	})
	switch {
	case errors.Is(err, gitproc.ErrDeadline) && ctx.Err() == nil:
		return merged{state: code.MergeUnknown}, nil
	case err != nil:
		return merged{}, fmt.Errorf("home: merge %s into %s: %w", head, target, err)
	}
	if closeErr := out.close(); closeErr != nil {
		return merged{}, fmt.Errorf("home: merge %s into %s: %w", head, target, closeErr)
	}
	switch {
	case out.tree == "":
		return merged{}, fmt.Errorf("home: merge %s into %s answered no tree (exit %d): %s", head, target, res.Exit,
			res.Stderr)
	case res.Exit == 0 && (len(out.conflicts.Paths) > 0 || out.conflicts.Omitted > 0):
		return merged{}, fmt.Errorf("home: merge %s into %s answered clean and named conflicted paths", head,
			target)
	case res.Exit == 0:
		return merged{state: code.MergeClean}, nil
	}
	return merged{state: code.MergeConflict, conflicts: out.conflicts}, nil
}

// mergeOutput is a merge's standard output as it streams: its first record
// the merged tree's id, every record after it one conflicted path, each ended
// by a NUL — held by [code.ConflictsHeld] as it arrives, so a merge that
// conflicts in a hundred thousand files holds a hundred of them and counts
// the rest, never the whole list.
type mergeOutput struct {
	tree      string
	conflicts code.ConflictsHeld
	pending   []byte
	err       error
}

// Write splits p into records, carrying a partial one into the next Write; a
// record past gitproc.MaxLine — longer than any id or path git writes — or a
// first record that is no object id stops git.
func (m *mergeOutput) Write(p []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, 0)
		if i < 0 {
			if len(m.pending)+len(p) > gitproc.MaxLine {
				m.err = gitproc.ErrLineTooLong
				return 0, m.err
			}
			m.pending = append(m.pending, p...)
			break
		}
		rec := p[:i]
		if len(m.pending) > 0 {
			rec = append(m.pending, rec...)
		}
		if len(rec) > gitproc.MaxLine {
			m.err = gitproc.ErrLineTooLong
			return 0, m.err
		}
		if err := m.record(rec); err != nil {
			m.err = err
			return 0, err
		}
		m.pending = m.pending[:0]
		p = p[i+1:]
	}
	return n, nil
}

// record reads one record: the tree's id first, a conflicted path after it.
func (m *mergeOutput) record(rec []byte) error {
	if m.tree == "" {
		if !gitref.ValidOID(rec) {
			return fmt.Errorf("home: merge-tree answered %q where the merged tree's id is", rec)
		}
		m.tree = string(rec)
		return nil
	}
	m.conflicts.Add(string(rec))
	return nil
}

// close is the output's end: an error where it ended inside a record.
func (m *mergeOutput) close() error {
	if m.err == nil && len(m.pending) > 0 {
		m.err = gitproc.ErrPartialLine
	}
	return m.err
}
