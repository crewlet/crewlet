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
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fakeGit is one git node's git half: it records every read it is asked —
// stamped home or not — and how long the context it was asked under had left,
// and answers each with an answer naming itself, or unready where it is told
// to be.
type fakeGit struct {
	name string

	mu    sync.Mutex
	asked []code.GitQuery
	// left is, for every read asked, what its context had left of its
	// deadline when the read began; noDeadline where it had none.
	left []time.Duration
	// unready answers every read code.ErrGitUnready for this reason, where
	// set — a SEAT'S read alone where seatsOnly is, as a node behind its
	// configuration judges no seat and serves a person.
	unready   code.UnreadyReason
	seatsOnly bool
	// held is the repositories it holds a copy of, where it is set — a read
	// not stamped home of any other is not_held, as the query server answers
	// it — and nil for a node holding every repository.
	held map[string]bool
	// share, where set, is the node's seat git-work share, a unit of which
	// every read takes before it answers — and a read the context's end
	// stops waiting for one answers as the query server does: unready, at
	// its share, where its answer fell due (code.AnswerDue), and the
	// context's own error where its caller went away.
	share *code.GitShare
}

// noDeadline is what a fakeGit records of a read asked under a context with
// no deadline.
const noDeadline = time.Duration(-1)

func (g *fakeGit) Read(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	g.mu.Lock()
	g.asked = append(g.asked, q)
	left := noDeadline
	if deadline, ok := ctx.Deadline(); ok {
		left = time.Until(deadline)
	}
	g.left = append(g.left, left)
	share := g.share
	g.mu.Unlock()
	if share != nil {
		release, err := share.Take(ctx, code.SeatWorkRead)
		if err != nil {
			if code.AnswerDue(ctx) {
				return code.GitAnswer{}, &code.ErrGitUnready{Reason: code.UnreadyShare,
					Detail: g.name + "'s seat git work held its whole share until the read's answer fell due"}
			}
			return code.GitAnswer{}, err
		}
		release()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.unready != "" && (!g.seatsOnly || q.Asker.Kind == code.PrincipalAgent):
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

// lefts is what each read's context had left of its deadline as it began, in
// order.
func (g *fakeGit) lefts() []time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.left)
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
		LocalGit: local, Session: NewSession(), Timing: Production(),
		Envelopes: fleetEnvelopes})
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
// that answers it stamped home, and a git node is home (HomeOf) for exactly
// the repositories that name it. With no demotion: a home that answered
// nothing is still the home. A placement that cannot say who serves git is an
// error — unknown, never "this node" or "home to none" — and a fleet with no
// git node is ErrNoGitNode.
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
	homeOf := map[string]string{}
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
		homes[read.Rev]++
		homeOf[repo] = read.Rev
	}
	if len(homes) != len(holders) {
		t.Fatalf("two hundred repositories are homed on %v, want every git node home to some", homes)
	}
	own, err := ownHalf.HomeOf(slices.Collect(maps.Keys(homeOf)))
	if err != nil {
		t.Fatalf("git-c's HomeOf the two hundred repositories: %v", err)
	}
	for repo, home := range homeOf {
		if own[repo] != (home == "git-c") {
			t.Fatalf("git-c's HomeOf names %s %t, and the home is %s", repo, own[repo], home)
		}
	}
	if len(own) != homes["git-c"] {
		t.Fatalf("git-c's HomeOf names %d repositories, and it is the home of %d", len(own), homes["git-c"])
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
	if own, err := ownHalf.HomeOf([]string{repo}); err == nil || own != nil {
		t.Fatalf("HomeOf with the git placement unknown = (%v, %v), want no answer and an error", own, err)
	}
	none := newFleet(t)
	if home, err := none.client.Home(repo); !errors.Is(err, ErrNoGitNode) {
		t.Fatalf("Home on a fleet with no git node = (%s, %v), want ErrNoGitNode", home, err)
	}
	if own, err := none.client.HomeOf([]string{repo}); !errors.Is(err, ErrNoGitNode) || own != nil {
		t.Fatalf("HomeOf on a fleet with no git node = (%v, %v), want ErrNoGitNode", own, err)
	}
}

