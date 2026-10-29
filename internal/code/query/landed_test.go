package query_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/statelog"
)

// landedQ is a person's landed-in read of repo — whether commit is in target's
// history at tip, resolved at at — asked of the node it is handed as its
// walk's home or not: a home's read wants the repository's history (step 9),
// which a case about what the index holds does not.
func landedQ(repo, commit, target, tip string, at statelog.Position, home bool) code.GitQuery {
	return code.GitQuery{Kind: code.GitLandedIn, RepoID: repo, Asker: person, Home: home,
		LandedIn: &code.LandedInQuery{Commit: commit, Target: target, TargetOID: tip, At: at}}
}

// forceable is codetest.Settings with force-pushes to main allowed: what a
// case that rewrites main declares.
func forceable() code.RepoSettings {
	s := codetest.Settings()
	s.Protections = []code.Protection{{PushRule: code.PushRule{Pattern: "refs/heads/main",
		DirectPush: code.DirectPersons, AllowForce: true}}}
	return s
}

// tip is ref's commit in codetest.RepoName, resolved at the log's head, and
// the position of the push that moved it there (code.ResolvedRef.Moved): what a
// seat's tool hands a landed-in read.
func (x *fixture) tip(ref string) (string, statelog.Position) {
	x.t.Helper()
	a, err := x.f.Data().Served().ResolveRef(x.t.Context(), code.RefQuery{Repo: codetest.RepoName,
		Refs: []string{ref}, Fresh: codetest.Linearizable, Asker: person})
	if err == nil {
		err = a.Err()
	}
	if err != nil {
		x.t.Fatalf("%s of %s: %v", ref, codetest.RepoName, err)
	}
	return a.Refs[0].OID, a.Refs[0].Moved
}

// pushesMoving is every push the data node's rows hold that moved ref to oid,
// in log order.
func (x *fixture) pushesMoving(repo, ref, oid string) []code.IntroducedRow {
	x.t.Helper()
	q := code.ReadQuery{Kind: code.ReadIntroduced, Fresh: codetest.Linearizable,
		Introduced: &code.IntroducedQuery{RepoID: repo}}
	var found []code.IntroducedRow
	for {
		a, err := x.f.Data().Served().Read(x.t.Context(), q)
		if err != nil {
			x.t.Fatalf("the pushes of %s: %v", repo, err)
		}
		for _, row := range a.Introduced {
			for _, m := range row.Moves {
				if m.Ref == ref && m.New == oid {
					found = append(found, row)
				}
			}
		}
		if a.Next == "" {
			break
		}
		q.Cursor = a.Next
	}
	if len(found) == 0 {
		x.t.Fatalf("no push of %s moved %s to %s", repo, ref, oid)
	}
	return found
}

// pushVia pushes refspecs of the work tree to name through the i-th git node,
// whichever others are running.
func (x *fixture) pushVia(i int, name string, refspecs ...string) {
	x.t.Helper()
	url := x.f.GitNode(i).Node().URL() + "/git/" + name
	if res := x.p.PushArgs(x.t.Context(), x.work, []string{"--force"}, url, refspecs...); res.Err != nil {
		x.t.Fatalf("git push %v through node %d: %s", refspecs, i, res)
	}
}

