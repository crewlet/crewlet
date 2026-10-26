package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fakeGit is one git node's git half: it records every read it is asked —
// stamped home or not — and answers each with an answer naming itself, or
// unready where it is told to be.
type fakeGit struct {
	name string

	mu    sync.Mutex
	asked []code.GitQuery
	// unready answers every read code.ErrGitUnready for this reason, where
	// set.
	unready code.UnreadyReason
	// held is the repositories it holds a copy of, where it is set — a read
	// not stamped home of any other is not_held, as the query server answers
	// it — and nil for a node holding every repository.
	held map[string]bool
}

func (g *fakeGit) Read(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, q)
	switch {
	case g.unready != "":
		return code.GitAnswer{}, &code.ErrGitUnready{Reason: g.unready,
			Detail: fmt.Sprintf("%s is %s for repository %s", g.name, g.unready, q.RepoID)}
	case g.held != nil && !g.held[q.RepoID] && !q.Home:
		return code.GitAnswer{}, &code.ErrGitUnready{Reason: code.UnreadyNotHeld,
			Detail: fmt.Sprintf("%s holds no copy of repository %s", g.name, q.RepoID)}
	}
	if g.held != nil {
		g.held[q.RepoID] = true
	}
	return code.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Rev: g.name}, nil
}

func (g *fakeGit) set(change func(*fakeGit)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g)
}

// reads is every read the half was asked, in order.
func (g *fakeGit) reads() []code.GitQuery {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.asked)
}

// gitHalf is a node's git half as [LocalGit] answers it: node's, and none —
// a code host not built — where node is nil.
type gitHalf struct{ node *fakeGit }

func (h gitHalf) Git() (GitServer, bool) {
	if h.node == nil {
		return nil, false
	}
	return h.node, true
}