// HOMES ARE JUDGED AGAINST ONE READING: HomeOf over a thousand repositories
// reads who serves git ONCE, and names exactly the repositories Home names
// this node for — a compactor's pass over its listing pays one read of the
// presence view, never one a repository — and asked of no repository it reads
// nothing and names none.
//
// Mutation: read who serves git again for every repository (HomeOf over
// Home), and the placement is read a thousand times.
func TestHomeOfJudgesEveryRepositoryAgainstOneReading(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c", "git-d", "git-e"}
	halves := f.gitNodes(t, holders)
	ownHalf := f.gitRouter(t, "git-c", gitHalf{node: halves["git-c"]})
	repos := make([]string, 1000)
	for i := range repos {
		repos[i] = repoID(i)
	}
	before := f.git.readsSoFar()
	own, err := ownHalf.HomeOf(repos)
	if err != nil {
		t.Fatalf("HomeOf a thousand repositories: %v", err)
	}
	if reads := f.git.readsSoFar() - before; reads != 1 {
		t.Errorf("HomeOf a thousand repositories read who serves git %d times, want once", reads)
	}
	for _, repo := range repos {
		home, err := ownHalf.Home(repo)
		if err != nil {
			t.Fatal(err)
		}
		if own[repo] != (home == "git-c") {
			t.Fatalf("HomeOf names %s %t, and Home names %s", repo, own[repo], home)
		}
	}
	if len(own) == 0 || len(own) == len(repos) {
		t.Fatalf("the premise: git-c is the home of %d of %d repositories, want some and not all", len(own),
			len(repos))
	}
	before = f.git.readsSoFar()
	if none, err := ownHalf.HomeOf(nil); err != nil || len(none) != 0 || f.git.readsSoFar() != before {
		t.Errorf("HomeOf no repository = (%v, %v) after %d reads of who serves git, want none and no read", none,
			err, f.git.readsSoFar()-before)
	}
}

// THE MAINTAINER IS THE FIRST NODE IN ORDER THAT HOLDS THE CHARGE: over five
// git nodes advertising {8, 8, 64, 8, 8} GiB of background budget and a
// thousand repositories, work every node holds (4 GiB) is each repository's
// home's — the maintainer IS the home on alike work — work only the large node
// holds (40 GiB) is that node's for every repository, and work no node holds
// (100 GiB) is the home's, whose own admission then refuses it. A node that
// SAYS it has nothing to give — its code host not built — is passed over as
// one too small is: with the large node saying so, the 40 GiB work is each
// home's again. And a git node is the maintainer (IsMaintainer) of exactly the
// repositories that name it, its own git half in process.
//
// NOTHING MOVES PAST A HOME ON MISSING INFORMATION. A node NOT SAYING — its row
// carries no figure — is passed over where it is a runner-up and keeps the
// repositories it is home to. So a small runner-up not saying leaves the large
// node the maintainer of the 40 GiB work of every repository it ranks ahead of
// the large node for and is not home to — never handed to a node that holds no
// copy of it, only for that node's own admission to refuse it after a cold sync
// of its whole history — while the repositories it is home to stay with it; the
// large node not saying is passed over for every repository but its own, so the
// 40 GiB work is each home's; and a home not saying keeps its own repositories'
// 4 GiB work rather than hand it to their runners-up.
//
// Mutations: walk the nodes by budget rather than in the repository's order —
// the large node maintains every repository's 4 GiB work, red; keep a runner-up
// not saying in its place — the small runner-up takes the large node's 40 GiB
// work, red; pass over a home not saying — the quiet home's repositories move to
// their runners-up, red; answer the order's first node whatever it holds — the
// 40 GiB work stays with the homes, red.
func TestTheMaintainerIsTheFirstNodeInOrderThatHoldsTheCharge(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c", "git-d", "git-e"}
	f.git.set(func(p *fakePlacement) { p.nodes = slices.Clone(holders) })
	advertised := map[string]int64{"git-a": 8 * gib, "git-b": 8 * gib, "git-c": 64 * gib, "git-d": 8 * gib,
		"git-e": 8 * gib}
	// says has every git node advertise its budget but as change leaves
	// it: a node it deletes is not saying.
	says := func(change func(said map[string]int64)) {
		said := maps.Clone(advertised)
		change(said)
		f.git.set(func(p *fakePlacement) { p.said = said })
	}
	large := f.gitRouter(t, "git-c", gitHalf{node: &fakeGit{name: "git-c"}})
	// home is the repository's home as the router names it: the first of
	// its rendezvous order over the git nodes (TestHomeIsTheReadsFirstGitNode).
	home := func(repo string) string {
		h, err := f.client.Home(repo)
		if err != nil {
			t.Fatalf("Home(%s): %v", repo, err)
		}
		return h
	}
	each := func(arm string, charge int64, want func(repo string) string) {
		t.Helper()
		for i := range 1000 {
			repo := repoID(i)
			got, err := f.client.Maintainer(repo, charge)
			if err != nil || got != want(repo) {
				t.Fatalf("%s: Maintainer(%s, %d GiB) = (%s, %v), want %s", arm, repo, charge/gib, got, err,
					want(repo))
			}
			is, err := large.IsMaintainer(repo, charge)
			if err != nil || is != (got == "git-c") {
				t.Fatalf("%s: git-c's IsMaintainer(%s, %d GiB) = (%v, %v), and the maintainer is %s", arm,
					repo, charge/gib, is, err, got)
			}
		}
	}
	says(func(map[string]int64) {})
	each("work every node holds", 4*gib, home)
	each("work only the large node holds", 40*gib, func(string) string { return "git-c" })
	each("work no node holds", 100*gib, home)

	says(func(said map[string]int64) { said["git-c"] = 0 })
	each("the large node saying it has nothing to give", 40*gib, home)

	// THE PREMISES the quiet arms stand on: repositories the small node
	// git-b ranks ahead of the large node for without being their home —
	// the ones a quiet runner-up kept in its place would take — and
	// repositories git-b and git-a are each home to.
	ahead, homed := 0, map[string]int{}
	for i := range 1000 {
		repo := repoID(i)
		order := rendezvous.Order(repo, holders)
		homed[order[0]]++
		if order[0] != "git-b" && slices.Index(order, "git-b") < slices.Index(order, "git-c") {
			ahead++
		}
	}
	if ahead == 0 || homed["git-b"] == 0 || homed["git-a"] == 0 {
		t.Fatalf("the premises: git-b ranks ahead of git-c for %d repositories it is no home to, and git-b "+
			"and git-a are home to %d and %d, want some of each", ahead, homed["git-b"], homed["git-a"])
	}
	says(func(said map[string]int64) { delete(said, "git-b") })
	each("a small runner-up not saying", 40*gib, func(repo string) string {
		if home(repo) == "git-b" {
			return "git-b"
		}
		return "git-c"
	})

	says(func(said map[string]int64) { delete(said, "git-c") })
	each("the large node not saying", 40*gib, home)

	says(func(said map[string]int64) { delete(said, "git-a") })
	each("a home not saying", 4*gib, home)
}

