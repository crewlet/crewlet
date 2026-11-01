package home_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE HOME'S MERGE OVER A REAL COPY: a solo fleet's one git node — its cache
// and its pool — holding a repository a person pushed branches to, and the
// home's loop over it, its estate the stand-in that lists the changes a case
// makes due (estate): so each case merges real commits with the git node's
// own git, in the scratch the loop makes beside the node's copy, and reads
// what the loop asked to be recorded.

// gitHome is a solo fleet holding codetest.RepoName, a person who pushes to
// it and their work tree, the repository's id once pushed, and the stand-in
// estate a home lists its changes from, its position the rows' own.
type gitHome struct {
	t    testing.TB
	f    *codetest.SoloFleet
	p    *codetest.Person
	work string
	repo string
	e    *estate
	// logs, where set, is what the home's loop logs to.
	logs *lines
}

// newGitHome is a fleet holding codetest.RepoName declared under settings.
func newGitHome(t testing.TB, settings code.RepoSettings) *gitHome {
	t.Helper()
	f := codetest.SoloNode(t, codetest.SoloOptions{})
	f.Declare(t, code.Declared{Name: codetest.RepoName, Settings: settings})
	h := &gitHome{t: t, f: f, p: codetest.NewPerson(t, f, "ceo"), e: newEstate()}
	h.work = h.p.Init()
	return h
}

// node is the fleet's one git node.
func (h *gitHome) node() codetest.Node { return h.f.Nodes()[0] }

// commit commits files on the work tree's branch, answering the commit.
func (h *gitHome) commit(files map[string]string, message string) string {
	h.t.Helper()
	return h.p.Commit(h.work, files, message)
}

// git runs the person's git in the work tree.
func (h *gitHome) git(args ...string) string {
	h.t.Helper()
	return h.p.Git().Run(h.t, h.work, args...)
}

