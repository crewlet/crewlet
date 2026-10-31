package host_test

import (
	"bufio"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// walkCommits is the history TestAPushsWalksGoByGeneration walks: long enough
// that a walk of all of it is tens of milliseconds of git's CPU even read
// from a commit-graph (0.42 µs a commit on the node pin, git 2.56.0: the
// listing in date order 50 ms over these 120 000, against 1.1–2.4 ms for each
// of the host's walks by generation), against the few milliseconds a process
// costs to start — a gap no load closes, since a
// process's CPU is its own whatever else the machine runs.
const walkCommits = 120_000

// walkEpoch is the history's first commit's time; each later one a second on.
const walkEpoch = 1_767_225_600

// A PUSH'S WALKS GO BY GENERATION: over a history its node holds a
// commit-graph of, a tip dated before its parent — a person's skewed clock —
// is answered by the force check, the listing of what the push introduces and
// the search for a blob's path in about the CPU a tip dated after its parent
// costs, never a walk of every commit. The fixture proves it can tell: the
// listing asked in date order, as rev-list gives it without --topo-order, is
// held to cost several times the host's own on the same tip, or the case
// fails as unable to judge rather than passing.
//
// Mutation: drop --topo-order from introduced or pathOf (scan.go), and that
// walk costs what the control does.
func TestAPushsWalksGoByGeneration(t *testing.T) {
	t.Parallel()
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	p := codetest.NewPerson(t, c.Fleet, "ceo")

	// THE HISTORY: walkCommits commits of one empty tree, each a second
	// after the one before, landed in one push.
	src := gittest.Empty(t)
	gittest.FastImport(t, p.Git(), src, func(w *bufio.Writer) error {
		for i := range walkCommits {
			msg := fmt.Sprintf("commit %d\n", i)
			fmt.Fprintf(w, "commit refs/heads/main\ncommitter Crewlet Walk <walk@example.com> %d +0000\ndata %d\n%s\n",
				walkEpoch+i, len(msg), msg)
		}
		return nil
	})
	tip := strings.TrimSpace(p.Git().Run(t, src, "rev-parse", "main"))
	url := c.Fleet.Nodes()[0].URL() + "/git/" + codetest.RepoName
	if res := p.Push(c.Ctx, src, url, "main"); res.Err != nil {
		t.Fatalf("push the history: %s", res)
	}

	// THE SCRATCH a push of one commit on top would be received in, with a
	// commit-graph over the history — the maintainer writes one into the
	// cache first; here it is the scratch's own, which git reads the same
	// way — and two commits on top of the tip, outside it: one dated after
	// its parent, one dated before the history began.
	synced, err := node.Cache().Sync(c.Ctx, c.Rows().Repo.ID, codetest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(synced.Release)
	sc, err := node.Cache().NewScratch(c.Ctx, synced, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sc.Remove)
	if _, stderr, err := p.Git().Try(t, sc.Dir, strings.NewReader(tip+"\n"), "commit-graph", "write",
		"--stdin-commits", "--no-progress"); err != nil {
		t.Fatalf("write a commit-graph over the history: %v\n%s", err, stderr)
	}
	tree := strings.TrimSpace(p.Git().Run(t, sc.Dir, "rev-parse", tip+"^{tree}"))
	commit := func(date int64, msg string) string {
		stamp := fmt.Sprintf("@%d +0000", date)
		return strings.TrimSpace(p.Git().Env("GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp).Run(t, sc.Dir,
			"commit-tree", tree, "-p", tip, "-m", msg))
	}
	normal := commit(walkEpoch+walkCommits, "dated after its parent")
	skewed := commit(walkEpoch-86_400, "dated before the history")
	objects := synced.Repo.LiveObjects

	// cpuOf is the least CPU the node's read lane spent on run, of three
	// runs: a process's own CPU, which no other process inflates.
	cpuOf := func(run func()) time.Duration {
		least := time.Duration(1<<63 - 1)
		for range 3 {
			before := cpuMillis(node.Metrics(), gitproc.ClassRead)
			run()
			least = min(least, time.Duration((cpuMillis(node.Metrics(), gitproc.ClassRead)-before)*
				float64(time.Millisecond)))
		}
		return least
	}
	listing := func(tip string) func() {
		return func() {
			ids, count, err := host.Introduced(c.Ctx, node.Host(), sc, objects, []string{tip}, 0)
			if err != nil || count != 1 || !slices.Equal(ids, []string{tip}) {
				t.Fatalf("the listing of a push of %s answered %v (%d) and %v, want the one commit", tip, ids,
					count, err)
			}
		}
	}
	path := func(tip string) func() {
		return func() {
			if got := host.PathOf(c.Ctx, node.Host(), sc, objects, []string{tip}, strings.Repeat("e", 40), 0); got !=
				"(unreachable in this push)" {
				t.Fatalf("the path of a blob no push names was %q", got)
			}
		}
	}
	force := func(next string) func() {
		return func() {
			forced, stale, err := host.IsForced(c.Ctx, node.Host(), sc, objects, tip, next)
			if err != nil || forced || stale {
				t.Fatalf("main to %s answered forced %v, stale %v, %v: it is a fast-forward", next, forced, stale, err)
			}
		}
	}
	// THE CONTROL: the listing in date order, which rev-list gives without
	// --topo-order — the walk the host's must not be.
	byDate := func(args ...string) func() {
		return func() {
			if _, err := node.Pool().Run(c.Ctx, gitproc.Cmd{
				Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassRead, Deadline: time.Minute,
				Args:   slices.Concat([]string{"rev-list"}, args, []string{skewed, "--not", "--alternate-refs"}),
				Stdout: gitproc.NewLines(func([]byte) error { return nil }),
			}); err != nil {
				t.Fatalf("rev-list %v by date: %v", args, err)
			}
		}
	}
	control, controlObjects := cpuOf(byDate()), cpuOf(byDate("--objects"))
	floor := cpuOf(listing(normal))
	if control < 4*floor {
		t.Fatalf("a listing of %d commits by date cost %v of CPU and a tip dated after its parent %v: "+
			"the history is too short on this machine to tell a walk by date from one by generation — "+
			"lengthen walkCommits", walkCommits, control, floor)
	}
	for _, w := range []struct {
		name        string
		skewed, ref func()
		byDate      time.Duration
	}{
		{"the listing of what a push introduces", listing(skewed), listing(normal), control},
		{"the search for a blob's path", path(skewed), path(normal), controlObjects},
		{"the force check", force(skewed), force(normal), control},
	} {
		got, ref := cpuOf(w.skewed), cpuOf(w.ref)
		t.Logf("%s: %v skewed, %v after its parent, %v by date", w.name, got, ref, w.byDate)
		if got*3 > w.byDate || got > 3*ref+5*time.Millisecond {
			t.Errorf("%s cost %v of CPU for a tip dated before the history, against %v for one dated after "+
				"its parent and %v for a walk by date of all %d commits: it walks by date", w.name, got, ref,
				w.byDate, walkCommits)
		}
	}
}

// measuredWalkPerCommit is the dearest a walk by date costs a commit of the
// history with no commit-graph: the node pin, git 2.56.0, on two E-cores, the
// force check's merge-base over the gigabyte history BenchmarkPushSmall
// pushes (its step= arms: 4.96 µs, the listing of what a push introduces
// 4.5, the path search 4.6, receive-pack's connectivity check 4.6;
// gitproc.WalkPerObject says where it comes from), rounded up.
const measuredWalkPerCommit = 5000 * time.Nanosecond

// NO WALK OF A PUSH'S IS BOUNDED BY THE PUSH ALONE: each is given what a walk
// of the whole history may cost beside what the push sends, at the bound the
// repository's live objects set — which bound its commits — so a commit dated
// before its parent pushed into a large repository with no commit-graph yet
// is slow and never refused. Held at every size the defaults admit, for a
// history of nothing but commits (every live object one); a bound of one read
// attempt alone refused such a push into a repository of 2.2 million.
//
// Mutation: drop the history term from forceDeadline, listingDeadline or
// hookWalks, or count no force check in hookWalks, and a push is refused.
func TestAPushsWalksAreBoundedByItsHistory(t *testing.T) {
	t.Parallel()
	if gitproc.WalkPerObject < 2*measuredWalkPerCommit {
		t.Fatalf("WalkPerObject (%v) is under twice the measured %v a commit: a loaded node's walk would be "+
			"refused", gitproc.WalkPerObject, measuredWalkPerCommit)
	}
	for _, objects := range []int64{10_000, 2_200_000, 60_000_000} {
		walk := time.Duration(objects) * measuredWalkPerCommit
		if d := host.ForceDeadline(objects); d < gitproc.ReadDeadline+walk {
			t.Errorf("a force check over %d objects is given %v, under a read attempt and its walk's %v", objects,
				d, walk)
		}
		if d := host.ListingDeadline(1<<20, objects); d < gitproc.PackWorkDeadline(1<<20)+walk {
			t.Errorf("a listing over %d objects is given %v, under the pack's work and its walk's %v", objects,
				d, walk)
		}
	}
	if walk := 2_200_000 * measuredWalkPerCommit; gitproc.ReadDeadline >= walk {
		t.Fatalf("a read attempt (%v) holds the walk of 2.2 million commits (%v): this case no longer tells a "+
			"bound of the history from one of the push", gitproc.ReadDeadline, walk)
	}
	const objects = 3_000_000
	z := code.ZeroOID
	one, two := strings.Repeat("1", 40), strings.Repeat("2", 40)
	push := [][3]string{
		{one, two, "refs/heads/main"},  // a branch update: a force check
		{one, two, "refs/heads/topic"}, // and another
		{z, two, "refs/heads/new"},     // a creation asks nothing
		{one, z, "refs/heads/gone"},    // nor does a deletion
		{one, two, "refs/tags/v1"},     // nor a tag
	}
	if got, want := host.HookWalks(push, objects), 4*gitproc.HistoryWalk(objects); got != want {
		t.Errorf("a push of two branch updates may walk the history %v beside its hook budget, want %v: two "+
			"force checks, the listing and a blob's path", got, want)
	}
	if got := host.HookWalks(push, 0); got != 0 {
		t.Errorf("a push into an empty repository may walk %v of a history it has none of", got)
	}
}

// A PUSH'S DEADLINES REACH THE POOL WITH ITS HISTORY: every bound of a push
// that a walk of the repository's history can reach — receive-pack's (git's
// own connectivity check, and the landing its hook runs inside the process),
// the force check, the listing of what the push introduces and a blob's path
// — carries the history term of the LIVE OBJECTS its synced row counts, in
// the Cmd the pool is HANDED. TestAPushsWalksAreBoundedByItsHistory holds the
// helpers; this holds the call sites, since a call site that handed a helper
// no history would refuse a skewed push into a large history with every
// helper right.
//
// Mutation: hand 0 for the live objects to receivePackDeadline's history
// term or to hookWalks, drop hookWalks from it, hand it a row other than the
// synced one in receive.go, or hand forceChecks, pathOf or introduced 0
// rather than the landing's objects(), and the case fails.
func TestAPushsDeadlinesReachThePoolWithItsHistory(t *testing.T) {
	t.Parallel()
	t.Run("receive-pack", func(t *testing.T) {
		t.Parallel()
		c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
		ran := host.RecordGit(c.Fleet.Nodes()[0].Host())
		p := codetest.NewPerson(t, c.Fleet, "ceo")
		work := p.Init()
		first := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
		empty := c.Rows().Repo.LiveObjects
		c.MustPush(p, work, "main")
		created := receivePackOf(t, ran())
		held := c.Rows().Repo.LiveObjects
		if held <= empty {
			t.Fatalf("the repository holds %d live objects after a push, %d before it: the case needs a history "+
				"to bound", held, empty)
		}
		second := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
		c.MustPush(p, work, "main")
		updated := receivePackOf(t, ran())

		// The two pushes differ in nothing a deadline reads but the history
		// each was judged on and the walks its commands ask for, so the
		// difference of their deadlines is exactly the history terms.
		terms := func(objects int64, old, new string) time.Duration {
			return gitproc.HistoryWalk(objects) + host.HookWalks([][3]string{{old, new, "refs/heads/main"}}, objects)
		}
		want := terms(held, first, second) - terms(empty, code.ZeroOID, first)
		if got := updated.Deadline - created.Deadline; got != want {
			t.Errorf("a push into %d live objects was given %v more than one into %d, want %v: the history "+
				"term of receive-pack's connectivity check and of its landing's walks", held, got, empty, want)
		}
	})
	t.Run("the landing's walks", func(t *testing.T) {
		t.Parallel()
		c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Limits: code.Limits{MaxBlobBytes: 1 << 10}}},
			codetest.Settings())
		node := c.Fleet.Nodes()[0]
		ran := host.RecordGit(node.Host())
		p := codetest.NewPerson(t, c.Fleet, "ceo")
		work := p.Init()
		first := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
		c.MustPush(p, work, "main")
		// A row claiming a billion live objects: a walk of three hours, past
		// every other term of these bounds over the largest pack one push may
		// store, so a bound that reaches it carries the history.
		const objects = 1_000_000_000
		walk := gitproc.HistoryWalk(objects)
		if host.ListingDeadline(code.MaxPackBytes, 0) >= walk || host.ForceDeadline(0) >= walk {
			t.Fatalf("a walk of %d objects (%v) is no longer past a bound without one: this case cannot tell "+
				"them apart", objects, walk)
		}
		land := func(old, new string, revs ...string) []gitproc.Cmd {
			t.Helper()
			l, _ := scratchFor(t, c, p, "ceo", work, revs...)
			l.Commands = []code.Command{{Ref: "refs/heads/main", Old: old, New: new}}
			l.Synced.Repo.LiveObjects = objects
			from := len(ran())
			if _, err := host.Land(c.Ctx, node.Host(), l); err != nil {
				t.Fatalf("land %s over %s: %v", new, old, err)
			}
			return ran()[from:]
		}
		second := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
		listed := land(first, second, second, "^"+first)
		third := p.Commit(work, map[string]string{"big.bin": strings.Repeat("x", 2<<10)}, "a blob past the limit")
		pathed := land(second, third, third, "^"+second)

		for _, step := range []struct {
			name  string
			cmds  []gitproc.Cmd
			match func(gitproc.Cmd) bool
			want  time.Duration
			exact bool
		}{
			{"the force check of a fast-forward", listed, argsStart("merge-base", "--is-ancestor"),
				host.ForceDeadline(objects), true},
			{"the listing of what it introduces", listed, argsStart("rev-list", "--topo-order"), walk, false},
			{"the force check of the push with a blob past the limit", pathed,
				argsStart("merge-base", "--is-ancestor"), host.ForceDeadline(objects), true},
			{"the search for that blob's path", pathed, argsStart("rev-list", "--objects"), walk, false},
		} {
			i := slices.IndexFunc(step.cmds, step.match)
			if i < 0 {
				t.Errorf("%s ran no git: %v", step.name, argsOf(step.cmds))
				continue
			}
			got, atLeast := step.cmds[i].Deadline, ""
			if !step.exact {
				atLeast = "at least "
			}
			if (step.exact && got != step.want) || (!step.exact && got < step.want) {
				t.Errorf("%s over %d live objects was handed the pool with a deadline of %v, want %s%v",
					step.name, int64(objects), got, atLeast, step.want)
			}
		}
	})
}

