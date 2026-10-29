package codetest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/host"
)

// CODING BOXES: a seat's coding run reaching the code host as a git user,
// through /box/git/ with its run's token — admitted by its run's row, its
// reads at its run's SESSION on the code log, which every ref answer it is
// shown and every push it lands raises (M3-decisions M3-O56).

// boxScene is what a box case needs of its topology: a repository, two git
// nodes to reach it through, and the fleet's runs.
type boxScene struct {
	t    *testing.T
	f    Fleet
	ctx  context.Context
	repo string
	// a and b are two git nodes; on a cluster, neither is the repository's
	// committer, so each node's copy follows the log as a peer's does.
	a, b Node
}

// launch is a run of seat given the scene's repository at access, its floor
// the repository's position now, and its box's git.
func (s boxScene) launch(seat string, access code.Access) (host.BoxRunRef, *Person) {
	s.t.Helper()
	rows, err := s.f.Rows(s.ctx, s.repo, Linearizable)
	if err != nil {
		s.t.Fatalf("the rows of %s: %v", s.repo, err)
	}
	ref := s.f.Runs().Launch(seat, rows.Position, host.BoxGrant{RepoID: rows.Repo.ID, Name: s.repo,
		Access: access, Branch: code.SeatNamespace(seat) + "work"})
	return ref, NewBox(s.t, s.f, ref)
}

// personURL and boxURL are the repository's URLs through n.
func (s boxScene) personURL(n Node) string { return n.URL() + "/git/" + s.repo }
func (s boxScene) boxURL(n Node) string    { return BoxURL(n, s.repo) }

// F43: a box clones its run's repository, commits, pushes its branch and
// fetches it back — every cache at the head equal to the rows.
func aBoxPushesAndFetchesItsGrants(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	_, box := c.LaunchBox(FixtureAlice, code.AccessWrite)
	clone := box.Clone(c.BoxURL(0))
	head := box.Commit(clone, map[string]string{"src/a.go": "package a\n"}, "the box's work")
	branch := code.SeatNamespace(FixtureAlice) + "work"
	if res := box.Push(c.Ctx, clone, c.BoxURL(0), "HEAD:"+branch); res.Err != nil {
		t.Fatalf("the box's push of %s: %s", branch, res)
	}
	if got := box.LsRemote(c.BoxURL(0), 2)[branch]; got != head {
		t.Errorf("the box reads %s back at %q, want its push %s", branch, got, head)
	}
	if got := p.LsRemote(c.URL(0), 0)[branch]; got != head {
		t.Errorf("a person reads the box's %s at %q, want %s", branch, got, head)
	}
	assertConverged(t, c.Fleet)
}

// F44: a run that ended between two of its box's requests: the first is
// served, the second is refused 403 in git's words.
func anEndedRunIsRefused(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	ref, box := c.LaunchBox(FixtureAlice, code.AccessWrite)
	if got := box.LsRemote(c.BoxURL(0), 2)["refs/heads/main"]; got == "" {
		t.Fatal("a live run's box reads no main")
	}
	c.Fleet.Runs().End(ref)
	_, stderr, err := box.Git().Try(t, "", nil, "ls-remote", c.BoxURL(0))
	if err == nil || !strings.Contains(stderr, "has ended") || !strings.Contains(stderr, "403") {
		t.Errorf("an ended run's box was answered %v:\n%s\nwant 403 naming the ended run", err, stderr)
	}
	assertConverged(t, c.Fleet)
}

