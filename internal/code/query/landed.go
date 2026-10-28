package query

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// WHETHER A COMMIT LANDED IN A TARGET, AND WHICH PUSH LANDED IT. The first half
// is one ancestry check against the tip the asker was shown (`git merge-base
// --is-ancestor <commit> <tip>`); the second is the copy's index of pushes
// (cache.Provenance): the target's moves in log order up to the move that put
// that tip there, searched with ancestry checks — never a walk of the log or
// of history. The landing push is the first move after which EVERY move
// kept the commit reachable, so a commit a force-push removed and a later push
// put back is answered with the push that put it back.
//
// THE SEARCH. The moves split into SEGMENTS at every forced move, every
// deletion and the index's gap: inside one, every move is a fast-forward of
// the one before it, read whole, so whether the commit is reachable from a
// move's new value turns from no to yes at most once, and a binary search
// finds where. It starts in the newest segment; where that segment's first
// move already reaches the commit and a forced move began it, the commit was
// reachable just before it exactly when it is reachable from that move's old
// value, which one more check asks — and if it was, the landing lies below,
// in the segment before. MaxLandedProbes bounds the checks, and so does the
// read's one budget of git, which they share with the check against the tip:
// past either, the answer names how far the search reached rather than a push.
//
// NEVER A WRONG PUSH. A move is the landing only where the commit was NOT
// reachable from its old value: inside a segment the move before it says so,
// but at the view's first move, or the first above a gap — whose moves were
// never read — the old value is asked, and where it reaches the commit the
// landing predates what the index holds, answered with no push and a detail
// naming the range (M3-decisions M3-O20), never the first push the index
// happens to hold.

// MaxLandedProbes is the most ancestry checks a landed-in search runs beside
// its first, against the tip.
//
// 1024, BY M3-decisions M3-O22's RULE for this provisional value: the largest
// power of two whose checks fit the read's one budget of git
// (gitproc.ReadDeadline, ten seconds) beside the one other process that
// budget pays for, the check against the tip — n + 1 checks, each at the
// 99th percentile of one. BenchmarkLandedIn measured that percentile at
// 6.7 ms (11.3 ms the slowest of 3 075) over gittest.History's 32 MB, 14 080
// commits with the copy's commit-graph written, on git 2.56.0 and two
// E-cores of a Core Ultra 7 265U: 1 025 checks are 6.9 s of the budget and
// 2 049 would be 13.7 s, past it, and the read that runs all 1 024 took
// 4.8 s whole. A check is one git process, its start most of it, walking by
// generation from the move it asks about down to the commit.
//
// THE BOUND IS WHERE THE SEARCH STOPS ASKING AND ANSWERS how far it reached,
// before the budget would end it: every process of a read's own git shares
// that one budget (server.go), so it is the budget, not this count, that
// holds a search's time and its use of the read lane — no more than the ten
// seconds any read's own git may run under its one unit of the seat share.
// An unforced branch with a million moves in the window takes twenty checks
// and each forced move since the landing two more, so a branch forced about
// five hundred times since the commit landed on it — a seat's branch rewritten
// at every review — is searched whole. A check that costs more than the
// measure — an old commit's walk over a gigabyte (gittest.LargeEnv's arm,
// which re-derives this by the same rule where it is run), a copy whose
// commit-graph is not written yet (26 ms a check over the same history,
// walking by date), a loaded node — runs the budget out first, and the search
// answers how far it reached all the same (landing.stopped).
//
// It was 64, kept as a judgment while each check had a walk bound of its own,
// so that this count was all that held a search's time.
const MaxLandedProbes = 1024

// landedRead answers whether the query's commit is in its target's history at
// the tip it names, and the push that landed it there.
func (r *read) landedRead(ctx context.Context, objs found) (code.GitAnswer, error) {
	q := r.q.LandedIn
	commit, tip := objs.revs[q.Commit], objs.revs[q.TargetOID]
	for _, side := range []struct {
		field, rev string
		o          object
	}{{"landed_in.commit", q.Commit, commit}, {"landed_in.target_oid", q.TargetOID, tip}} {
		if side.o.peeledKind != "commit" {
			return code.GitAnswer{}, fmt.Errorf("%w: %s: %s names a %s; a landed-in read asks about commits",
				code.ErrInvalid, side.field, side.rev, side.o.peeledKind)
		}
	}
	l := &landing{r: r, ctx: ctx, commit: commit.peeled}
	ans := &code.LandedInAnswer{Commit: commit.peeled, Target: q.Target, TargetOID: q.TargetOID, At: q.At}
	a := code.GitAnswer{Kind: code.GitLandedIn, Rev: commit.peeled, LandedIn: ans}
	landed, err := l.ancestor(tip.peeled)
	if err != nil {
		return code.GitAnswer{}, err
	}
	ans.Landed = landed
	if !landed {
		return a, nil
	}
	if !r.prov.Indexed() {
		ans.Detail = fmt.Sprintf("%s keeps no index of pushes it can read for repository %s yet, so which push "+
			"landed the commit is not answered; ask again, or ask another git node", r.s.o.NodeID,
			r.synced.Repo.Name)
		return a, nil
	}
	if by, ok := r.prov.Commit(commit.peeled); ok {
		ans.IntroducedBy = &by
	}
	l.view = r.prov.Moves(q.Target, q.At)
	j, detail, err := l.search()
	ans.Probes = l.probes
	switch {
	case err != nil:
		return code.GitAnswer{}, err
	case j < 0:
		ans.Detail = detail
		return a, nil
	}
	m := l.view.At(j)
	ans.LandedBy, ans.Move = &m.By, &m.RefMove
	return a, nil
}