// indexed waits until the i-th git node's index of repo can be read at the
// log's head: a run an earlier process wrote is read only once the backfill
// has checked it, and a read meanwhile is answered without the index rather
// than waiting on it.
func (x *fixture) indexed(i int, repo string) {
	x.t.Helper()
	c := x.f.Nodes()[i].Cache()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s, err := c.Sync(x.t.Context(), repo, codetest.Linearizable)
		if err != nil {
			x.t.Fatalf("sync node %d's copy of %s: %v", i, repo, err)
		}
		p, err := c.Provenance(s)
		s.Release()
		switch {
		case err == nil && p.Indexed():
			return
		case time.Now().After(deadline):
			x.t.Fatalf("node %d's index of %s is unreadable 30s on: %v", i, repo, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A LANDED COMMIT NAMES THE PUSH THAT LANDED IT, and the push that introduced
// it, which may be another branch's: a feature branch merged into main by a
// person's push is landed by that push and introduced by the branch's; a
// fast-forward lands and introduces a commit with one push; a commit a
// force-push removed from main and a later push put back is landed by the push
// that put it back, never the first; and a commit main does not hold is not
// landed.
//
// Mutation: answer Landed from IntroducedBy (a commit introduced anywhere is
// landed) — the side branch's commit is landed in main.
func TestALandedCommitNamesThePushThatLandedIt(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, forceable())
	x.commit(map[string]string{"base.txt": "base\n"}, "base")
	x.git("checkout", "-q", "-b", "feature")
	feat := x.p.Commit(x.work, map[string]string{"feature.txt": "f\n"}, "feature work")
	x.push(codetest.RepoName, "feature")
	x.git("checkout", "-q", "main")
	x.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	x.push(codetest.RepoName, "main")
	merged := x.p.Head(x.work)
	ff := x.commit(map[string]string{"ff.txt": "ff\n"}, "a fast-forward")
	back := x.commit(map[string]string{"back.txt": "back\n"}, "removed, then put back")
	x.git("reset", "-q", "--hard", ff)
	x.push(codetest.RepoName, "main")
	x.git("reset", "-q", "--hard", back)
	x.push(codetest.RepoName, "main")
	x.git("checkout", "-q", "-b", "side")
	side := x.p.Commit(x.work, map[string]string{"side.txt": "side\n"}, "never merged")
	x.push(codetest.RepoName, "side")

	repo := x.repo(codetest.RepoName)
	tip, at := x.tip("refs/heads/main")
	s := x.server(0, query.Options{})
	ask := func(commit string) *code.LandedInAnswer {
		t.Helper()
		a := mustRead(t, s, landedQ(repo, commit, "refs/heads/main", tip, at, false))
		if a.LandedIn == nil || a.LandedIn.Commit != commit || a.LandedIn.TargetOID != tip {
			t.Fatalf("the landed-in answer is %+v, want one about %s at %s", a.LandedIn, commit, tip)
		}
		return a.LandedIn
	}
	landedBy := func(name string, got *code.LandedInAnswer, want code.IntroducedRow) {
		t.Helper()
		if got.LandedBy == nil || got.LandedBy.PushID != want.PushID {
			t.Errorf("%s: landed by %+v (%q), want push %s", name, got.LandedBy, got.Detail, want.PushID)
		}
	}
	main := func(oid string) []code.IntroducedRow { return x.pushesMoving(repo, "refs/heads/main", oid) }

	f := ask(feat)
	landedBy("the merged branch's commit", f, main(merged)[0])
	if want := x.pushesMoving(repo, "refs/heads/feature", feat)[0]; f.IntroducedBy == nil ||
		f.IntroducedBy.PushID != want.PushID {
		t.Errorf("the merged branch's commit was introduced by %+v, want the branch's push %s", f.IntroducedBy,
			want.PushID)
	}
	// main moved to ff twice: by the push that fast-forwarded it there, and
	// by the force-push that took back off it — which did not land ff.
	fwd := ask(ff)
	landedBy("the fast-forward", fwd, main(ff)[0])
	if fwd.IntroducedBy == nil || fwd.LandedBy == nil || fwd.IntroducedBy.PushID != fwd.LandedBy.PushID {
		t.Errorf("the fast-forward was introduced by %+v and landed by %+v, want one push", fwd.IntroducedBy,
			fwd.LandedBy)
	}
	// main moved to back twice too: first landed, then put back after the
	// force-push removed it — the push that put it back landed it.
	puts := main(back)
	landedBy("the commit put back", ask(back), puts[len(puts)-1])
	if got := ask(side); got.Landed || got.LandedBy != nil {
		t.Errorf("a commit main does not hold is answered %+v, want not landed", got)
	}
}

// THE LANDING SEARCH IS BOUNDED, AND SAYS SO. Every forced move begins a
// segment the search may have to look past — one more ancestry check each,
// where the commit was already reachable before it — so past its bound of
// checks it stops, names no push, and says how far it reached: the oldest
// push it knows kept the commit, and how many forced moves it did not search
// past, rather than running on within the read's deadline. Here the bound is
// scaled to three, over five force-pushes of main that each keep the commit:
// the newest move is one check, and the two forced moves above the third
// rewrite one each.
//
// Mutation: search past the bound (ancestorBudgeted counting nothing) — the
// landing push is named.
func TestTheLandingSearchIsBoundedAndSaysSo(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, forceable())
	x.commit(map[string]string{"base.txt": "base\n"}, "base")
	kept := x.commit(map[string]string{"kept.txt": "kept\n"}, "the commit asked about")
	x.commit(map[string]string{"top.txt": "0\n"}, "a tip to rewrite")
	var rewrites []string
	for _, m := range []string{"a", "b", "c", "d", "e"} {
		x.git("commit", "-q", "--amend", "-m", "rewritten "+m)
		x.push(codetest.RepoName, "main")
		rewrites = append(rewrites, x.p.Head(x.work))
	}
	repo := x.repo(codetest.RepoName)
	tip, at := x.tip("refs/heads/main")
	s := x.server(0, query.Options{})
	query.SetLandedProbes(s, 3)

	a := mustRead(t, s, landedQ(repo, kept, "refs/heads/main", tip, at, false)).LandedIn
	reached := x.pushesMoving(repo, "refs/heads/main", rewrites[2])[0]
	switch {
	case !a.Landed:
		t.Fatalf("the commit is not landed in main: %+v", a)
	case a.LandedBy != nil:
		t.Errorf("past its bound the search named push %s", a.LandedBy.PushID)
	case a.Probes != 3:
		t.Errorf("the search ran %d checks, want its bound of 3", a.Probes)
	case !strings.Contains(a.Detail, "landed at or before push "+reached.PushID) ||
		!strings.Contains(a.Detail, "3 forced moves of refs/heads/main were not searched past"):
		t.Errorf("the answer's detail is %q, want it to name push %s, the third rewrite, and the 3 forced "+
			"moves at and below it", a.Detail, reached.PushID)
	}
}

// A LANDING SEARCH THE READ'S BUDGET ENDS SAYS HOW FAR IT REACHED, as one its
// bound of checks ends does: the search's checks share the read's one budget
// of git with the check against the tip, and a check that runs out what the
// budget had left leaves the search knowing the oldest move that kept the
// commit, which is answered — never the read refused as too large, which
// would throw that away and ask for a narrowing no landed-in read can make.
// The check against the tip is not the search's: run out, nothing is known,
// and the read is too large. Here main is rewritten by five force-pushes that
// each keep the commit, and the fourth ancestry check — the search's third,
// below the second rewrite from the top — runs the budget out.
//
// Mutations: answer every check the budget ends as the read too large
// (landing.stopped with only its bound's case) — the search's is refused, red;
// answer the tip's check the budget ends as a search's — the read is
// answered, red.
func TestALandingSearchTheBudgetEndsSaysHowFarItReached(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, forceable())
	x.commit(map[string]string{"base.txt": "base\n"}, "base")
	kept := x.commit(map[string]string{"kept.txt": "kept\n"}, "the commit asked about")
	x.commit(map[string]string{"top.txt": "0\n"}, "a tip to rewrite")
	var rewrites []string
	for _, m := range []string{"a", "b", "c", "d", "e"} {
		x.git("commit", "-q", "--amend", "-m", "rewritten "+m)
		x.push(codetest.RepoName, "main")
		rewrites = append(rewrites, x.p.Head(x.work))
	}
	repo := x.repo(codetest.RepoName)
	tip, at := x.tip("refs/heads/main")
	q := landedQ(repo, kept, "refs/heads/main", tip, at, false)
	reached := x.pushesMoving(repo, "refs/heads/main", rewrites[3])[0]
	pool := x.f.Nodes()[0].Pool()
	searching := x.server(0, query.Options{Git: &checkEnding{Pool: pool, n: 4}})
	tipping := x.server(0, query.Options{Git: &checkEnding{Pool: pool, n: 1}})

	t.Run("a check of the search", func(t *testing.T) {
		a := mustRead(t, searching, q).LandedIn
		switch {
		case !a.Landed:
			t.Fatalf("the commit is not landed in main: %+v", a)
		case a.LandedBy != nil:
			t.Errorf("a search its budget ended named push %s", a.LandedBy.PushID)
		case a.Probes != 3:
			t.Errorf("the search ran %d checks, want the 3 the budget ended it on", a.Probes)
		case !strings.Contains(a.Detail, "landed at or before push "+reached.PushID) ||
			!strings.Contains(a.Detail, "4 forced moves of refs/heads/main were not searched past within "+
				"the read's 10s of git"):
			t.Errorf("the answer's detail is %q, want it to name push %s, the second rewrite from the top, the "+
				"4 forced moves at and below it, and the read's budget", a.Detail, reached.PushID)
		}
	})
	t.Run("the check against the tip", func(t *testing.T) {
		if _, err := tipping.Read(t.Context(), q); !errors.Is(err, code.ErrSliceTooLarge) {
			t.Errorf("a read whose check against the tip ran out its budget = %v, want code.ErrSliceTooLarge", err)
		}
	})
}