// push pushes refspecs through the git node, forced, and reads the
// repository's rows at the log's head: its id, and the position its listing
// is answered at.
func (h *gitHome) push(refspecs ...string) {
	h.t.Helper()
	url := h.node().URL() + "/git/" + codetest.RepoName
	if res := h.p.PushArgs(context.Background(), h.work, []string{"--force"}, url, refspecs...); res.Err != nil {
		h.t.Fatalf("git push %v: %s", refspecs, res)
	}
	rows, err := h.f.Rows(context.Background(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		h.t.Fatalf("the rows of %s: %v", codetest.RepoName, err)
	}
	h.repo = rows.Repo.ID
	h.e.mu.Lock()
	h.e.at = rows.Position
	h.e.mu.Unlock()
}

// due is a change's head revision — head, into main at tip — as a home's
// listing names it, owing its merge alone.
func (h *gitHome) due(head, tip string) code.ChangeDue {
	return code.ChangeDue{ID: uuid.Must(uuid.NewV7()).String(), Number: 1, Revision: 1, Head: head,
		SourceRef: "refs/heads/change-" + head[:8], TargetRef: "refs/heads/main", Tip: tip,
		TipPosition: h.e.at, MergeDue: true}
}

// mergedTree is the person's own git's merge of head into tip: the id of the
// tree it writes, which is the same tree whichever repository writes it.
func (h *gitHome) mergedTree(tip, head string) string {
	h.t.Helper()
	out, stderr, err := h.p.Git().Try(h.t, h.work, nil, "merge-tree", "--write-tree",
		"--allow-unrelated-histories", tip, head)
	tree, _, _ := strings.Cut(out, "\n")
	if len(tree) != 40 {
		h.t.Fatalf("git merge-tree %s %s answered %q (%v): %s", tip, head, out, err, stderr)
	}
	return tree
}

// incarnation is the git node's copy of the repository, synced to the log's
// head: the directory every merge's scratch leans on.
func (h *gitHome) incarnation() string {
	h.t.Helper()
	s, err := h.node().Cache().Sync(context.Background(), h.repo, codetest.Linearizable)
	if err != nil {
		h.t.Fatalf("sync the node's copy of %s: %v", h.repo, err)
	}
	defer s.Release()
	return s.Dir
}

// holds reports whether the repository at dir holds the object id.
func (h *gitHome) holds(dir, id string) bool {
	h.t.Helper()
	_, _, err := h.p.Git().Try(h.t, "", nil, "--git-dir="+dir, "cat-file", "-e", id)
	return err == nil
}

// homeLoop is the git node's loop over h's estate — home to the repository,
// merging over the node's own cache and pool — its backstop backstop and its
// clock now, started; tune sets what a case scales before the start.
func (h *gitHome) homeLoop(t *testing.T, n *nudges, backstop time.Duration, now func() time.Time,
	tune ...func(*home.Loop)) (*home.Loop, *metrics.Recorder) {

	t.Helper()
	node := h.node()
	p := &placement{homes: map[string]string{h.repo: node.ID()}}
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	o := home.Options{Estate: h.e, Placement: asNode{placement: p, node: node.ID()}, Nudges: n,
		Cache: node.Cache(), Git: node.Pool(), Chains: unqueued{t: t}, Tasks: unqueued{t: t}, NodeID: node.ID(),
		Share:   units(2),
		Metrics: rec, Now: now}
	if h.logs != nil {
		o.Logger = slog.New(slog.NewTextHandler(h.logs, nil))
	}
	l, err := home.New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	home.SetBackstop(l, backstop)
	for _, f := range tune {
		f(l)
	}
	l.Start()
	t.Cleanup(func() { l.Stop(context.WithoutCancel(t.Context())) })
	return l, rec
}

// waitTurns waits until rec counts n turns of the changes worker ended, or
// fails t: a turn's merges have removed their scratches by then.
func waitTurns(t *testing.T, rec *metrics.Recorder, n uint64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for counted(rec, metrics.CodeHomePasses, metrics.Attrs{"worker": home.WorkerChanges}) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the home ended %d turns in 30s, want %d", counted(rec, metrics.CodeHomePasses,
				metrics.Attrs{"worker": home.WorkerChanges}), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// looseObjects is every loose object a repository's directory holds, by path.
func looseObjects(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	fans, err := filepath.Glob(filepath.Join(dir, "objects", "[0-9a-f][0-9a-f]", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fans {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = info.Size()
	}
	return out
}

// MERGEABILITY RUNS IN A SCRATCH: a home's merge of a change into its target —
// one that merges clean and one that conflicts — writes the merged tree, and
// every blob a conflict leaves marked, into a scratch of its own beside the
// node's copy and never into the copy, whose objects are its rows' alone (the
// cache's writers gate): after both merges are recorded the copy holds no
// object it did not hold before — no loose object, and neither merged tree —
// and the scratches are gone with the turn that made them.
//
// Mutation: run the merge-tree in the copy's own directory (the synced
// incarnation) rather than the scratch, and the copy holds the merged trees.
func TestMergeabilityRunsInAScratch(t *testing.T) {
	t.Parallel()
	h := newGitHome(t, codetest.Settings())
	base := h.commit(map[string]string{"a.go": "a\n", "b.go": "b\n"}, "base")
	h.git("checkout", "-q", "-b", "change-1", base)
	clean := h.commit(map[string]string{"a.go": "a, changed\n"}, "a change")
	h.git("checkout", "-q", "-b", "change-2", base)
	conflicted := h.commit(map[string]string{"b.go": "b, one way\n"}, "a conflicting change")
	h.git("checkout", "-q", "main")
	tip := h.commit(map[string]string{"b.go": "b, the other way\n", "c.go": "c\n"}, "main moves")
	h.push("refs/heads/main", "refs/heads/change-1", "refs/heads/change-2")

	inc := h.incarnation()
	before := looseObjects(t, inc)
	trees := map[string]string{clean: h.mergedTree(tip, clean), conflicted: h.mergedTree(tip, conflicted)}
	for head, tree := range trees {
		if h.holds(inc, tree) {
			t.Fatalf("the premise: the node's copy holds %s's merged tree %s before any merge", head, tree)
		}
	}
	h.e.owe(h.repo, h.due(clean, tip))
	h.e.owe(h.repo, h.due(conflicted, tip))
	_, rec := h.homeLoop(t, newNudges(), time.Hour, nil)
	merges := h.e.waitMerged(t, 2, 30*time.Second)
	waitTurns(t, rec, 1)

	states := map[string]code.MergeState{}
	for _, m := range merges {
		states[m.HeadOID] = m.State
	}
	if states[clean] != code.MergeClean || states[conflicted] != code.MergeConflict {
		t.Fatalf("the merges recorded %v, want %s clean and %s in conflict", states, clean, conflicted)
	}
	if after := looseObjects(t, inc); !maps.Equal(before, after) {
		t.Errorf("the node's copy held loose objects %v before its merges and %v after: a merge wrote into "+
			"the copy", before, after)
	}
	for head, tree := range trees {
		if h.holds(inc, tree) {
			t.Errorf("the node's copy holds %s's merged tree %s: the merge ran in the copy, not a scratch", head,
				tree)
		}
	}
	scratches, err := os.ReadDir(filepath.Join(h.node().CacheDir(), "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scratches) != 0 {
		var names []string
		for _, s := range scratches {
			names = append(names, s.Name())
		}
		t.Errorf("the merges left scratches %v behind their turn", names)
	}
}

// A MERGE'S LANDING THAT DECIDED NOTHING FAILS THE TURN: a change owing its
// merge alone whose head its target came to hold — found by the merge's
// ancestry, its landing asked of the committer — whose landing the committer
// answers busy ends its turn FAILED, counted so and logged
// code_home_turn_failed, with no merge recorded of it.
//
// Mutation: drop the landing's error on the merge branch of the change's step
// (answer `true` and the other steps' errors alone once the merge found it
// landed), and the turn counts worked.
func TestAMergesLandingThatDecidedNothingFailsTheTurn(t *testing.T) {
	t.Parallel()
	h := newGitHome(t, codetest.Settings())
	base := h.commit(map[string]string{"a.go": "a\n"}, "base")
	h.git("checkout", "-q", "-b", "change", base)
	head := h.commit(map[string]string{"a.go": "a, changed\n"}, "a change")
	h.git("checkout", "-q", "main")
	h.git("merge", "-q", "--ff-only", "change")
	tip := h.commit(map[string]string{"b.go": "b\n"}, "main moves past the change")
	h.push("refs/heads/main", "refs/heads/change")
	h.e.owe(h.repo, h.due(head, tip))
	h.e.mu.Lock()
	h.e.landRefusal = code.ChangeRefusalBusy
	h.e.mu.Unlock()
	h.logs = &lines{}
	_, rec := h.homeLoop(t, newNudges(), time.Hour, nil)
	waitTurns(t, rec, 1)
	assertTurnFailed(t, rec, h.logs)
	if _, landings := h.e.wrote(); len(landings) != 1 || landings[0].HeadOID != head || landings[0].TargetOID != tip {
		t.Errorf("the turn asked the landings %+v, want the change's once, at %s into %s", landings, head, tip)
	}
	if merged := h.e.merged(); len(merged) != 0 {
		t.Errorf("the turn recorded %+v of a change its ancestry found landed, want no merge", merged)
	}
}

// A MERGE IS TIMED ALONE: crewlet.code.home.merge.duration observes a home's
// merge when the merge answers, never the record of it on the change — a
// change writer that takes an hour on the home's clock adds nothing to it —
// and observes a preview only once its merge-tree ran: a preview its asker's
// grant refuses is no merge, and observes nothing.
//
// Mutations: observe the home's merge once its record has answered, and it
// takes the writer's hour; observe a preview from its start, and the refused
// one is observed as a failed merge.
func TestAMergeIsTimedAlone(t *testing.T) {
	t.Parallel()
	settings := codetest.Settings()
	settings.Visibility = code.VisibilityUnit
	h := newGitHome(t, settings)
	base := h.commit(map[string]string{"a.go": "a\n"}, "base")
	h.git("checkout", "-q", "-b", "change", base)
	head := h.commit(map[string]string{"a.go": "a, changed\n"}, "a change")
	h.git("checkout", "-q", "main")
	tip := h.commit(map[string]string{"b.go": "b\n"}, "main moves")
	h.push("refs/heads/main", "refs/heads/change")

	c := &clock{now: time.Now().UTC()}
	h.e.mu.Lock()
	h.e.recording = func() { c.advance(time.Hour) }
	h.e.mu.Unlock()
	h.e.owe(h.repo, h.due(head, tip))
	_, rec := h.homeLoop(t, newNudges(), time.Hour, c.Now)
	h.e.waitMerged(t, 1, 30*time.Second)
	waitTurns(t, rec, 1)
	n, sum := observed(rec, metrics.CodeHomeMergeDuration, metrics.Attrs{"work": "mergeability", "outcome": "clean"})
	if n != 1 || sum >= float64(time.Hour.Milliseconds()) {
		t.Errorf("the home's merge was observed %d times summing %.0f ms, want once, without its record's hour", n,
			sum)
	}

	prec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	node := h.node()
	p, err := home.NewPreviews(home.PreviewOptions{NodeID: node.ID(), Cache: node.Cache(), Git: node.Pool(),
		Share: node.Share(), Roster: codetest.FixtureRoster, Fetched: (&fetches{}).record, Metrics: prec})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop(context.WithoutCancel(t.Context())) })
	if _, err := p.MergePreview(t.Context(), code.MergePreviewQuery{RepoID: h.repo, Base: tip, Head: head,
		Home: true, Asker: code.Principal{Kind: code.PrincipalAgent, Seat: "outsider"}}); !errors.Is(err,
		code.ErrNotPermitted) {
		t.Fatalf("the outsider's preview answered %v, want %v", err, code.ErrNotPermitted)
	}
	if n, _ := observed(prec, metrics.CodeHomeMergeDuration, metrics.Attrs{"work": "preview"}); n != 0 {
		t.Errorf("a preview its asker's grant refused was observed %d times as a merge, want none", n)
	}
	if _, err := p.MergePreview(t.Context(), code.MergePreviewQuery{RepoID: h.repo, Base: tip, Head: head,
		Home: true, Asker: person}); err != nil {
		t.Fatal(err)
	}
	if n, _ := observed(prec, metrics.CodeHomeMergeDuration, metrics.Attrs{"work": "preview",
		"outcome": "clean"}); n != 1 {
		t.Errorf("a preview that merged was observed %d times, want once, clean", n)
	}
}

// A CONFLICT NAMES ITS PATHS: a home's merge that leaves paths conflicted
// records each once, in git's order — a content conflict, a modification
// against a deletion, two adds of one path, a nested path — and never a path
// that merged cleanly beside them; a path a record cannot carry (not UTF-8) is
// counted rather than listed; past MaxConflictPaths the rest are counted, the
// list cut where it fills; and a history sharing no commit with its target's
// merges from the empty tree, every path both sides add differently a
// conflict and every other path merged.
//
// Mutations: list every record merge-tree writes rather than the paths alone
// (drop --name-only), and each conflict's stages arrive as paths; refuse an
// unrelated history (drop --allow-unrelated-histories), and its merge fails
// every pass rather than record what its facts measure.
func TestAConflictNamesItsPaths(t *testing.T) {
	t.Parallel()
	h := newGitHome(t, codetest.Settings())
	files := map[string]string{"a.go": "a\n", "b.go": "b\n", "docs/x.md": "x\n", "keep.go": "k\n",
		"bad\xff.go": "bad\n"}
	many := func(side string) map[string]string {
		out := map[string]string{}
		for i := range code.MaxConflictPaths + 5 {
			out[fmt.Sprintf("many/f%03d.go", i)] = side + "\n"
		}
		return out
	}
	maps.Copy(files, many("base"))
	base := h.commit(files, "base")

	h.git("checkout", "-q", "-b", "change-1", base)
	h.git("rm", "-q", "b.go")
	one := h.commit(map[string]string{"a.go": "a, one way\n", "docs/x.md": "x, one way\n",
		"new.go": "new, one way\n", "bad\xff.go": "bad, one way\n"}, "conflicts of every kind")
	h.git("checkout", "-q", "-b", "change-2", base)
	two := h.commit(many("one way"), "a conflict past the bound")
	h.git("checkout", "-q", "--orphan", "change-3")
	h.git("rm", "-rqf", ".")
	three := h.commit(map[string]string{"a.go": "an unrelated a\n", "solo.go": "only here\n"},
		"a history of its own")
	h.git("checkout", "-q", "main")
	moved := map[string]string{"a.go": "a, the other way\n", "b.go": "b, changed\n",
		"docs/x.md": "x, the other way\n", "new.go": "new, the other way\n", "keep.go": "k, changed\n",
		"bad\xff.go": "bad, the other way\n"}
	maps.Copy(moved, many("the other way"))
	tip := h.commit(moved, "main moves")
	h.push("refs/heads/main", "refs/heads/change-1", "refs/heads/change-2", "refs/heads/change-3")

	for _, head := range []string{one, two, three} {
		h.e.owe(h.repo, h.due(head, tip))
	}
	h.homeLoop(t, newNudges(), time.Hour, nil)
	got := map[string]code.Mergeability{}
	for _, m := range h.e.waitMerged(t, 3, 60*time.Second) {
		got[m.HeadOID] = m
	}
	var bound []string
	for i := range code.MaxConflictPaths {
		bound = append(bound, fmt.Sprintf("many/f%03d.go", i))
	}
	for name, tc := range map[string]struct {
		head      string
		conflicts []string
		omitted   int64
	}{
		"conflicts of every kind":   {one, []string{"a.go", "b.go", "docs/x.md", "new.go"}, 1},
		"a conflict past the bound": {two, bound, 5},
		"a history of its own":      {three, []string{"a.go"}, 0},
	} {
		m, ok := got[tc.head]
		switch {
		case !ok:
			t.Errorf("%s: no mergeability was recorded of %s", name, tc.head)
		case m.State != code.MergeConflict || !slices.Equal(m.Conflicts, tc.conflicts) ||
			m.ConflictsOmitted != tc.omitted:
			t.Errorf("%s: recorded %s naming %v and %d more, want a conflict naming %v and %d more", name, m.State,
				m.Conflicts, m.ConflictsOmitted, tc.conflicts, tc.omitted)
		case m.TargetOID != tip || m.Node != h.node().ID():
			t.Errorf("%s: recorded into %s by %s, want into the tip %s by the home", name, m.TargetOID, m.Node, tip)
		}
	}
}

// clock is a loop's clock, moved by its case.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// A TARGET MOVE RECHECKS WITHIN ITS BUDGET: the changes a moved target asks
// merged again are merged the oldest first, and no more of them in one window
// of the home's backstop than a repository's budget — whichever turns, a
// pass's or a nudge's, run them — each listing asking only what is left of the
// budget, and asked at the home's clock less its refresh; a new head
// revision's merge takes none of the budget, merged at once while the budget
// is spent; and the next window merges the recheck the last one had no room
// for.
//
// Mutations: spend nothing on a recheck, and the third is merged in the first
// window; count a new revision's merge against the budget, and it waits for
// the next window; ask the listing for the whole budget every turn, and the
// nudged turn merges past it.
func TestATargetMoveRechecksWithinItsBudget(t *testing.T) {
	t.Parallel()
	h := newGitHome(t, codetest.Settings())
	base := h.commit(map[string]string{"a.go": "a\n"}, "base")
	var heads []string
	for i := range 4 {
		h.git("checkout", "-q", "-b", fmt.Sprintf("change-%d", i), base)
		heads = append(heads, h.commit(map[string]string{fmt.Sprintf("c%d.go", i): "change\n"}, "a change"))
	}
	h.git("checkout", "-q", "main")
	tip := h.commit(map[string]string{"main.go": "moved\n"}, "main moves")
	h.push("refs/heads/main", "refs/heads/change-0", "refs/heads/change-1", "refs/heads/change-2",
		"refs/heads/change-3")

	c := &clock{now: time.Now().UTC()}
	checked := c.Now().Add(-2 * home.MergeabilityRefresh)
	for i, head := range heads[:3] {
		again := h.due(head, tip)
		again.MergeDue, again.Checked = false, checked.Add(time.Duration(i)*time.Second)
		h.e.recheck(h.repo, again)
	}
	n := newNudges()
	_, rec := h.homeLoop(t, n, time.Hour, c.Now, func(l *home.Loop) { home.SetMergesPerPass(l, 2) })

	merged := h.e.waitMerged(t, 2, 30*time.Second)
	waitTurns(t, rec, 1)
	if got := []string{merged[0].HeadOID, merged[1].HeadOID}; !slices.Equal(got, heads[:2]) ||
		len(h.e.merged()) != 2 {
		t.Fatalf("the first window merged %v, want the two oldest rechecks alone, %v", h.e.merged(), heads[:2])
	}
	h.e.mu.Lock()
	asked := slices.Clone(h.e.asked)
	h.e.mu.Unlock()
	for _, q := range asked {
		if want := c.Now().Add(-home.MergeabilityRefresh); !q.RecheckBefore.Equal(want) {
			t.Errorf("a listing asked rechecks recorded at or before %s, want the home's clock less its "+
				"refresh, %s", q.RecheckBefore, want)
		}
	}
	if last := asked[len(asked)-1]; last.RepoID != h.repo || last.Rechecks != 2 {
		t.Fatalf("the first turn asked %+v, want the repository's listing asking its whole budget of 2", last)
	}

	// THE BUDGET SPENT: a nudged turn in the same window merges the new
	// revision it owes, and no recheck.
	newRevision := h.due(heads[3], tip)
	h.e.owe(h.repo, newRevision)
	n.subject(t, h.repo, 90)
	merged = h.e.waitMerged(t, 3, 30*time.Second)
	waitTurns(t, rec, 2)
	if merged[2].HeadOID != heads[3] || len(h.e.merged()) != 3 {
		t.Fatalf("the nudged turn merged %+v, want the new revision's merge alone", h.e.merged()[2:])
	}
	h.e.mu.Lock()
	last := h.e.asked[len(h.e.asked)-1]
	h.e.mu.Unlock()
	if last.Rechecks != 0 {
		t.Fatalf("the nudged turn asked %d rechecks, want none: the window's budget is spent", last.Rechecks)
	}

	// THE NEXT WINDOW merges the recheck the first had no room for.
	c.advance(time.Hour)
	n.subject(t, h.repo, 91)
	merged = h.e.waitMerged(t, 4, 30*time.Second)
	if merged[3].HeadOID != heads[2] {
		t.Fatalf("the next window merged %s, want the recheck left over, %s", merged[3].HeadOID, heads[2])
	}
	for _, m := range merged {
		if m.TargetOID != tip || m.State != code.MergeClean {
			t.Errorf("a merge was recorded %s into %s, want clean into the moved tip %s", m.State, m.TargetOID, tip)
		}
	}
}

// previewsOn is a node's merge previews over its cache, pool and share, the
// fixture chart judging a seat, recording what they record into fetched.
func previewsOn(t *testing.T, node codetest.Node, roster func() code.Roster, fetched *fetches) *home.Previews {
	t.Helper()
	p, err := home.NewPreviews(home.PreviewOptions{NodeID: node.ID(), Cache: node.Cache(), Git: node.Pool(),
		Share: node.Share(), Roster: roster, Fetched: fetched.record})
	if err != nil {
		t.Fatalf("NewPreviews: %v", err)
	}
	t.Cleanup(func() { p.Stop(context.WithoutCancel(t.Context())) })
	return p
}

// fetches is what a node's previews recorded as reads.
type fetches struct {
	mu   sync.Mutex
	seen []types.CodeFetched
}

func (f *fetches) record(_ context.Context, e types.CodeFetched) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, e)
}