// A MAINTAINER NOBODY CAN READ IS UNKNOWN, NEVER A GUESS: where the listing of
// who serves git and what each advertises cannot be read, Maintainer answers
// its error — never "this node" and never "nobody" — and IsMaintainer answers
// it beside false, so a node that cannot tell compacts and publishes nothing
// that pass. And a listing that carries no git node has nobody to maintain
// anything, ErrNoGitNode — a node whose own git half is built included, which
// no listing carries: its peers name nobody either.
//
// Mutation: answer a listing that could not be read with the node this one's
// own git half would make it — IsMaintainer true, red.
func TestAMaintainerOfAnUnreadablePlacementIsUnknown(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.git.set(func(p *fakePlacement) { p.nodes = []string{"git-a", "git-b"} })
	own := f.gitRouter(t, "git-a", gitHalf{node: &fakeGit{name: "git-a"}})
	repo := repoID(5)
	if _, err := own.Maintainer(repo, 1); err != nil {
		t.Fatalf("the premise: a readable placement names a maintainer: %v", err)
	}

	f.git.set(func(p *fakePlacement) { p.err = errors.New("the presence view answers unknown") })
	if m, err := own.Maintainer(repo, 1); err == nil || errors.Is(err, ErrNoGitNode) ||
		!strings.Contains(err.Error(), "background work") {
		t.Errorf("Maintainer with the git listing unknown = (%s, %v), want the listing's error", m, err)
	}
	if is, err := own.IsMaintainer(repo, 1); err == nil || is {
		t.Errorf("IsMaintainer with the git listing unknown = (%v, %v), want (false, an error)", is, err)
	}

	none := newFleet(t)
	if m, err := none.client.Maintainer(repo, 1); !errors.Is(err, ErrNoGitNode) {
		t.Errorf("Maintainer on a fleet with no git node = (%s, %v), want ErrNoGitNode", m, err)
	}
	if is, err := none.client.IsMaintainer(repo, 1); is || !errors.Is(err, ErrNoGitNode) {
		t.Errorf("IsMaintainer on a fleet with no git node = (%v, %v), want (false, ErrNoGitNode)", is, err)
	}
	unlisted := none.gitRouter(t, "git-z", gitHalf{node: &fakeGit{name: "git-z"}})
	if home, err := unlisted.Home(repo); err != nil || home != "git-z" {
		t.Fatalf("the premise: a node whose git half is built is its own home on no listing (%s, %v)", home, err)
	}
	if is, err := unlisted.IsMaintainer(repo, 1); is || !errors.Is(err, ErrNoGitNode) {
		t.Errorf("a node no listing carries, on a listing with no git node, maintains (%v, %v), want (false, "+
			"ErrNoGitNode)", is, err)
	}
}

// shiftingListing is who serves git on a fleet whose presence view lists again
// between ANY two reads: every read — of the holders or of the listing — is
// answered from the listing it holds now, and the next listing of its script is
// put in its place. listings counts the reads of the listing.
type shiftingListing struct {
	mu       sync.Mutex
	script   []GitListing
	next     int
	listings int
}

func (s *shiftingListing) read() GitListing {
	l := s.script[s.next%len(s.script)]
	s.next++
	return GitListing{Nodes: slices.Clone(l.Nodes), Budgets: maps.Clone(l.Budgets)}
}