// gitNodes serves each of names as a GIT node of the fleet — a data node too,
// as every git node is in this release, over a copy of its own — answering
// from a fake git half, and lists them in the routers' git placement. A name
// in idle is served with no code host.
func (f *fleet) gitNodes(t *testing.T, names []string, idle ...string) map[string]*fakeGit {
	t.Helper()
	halves := map[string]*fakeGit{}
	for _, name := range names {
		node := &fakeNode{name: name, units: chartOf(name)}
		f.nodes[name] = node
		half := gitHalf{}
		if !slices.Contains(idle, name) {
			half.node = &fakeGit{name: name}
			halves[name] = half.node
		}
		stop, err := Serve(t.Context(), silencer{q: f.start(t), node: node}, ServerOptions{
			Self: name, Local: node, Git: half, Seams: node.seams(), Envelopes: fleetEnvelopes,
		})
		if err != nil {
			t.Fatalf("serve %s: %v", name, err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
	}
	f.git.set(func(p *fakePlacement) { p.nodes = slices.Sorted(slices.Values(append(p.nodes, names...))) })
	return halves
}

// gitRouter is a router on node self over the fleet whose own git half is
// local, holding no copy of the estate.
func (f *fleet) gitRouter(t *testing.T, self string, local LocalGit) *Router {
	t.Helper()
	r, err := NewRouter(RouterOptions{Queue: f.start(t), Self: self, Placement: f.placement, Git: f.git,
		LocalGit: local, Session: NewSession(), Timing: Production(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// repoID is the i-th repository id of a case's series.
func repoID(i int) string { return fmt.Sprintf("019a0c2e-0000-7000-8000-%012x", i) }

// treeOf is a person's read of repo's root at a commit.
func treeOf(repo string) code.GitQuery {
	return code.GitQuery{Kind: code.GitTree, RepoID: repo,
		Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"},
		Tree:  &code.TreeQuery{Rev: "1111111111111111111111111111111111111111"}}
}

// asks is how many reads each half has been asked, by node.
func asks(halves map[string]*fakeGit) map[string]int {
	out := map[string]int{}
	for name, g := range halves {
		out[name] = len(g.reads())
	}
	return out
}

// A GIT OPERATION IS ASKED OF ITS REPOSITORY'S HOME (ADR-0059): for each of a
// thousand repositories, the first git node asked — and the one that answers,
// every node answering — is the first of rendezvous.Order(repository, every
// live git node), from a node that holds nothing and from a git node whose own
// half answers in process. Never the node that answered last (a keyed order
// has no sticky node) and never the asking git node out of its turn: asked
// first wherever it is not the home, it would warm a copy of every repository
// its seats read.
//
// Mutation: order by the asking node's own id rather than the repository's —
// most reads answered by a node that is not their home, red.
func TestAGitOperationIsAskedOfItsRepositorysHome(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c"}
	halves := f.gitNodes(t, holders)
	routers := map[string]*Router{
		"a node that holds nothing":       f.client,
		"a git node, its half in process": f.gitRouter(t, "git-b", gitHalf{node: halves["git-b"]}),
	}
	for i := range 1000 {
		repo := repoID(i)
		home := rendezvous.Order(repo, holders)[0]
		for name, r := range routers {
			before := asks(halves)
			a, err := r.Code().GitRead(t.Context(), treeOf(repo))
			if err != nil || a.Rev != home {
				t.Fatalf("%s: the read of %s = (%s, %v), want it answered by its home %s", name, repo, a.Rev,
					err, home)
			}
			after := asks(halves)
			for _, n := range holders {
				want := before[n]
				if n == home {
					want++
				}
				if after[n] != want {
					t.Fatalf("%s: reading %s asked %s %d times, want only its home %s asked, once", name,
						repo, n, after[n]-before[n], home)
				}
			}
			if q := halves[home].reads()[after[home]-1]; !q.Home {
				t.Fatalf("%s: the home %s was asked %s's read not stamped home", name, home, repo)
			}
		}
	}
}

// HOME IS THE READS' FIRST GIT NODE: for two hundred repositories over five
// git nodes, the home every router names — one on a node that holds nothing,
// and one on a git node whose own half is built, asked in process — is the git
// node the walk asks a read of the repository first on a quiet fleet, the one
// that answers it stamped home, and a git node is home (IsHome) for exactly the
// repositories that name it. With no demotion: a home that answered nothing is
// still the home. A placement that cannot say who serves git is an error —
// unknown, never "this node" — and a fleet with no git node is ErrNoGitNode.
//
// Mutations: key Home on the asking node's id rather than the repository's —
// most homes are not the node the reads ask first, red; order Home with the
// walk's demotions — the suspect home is no longer named, red.
func TestHomeIsTheReadsFirstGitNode(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c", "git-d", "git-e"}
	halves := f.gitNodes(t, holders)
	ownHalf := f.gitRouter(t, "git-c", gitHalf{node: halves["git-c"]})
	homes := map[string]int{}
	for i := range 200 {
		repo := repoID(i)
		before := asks(halves)
		read, err := f.client.Code().GitRead(t.Context(), treeOf(repo))
		if err != nil {
			t.Fatalf("the read of %s: %v", repo, err)
		}
		for name, r := range map[string]*Router{"a node that holds nothing": f.client,
			"a git node, its half in process": ownHalf} {
			home, err := r.Home(repo)
			if err != nil || home != read.Rev {
				t.Fatalf("%s: Home(%s) = (%s, %v), and the read's first git node, which answered it, is %s",
					name, repo, home, err, read.Rev)
			}
		}
		after := asks(halves)
		if q := halves[read.Rev].reads(); after[read.Rev] != before[read.Rev]+1 || !q[len(q)-1].Home {
			t.Fatalf("the home %s of %s was not the one node asked, stamped home", read.Rev, repo)
		}
		isHome, err := ownHalf.IsHome(repo)
		if err != nil || isHome != (read.Rev == "git-c") {
			t.Fatalf("git-c's IsHome(%s) = (%v, %v), and the home is %s", repo, isHome, err, read.Rev)
		}
		homes[read.Rev]++
	}
	if len(homes) != len(holders) {
		t.Fatalf("two hundred repositories are homed on %v, want every git node home to some", homes)
	}

	// NO DEMOTION: a home the router remembers silent keeps its
	// repositories, where the walk asks it last.
	repo := repoID(3)
	home, err := f.client.Home(repo)
	if err != nil {
		t.Fatal(err)
	}
	f.client.markSuspect(home)
	if order := f.client.keyedOrder(opCodeGitRead.spec, repo, holders, false); order[0] == home {
		t.Fatalf("the premise: the walk still asks the suspect home %s first: %v", home, order)
	}
	if again, err := f.client.Home(repo); err != nil || again != home {
		t.Fatalf("Home(%s) once its home %s went silent = (%s, %v), want the home still", repo, home, again, err)
	}

	// UNKNOWN IS NOT AN ANSWER.
	f.git.set(func(p *fakePlacement) { p.err = errors.New("the presence view answers unknown") })
	if home, err := ownHalf.Home(repo); err == nil || errors.Is(err, ErrNoGitNode) {
		t.Fatalf("Home with the git placement unknown = (%s, %v), want the placement's error", home, err)
	}
	if isHome, err := ownHalf.IsHome(repo); err == nil || isHome {
		t.Fatalf("IsHome with the git placement unknown = (%v, %v), want (false, an error)", isHome, err)
	}
	none := newFleet(t)
	if home, err := none.client.Home(repo); !errors.Is(err, ErrNoGitNode) {
		t.Fatalf("Home on a fleet with no git node = (%s, %v), want ErrNoGitNode", home, err)
	}
}

// A GIT NODE THAT IS SYNCING SENDS THE ASKER TO THE RUNNER-UP, and is not held
// against it: a home whose copy cannot answer inside its budget — still
// downloading the repository — says `behind`, the walk asks the repository's
// runner-up, and the next read asks the home first again, since a node syncing
// a repository is the right node a moment later. Demoted like a node that ran
// nothing, every repository a home was filling would move to its runner-up for
// half a minute and fill there too.
//
// Mutation: read `behind` as `no_backend` — the home declined, and the second
// read asks the runner-up first, red.
func TestAGitNodeThatIsSyncingSendsTheAskerToTheRunnerUp(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c"}
	halves := f.gitNodes(t, holders)
	repo := repoID(7)
	order := rendezvous.Order(repo, holders)
	home, runnerUp := halves[order[0]], halves[order[1]]
	home.set(func(g *fakeGit) { g.unready = code.UnreadySyncing })

	a, err := f.client.Code().GitRead(t.Context(), treeOf(repo))
	if err != nil || a.Rev != order[1] {
		t.Fatalf("a read whose home is syncing = (%s, %v), want it answered by the runner-up %s", a.Rev, err,
			order[1])
	}
	if got := asks(halves); got[order[0]] != 1 || got[order[1]] != 1 || got[order[2]] != 0 {
		t.Fatalf("the walk asked %v, want the home and then its runner-up, once each", got)
	}

	home.set(func(g *fakeGit) { g.unready = "" })
	a, err = f.client.Code().GitRead(t.Context(), treeOf(repo))
	if err != nil || a.Rev != order[0] {
		t.Fatalf("the next read, its home ready = (%s, %v), want it answered by the home %s: a syncing "+
			"home is not demoted", a.Rev, err, order[0])
	}
	if got := len(runnerUp.reads()); got != 1 {
		t.Errorf("the runner-up was asked %d reads, want the one its home could not answer", got)
	}
}

// ONLY THE EFFECTIVE HOME IS ASKED TO FILL (ADR-0059, the scale critic's rule):
// over four git nodes, the walk's first attempt is stamped home and every later
// one is not — the one node a walk lets start a download of a repository it
// holds no copy of — and once the home has gone silent, the router asks it
// last and its runner-up is the walk's first node, so the runner-up's attempt
// is the one stamped. A home's death so costs the fleet one download of each
// repository read, by its runner-up, never one by every git node a walk
// reaches. And the git placement is told the home went unanswered, as the data
// placement is told of a silent data node, so it lists the git nodes again.
//
// Mutations: stamp every attempt home — the later attempts stamped, red; tell
// only the data placement of a silent node — the git placement never told, red.
func TestOnlyTheEffectiveHomeIsAskedToFill(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c", "git-d"}
	halves := f.gitNodes(t, holders)
	repo := repoID(11)
	order := rendezvous.Order(repo, holders)
	for _, n := range order[:3] {
		halves[n].set(func(g *fakeGit) { g.unready = code.UnreadySyncing })
	}
	if a, err := f.client.Code().GitRead(t.Context(), treeOf(repo)); err != nil || a.Rev != order[3] {
		t.Fatalf("a read three nodes could not answer = (%s, %v), want the fourth's answer", a.Rev, err)
	}
	for i, n := range order {
		reads := halves[n].reads()
		if len(reads) != 1 || reads[0].Home != (i == 0) {
			t.Errorf("%s, %d in the order, was asked %+v, want one read stamped home exactly where it is "+
				"the walk's first", n, i, reads)
		}
	}

	// THE HOME GOES SILENT, and the runner-up and the rest hold no copy:
	// the walk that meets the silence asks the rest unstamped, and none
	// fills; the next walk asks the runner-up first, stamped, and it fills.
	f.nodes[order[0]].set(func(n *fakeNode) { n.silent = true })
	for _, n := range order[1:] {
		halves[n].set(func(g *fakeGit) { g.unready, g.held = "", map[string]bool{} })
	}
	var unserved *ErrUnserved
	if _, err := f.client.Code().GitRead(t.Context(), treeOf(repo)); !errors.As(err, &unserved) {
		t.Fatalf("a read whose home is silent and whose other nodes hold no copy = %v, want it unserved", err)
	}
	// THE GIT PLACEMENT IS TOLD, so it lists its git nodes again rather
	// than wait out the silent one's heartbeat.
	f.git.mu.Lock()
	told := slices.Contains(f.git.unanswered, order[0])
	f.git.mu.Unlock()
	if !told {
		t.Errorf("the git placement was not told its silent home %s went unanswered", order[0])
	}
	if a, err := f.client.Code().GitRead(t.Context(), treeOf(repo)); err != nil || a.Rev != order[1] {
		t.Fatalf("the next read, its home asked last = (%s, %v), want the runner-up %s to fill and answer",
			a.Rev, err, order[1])
	}
	reads := halves[order[1]].reads()
	if last := reads[len(reads)-1]; !last.Home {
		t.Errorf("the runner-up, first once the silent home is asked last, was asked %+v, want it stamped "+
			"home", last)
	}
	for _, n := range order[2:] {
		for _, q := range halves[n].reads()[1:] {
			if q.Home {
				t.Errorf("%s, behind the runner-up, was asked to fill: %+v", n, q)
			}
		}
	}
}

// A NODE WITH NO CODE HOST RUNS NO GIT READ: a git node whose company has not
// built its code host answers `no_backend` — never silence, which its asker
// would read as a node gone — so the walk asks the repository's runner-up and
// asks that node LAST for git reads for the next [suspectFor], where it is
// passed over and its runner-up is the walk's effective home. And a git node
// asking for itself with no code host leaves itself out of its own order,
// saying why.
//
// Mutation: forget a `no_backend` for a git read — the second read asks the
// node with no code host first again, red.
func TestANodeWithNoCodeHostRunsNoGitRead(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c"}
	halves := f.gitNodes(t, holders, "git-a")
	var homedOnA []string
	for i := 0; len(homedOnA) < 2; i++ {
		if repo := repoID(i); rendezvous.Order(repo, holders)[0] == "git-a" {
			homedOnA = append(homedOnA, repo)
		}
	}
	first, second := homedOnA[0], homedOnA[1]
	a, err := f.client.Code().GitRead(t.Context(), treeOf(first))
	if want := rendezvous.Order(first, holders)[1]; err != nil || a.Rev != want {
		t.Fatalf("a read whose home runs no code host = (%s, %v), want its runner-up %s's answer", a.Rev, err,
			want)
	}
	idle := f.nodes["git-a"]
	idle.mu.Lock()
	asked := idle.requests
	idle.mu.Unlock()
	if asked != 1 {
		t.Fatalf("the node with no code host was asked %d requests, want the one read", asked)
	}

	runnerUp := rendezvous.Order(second, holders)[1]
	before := len(halves[runnerUp].reads())
	if a, err := f.client.Code().GitRead(t.Context(), treeOf(second)); err != nil || a.Rev != runnerUp {
		t.Fatalf("the next read homed on the node with no code host = (%s, %v), want %s's", a.Rev, err, runnerUp)
	}
	idle.mu.Lock()
	asked = idle.requests
	idle.mu.Unlock()
	if asked != 1 {
		t.Errorf("the node with no code host was asked again (%d requests) inside the window it ran "+
			"nothing for git reads", asked)
	}
	if reads := halves[runnerUp].reads(); len(reads) != before+1 || !reads[before].Home {
		t.Errorf("the runner-up, first once the node with no code host is asked last, was asked %+v after "+
			"%d reads, want one stamped home", reads, before)
	}

	// THIS NODE, A GIT NODE WITH NO CODE HOST, AND NOBODY ELSE.
	alone := newFleet(t)
	alone.git.set(func(p *fakePlacement) { p.nodes = []string{"git-z"} })
	_, err = alone.gitRouter(t, "git-z", gitHalf{}).Code().GitRead(t.Context(), treeOf(first))
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "git-z: runs no code host") {
		t.Errorf("a read on the only git node, which runs no code host = %v, want it unserved saying so", err)
	}
}

// A GIT READ IS NEVER ASKED ON THE SHARED LANE: a git node answers a git read
// on its git lane alone and a data operation on the shared lane alone, each
// asked on the other answered unserved naming the lane it is answered on and
// nothing run — the separation is the server's to keep, so a git read's sync
// and its git process never hold a slot the node's replicated reads and a
// push's resolve are answered through.
func TestAGitReadIsNeverAskedOnTheSharedLane(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	halves := f.gitNodes(t, []string{"git-a"})
	args, err := json.Marshal(treeOf(repoID(1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		op      string
		args    json.RawMessage
		subject string
		want    string
	}{
		{opCodeGitRead.spec.name, args, Subject("git-a"), "the git lane"},
		{opTasks.spec.name, json.RawMessage(`{}`), laneGitReads.subject("git-a"), "the shared lane"},
	} {
		rep := askSignedOn(t, f.start(t), tc.subject, request{Op: tc.op, Args: tc.args, From: "agent-9"})
		if rep.Unserved != unservedNoBackend || !strings.Contains(rep.Detail, tc.want) {
			t.Errorf("%s asked on %s answered %+v, want it unserved naming %s", tc.op, tc.subject, rep, tc.want)
		}
	}
	if got := halves["git-a"].reads(); len(got) != 0 || f.nodes["git-a"].askedFor("tasks") {
		t.Errorf("an operation asked on another lane ran: reads %v, %v", got, f.nodes["git-a"].asked)
	}
}

// THE WIRE CARRIES NO UNREADY GIT ERROR: a git node whose copy cannot answer
// inside its budget answers `behind`, naming why in the reply's detail and
// carrying no error — so the asker reads it as a node that ran nothing, moves
// on without demoting it, and a walk nobody served is ErrUnserved, never a
// code.ErrGitUnready a caller could take for the repository's own answer. The
// error type is registered all the same, as every error of the code package
// is, for the reason the list is complete at all.
func TestTheWireCarriesNoUnreadyGitError(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	halves := f.gitNodes(t, []string{"git-a"})
	halves["git-a"].set(func(g *fakeGit) { g.unready = code.UnreadySyncing })
	args, err := json.Marshal(treeOf(repoID(2)))
	if err != nil {
		t.Fatal(err)
	}
	rep := askSignedOn(t, f.start(t), laneGitReads.subject("git-a"),
		request{Op: opCodeGitRead.spec.name, Args: args, From: "agent-9"})
	if rep.Unserved != unservedBehind || rep.Err != nil || !strings.Contains(rep.Detail, "git-a is syncing") {
		t.Fatalf("a git node whose copy is syncing answered %+v, want `behind` naming why, and no error", rep)
	}
	for name, r := range map[string]*Router{
		"a node that holds nothing": f.client,
		"the git node, in process":  f.gitRouter(t, "git-a", gitHalf{node: halves["git-a"]}),
	} {
		_, err := r.Code().GitRead(t.Context(), treeOf(repoID(2)))
		var (
			unserved *ErrUnserved
			unready  *code.ErrGitUnready
		)
		if !errors.As(err, &unserved) || errors.As(err, &unready) || !strings.Contains(unserved.Detail, "syncing") {
			t.Errorf("%s: a read only a syncing node was asked = %v, want ErrUnserved naming why and no "+
				"unready error", name, err)
		}
	}
}

// A NODE BEHIND THE COMPANY'S CONFIGURATION DECIDES NO GRANT (M3-O8's hard arm).
// A read whose asker is a SEAT is judged by the serving node's chart — which
// repositories it lists, whether it reads one — so a data node confirmed
// behind the company's configuration (shed or stuck) answers none of it: asked
// first it says `behind_config` and a current node answers; asked LAST, the
// only data node, told to answer anyway, it refuses again and the walk ends
// unserved, having run nothing — a revision it failed to apply may be the one
// that revoked the grant. A person's read consults no chart, and the same node
// answers it as its last resort. In process alike: this node's own copy behind
// the configuration refuses its own seat's read and serves its person's.
//
// Mutations: drop the hard arm — the seat's read runs on the node behind, red;
// let the last resort skip it (a request told to answer anyway judged with no
// posture) — the asked-last seat read runs, red.
func TestANodeBehindTheConfigurationDecidesNoGrant(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	seat := code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}
	person := code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"}
	for _, tc := range []struct {
		op  string
		run func(r *Router, asker code.Principal) error
	}{
		{opCodeRead.spec.name, func(r *Router, asker code.Principal) error {
			_, err := r.Code().Read(ctx, code.ReadQuery{Asker: asker, Kind: code.ReadRepos})
			return err
		}},
		{opCodeResolveRef.spec.name, func(r *Router, asker code.Principal) error {
			_, err := r.Code().ResolveRef(ctx, code.RefQuery{Repo: "app", Ref: "main", Asker: asker})
			return err
		}},
	} {
		f := newFleet(t, "data-a", "data-b")
		_, behind := f.first(t, f.client)
		current := f.other(behind)
		behind.set(func(n *fakeNode) { n.behindConfig = true })
		if err := tc.run(f.client, seat); err != nil {
			t.Fatalf("%s: a seat's read with a current data node: %v", tc.op, err)
		}
		if behind.askedFor(tc.op) || !current.askedFor(tc.op) {
			t.Fatalf("%s: asked first, the node behind ran it %v and the current one %v: want the current "+
				"node's answer", tc.op, behind.askedFor(tc.op), current.askedFor(tc.op))
		}

		f.placement.set(func(p *fakePlacement) { p.nodes = []string{behind.name} })
		err := tc.run(f.client, seat)
		var unserved *ErrUnserved
		if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "decides no grant") {
			t.Fatalf("%s: a seat's read whose only data node is behind the configuration = %v, want it "+
				"unserved, the node saying it decides no grant", tc.op, err)
		}
		behind.mu.Lock()
		anyway := behind.anyway
		behind.mu.Unlock()
		if behind.askedFor(tc.op) || anyway == 0 {
			t.Fatalf("%s: the node behind ran the seat's read (%v), or was never asked it as the last resort "+
				"(%d asks told to answer anyway)", tc.op, behind.askedFor(tc.op), anyway)
		}
		if err := tc.run(f.client, person); err != nil || !behind.askedFor(tc.op) {
			t.Fatalf("%s: a person's read on the same node = %v (ran %v), want it answered as the last "+
				"resort: it consults no chart", tc.op, err, behind.askedFor(tc.op))
		}

		alone := &fakeNode{name: "data-a", units: chartOf("a"), behindConfig: true}
		own := newFleet(t).router(t, "data-a", alone)
		if err := tc.run(own, seat); !errors.As(err, &unserved) ||
			!strings.Contains(unserved.Detail, "decides no grant") || alone.askedFor(tc.op) {
			t.Errorf("%s: this node's own copy behind the configuration, its own seat's read = %v (ran %v), "+
				"want it unserved and nothing run", tc.op, err, alone.askedFor(tc.op))
		}
		if err := tc.run(own, person); err != nil || !alone.askedFor(tc.op) {
			t.Errorf("%s: this node's own copy behind the configuration, its person's read = %v, want it "+
				"answered", tc.op, err)
		}
	}
}

// EVERY OPERATION THAT JUDGES A GRANT IS DECLARED. The hard arm is a
// declaration rather than a list kept beside the registry: every operation on
// a domain's copy whose arguments carry a code.Principal named Asker declares
// judgesGrant, so a read added later that a seat asks is held by the arm the
// day it is declared — and none that names no asker does. A git operation
// declares none: its git node judges the grant and answers a seat itself where
// it is behind its configuration (code.UnreadyBehindConfig), which the walk
// moves on from; declared, the git node would refuse persons' reads too.
// And each declaration judges a SEAT and no other asker: a person's read
// consults no chart, and an introduced page names nobody.
//
// Mutations: drop code.resolve_ref's declaration, or declare code.git_read —
// each red.
func TestEveryOperationThatJudgesAGrantIsDeclared(t *testing.T) {
	t.Parallel()
	principal := reflect.TypeFor[code.Principal]()
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		args := spec.args
		for args.Kind() == reflect.Pointer {
			args = args.Elem()
		}
		asker := false
		if args.Kind() == reflect.Struct {
			if field, ok := args.FieldByName("Asker"); ok && field.Type == principal {
				asker = true
			}
		}
		switch {
		case spec.git && spec.judges != nil:
			t.Errorf("%s is a git operation and declares that it judges a grant: its git node judges a "+
				"seat itself, and declared here a node behind its configuration would refuse a person's "+
				"read too", name)
		case spec.git:
		case asker && spec.judges == nil:
			t.Errorf("%s carries %s.Asker and declares nothing about the grant it judges: declare it "+
				"judgesGrant, so a node behind the company's configuration answers none of a seat's", name, args)
		case !asker && spec.judges != nil:
			t.Errorf("%s declares that it judges a grant and its arguments name no asker", name)
		}
	}
	seat := code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}
	person := code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"}
	for _, tc := range []struct {
		spec *opSpec
		args any
		want bool
	}{
		{opCodeRead.spec, code.ReadQuery{Asker: seat, Kind: code.ReadRepos}, true},
		{opCodeRead.spec, code.ReadQuery{Asker: person, Kind: code.ReadRepos}, false},
		{opCodeRead.spec, code.ReadQuery{Kind: code.ReadIntroduced}, false},
		{opCodeResolveRef.spec, code.RefQuery{Asker: seat}, true},
		{opCodeResolveRef.spec, code.RefQuery{Asker: person}, false},
	} {
		if tc.spec.judges == nil {
			t.Errorf("%s declares no grant", tc.spec.name)
			continue
		}
		if got := tc.spec.judges(tc.args); got != tc.want {
			t.Errorf("%s judges a grant for %+v: %v, want %v", tc.spec.name, tc.args, got, tc.want)
		}
	}
}

// PRODUCTION'S GIT READ ATTEMPT HOLDS A GIT READ'S WAITS at their whole budgets
// with the reply's margin — a free unit of the node's seat git-work share, the
// cache's sync to the asker's floor, one linearizable re-sync of an id the copy
// lacks, the read's git process and the answer's trip back: 2 + 2 + 2 + 10 + 1,
// seventeen seconds — and it is the attempt a router gives a git read, never a
// data read's ten seconds, which a git read's waits at their budgets outlast.
// The sum is spelled here from each term's own constant, so a term raised
// without the attempt fails here rather than as a healthy home heard silent.
func TestProductionsGitReadAttemptHoldsAGitReadsWaits(t *testing.T) {
	t.Parallel()
	waits := code.GitShareWait + 2*statelog.ReadBudget + gitproc.ReadDeadline + code.ReplyMargin
	if got := Production().GitReadAttempt; got != waits || waits != 17*time.Second {
		t.Errorf("production's git read attempt is %v, want the %v a git read's waits take at their whole "+
			"budgets with the reply's margin (17s)", got, waits)
	}
	r := newFleet(t).client
	if got := r.wholeAttempt(opCodeGitRead.spec); got != Production().GitReadAttempt {
		t.Errorf("a router gives a git read %v, want its git read attempt %v", got, Production().GitReadAttempt)
	}
	if got := r.wholeAttempt(opCodeRead.spec); got != Production().ReadAttempt {
		t.Errorf("a router gives a read of the rows %v, want its read attempt %v", got, Production().ReadAttempt)
	}
}

// A ROUTER NEEDS A GIT PLACEMENT: built without one, it would answer every git
// read as a fleet with no git node in it — a wiring mistake that reads as a
// deployment fact — so it is refused by name, and an empty placement is the
// one way to say a fleet has no git node. A git read nobody answered then says
// it was the GIT nodes that could not answer, and a data operation the same
// router could not place still names the data nodes.
//
// Mutation: leave a git walk's refusal unmarked — it says no data node serves
// the estate, red.
func TestARouterNeedsAGitPlacement(t *testing.T) {
	t.Parallel()
	_, err := NewRouter(RouterOptions{Self: "agent-1", Queue: &recordingAsker{}, Placement: &fakePlacement{},
		Session: NewSession(), Timing: Production(), Envelopes: fleetEnvelopes})
	if err == nil || !strings.Contains(err.Error(), "git placement") {
		t.Fatalf("a router with no git placement = %v, want a refusal naming it", err)
	}
	r, err := NewRouter(RouterOptions{Self: "agent-1", Queue: &recordingAsker{}, Placement: &fakePlacement{},
		Git: &fakePlacement{}, Session: NewSession(), Timing: Production(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatalf("a router with an empty git placement: %v", err)
	}
	_, err = r.Code().GitRead(t.Context(), treeOf(repoID(3)))
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "no node with the git role is live") {
		t.Errorf("a read with no git node live = %v, want it unserved saying so", err)
	}
	// AND IT NAMES THE GIT NODES, never the data nodes, which serve: the
	// refusal is read by whoever has to find which nodes could not answer.
	if err == nil || !unserved.Git || !strings.Contains(err.Error(), "no git node serves") ||
		strings.Contains(err.Error(), "data node") {
		t.Errorf("a git read nobody answered says %q, want it to name the git nodes", err)
	}
	// A DATA OPERATION IS NOT ASKED OF THE GIT NODES: the placements are two.
	if _, err := r.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); !errors.As(err, &unserved) ||
		unserved.Git || !strings.Contains(err.Error(), "no data node serves") {
		t.Errorf("a tracker read with no data node = %v, want it unserved, naming the data nodes", err)
	}
}