// receivePackOf is the last receive-pack — not an advertisement — in cmds.
func receivePackOf(t *testing.T, cmds []gitproc.Cmd) gitproc.Cmd {
	t.Helper()
	for i := len(cmds) - 1; i >= 0; i-- {
		if args := cmds[i].Args; len(args) > 0 && args[0] == "receive-pack" &&
			!slices.Contains(args, "--advertise-refs") {
			return cmds[i]
		}
	}
	t.Fatalf("no receive-pack among %v", argsOf(cmds))
	return gitproc.Cmd{}
}

// argsStart matches a Cmd whose arguments begin with prefix.
func argsStart(prefix ...string) func(gitproc.Cmd) bool {
	return func(c gitproc.Cmd) bool {
		return len(c.Args) >= len(prefix) && slices.Equal(c.Args[:len(prefix)], prefix)
	}
}

// argsOf is each Cmd's arguments, for a failure to name what ran.
func argsOf(cmds []gitproc.Cmd) [][]string {
	out := make([][]string, len(cmds))
	for i, c := range cmds {
		out[i] = c.Args
	}
	return out
}

// observed is the count and sum of every series of the histogram name on rec
// whose attributes include attrs.
func observed(rec *metrics.Recorder, name string, attrs map[string]string) (uint64, float64) {
	var n uint64
	var sum float64
	for _, s := range rec.Read() {
		if s.Name != name {
			continue
		}
		match := true
		for k, v := range attrs {
			match = match && s.Attrs[k] == v
		}
		if match {
			n, sum = n+s.Count, sum+s.Sum
		}
	}
	return n, sum
}

// cpuMillis is the CPU the git processes of class have spent on rec, in
// milliseconds (crewlet.code.git.cpu).
func cpuMillis(rec *metrics.Recorder, class gitproc.Class) float64 {
	_, sum := observed(rec, metrics.CodeGitCPU, map[string]string{"class": string(class)})
	return sum
}