// landing is one landed-in search: the commit it asks about, the target's
// moves it searches and the checks it has run.
type landing struct {
	r      *read
	ctx    context.Context
	commit string
	view   cache.MoveView
	probes int
}

// errProbes ends a search whose checks reached MaxLandedProbes.
var errProbes = errors.New("query: the landed-in search reached its bound of ancestry checks")

// search is the index of the move that landed the commit, or -1 with why no
// move is named; an error is a check's failure.
func (l *landing) search() (int, string, error) {
	v, n := l.view, l.view.Len()
	target := l.r.q.LandedIn.Target
	if n == 0 {
		return -1, l.before0(), nil
	}
	bounds, gaps := v.Segments(), v.Gaps()
	// The newest move up to the tip must keep the commit, or no move this
	// index holds is the one that put the tip where it was shown.
	switch keeps, err := l.reach(n - 1); {
	case err != nil:
		return l.stopped(err, bounds, len(bounds)-1, n)
	case !keeps:
		return -1, fmt.Sprintf("the newest move of %s this node indexes up to position %s, push %s, does not "+
			"keep the commit: the move that put the tip asked about there is one this node has not indexed yet",
			target, l.r.q.LandedIn.At, v.At(n-1).By.PushID), nil
	}
	// last is the oldest move known to keep the commit, as every move after
	// it does: the landing is it or one before it.
	k, hi, last := len(bounds)-1, n, n-1
	for {
		lo := bounds[k]
		// The first move of [lo, hi) that reaches the commit, given hi-1 does.
		first := lo
		for first < last {
			mid := int(uint(first+last) >> 1)
			keeps, err := l.reach(mid)
			if err != nil {
				return l.stopped(err, bounds, k, last)
			}
			if keeps {
				last = mid
			} else {
				first = mid + 1
			}
		}
		if first > lo {
			// The move before it, in the same segment and read whole beside
			// it, did not reach the commit: this one landed it.
			return first, "", nil
		}
		if lo == 0 || slices.Contains(gaps, lo) {
			// The view's first move, or the first above the gap: what lay
			// before it is unknown, so its old value is asked.
			was, err := l.before(lo)
			switch {
			case err != nil:
				return l.stopped(err, bounds, k, last)
			case was:
				return -1, l.unindexed(lo, gaps), nil
			}
			return lo, "", nil
		}
		// A forced move began the segment, and reached the commit: did the
		// commit stand on the target just before it?
		was, err := l.before(lo)
		switch {
		case err != nil:
			return l.stopped(err, bounds, k, last)
		case !was:
			return lo, "", nil
		}
		// It did: the landing lies in the segment below, whose newest move —
		// the one before this forced move, read whole beside it — reaches
		// the commit, being where this move's old value came from.
		k, hi = k-1, lo
		last = hi - 1
	}
}

// stopped is the answer of a search a check ended. TWO ENDS ARE THE SEARCH'S
// OWN, and each is answered with how far it reached: past MaxLandedProbes
// (errProbes), and past the read's one budget of git, which its checks share
// with the check against the tip (code.ErrSliceTooLarge from a check of the
// search: one that ran all the budget had left, or one it had nothing left
// for) — no push, the oldest move known to keep the commit (last, n where the
// search knew none), and how many forced moves below it, the segment it
// stopped in begun by bounds[k] among them, it did not search past. Any other
// failure is the check's, and the read's answer.
//
// THE BUDGET'S END IS NOT THE READ TOO LARGE, as it is for a comparison,
// which has nothing to answer until its walks finish: the search has already
// found a move that keeps the commit, and refusing the read would throw that
// away and ask for a narrowing no landed-in read can make. Only the check
// against the tip, which comes before the search and decides whether the
// commit landed at all, runs the budget out as the read too large.
func (l *landing) stopped(err error, bounds []int, k, last int) (int, string, error) {
	// within is what the search ran out of; the answer's Probes says how
	// many checks that was.
	var within string
	switch {
	case errors.Is(err, errProbes):
		within = fmt.Sprintf("the %d ancestry checks a search runs", l.r.s.probes)
	case errors.Is(err, code.ErrSliceTooLarge):
		within = fmt.Sprintf("the read's %s of git", l.r.s.gitBudget)
	default:
		return -1, "", err
	}
	target := l.r.q.LandedIn.Target
	if last >= l.view.Len() {
		return -1, fmt.Sprintf("the search ran out of %s before it read a move of %s", within, target), nil
	}
	// A segment the index's gap began, rather than a forced move or a
	// deletion, is said apart.
	forced, gap := 0, false
	for _, b := range bounds[1 : k+1] {
		if l.view.Forced(b) {
			forced++
		} else {
			gap = true
		}
	}
	at := l.view.At(last).By
	detail := fmt.Sprintf("landed at or before push %s (position %s); %d forced moves of %s were not searched "+
		"past within %s", at.PushID, at.Position, forced, target, within)
	if gap {
		detail += ", nor a range of its moves this node has not indexed yet"
	}
	return -1, detail, nil
}

