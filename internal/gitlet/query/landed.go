package query

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// WHETHER A COMMIT LANDED IN A TARGET, AND WHICH PUSH LANDED IT: one ancestry
// check against the tip the asker was shown, then a search of the target's
// moves in the copy's index of pushes (cache.Provenance), never a walk of the
// log or of history (package doc, "Which push, from the copy's index"). The
// landing is the first move after which EVERY move kept the commit, so a
// commit a force-push removed and a later push put back names the later push.
//
// THE SEARCH. Forced moves, deletions and the index's gap split the moves into
// SEGMENTS of fast-forwards, in each of which reachability turns from no to
// yes at most once: a binary search finds where, newest segment first. Where a
// forced move began one whose first move reaches the commit, its old value is
// asked; if that reaches the commit too, the landing is in the segment below.

// NEVER A WRONG PUSH: a move is the landing only where its old value does not
// reach the commit. Inside a segment the move before it says so; at the view's
// first move, or the first above a gap, the old value is asked, and where it
// reaches the commit the landing predates the index: answered with no push and
// the range (M3-decisions M3-O20), never the oldest push the index holds.

// MaxLandedProbes is the most ancestry checks a landed-in search runs beside
// its first, against the tip.
//
// 1024 by M3-decisions M3-O22's rule: the largest power of two whose n + 1
// checks at BenchmarkLandedIn's p99 (6.7 ms) fit gitproc.ReadDeadline (package
// doc, "Measured"). At two checks a forced move, it searches whole a branch
// forced ~500 times since the landing. The read's budget, not this count,
// holds the checks' run time; their lane waits are charged nothing, so the
// answer falling due or a lane busy past gitproc.SlotWait also ends a search
// with how far it reached (landing.stopped). Neither a bound fitted to those
// waits (a load nobody can name) nor one slot held for the search (push scans
// queued behind it for seconds) is the fix.
const MaxLandedProbes = 1024

// landedRead answers whether the query's commit is in its target's history at
// the tip it names, and the push that landed it there.
func (r *read) landedRead(ctx context.Context, objs found) (gitlet.GitAnswer, error) {
	q := r.q.LandedIn
	commit, tip := objs.revs[q.Commit], objs.revs[q.TargetOID]
	for _, side := range []struct {
		field, rev string
		o          object
	}{{"landed_in.commit", q.Commit, commit}, {"landed_in.target_oid", q.TargetOID, tip}} {
		if side.o.peeledKind != "commit" {
			return gitlet.GitAnswer{}, fmt.Errorf("%w: %s: %s names a %s; a landed-in read asks about commits",
				gitlet.ErrInvalid, side.field, side.rev, side.o.peeledKind)
		}
	}
	l := &landing{r: r, ctx: ctx, commit: commit.peeled}
	ans := &gitlet.LandedInAnswer{Commit: commit.peeled, Target: q.Target, TargetOID: q.TargetOID, At: q.At}
	a := gitlet.GitAnswer{Kind: gitlet.GitLandedIn, Rev: commit.peeled, LandedIn: ans}
	landed, err := l.ancestor(tip.peeled)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	ans.Landed = landed
	if !landed {
		return a, nil
	}
	if !r.prov.Indexed() {
		ans.Detail = fmt.Sprintf("%s keeps no index of pushes it can read for repository %s yet, so which push "+
			"landed the commit is not answered; ask again, or ask another Gitlet node", r.s.o.NodeID,
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
		return gitlet.GitAnswer{}, err
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
	k, last := len(bounds)-1, n-1
	for {
		lo := bounds[k]
		// The first move of [lo, last] that reaches the commit, given last does.
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
		k, last = k-1, lo-1
	}
}

// stopped is the answer of a search a check ended. Four ends are the search's
// own: MaxLandedProbes (errProbes), the read's one budget of git
// (gitlet.ErrSliceTooLarge), the answer falling due (gitlet.AnswerDue), and a
// lane busy past gitproc.SlotWait (gitproc.ErrBusy); read.failed answers the
// last two unready at the share (gitlet.UnreadyShare), as nothing else a check
// meets is, the read's unit of the share being taken first. Each is answered
// with no push, the oldest move known to keep the commit (last; n where none
// is) and how many forced moves below it, bounds[k]'s included, went
// unsearched. Any other failure is the read's answer. None refuses the read:
// the search already holds a move that keeps the commit, no landed-in read can
// narrow, and the next node would rerun it. Only the check against the tip
// answers them as any read's git does (too large, or unready).
func (l *landing) stopped(err error, bounds []int, k, last int) (int, string, error) {
	// within is what the search ran out of; the answer's Probes says how
	// many checks that was.
	var within string
	var unready *gitlet.ErrGitUnready
	switch {
	case errors.Is(err, errProbes):
		within = fmt.Sprintf("the %d ancestry checks a search runs", l.r.s.probes)
	case errors.Is(err, gitlet.ErrSliceTooLarge):
		within = fmt.Sprintf("the read's %s of git", l.r.s.gitBudget)
	case !errors.As(err, &unready) || unready.Reason != gitlet.UnreadyShare:
		return -1, "", err
	case gitlet.AnswerDue(l.ctx):
		within = "the read's attempt, whose answer fell due"
	default:
		within = fmt.Sprintf("this node's read lane, busy past %s", gitproc.SlotWait)
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
	if old == gitlet.ZeroOID {
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
// merge-base --is-ancestor`, by generation where the copy holds a
// commit-graph, run to what is left of the read's budget ([read.budget]) and
// charged what it ran ([read.failed]), never gitproc.HistoryWalk's bound,
// which no asker's seventeen-second attempt waits for (compare.go). The
// budget's end is gitlet.ErrSliceTooLarge; the answer falling due or a busy
// lane is unready at the share (read.failed). At the tip those are the read's
// answer; in the search, landing.stopped answers how far it reached.
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
		gitlet.ErrSliceTooLarge, l.commit, tip, l.r.s.gitBudget)
	if err = l.r.failed(l.ctx, "ancestry check", res, err, tooLong); err != nil {
		return false, err
	}
	return res.Exit == 0, nil
}
