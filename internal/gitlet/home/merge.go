package home

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// MergeabilityDeadline bounds the home's one `merge-tree` of a change before
// it is recorded `unknown` and retried at the next refresh (so costing one
// deadline a refresh, never one a pass): thirty seconds, three times
// gitproc.ReadDeadline, since a merge is a read's work three times over at
// most (the base found, both sides diffed against it, their changes merged)
// and no client waits on it. It is gitlet.MergeBuildDeadline, so an
// engine-built merge and the mergeability the same merge-tree records share
// one budget rather than two that drift.
//
// BenchmarkMergeability's worst arm sits some 240 times inside it, kept so a
// far larger tree or change, merging in seconds, still decides. PROVISIONAL
// (M4's spec): a production p99 nearing it moves it.
const MergeabilityDeadline = gitlet.MergeBuildDeadline

// MergeabilityRefresh is how long a change's merge stands before a moved
// target, or an undecided merge, has its home merge it again. Merged at every
// move, a hot target would cost its home a merge per change per push; ten
// minutes is how stale "as of <tip>" may grow, about the time a person takes
// to read a change. A NEW head revision is merged at once, whatever the
// refresh. PROVISIONAL (M4's spec): a hot target's cost is bounded by
// [MergeabilityPerPass]; a change waiting longer than its target's
// readers tolerate moves it.
const MergeabilityRefresh = 10 * time.Minute

// MergeabilityPerPass is the most merges of a repository's moved targets one
// home runs in one pass (one [Backstop]), the oldest merge first, so changes
// past it behind a moved target are merged again in turn, never the same
// first ones each refresh. A NEW head revision's merge takes none of it. At
// BenchmarkMergeability's figures, sixty-four a minute are a twentieth to an
// eighth of one core, never a home's core taken from its seats' reads.
// PROVISIONAL (M4's spec): a production merge time past a second moves it.
const MergeabilityPerPass = 64

// MergeScratchID is the id of the scratch the home merges revision rev of
// change in, against its target's tip at position at: a bare UUIDv7 derived
// from the three and minted at the change's own instant, so the scratch says
// which merge it is. A repository's work is one turn at a time and the
// scratch is removed before the turn moves on, so a merge retried at the same
// tip reuses the name and never meets the last one's scratch.
func MergeScratchID(change string, rev int64, at statelog.Position) string {
	minted, _ := statelog.OpMintedAt(change)
	return statelog.DeriveOpID(minted, "", "code_merge", change, strconv.FormatInt(rev, 10),
		strconv.FormatInt(at.Packed(), 10))
}

// merged is what one merge did: its state, and on a conflict the paths it
// left conflicted as a record holds them.
type merged struct {
	state     gitlet.MergeState
	conflicts gitlet.ConflictsHeld
}

// mergeIn is THE merge a change's mergeability and a merge preview both run,
// so the two agree: head merged into target (whole commit ids) in sc, whose
// own objects take what the merge writes while its alternate reads the copy.
// CLEAN where no path is left conflicted, CONFLICT naming the paths, UNKNOWN
// where deadline ran out while ctx lived; any other end is an error.
//
// --write-tree is the one way merge-tree merges without a working tree; a
// history unrelated to its target merges from the empty tree
// (gitlet.Mergeability's doc); the merge base is git's own, every one it
// finds, as `git merge` would take. It runs in a scratch, never a cache: it
// writes objects, and a cache is a function of rows (internal/gitlet/cache's
// writers gate); what it wrote leaves with the scratch.
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
		return merged{state: gitlet.MergeUnknown}, nil
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
		return merged{state: gitlet.MergeClean}, nil
	}
	return merged{state: gitlet.MergeConflict, conflicts: out.conflicts}, nil
}

// mergeOutput is a merge's standard output as it streams: the merged tree's
// id, then one conflicted path a record, each ended by a NUL, held by
// [gitlet.ConflictsHeld] as it arrives so a merge conflicting in a hundred
// thousand files holds a bounded few and counts the rest.
type mergeOutput struct {
	tree      string
	conflicts gitlet.ConflictsHeld
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