// F51: a box's push is recorded by its RUN, whatever its commits' trailers say
// (M3-decisions U25 a): the code host refuses nothing because of a trailer and
// stamps every push row from the run's row alone. A box of run R1 pushes a
// commit whose trailers name another seat, another live run R2 and another
// work item; a commit with no trailer; and a commit whose message claims R2
// a hundred times. All land, every row names R1's seat, launch, coding agent
// and work item, and R2's pushes are none.
//
// Mutation: stamp run_id from the newest commit's Crewlet-Run trailer → the
// forged and the many rows name R2, red.
func aBoxPushIsRecordedByItsRunWhateverItsTrailersSay(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	launched := c.Rows()
	r1, box := c.LaunchBox(FixtureAlice, code.AccessWrite)
	r2, _ := c.LaunchBox(FixtureLead, code.AccessWrite)
	clone := box.Clone(c.BoxURL(0))
	branch := code.SeatNamespace(FixtureAlice) + "work"
	messages := []string{
		"a box's change\n\nCrewlet-Seat: " + FixtureLead + "\nCrewlet-Run: " + r2.LaunchID +
			"\nCrewlet-Work-Item: jira OPS-9\n",
		"a box's change with no trailer",
		"a box's change claiming another run\n\n" + strings.Repeat("Crewlet-Run: "+r2.LaunchID+"\n", 100),
	}
	for i, message := range messages {
		box.Commit(clone, map[string]string{fmt.Sprintf("src/%d.txt", i): "the box's work\n"}, message)
		if res := box.Push(c.Ctx, clone, c.BoxURL(0), "HEAD:"+branch); res.Err != nil {
			t.Fatalf("the box's push %d: %s", i, res)
		}
	}

	var boxes []code.PushRow
	for _, row := range pushRows(t, c) {
		if row.Run.RunID != "" {
			boxes = append(boxes, row)
		}
	}
	if len(boxes) != len(messages) {
		t.Fatalf("%d box pushes have rows, want %d", len(boxes), len(messages))
	}
	for _, row := range boxes {
		if row.ActorSeat != FixtureAlice || row.Run.RunID != r1.LaunchID || row.Run.CodingAgent != "claude-code" ||
			row.Run.WorkBackend != "native" || row.Run.WorkKey != "ENG-1" {
			t.Errorf("push %s is recorded as seat %q, run %q by %q, work %s %s; want run %s's: seat %s, "+
				"claude-code, native ENG-1", row.PushID, row.ActorSeat, row.Run.RunID, row.Run.CodingAgent,
				row.Run.WorkBackend, row.Run.WorkKey, r1.LaunchID, FixtureAlice)
		}
	}
	answer, err := c.Fleet.Nodes()[0].Read(c.Ctx, code.ReadQuery{Kind: code.ReadRunPushes, Limit: 200,
		RunPushes: &code.RunPushesQuery{Run: r2.LaunchID, Repos: []string{launched.Repo.ID}, After: launched.Position},
		Fresh:     Linearizable})
	if err != nil {
		t.Fatalf("read run %s's pushes: %v", r2.LaunchID, err)
	}
	if len(answer.Pushes) != 0 {
		t.Errorf("run %s, which pushed nothing, is read as %d pushes its trailers were named in", r2.LaunchID,
			len(answer.Pushes))
	}
	assertConverged(t, c.Fleet)
}

// F45: a box reads its own push through another node at once, with no
// barrier: the push raised the run's floor to its row, and the other node's
// session sync reaches it.
func aBoxReadsItsOwnPushThroughAnotherNode(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	nodes := c.Fleet.Nodes()
	s := boxScene{t: t, f: c.Fleet, ctx: c.Ctx, repo: RepoName, a: nodes[0], b: nodes[1]}
	s.seed()
	s.readsItsOwnPush()
	assertConverged(t, c.Fleet)
}

// F46: a box's push whose advertisement one node served past the box's floor
// and whose receive-pack another node — its copy last synced below that —
// receives lands whole: the advertisement raised the floor before its first
// byte, so the receive-pack's session sync holds every base the thin pack
// names.
func aBoxPushesOnTopOfWhatAnotherNodeShowedIt(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	nodes := c.Fleet.Nodes()
	s := boxScene{t: t, f: c.Fleet, ctx: c.Ctx, repo: RepoName, a: nodes[0], b: nodes[1]}
	s.seed()
	s.pushesOnTopOfWhatAnotherNodeShowed()
	assertConverged(t, c.Fleet)
}

// F47: a box never sees a branch go back between two nodes: the `ls-refs` a
// node served past the run's floor raised it, so the other node — its copy
// behind — answers at least that far.
func aBoxNeverSeesABranchGoBack(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	nodes := c.Fleet.Nodes()
	s := boxScene{t: t, f: c.Fleet, ctx: c.Ctx, repo: RepoName, a: nodes[0], b: nodes[1]}
	s.seed()
	s.neverSeesABranchGoBack()
	assertConverged(t, c.Fleet)
}

// The RunFleet halves of F45–F47, on a cluster: the two nodes are members
// that are not the repository's committer, so every push is decided by a
// third.

func aBoxReadsItsOwnPushThroughAnotherMember(t *testing.T, c Cluster) {
	s, rc := memberScene(t, c, "fleet-f45")
	s.seed()
	s.readsItsOwnPush()
	rc.converged()
}

func aBoxPushesOnTopOfWhatAnotherMemberShowedIt(t *testing.T, c Cluster) {
	s, rc := memberScene(t, c, "fleet-f46")
	s.seed()
	s.pushesOnTopOfWhatAnotherNodeShowed()
	rc.converged()
}

func aBoxNeverSeesABranchGoBackAcrossMembers(t *testing.T, c Cluster) {
	s, rc := memberScene(t, c, "fleet-f47")
	s.seed()
	s.neverSeesABranchGoBack()
	rc.converged()
}

// memberScene declares name on c and picks its two members that are not its
// committer.
func memberScene(t *testing.T, c Cluster, name string) (boxScene, *repoCase) {
	t.Helper()
	rc := newRepoCase(t, c, name)
	committer := rc.committer()
	a := rc.other(committer)
	b := rc.other(committer, a)
	byID := func(id string) Node {
		for _, n := range c.Nodes() {
			if n.ID() == id {
				return n
			}
		}
		t.Fatalf("codetest: member %s serves no git right now", id)
		return nil
	}
	return boxScene{t: t, f: c, ctx: rc.ctx, repo: name, a: byID(a), b: byID(b)}, rc
}