func (s *shiftingListing) Holders() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read().Nodes, nil
}

func (s *shiftingListing) Unanswered(string) {}

func (s *shiftingListing) Listing() (GitListing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listings++
	return s.read(), nil
}

// listed is how many times the listing has been read.
func (s *shiftingListing) listed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listings
}

// A MAINTAINER IS WEIGHED OFF THE LISTING THAT NAMES IT. On a fleet whose view
// lists again between any two reads — the small node git-a in one listing and
// gone from the next — every maintainer question reads the listing ONCE, and
// the node it names for work only the large node holds (40 GiB) is the large
// node for every one of a thousand repositories, whichever listing the
// question read: never git-a, which one listing names too small to hold the
// work and the other does not name at all. Ordered off one listing and weighed
// off the next, git-a would read as a home not saying and keep the work of
// every repository it is home to — on every node reading in that window.
//
// And a git node the listing does not carry is no maintainer — GONE, never a
// node not saying — even where its own git half is built: a node that gave its
// presence back to drain, or whose row no listing has carried yet, takes no
// repository's maintenance, and names what every listed node names.
//
// Mutations: weigh each node by a read of the listing of its own — git-a,
// ordered off one listing and weighed off the next, keeps the large node's
// work, and the listing is read more than once a question, red; place this
// node by its own git half rather than by the listing — the unlisted node
// maintains the repositories it would be home to, red.
func TestAMaintainerIsWeighedOffTheListingThatNamesIt(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	holders := []string{"git-a", "git-b", "git-c", "git-d", "git-e"}
	said := map[string]int64{"git-a": 8 * gib, "git-b": 8 * gib, "git-c": 64 * gib, "git-d": 8 * gib,
		"git-e": 8 * gib}
	gone := maps.Clone(said)
	delete(gone, "git-a")
	shifting := &shiftingListing{script: []GitListing{
		{Nodes: holders, Budgets: said},
		{Nodes: slices.DeleteFunc(slices.Clone(holders), func(n string) bool { return n == "git-a" }), Budgets: gone},
	}}
	f := newFleet(t)
	moving, err := NewRouter(RouterOptions{Queue: f.start(t), Self: "agent-1", Placement: f.placement,
		Git: shifting, Session: NewSession(), Timing: Production(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	homed := 0
	for i := range 1000 {
		repo := repoID(i)
		if rendezvous.Order(repo, holders)[0] == "git-a" {
			homed++
		}
		before := shifting.listed()
		if got, err := moving.Maintainer(repo, 40*gib); err != nil || got != "git-c" {
			t.Fatalf("Maintainer(%s, 40 GiB) over a listing that moves between reads = (%s, %v), want git-c, the "+
				"one node either listing says can hold it", repo, got, err)
		}
		if reads := shifting.listed() - before; reads != 1 {
			t.Fatalf("Maintainer(%s) read the listing %d times, want once: a node is weighed by the listing that "+
				"names it", repo, reads)
		}
	}
	if homed == 0 {
		t.Fatal("the premise: git-a is home to none of the thousand repositories")
	}

	// GONE, NEVER NOT SAYING: git-f's own git half is built, and no listing
	// carries it.
	f.git.set(func(p *fakePlacement) { p.nodes, p.said = slices.Clone(holders), maps.Clone(said) })
	unlisted := f.gitRouter(t, "git-f", gitHalf{node: &fakeGit{name: "git-f"}})
	ahead := 0
	for i := range 1000 {
		repo := repoID(i)
		if rendezvous.Order(repo, append(slices.Clone(holders), "git-f"))[0] == "git-f" {
			ahead++
		}
		for _, charge := range []int64{4 * gib, 40 * gib} {
			want, err := f.client.Maintainer(repo, charge)
			if err != nil {
				t.Fatal(err)
			}
			got, err := unlisted.Maintainer(repo, charge)
			if err != nil || got != want {
				t.Fatalf("git-f, which no listing carries, names %s's maintainer for %d GiB (%s, %v), want %s, "+
					"the node every listed node names", repo, charge/gib, got, err, want)
			}
			if is, err := unlisted.IsMaintainer(repo, charge); err != nil || is {
				t.Fatalf("git-f, which no listing carries, takes %s's %d GiB work (%v, %v)", repo, charge/gib, is,
					err)
			}
		}
	}
	if ahead == 0 {
		t.Fatal("the premise: git-f, were it listed, would be home to none of the thousand repositories")
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

// seatTreeOf is a seat's read of repo's root at a commit: one a node behind
// the company's configuration judges no grant for.
func seatTreeOf(repo string) code.GitQuery {
	q := treeOf(repo)
	q.Asker = code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}
	return q
}

// stamped is every node whose half has been asked a read stamped home.
func stamped(halves map[string]*fakeGit) []string {
	var out []string
	for name, g := range halves {
		if slices.ContainsFunc(g.reads(), func(q code.GitQuery) bool { return q.Home }) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// A HOME THAT JUDGES NO SEAT HANDS THE FILL TO ITS RUNNER-UP (ADR-0059). A
// home confirmed behind the company's configuration — shed, or stuck because
// its apply failed while a peer applied, and live in the git placement all the
// while — refuses every seat's read `behind_config` before it syncs anything,
// for as long as it stays behind. Its runner-up and the third node hold no
// copy. A walk that kept the fill on its first node asked them unstamped, both
// answered `not_held`, and every seat read of the repository was answered by
// nobody until the home recovered. The fill moves on instead: the runner-up is
// stamped, fills, and serves; the next seat read asks the home first again —
// it is not demoted — and the runner-up again; a person's read, which judges
// no chart, is the home's; the third node is never asked to fill. And the
// asker remembers the home's posture for its data operations, as it would had
// a data operation told it.
//
// Mutation: keep the stamp on the walk's first node alone — the runner-up is
// asked unstamped and the seat read is unserved, red.
func TestAHomeThatJudgesNoSeatHandsTheFillToItsRunnerUp(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c"}
	halves := f.gitNodes(t, holders)
	repo := repoID(23)
	order := rendezvous.Order(repo, holders)
	home, runnerUp, third := halves[order[0]], halves[order[1]], halves[order[2]]
	home.set(func(g *fakeGit) { g.unready, g.seatsOnly = code.UnreadyBehindConfig, true })
	for _, g := range []*fakeGit{runnerUp, third} {
		g.set(func(g *fakeGit) { g.held = map[string]bool{} })
	}

	for i := range 2 {
		a, err := f.client.Code().GitRead(t.Context(), seatTreeOf(repo))
		if err != nil || a.Rev != order[1] {
			t.Fatalf("seat read %d, its home behind the configuration and nobody else holding a copy = (%s, %v), "+
				"want the runner-up %s to fill and answer", i, a.Rev, err, order[1])
		}
		if got := len(home.reads()); got != i+1 {
			t.Errorf("after seat read %d the home was asked %d reads, want it asked first every time: it is "+
				"not demoted", i, got)
		}
	}
	for _, q := range runnerUp.reads() {
		if !q.Home {
			t.Errorf("the runner-up was asked %+v unstamped: the walk let nobody fill", q)
		}
	}
	if got := third.reads(); len(got) != 0 {
		t.Errorf("the third node was asked %+v, want nothing: the runner-up served", got)
	}

	a, err := f.client.Code().GitRead(t.Context(), treeOf(repo))
	if err != nil || a.Rev != order[0] {
		t.Errorf("a person's read of the same repository = (%s, %v), want the home's: it judges no chart",
			a.Rev, err)
	}
	if got, want := stamped(halves), slices.Sorted(slices.Values(order[:2])); !slices.Equal(got, want) {
		t.Errorf("the nodes asked to fill are %v, want the home and its runner-up %v alone", got, want)
	}
	if !f.client.remembersBehind(opTasks.spec, order[0]) {
		t.Errorf("the asker does not remember %s behind the company's configuration for its data operations", order[0])
	}
}

// THE FILL MOVES ON ONLY PAST A NODE BRINGING NO COPY UP. For every reason a
// home may give for not answering, and for a home that runs no code host, over
// a runner-up and a third node that hold no copy: where no copy serving the
// read is coming on the home — behind the configuration (a seat's read), a
// failure of its own, no code host — its runner-up is stamped, fills and
// serves; where one may be — the home syncing, at its share, its chart
// moments away — the runner-up and the third are asked unstamped, start
// nothing, and the read is unserved: a second download beside the home's
// would double the fill a walk exists to make once. A reason a newer build
// gives, which this one does not know, keeps the fill where it is.
//
// Mutations: let `syncing` pass the fill — its row served, red; keep the fill
// past a home with no code host — that row unserved, red.
func TestTheFillMovesOnlyPastANodeBringingNoCopyUp(t *testing.T) {
	t.Parallel()
	cases := map[string]code.UnreadyReason{"no code host": "", "an unknown reason": "drowsy"}
	for _, why := range code.UnreadyReasons() {
		cases[string(why)] = why
	}
	for name, why := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t)
			holders := []string{"git-a", "git-b", "git-c"}
			repo := repoID(29)
			order := rendezvous.Order(repo, holders)
			var idle []string
			if name == "no code host" {
				idle = order[:1]
			}
			halves := f.gitNodes(t, holders, idle...)
			if why != "" {
				halves[order[0]].set(func(g *fakeGit) { g.unready = why })
			}
			for _, n := range order[1:] {
				halves[n].set(func(g *fakeGit) { g.held = map[string]bool{} })
			}
			passes := name == "no code host" || why.PassesFill()

			a, err := f.client.Code().GitRead(t.Context(), seatTreeOf(repo))
			switch {
			case passes && (err != nil || a.Rev != order[1]):
				t.Fatalf("a read whose home is %s = (%s, %v), want its runner-up %s to fill and answer", name,
					a.Rev, err, order[1])
			case !passes && !errors.As(err, new(*ErrUnserved)):
				t.Fatalf("a read whose home is %s = (%s, %v), want it unserved, nobody filling beside the home",
					name, a.Rev, err)
			}
			var want []string
			switch {
			case name == "no code host":
				want = []string{order[1]} // the home has no half to record a stamp
			case passes:
				want = slices.Sorted(slices.Values(order[:2]))
			default:
				want = []string{order[0]}
			}
			if got := stamped(halves); !slices.Equal(got, want) {
				t.Errorf("a home that is %s: the nodes asked to fill are %v, want %v", name, got, want)
			}
		})
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
// is, for the reason the list is complete at all. What crosses is its REASON,
// beside the `behind` (reply.Unready), which the walk reads to know who may
// fill a copy; and a seat's read refused for the node's configuration says so
// as `behind_config`, the node's posture, as every operation says it.
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
	if rep.Unserved != unservedBehind || rep.Err != nil || !strings.Contains(rep.Detail, "git-a is syncing") ||
		rep.Unready != code.UnreadySyncing {
		t.Fatalf("a git node whose copy is syncing answered %+v, want `behind` naming why, its reason beside "+
			"it, and no error", rep)
	}
	halves["git-a"].set(func(g *fakeGit) { g.unready = code.UnreadyBehindConfig })
	rep = askSignedOn(t, f.start(t), laneGitReads.subject("git-a"),
		request{Op: opCodeGitRead.spec.name, Args: args, From: "agent-9"})
	if rep.Unserved != unservedBehindConfig || rep.Err != nil || rep.Unready != code.UnreadyBehindConfig {
		t.Fatalf("a git node behind its configuration answered %+v, want `behind_config` with its reason", rep)
	}
	halves["git-a"].set(func(g *fakeGit) { g.unready = code.UnreadySyncing })
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

// A GIT NODE AT ITS SHARE IS HEARD INSIDE ITS ASKER'S ATTEMPT. A git node
// whose seat git-work share is held, asked by a router whose git attempt is
// shorter than the share's own wait and the reply's margin together, is
// served under a context that ends a reply's margin before the attempt does:
// its read stops waiting for a unit there and its `behind` — at its share —
// reaches the asker while it still listens. So the walk ends naming the
// share, the node was heard, and neither placement is told it went
// unanswered. Served to the request's deadline itself, the read waited for
// its unit until the asker had stopped listening, and the asker, its attempt
// spent and nothing heard, took a busy node for a gone one: asked it last for
// every operation and told both placements.
//
// Mutation: serve the git arm until the request's deadline — the placements
// told the node went unanswered, red.
func TestAGitNodeAtItsShareIsHeardInsideItsAskersAttempt(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	halves := f.gitNodes(t, []string{"git-a"})
	share := code.NewGitShare("git-a", 2, nil)
	unit, err := share.Take(t.Context(), code.SeatWorkRead)
	if err != nil {
		t.Fatal(err)
	}
	defer unit()
	halves["git-a"].set(func(g *fakeGit) { g.share = share })
	timing := Production()
	timing.GitReadAttempt = minReadAttempt + 500*time.Millisecond
	if timing.GitReadAttempt >= code.GitShareWait+code.ReplyMargin {
		t.Fatalf("the premise: a %v attempt must end before the share's own wait and the margin would",
			timing.GitReadAttempt)
	}
	r := f.routerTimed(t, "agent-2", nil, timing)

	begun := time.Now()
	_, err = r.Code().GitRead(t.Context(), treeOf(repoID(5)))
	waited := time.Since(begun)
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || unserved.Silent() || !strings.Contains(unserved.Detail, "share") {
		t.Fatalf("a read of a git node at its share, at a %v attempt = %v, want it unserved with the node "+
			"heard saying it is at its share", timing.GitReadAttempt, err)
	}
	if waited >= timing.GitReadAttempt {
		t.Errorf("the walk took %v, its whole %v attempt: the node's answer came too late to hear", waited,
			timing.GitReadAttempt)
	}
	for name, p := range map[string]*fakePlacement{"git": f.git, "data": f.placement} {
		p.mu.Lock()
		told := slices.Clone(p.unanswered)
		p.mu.Unlock()
		if slices.Contains(told, "git-a") {
			t.Errorf("the %s placement was told git-a went unanswered (%v): a node heard at its share is busy, "+
				"not gone", name, told)
		}
	}
}

// A GIT READ IN PROCESS IS HELD TO THE ATTEMPT A REMOTE ONE IS. A git node's
// own seats' reads run in process; they are given the git attempt a remote
// asker gives the same read and served until the reply's margin before it,
// as the node's server serves the remote one — so the git half sees the same
// time left whichever node asked, and answers alike. Run under its caller's
// context, a read asked from the home itself had no bound but its caller's,
// and waited out a busy read lane or a slow sync that every other node's
// asker was answered unready for.
//
// Mutation: run the in-process read under its caller's context — its half
// sees no deadline, red.
func TestAGitReadInProcessIsHeldToTheAttemptARemoteOneIs(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	halves := f.gitNodes(t, []string{"git-a"})
	half := halves["git-a"]
	served := Production().GitReadAttempt - code.ReplyMargin
	for name, r := range map[string]*Router{
		"asked of the git node by another": f.client,
		"asked in process by the git node": f.gitRouter(t, "git-a", gitHalf{node: half}),
	} {
		if _, err := r.Code().GitRead(t.Context(), treeOf(repoID(9))); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		lefts := half.lefts()
		left := lefts[len(lefts)-1]
		// A SECOND'S SLACK below for the walk's own work before the half
		// is asked; none above, which no clock gives.
		if left == noDeadline || left > served || left < served-time.Second {
			t.Errorf("%s: the git half was served with %v left, want the %v a git read is served for — its "+
				"attempt less the reply's margin", name, left, served)
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
// A SEAT'S PUSH likewise, whose decide judges its grant from the COMMITTER's
// chart: a committer behind the configuration decides none of it, asked first
// or asked last. The node behind runs a REAL committer gated as the engine
// gates one — its Current is the node's posture — so its repository's
// committer first in the key's order, the seat's push is decided by the
// current runner-up; and the only data node left, told to answer anyway, the
// committer hands it on and the walk ends unserved, its log untouched — while
// a person's push, whose decide reads no chart, reaches that committer's log
// as the last resort. In process alike.
//
// Mutations: drop the hard arm — the seat's read runs on the node behind, red;
// let the last resort skip it (a request told to answer anyway judged with no
// posture) — the asked-last seat read runs, red; drop the committer's gate
// (code.CommitterOptions.Current) — the asked-last seat's push reaches the
// log of the committer behind, red.
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
			_, err := r.Code().ResolveRef(ctx, code.RefQuery{Repo: "app", Refs: []string{"main"}, Asker: asker})
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

	// THE PROPOSAL HALF: a seat's push and a person's, to a repository whose
	// committer — first in the key's order — is behind the configuration.
	const repo = "019a0c2e-0000-7000-8000-0000000000b1"
	pushes := 0
	push := func(asker code.Principal, ref string) code.Proposal {
		pushes++
		p := proposalTo(repo, fmt.Sprintf("019a0c2e-0000-7000-8000-%012x", pushes))
		p.Principal, p.Commands[0].Ref = asker, ref
		return p
	}
	seatPush := code.Principal{Kind: code.PrincipalAgent, Seat: "dev",
		Run: code.RunProvenance{TurnID: "019a0c2e-0000-7000-8000-00000000d00d"}}
	f := newFleet(t, "data-a", "data-b")
	o := rendezvous.Order(repo, []string{"data-a", "data-b"})
	behind, current := f.nodes[o[0]], f.nodes[o[1]]
	behind.set(func(n *fakeNode) { n.behindConfig = true })
	log := gatedCommitter(t, behind)
	got, err := f.client.Code().Propose(ctx, push(seatPush, "refs/heads/seats/dev/a"))
	if err != nil || got.OpID != current.name || log.appends() != 0 {
		t.Fatalf("a seat's push whose committer %s is behind the configuration = (%+v, %v), its log asked "+
			"%d times; want the current runner-up %s's answer and nothing of it on the committer behind",
			behind.name, got, err, log.appends(), current.name)
	}

	f.placement.set(func(p *fakePlacement) { p.nodes = []string{behind.name} })
	_, err = f.client.Code().Propose(ctx, push(seatPush, "refs/heads/seats/dev/b"))
	var unserved *ErrUnserved
	behind.mu.Lock()
	anyway := behind.anyway
	behind.mu.Unlock()
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "behind the company's configuration") ||
		log.appends() != 0 || anyway == 0 {
		t.Fatalf("a seat's push whose only committer is behind the configuration = %v, its log asked %d "+
			"times, %d asks told to answer anyway; want it unserved, the committer saying it is behind, "+
			"its log untouched, asked as the last resort", err, log.appends(), anyway)
	}
	if got, err := f.client.Code().Propose(ctx, push(person, "refs/heads/main")); err != nil ||
		got.Answer.Refused != code.RefusalBusy || log.appends() != 1 {
		t.Fatalf("a person's push to the same committer = (%+v, %v), its log asked %d times; want it "+
			"decided there as the last resort — its log asked once, and busy, the answer of a log that "+
			"takes no record", got, err, log.appends())
	}

	alone := &fakeNode{name: "data-a", units: chartOf("a"), behindConfig: true}
	ownLog := gatedCommitter(t, alone)
	own := newFleet(t).router(t, "data-a", alone)
	if _, err := own.Code().Propose(ctx, push(seatPush, "refs/heads/seats/dev/c")); !errors.As(err, &unserved) ||
		ownLog.appends() != 0 {
		t.Errorf("this node's own committer behind the configuration, its own seat's push = %v, its log "+
			"asked %d times; want it unserved and the log untouched", err, ownLog.appends())
	}
	if got, err := own.Code().Propose(ctx, push(person, "refs/heads/main")); err != nil ||
		got.Answer.Refused != code.RefusalBusy || ownLog.appends() != 1 {
		t.Errorf("this node's own committer behind the configuration, its person's push = (%+v, %v), its "+
			"log asked %d times; want it decided there", got, err, ownLog.appends())
	}
}

// gatedCommitter makes n's code half a REAL committer, gated as the engine
// gates one — confirmed behind the company's configuration exactly while n
// is — and judging a seat's push by [platform]'s chart, over a code log that
// takes no record: every append it is asked answers a conflict, which the
// committer answers busy, and is counted — so a push that reached it was
// decided on n, and one that did not was handed on before any decide.
func gatedCommitter(t *testing.T, n *fakeNode) *refusingLog {
	t.Helper()
	log := &refusingLog{}
	c, err := code.NewCommitter(code.CommitterOptions{
		Publisher: log, DB: codeRows(t, statelog.Position{Stream: codeStream, Generation: 1, Seq: 1}).Reader(),
		NodeID: n.name, Limits: code.DefaultLimits,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		SeatRules: func() (code.SeatRules, bool) {
			return code.SeatRules{Roster: platform{}, Zone: time.UTC}, true
		},
		Current: func() bool {
			n.mu.Lock()
			defer n.mu.Unlock()
			return !n.behindConfig
		},
	})
	if err != nil {
		t.Fatalf("the committer of %s: %v", n.name, err)
	}
	t.Cleanup(func() { c.Stop(context.Background()) })
	n.set(func(n *fakeNode) { n.codeHalf = code.Served{Committer: c} })
	return log
}

// refusingLog is a code log that takes no record, counting every append a
// committer asks of it.
type refusingLog struct{ asked atomic.Int32 }

func (l *refusingLog) Publish(context.Context, statelog.Request) (statelog.Result, error) {
	l.asked.Add(1)
	return statelog.Result{}, statelog.ErrConflict
}

// appends is how many appends the log was asked.
func (l *refusingLog) appends() int { return int(l.asked.Load()) }

// EVERY OPERATION THAT JUDGES A GRANT IS DECLARED. The hard arm is a
// declaration rather than a list kept beside the registry: every operation on
// a domain's copy whose arguments carry the code.Principal it judges — a
// read's Asker, a lifecycle request's Actor — declares judgesGrant, so a read
// or a write added later that a seat asks is held by the arm the day it is
// declared — and none that names no such principal does. A git operation
// declares none: its git node judges the grant and answers a seat itself where
// it is behind its configuration (code.UnreadyBehindConfig), which the walk
// moves on from; declared, the git node would refuse persons' reads too.
// And each READ's declaration judges a SEAT and no other asker: a person's
// read consults no chart, and an introduced page names nobody. A lifecycle
// request judges the chart for EVERY actor, a person's create's owner
// included, so its declaration holds of every request.
//
// Mutations: drop code.resolve_ref's or code.lifecycle's declaration, or
// declare code.git_read — each red.
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
			for _, judged := range []string{"Asker", "Actor"} {
				if field, ok := args.FieldByName(judged); ok && field.Type == principal {
					asker = true
				}
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
		// A SEAT'S WRITE OF A CHANGE, which its grant decides whether it may
		// make; a person's consults no chart.
		{opCodeChange.spec, code.ChangeProposal{Asker: seat}, true},
		{opCodeChange.spec, code.ChangeProposal{Asker: person}, false},
		{opCodeLifecycle.spec, code.LifecycleRequest{Kind: code.LifecycleHold, Actor: seat}, true},
		{opCodeLifecycle.spec, code.LifecycleRequest{Kind: code.LifecycleCreate, Actor: person}, true},
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
// lacks, the read's own git (the run time every process of its kind shares)
// and the answer's trip back: 2 + 2 + 2 + 10 + 1, seventeen seconds — and it
// is the attempt a router gives a git read, never a data read's ten seconds,
// which a git read's waits at their budgets outlast. The sum is spelled here
// from each term's own constant, so a term raised without the attempt fails
// here rather than as a healthy home heard silent.
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
		Git: &fakePlacement{}, Session: NewSession(), Timing: Production(),
		Envelopes: fleetEnvelopes})
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