// checkEnding is a query.Pool whose n-th ancestry check — the first a
// landed-in read runs being its check against the tip — runs out all the
// read's budget of git had left, answered as gitproc answers a run killed at
// its deadline; every other process runs on the pool beneath.
type checkEnding struct {
	query.Pool
	n    int64
	seen atomic.Int64
}

func (p *checkEnding) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if c.Args[0] == "merge-base" && p.seen.Add(1) == p.n {
		return gitproc.Result{Exit: -1, Took: c.Deadline}, fmt.Errorf("gitproc: git merge-base ran past its %v "+
			"deadline and was killed: %w", c.Deadline, gitproc.ErrDeadline)
	}
	return p.Pool.Run(ctx, c)
}

// A LANDING THE INDEX DOES NOT HOLD IS NOT ATTRIBUTED. The landing is a move
// the commit was not reachable before; where the commit already stood on the
// branch before the oldest move the node's index holds of it, or before the
// first move above the index's gap — moves never read — the answer names no
// push and says which range the landing lies in, never the first move the
// index happens to hold.
//
// Mutation: take the view's first move, or the first above the gap, as the
// landing without asking its old value — the first indexed push is named.
func TestALandingTheIndexDoesNotHoldIsNotAttributed(t *testing.T) {
	t.Parallel()
	t.Run("before the index begins", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
		early := x.commit(map[string]string{"a.txt": "a\n"}, "landed before the index")
		x.commit(map[string]string{"b.txt": "b\n"}, "also before")
		// A node that read nothing: its index begins at its first round,
		// and nothing reads the history below it until a home's read wants
		// it.
		g, dir := x.f.GitNode(0), x.f.Nodes()[0].CacheDir()
		g.Stop(t.Context())
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		g.Restart(t)
		late := x.commit(map[string]string{"c.txt": "c\n"}, "landed after it")
		repo := x.repo(codetest.RepoName)
		tip, at := x.tip("refs/heads/main")
		s := x.server(0, query.Options{})
		a := mustRead(t, s, landedQ(repo, early, "refs/heads/main", tip, at, false)).LandedIn
		first := x.pushesMoving(repo, "refs/heads/main", late)[0]
		switch {
		case !a.Landed:
			t.Fatalf("the early commit is not landed in main: %+v", a)
		case a.LandedBy != nil:
			t.Errorf("a commit landed before the index began is named landed by push %s", a.LandedBy.PushID)
		case !strings.Contains(a.Detail, "landed before push "+first.PushID) ||
			!strings.Contains(a.Detail, "the oldest move of refs/heads/main this node indexes"):
			t.Errorf("the answer's detail is %q, want the range: before push %s", a.Detail, first.PushID)
		}
	})

	t.Run("inside the gap", func(t *testing.T) {
		t.Parallel()
		var blocked atomic.Bool
		x := newFixture(t, codetest.SoloOptions{
			Shape: codetest.Shape{GitNodes: 2},
			// The backfill's pages fail while the case holds the gap open.
			WrapReads: func(next codetest.Reads) codetest.Reads {
				return func(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
					if blocked.Load() && q.Kind == code.ReadIntroduced {
						return code.ReadAnswer{}, errors.New("the data node is unreachable for the backfill")
					}
					return next(ctx, q)
				}
			},
		}, codetest.Settings())
		before := x.commit(map[string]string{"a.txt": "a\n"}, "indexed")
		repo := x.repo(codetest.RepoName)
		x.synced(0, repo)
		// Down while another node takes a push: its first round back reads
		// the repository whole, past a range of pushes it never read.
		g := x.f.GitNode(0)
		g.Stop(t.Context())
		blocked.Store(true)
		inGap := x.p.Commit(x.work, map[string]string{"b.txt": "b\n"}, "landed while the node was down")
		x.pushVia(1, codetest.RepoName, "main")
		g.Restart(t)
		after := x.commit(map[string]string{"c.txt": "c\n"}, "landed after the restart")
		x.indexed(0, repo)
		tip, at := x.tip("refs/heads/main")
		s := x.server(0, query.Options{})
		a := mustRead(t, s, landedQ(repo, inGap, "refs/heads/main", tip, at, false)).LandedIn
		below := x.pushesMoving(repo, "refs/heads/main", before)[0]
		above := x.pushesMoving(repo, "refs/heads/main", after)[0]
		switch {
		case !a.Landed:
			t.Fatalf("the commit is not landed in main: %+v", a)
		case a.LandedBy != nil:
			t.Errorf("a commit landed inside the gap is named landed by push %s", a.LandedBy.PushID)
		case !strings.Contains(a.Detail, "landed between push "+below.PushID) ||
			!strings.Contains(a.Detail, "and push "+above.PushID) ||
			!strings.Contains(a.Detail, "a range this node has not indexed yet"):
			t.Errorf("the answer's detail is %q, want the range between push %s and push %s", a.Detail,
				below.PushID, above.PushID)
		}
	})
}