// before0 is why no push is named for a commit the target holds where the
// index holds no move of it at all.
func (l *landing) before0() string {
	cov := l.r.prov.Coverage()
	if cov.Complete {
		return fmt.Sprintf("landed before the push history the data nodes keep: this node's index holds no move "+
			"of %s up to position %s", l.r.q.LandedIn.Target, l.r.q.LandedIn.At)
	}
	return fmt.Sprintf("landed before position %s, where this node's index of %s's moves begins",
		cov.From, l.r.q.LandedIn.Target)
}

// unindexed is why no push is named for a commit already reachable from the
// old value of move i, the view's first or the first above the gap.
func (l *landing) unindexed(i int, gaps []int) string {
	at := l.view.At(i).By
	target := l.r.q.LandedIn.Target
	if slices.Contains(gaps, i) && i > 0 {
		below := l.view.At(i - 1).By
		return fmt.Sprintf("landed between push %s (position %s) and push %s (position %s) of %s, a range this "+
			"node has not indexed yet", below.PushID, below.Position, at.PushID, at.Position, target)
	}
	if slices.Contains(gaps, i) {
		return fmt.Sprintf("landed before push %s (position %s), the oldest move of %s above a range this node "+
			"has not indexed yet", at.PushID, at.Position, target)
	}
	if l.r.prov.Coverage().Complete {
		return fmt.Sprintf("landed before push %s (position %s), the oldest move of %s in the push history the "+
			"data nodes keep", at.PushID, at.Position, target)
	}
	return fmt.Sprintf("landed before push %s (position %s), the oldest move of %s this node indexes", at.PushID,
		at.Position, target)
}

// reach reports whether the commit is reachable from move i's new value: no
// for a deletion, with no check.
func (l *landing) reach(i int) (bool, error) {
	if l.view.Deletion(i) {
		return false, nil
	}
	return l.ancestorBudgeted(l.view.At(i).New)
}

// before reports whether the commit was reachable from move i's old value: no
// for a create, with no check.
func (l *landing) before(i int) (bool, error) {
	old := l.view.At(i).Old
	if old == code.ZeroOID {
		return false, nil
	}
	return l.ancestorBudgeted(old)
}

// ancestorBudgeted is ancestor, counted against MaxLandedProbes.
func (l *landing) ancestorBudgeted(tip string) (bool, error) {
	if l.probes >= l.r.s.probes {
		return false, errProbes
	}
	l.probes++
	return l.ancestor(tip)
}

// ancestor reports whether the commit is reachable from tip: one `git
// merge-base --is-ancestor`, which goes by generation where the copy holds a
// commit-graph, given what is left of the read's one budget of git
// ([read.budget]) and charged what it ran ([read.failed]) — never a walk of
// the whole history (gitproc.HistoryWalk), which no asker waits for, a git
// read's attempt being seventeen seconds (compare.go). A check the budget
// ends — it ran all the budget had left, or the budget had nothing left for
// it — is code.ErrSliceTooLarge, and only that: the read too large where it
// is the check against the tip, how far the search reached where it is one of
// the search's (landing.stopped).
func (l *landing) ancestor(tip string) (bool, error) {
	if tip == l.commit {
		return true, nil
	}
	deadline, err := l.r.budget()
	if err != nil {
		return false, err
	}
	res, err := l.r.s.o.Git.Run(l.ctx, gitproc.Cmd{
		Dir: l.r.synced.Dir, Dropping: l.r.synced.Dropping,
		Args:   []string{"merge-base", "--is-ancestor", l.commit, tip},
		Config: l.r.s.o.Cache.ServeConfig(), Class: gitproc.ClassRead, Deadline: deadline, Answers: []int{1},
	})
	tooLong := fmt.Errorf("%w: whether %s is in %s's history did not finish in the read's %s of git",
		code.ErrSliceTooLarge, l.commit, tip, l.r.s.gitBudget)
	if err := l.r.failed(l.ctx, "ancestry check", res, err, tooLong); err != nil {
		return false, err
	}
	return res.Exit == 0, nil
}