func (f *fetches) all() []types.CodeFetched {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen)
}

// person is the person a preview is asked by, and alice a seat of the unit that
// owns the repository (codetest.FixtureRoster).
var (
	person = code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ceo", Seat: "ceo"}
	alice  = code.Principal{Kind: code.PrincipalAgent, Seat: codetest.FixtureAlice}
)

// A MERGE PREVIEW IS THE SAME MERGE: what a comparison's preview of merging a
// head into a tip answers — its state, the conflicted paths it holds and how
// many more there were — is what the repository's home records of the same
// two commits as a change's mergeability, for a clean merge, a conflict, a
// conflict past MaxConflictPaths and a history sharing no commit with its
// target's alike.
//
// Mutation: give the preview a merge of its own that refuses an unrelated
// history (no --allow-unrelated-histories), and the preview of a history of
// its own fails where its change records a conflict.
func TestAMergePreviewIsTheSameMerge(t *testing.T) {
	t.Parallel()
	h := newGitHome(t, codetest.Settings())
	files := map[string]string{"a.go": "a\n", "b.go": "b\n"}
	for i := range code.MaxConflictPaths + 3 {
		files[fmt.Sprintf("many/f%03d.go", i)] = "base\n"
	}
	base := h.commit(files, "base")
	h.git("checkout", "-q", "-b", "clean", base)
	clean := h.commit(map[string]string{"a.go": "a, changed\n"}, "a clean change")
	h.git("checkout", "-q", "-b", "conflict", base)
	conflict := h.commit(map[string]string{"b.go": "b, one way\n"}, "a conflicting change")
	h.git("checkout", "-q", "-b", "wide", base)
	wide := map[string]string{}
	for i := range code.MaxConflictPaths + 3 {
		wide[fmt.Sprintf("many/f%03d.go", i)] = "one way\n"
	}
	wideHead := h.commit(wide, "a wide conflict")
	h.git("checkout", "-q", "--orphan", "unrelated")
	h.git("rm", "-rqf", ".")
	unrelated := h.commit(map[string]string{"b.go": "an unrelated b\n", "solo.go": "only here\n"}, "its own")
	h.git("checkout", "-q", "main")
	moved := map[string]string{"b.go": "b, the other way\n", "c.go": "c\n"}
	for i := range code.MaxConflictPaths + 3 {
		moved[fmt.Sprintf("many/f%03d.go", i)] = "the other way\n"
	}
	tip := h.commit(moved, "main moves")
	h.push("refs/heads/main", "refs/heads/clean", "refs/heads/conflict", "refs/heads/wide",
		"refs/heads/unrelated")

	heads := []string{clean, conflict, wideHead, unrelated}
	for _, head := range heads {
		h.e.owe(h.repo, h.due(head, tip))
	}
	h.homeLoop(t, newNudges(), time.Hour, nil)
	recorded := map[string]code.Mergeability{}
	for _, m := range h.e.waitMerged(t, len(heads), 60*time.Second) {
		recorded[m.HeadOID] = m
	}
	p := previewsOn(t, h.node(), codetest.FixtureRoster, &fetches{})
	for _, head := range heads {
		a, err := p.MergePreview(t.Context(), code.MergePreviewQuery{RepoID: h.repo, Asker: person, Base: tip,
			Head: head, Home: true})
		r := recorded[head]
		switch {
		case err != nil:
			t.Errorf("the preview of %s into %s answered %v, where its change recorded %s", head, tip, err, r.State)
		case a.State != r.State || !slices.Equal(a.Conflicts, r.Conflicts) ||
			a.ConflictsOmitted != r.ConflictsOmitted:
			t.Errorf("the preview of %s answered %s naming %v and %d more; its change recorded %s naming %v and "+
				"%d more", head, a.State, a.Conflicts, a.ConflictsOmitted, r.State, r.Conflicts, r.ConflictsOmitted)
		case a.RepoID != h.repo || a.Base != tip || a.Head != head || a.Position.IsZero():
			t.Errorf("the preview answered %+v, want it naming the repository, its two commits and the copy's "+
				"position", a)
		}
	}
	if r := recorded[wideHead]; len(r.Conflicts) != code.MaxConflictPaths || r.ConflictsOmitted != 3 {
		t.Errorf("the premise: the wide conflict was recorded naming %d paths and %d more, want %d and 3",
			len(r.Conflicts), r.ConflictsOmitted, code.MaxConflictPaths)
	}
	if r := recorded[unrelated]; r.State != code.MergeConflict || !slices.Equal(r.Conflicts, []string{"b.go"}) {
		t.Errorf("the premise: the unrelated history was recorded %s naming %v, want a conflict in b.go",
			r.State, r.Conflicts)
	}
}