// AN ANSWER NAMES THE PUSH THAT INTRODUCED ITS COMMITS: a listing of a commit's
// tree, a diff of two and a comparison each carries the push that introduced
// every commit it names, from the node's index, and what that index holds.
//
// Mutation: decorate nothing (index returning at once) — the answers name no
// push.
func TestAnAnswerNamesThePushThatIntroducedItsCommits(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"a.txt": "a\n"}, "base")
	head := x.commit(map[string]string{"a.txt": "b\n"}, "head")
	repo := x.repo(codetest.RepoName)
	// The copy level with the log: a push's own pack is in it from the
	// moment the push is answered, but its row is folded into the index by
	// the round that reads it — and a read decorates from the index as it
	// stands, never waiting on it.
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	pushOf := map[string]string{
		base: x.pushesMoving(repo, "refs/heads/main", base)[0].PushID,
		head: x.pushesMoving(repo, "refs/heads/main", head)[0].PushID,
	}
	for _, tc := range []struct {
		name    string
		q       code.GitQuery
		commits []string
	}{
		{"a tree", treeQ(repo, code.TreeQuery{Rev: head}), []string{head}},
		{"a diff", diffQ(repo, code.DiffQuery{Base: base, Head: head}), []string{base, head}},
		{"a comparison", compareQ(repo, base, head), []string{base, head}},
	} {
		a := mustRead(t, s, tc.q)
		if a.Coverage == nil {
			t.Errorf("%s carries no coverage", tc.name)
		}
		for _, c := range tc.commits {
			if got := a.PushedBy[c].PushID; got != pushOf[c] {
				t.Errorf("%s names %q as introducing %s, want push %s", tc.name, got, c, pushOf[c])
			}
		}
	}
}