// seed is a person's first push of main through a.
func (s boxScene) seed() {
	s.t.Helper()
	p := NewPerson(s.t, s.f, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	if res := p.Push(s.ctx, work, s.personURL(s.a), "main"); res.Err != nil {
		s.t.Fatalf("the first push of main: %s", res)
	}
}

// readsItsOwnPush: the box warms b at its launch floor, pushes through a,
// and reads its branch through b at once.
func (s boxScene) readsItsOwnPush() {
	s.t.Helper()
	_, box := s.launch(FixtureAlice, code.AccessWrite)
	// b's COPY AT THE LAUNCH FLOOR, so what the read through it is held to
	// below is the run's floor and not a copy that never synced.
	_ = box.LsRemote(s.boxURL(s.b), 0)
	clone := box.Clone(s.boxURL(s.a))
	head := box.Commit(clone, map[string]string{"src/a.go": "package a\n"}, "the box's work")
	branch := code.SeatNamespace(FixtureAlice) + "work"
	if res := box.Push(s.ctx, clone, s.boxURL(s.a), "HEAD:"+branch); res.Err != nil {
		s.t.Fatalf("the box's push through %s: %s", s.a.ID(), res)
	}
	for _, v := range []int{0, 2} {
		if got := box.LsRemote(s.boxURL(s.b), v)[branch]; got != head {
			s.t.Errorf("at once after its push through %s, the box reads %s through %s at %q under protocol %d, "+
				"want its push %s", s.a.ID(), branch, s.b.ID(), got, v, head)
		}
	}
}

// pushesOnTopOfWhatAnotherNodeShowed: the box clones through b at its launch
// floor; a person pushes main through a; the box fetches main through a,
// commits on top of it, and pushes with the advertisement served by a and the
// receive-pack by b.
func (s boxScene) pushesOnTopOfWhatAnotherNodeShowed() {
	s.t.Helper()
	_, box := s.launch(FixtureAlice, code.AccessWrite)
	clone := box.Clone(s.boxURL(s.b))
	p := NewPerson(s.t, s.f, "ceo")
	work := p.Clone(s.personURL(s.a))
	two := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	if res := p.Push(s.ctx, work, s.personURL(s.a), "main"); res.Err != nil {
		s.t.Fatalf("the person's second push of main: %s", res)
	}
	// a's COPY CURRENT, as a person's next read through it leaves it: a
	// push links its pack into its node's copy and moves no position, and a
	// linearizable read rounds the copy up to the log's head — past the box's
	// floor.
	_ = p.LsRemote(s.personURL(s.a), 0)
	box.Git().Run(s.t, clone, "fetch", "--quiet", s.boxURL(s.a), "main")
	box.Git().Run(s.t, clone, "checkout", "--quiet", "-B", "work", "FETCH_HEAD")
	if got := box.Head(clone); got != two {
		s.t.Fatalf("the box fetched main through %s at %s, want the person's %s", s.a.ID(), got, two)
	}
	head := box.Commit(clone, map[string]string{"src/b.go": "package b\n"}, "on top of two")
	branch := code.SeatNamespace(FixtureAlice) + "work"
	split := SplitURL(s.t, s.a, s.b, "/box/git/"+s.repo)
	if res := box.Push(s.ctx, clone, split, "HEAD:"+branch); res.Err != nil {
		s.t.Fatalf("the box's push advertised by %s and received by %s: %s", s.a.ID(), s.b.ID(), res)
	}
	if got := p.LsRemote(s.personURL(s.a), 0)[branch]; got != head {
		s.t.Errorf("the box's branch is at %q, want its push %s", got, head)
	}
}

// neverSeesABranchGoBack: the box reads main through b at its launch floor;
// a person pushes main through a; the box's `ls-refs` through a shows the
// new main, and then its `ls-refs` through b — its copy behind — shows no
// older one.
func (s boxScene) neverSeesABranchGoBack() {
	s.t.Helper()
	_, box := s.launch(FixtureAlice, code.AccessRead)
	if got := box.LsRemote(s.boxURL(s.b), 2)["refs/heads/main"]; got == "" {
		s.t.Fatalf("the box reads no main through %s", s.b.ID())
	}
	p := NewPerson(s.t, s.f, "ceo")
	work := p.Clone(s.personURL(s.a))
	two := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	if res := p.Push(s.ctx, work, s.personURL(s.a), "main"); res.Err != nil {
		s.t.Fatalf("the person's second push of main: %s", res)
	}
	// a's COPY CURRENT, as a person's next read through it leaves it
	// (pushesOnTopOfWhatAnotherNodeShowed says why).
	_ = p.LsRemote(s.personURL(s.a), 0)
	if got := box.LsRemote(s.boxURL(s.a), 2)["refs/heads/main"]; got != two {
		s.t.Fatalf("the box reads main through %s at %q, want the person's %s", s.a.ID(), got, two)
	}
	if got := box.LsRemote(s.boxURL(s.b), 2)["refs/heads/main"]; got != two {
		s.t.Errorf("having read main at %s through %s, the box reads it through %s at %q: a branch went back",
			two, s.a.ID(), s.b.ID(), got)
	}
}