// A MERGE PREVIEW IS ANSWERED AS A READ BY OBJECT ID IS: a seat's preview is
// refused about the node where it holds no chart or is behind the company's
// configuration; a node holding no copy answers a preview not its walk's
// home's not_held, having started nothing; a seat outside the repository's
// unit is not permitted; a commit the copy lacks is not found, an id of a tree
// is no commit to merge; a preview of a `visibility: unit` repository is its
// asker's read, recorded as a merge preview's; and stopped previews answer
// unready about the node.
//
// Mutations: judge no seat's grant, and the outsider is answered; record no
// preview, and the unit repository's read is missing; answer previews after
// the stop, and the stopped node merges.
func TestAMergePreviewIsAnsweredAsAReadByObjectIDIs(t *testing.T) {
	t.Parallel()
	settings := codetest.Settings()
	settings.Visibility = code.VisibilityUnit
	h := newGitHome(t, settings)
	base := h.commit(map[string]string{"a.go": "a\n"}, "base")
	h.git("checkout", "-q", "-b", "change", base)
	head := h.commit(map[string]string{"a.go": "a, changed\n"}, "a change")
	h.git("checkout", "-q", "main")
	tip := h.commit(map[string]string{"b.go": "b\n"}, "main moves")
	h.push("refs/heads/main", "refs/heads/change")
	tree := strings.TrimSpace(h.git("rev-parse", tip+"^{tree}"))
	node := h.node()
	preview := func(asker code.Principal, base, head string, homeAttempt bool) code.MergePreviewQuery {
		return code.MergePreviewQuery{RepoID: h.repo, Asker: asker, Base: base, Head: head, Home: homeAttempt}
	}
	unready := func(err error) code.UnreadyReason {
		var u *code.ErrGitUnready
		if !errors.As(err, &u) {
			return ""
		}
		return u.Reason
	}

	recorded := &fetches{}
	p := previewsOn(t, node, codetest.FixtureRoster, recorded)
	a, err := p.MergePreview(t.Context(), preview(alice, tip, head, true))
	if err != nil || a.State != code.MergeClean {
		t.Fatalf("a seat of the owning unit's preview answered (%+v, %v), want clean", a, err)
	}
	if got := recorded.all(); len(got) != 1 || got[0].Kind != home.PreviewReadKind || got[0].Principal != alice.Seat ||
		got[0].RepoID != h.repo || got[0].Via != types.FetchedViaTool || got[0].Reads != 1 || got[0].Bytes <= 0 {
		t.Fatalf("the unit repository's preview was recorded as %+v, want one merge preview read by %s", got,
			alice.Seat)
	}
	for name, tc := range map[string]struct {
		q    code.MergePreviewQuery
		want error
	}{
		"a seat outside the owning unit": {preview(code.Principal{Kind: code.PrincipalAgent, Seat: "outsider"},
			tip, head, true), code.ErrNotPermitted},
		"a commit the copy lacks": {preview(person, tip, strings.Repeat("9", 40), true), code.ErrObjectNotFound},
		"a tree's id":             {preview(person, tree, head, true), code.ErrInvalid},
	} {
		if _, err := p.MergePreview(t.Context(), tc.q); !errors.Is(err, tc.want) {
			t.Errorf("%s answered %v, want %v", name, err, tc.want)
		}
	}

	for name, tc := range map[string]struct {
		p    *home.Previews
		q    code.MergePreviewQuery
		want code.UnreadyReason
	}{
		"a seat on a node holding no chart": {previewsOn(t, node, func() code.Roster { return nil }, &fetches{}),
			preview(alice, tip, head, true), code.UnreadyNoChart},
	} {
		if _, err := tc.p.MergePreview(t.Context(), tc.q); unready(err) != tc.want {
			t.Errorf("%s answered %v, want unready %s", name, err, tc.want)
		}
	}
	behind, err := home.NewPreviews(home.PreviewOptions{NodeID: node.ID(), Cache: node.Cache(), Git: node.Pool(),
		Share: node.Share(), Roster: codetest.FixtureRoster, Current: func() bool { return false },
		Fetched: (&fetches{}).record})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { behind.Stop(context.WithoutCancel(t.Context())) })
	if _, err := behind.MergePreview(t.Context(), preview(alice, tip, head, true)); unready(err) !=
		code.UnreadyBehindConfig {
		t.Errorf("a seat's preview on a node behind the configuration answered %v, want unready %s", err,
			code.UnreadyBehindConfig)
	}

	// A REPOSITORY THE NODE HOLDS NO COPY OF: not its walk's home's, not_held.
	h.f.Declare(t, code.Declared{Name: codetest.RepoName, Settings: settings},
		code.Declared{Name: "other", Settings: codetest.Settings()})
	rows, err := h.f.Rows(t.Context(), "other", codetest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	other := preview(person, tip, head, false)
	other.RepoID = rows.Repo.ID
	if _, err := p.MergePreview(t.Context(), other); unready(err) != code.UnreadyNotHeld {
		t.Errorf("a preview of a repository the node holds no copy of, not the walk's home's, answered %v, want "+
			"unready %s", err, code.UnreadyNotHeld)
	}

	p.Stop(t.Context())
	if _, err := p.MergePreview(t.Context(), preview(person, tip, head, true)); unready(err) !=
		code.UnreadyNodeFailed {
		t.Errorf("a stopped node's preview answered %v, want unready %s", err, code.UnreadyNodeFailed)
	}
}