// A HOME'S READ WANTS THE HISTORY: a read this node answered as its walk's
// home marks the repository's history wanted, so the node reads it back — the
// one node every walk asks first, once per fleet — and a read asked of it as
// any other node does not.
//
// Mutation: want the history on every read — the read not asked as the home
// wants it too.
func TestAHomesReadWantsTheHistory(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	head := x.commit(map[string]string{"a.txt": "a\n"}, "base")
	repo := x.repo(codetest.RepoName)
	want := &wanting{Cache: x.f.Nodes()[0].Cache()}
	s := x.server(0, query.Options{Cache: want})
	notHome := treeQ(repo, code.TreeQuery{Rev: head})
	notHome.Home = false
	mustRead(t, s, notHome)
	if got := want.of(); len(got) != 0 {
		t.Fatalf("a read not asked as the home wanted the history of %v", got)
	}
	mustRead(t, s, treeQ(repo, code.TreeQuery{Rev: head}))
	if got := want.of(); len(got) != 1 || got[0] != repo {
		t.Errorf("a home's read wanted the history of %v, want [%s]", got, repo)
	}
}

// wanting is the node's cache, recording every repository a read wanted the
// history of.
type wanting struct {
	*cache.Cache
	mu     sync.Mutex
	wanted []string
}

func (w *wanting) WantHistory(repoID string) {
	w.mu.Lock()
	w.wanted = append(w.wanted, repoID)
	w.mu.Unlock()
	w.Cache.WantHistory(repoID)
}

func (w *wanting) of() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.wanted...)
}