// BenchmarkMergeability is one home's merge of a change into its moved target
// over a copy of the repository on its git node — the step a change's
// mergeability is recorded by, and what a moved target's rechecks cost a home
// one each: the copy synced and pinned, a scratch beside it, the ancestry
// asked, the merge, the scratch removed. Each arm merges a change touching
// `touched` files into a target whose tip moved by `touched` files, over a
// tree of `files` files in directories of a hundred, each of the target's
// files beside one of the change's in its directory — so the merge descends
// into every directory either side touched, where one side's directories
// apart from the other's would be taken whole — or, `conflicting`, the same
// files both ways, each a content merge that conflicts: the figures
// MergeabilityDeadline and MergeabilityPerPass are read against (a merge well
// inside the deadline; a pass's PerPass merges inside one core of a minute).
func BenchmarkMergeability(b *testing.B) {
	for _, arm := range []struct {
		files, touched int
		conflicting    bool
	}{{20_000, 10, false}, {20_000, 1_000, false}, {20_000, 1_000, true}} {
		name := fmt.Sprintf("files=%d/touched=%d/conflicting=%t", arm.files, arm.touched, arm.conflicting)
		b.Run(name, func(b *testing.B) {
			h := newGitHome(b, codetest.Settings())
			tree := map[string]string{}
			for i := range arm.files {
				tree[fmt.Sprintf("d%03d/f%03d.go", i/100, i%100)] = fmt.Sprintf("package d\n\n// file %d\n", i)
			}
			base := h.commit(tree, "base")
			// EVERY stride-th FILE the change's, the target's the file beside
			// each in its directory — or the change's own files, where the arm
			// conflicts.
			stride := arm.files / arm.touched
			offset := 1
			if arm.conflicting {
				offset = 0
			}
			h.git("checkout", "-q", "-b", "change", base)
			change := map[string]string{}
			for k := range arm.touched {
				i := k * stride
				change[fmt.Sprintf("d%03d/f%03d.go", i/100, i%100)] = fmt.Sprintf("package d\n\n// change %d\n", i)
			}
			head := h.commit(change, "a change")
			h.git("checkout", "-q", "main")
			moved := map[string]string{}
			for k := range arm.touched {
				i := k*stride + offset
				moved[fmt.Sprintf("d%03d/f%03d.go", i/100, i%100)] = fmt.Sprintf("package d\n\n// moved %d\n", i)
			}
			tip := h.commit(moved, "main moves")
			h.push("refs/heads/main", "refs/heads/change")
			node := h.node()
			l, err := home.New(home.Options{Estate: h.e, Placement: asNode{placement: &placement{}, node: node.ID()},
				Nudges: newNudges(), Cache: node.Cache(), Git: node.Pool(), Chains: unqueued{t: b}, Tasks: unqueued{t: b},
				Share: units(2), NodeID: node.ID()})
			if err != nil {
				b.Fatal(err)
			}
			due := h.due(head, tip)
			at := h.e.at
			if _, err := home.MergeChange(b.Context(), l, h.repo, due, at); err != nil {
				b.Fatalf("the warm-up merge: %v", err)
			}
			b.ResetTimer()
			for range b.N {
				if _, err := home.MergeChange(b.Context(), l, h.repo, due, at); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			want := code.MergeClean
			if arm.conflicting {
				want = code.MergeConflict
			}
			if m := h.e.merged(); len(m) == 0 || m[len(m)-1].State != want {
				b.Fatalf("the merges recorded %+v, want the change's merge %s", m, want)
			}
		})
	}
}
