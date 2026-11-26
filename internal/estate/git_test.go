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

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fakeGit is one Gitlet node's git half: it records every read asked, stamped
// home or not, with its context's deadline, and answers naming itself, or
// unready where told to be.
type fakeGit struct {
	name string

	mu    sync.Mutex
	asked []gitlet.GitQuery
	// deadlines is, for every read asked, its context's deadline; the zero
	// time where it had none.
	deadlines []time.Time
	// unready, where set, answers every read gitlet.ErrGitUnready for this
	// reason — only a seat's where seatsOnly is, as a node behind its
	// configuration judges no seat and serves a person.
	unready   gitlet.UnreadyReason
	seatsOnly bool
	// held is the repositories it holds a copy of (nil: every one); a read of
	// any other not stamped home is not_held, as the query server answers.
	held map[string]bool
	// share, where set, is the node's seat git-work share, a unit of which
	// every read takes first. A read the context's end stops waiting answers
	// as the query server does: unready at its share where its answer fell
	// due (gitlet.AnswerDue), else the context's own error.
	share *gitlet.GitShare

	// written is every engine write asked, with its context's deadline.
	// ranNothing, where set, refuses every write having run nothing (Gitlet
	// stopping, or no epoch to sign by: gitlet.ErrNotServing). A write is
	// otherwise answered as a read is, else applied with the node's name as
	// its commit.
	written    []askedWrite
	ranNothing error

	// previewed is every merge preview asked, each answered as a read is,
	// else a clean merge of the two commits.
	previewed []gitlet.MergePreviewQuery
}

func (g *fakeGit) MergePreview(_ context.Context, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.previewed = append(g.previewed, q)
	switch {
	case g.unready != "" && (!g.seatsOnly || q.Asker.Kind == gitlet.PrincipalAgent):
		return gitlet.MergePreviewAnswer{}, &gitlet.ErrGitUnready{Reason: g.unready,
			Detail: fmt.Sprintf("%s is %s for repository %s", g.name, g.unready, q.RepoID)}
	case g.held != nil && !g.held[q.RepoID] && !q.Home:
		return gitlet.MergePreviewAnswer{}, &gitlet.ErrGitUnready{Reason: gitlet.UnreadyNotHeld,
			Detail: fmt.Sprintf("%s holds no copy of repository %s", g.name, q.RepoID)}
	}
	if g.held != nil {
		g.held[q.RepoID] = true
	}
	return gitlet.MergePreviewAnswer{RepoID: q.RepoID, Base: q.Base, Head: q.Head, State: gitlet.MergeClean}, nil
}

// previews is every merge preview the half was asked, in order.
func (g *fakeGit) previews() []gitlet.MergePreviewQuery {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.previewed)
}

// askedWrite is one engine write a fakeGit was asked, and its context's
// deadline (the zero time where it had none).
type askedWrite struct {
	w        gitlet.EngineWrite
	deadline time.Time
}

func (g *fakeGit) Write(ctx context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.written = append(g.written, askedWrite{w: w, deadline: deadlineOf(ctx)})
	switch {
	case g.ranNothing != nil:
		return gitlet.EngineWriteResult{}, g.ranNothing
	case g.unready != "" && (!g.seatsOnly || w.Principal.Kind == gitlet.PrincipalAgent):
		return gitlet.EngineWriteResult{}, &gitlet.ErrGitUnready{Reason: g.unready,
			Detail: fmt.Sprintf("%s is %s for repository %s", g.name, g.unready, w.RepoID)}
	case g.held != nil && !g.held[w.RepoID] && !w.Home:
		return gitlet.EngineWriteResult{}, &gitlet.ErrGitUnready{Reason: gitlet.UnreadyNotHeld,
			Detail: fmt.Sprintf("%s holds no copy of repository %s", g.name, w.RepoID)}
	}
	if g.held != nil {
		g.held[w.RepoID] = true
	}
	return gitlet.EngineWriteResult{Outcome: statelog.OutcomeApplied, Commit: g.name, PushID: gitlet.WritePushID(w.OpID),
		Command: gitlet.Command{Ref: w.Ref, Old: w.ExpectedHead, Result: gitlet.ResultOK}}, nil
}

// writes is every engine write the half was asked, in order.
func (g *fakeGit) writes() []askedWrite {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.written)
}

// servedAnAttempt reports whether deadline, what a git half was served until,
// is gitlet.ReplyMargin before the end of an attempt of whole begun at begun:
// exactly, as a synctest bubble ([inBubble]) has it, where the request's trip
// takes no time.
func servedAnAttempt(deadline, begun time.Time, whole time.Duration) bool {
	return !deadline.IsZero() && deadline.Equal(begun.Add(whole-gitlet.ReplyMargin))
}

func (g *fakeGit) Read(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	g.mu.Lock()
	g.asked = append(g.asked, q)
	g.deadlines = append(g.deadlines, deadlineOf(ctx))
	share := g.share
	g.mu.Unlock()
	if share != nil {
		release, err := share.Take(ctx, gitlet.SeatWorkRead)
		if err != nil {
			if gitlet.AnswerDue(ctx) {
				return gitlet.GitAnswer{}, &gitlet.ErrGitUnready{Reason: gitlet.UnreadyShare,
					Detail: g.name + "'s seat git work held its whole share until the read's answer fell due"}
			}
			return gitlet.GitAnswer{}, err
		}
		release()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.unready != "" && (!g.seatsOnly || q.Asker.Kind == gitlet.PrincipalAgent):
		return gitlet.GitAnswer{}, &gitlet.ErrGitUnready{Reason: g.unready,
			Detail: fmt.Sprintf("%s is %s for repository %s", g.name, g.unready, q.RepoID)}
	case g.held != nil && !g.held[q.RepoID] && !q.Home:
		return gitlet.GitAnswer{}, &gitlet.ErrGitUnready{Reason: gitlet.UnreadyNotHeld,
			Detail: fmt.Sprintf("%s holds no copy of repository %s", g.name, q.RepoID)}
	}
	if g.held != nil {
		g.held[q.RepoID] = true
	}
	return gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Rev: g.name}, nil
}

func (g *fakeGit) set(change func(*fakeGit)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g)
}

// reads is every read the half was asked, in order.
func (g *fakeGit) reads() []gitlet.GitQuery {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.asked)
}

// readDeadlines is each read's context's deadline, in order.
func (g *fakeGit) readDeadlines() []time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.deadlines)
}

// gitHalf is a node's git half as [LocalGit] answers it; none (Gitlet not
// built) where node is nil.
type gitHalf struct{ node *fakeGit }

func (h gitHalf) Git() (GitServer, bool) {
	if h.node == nil {
		return nil, false
	}
	return h.node, true
}

// gitletNodes serves each of names as a Gitlet node of the fleet (a data node
// too, over its own copy, as every Gitlet node is), answering from a fake git
// half, and lists them in the routers' git placement. A name in idle is served
// without Gitlet.
func (f *fleet) gitletNodes(t *testing.T, names []string, idle ...string) map[string]*fakeGit {
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
func treeOf(repo string) gitlet.GitQuery {
	return gitlet.GitQuery{Kind: gitlet.GitTree, RepoID: repo,
		Asker: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"},
		Tree:  &gitlet.TreeQuery{Rev: "1111111111111111111111111111111111111111"}}
}

// asks is how many reads each half has been asked, by node.
func asks(halves map[string]*fakeGit) map[string]int {
	out := map[string]int{}
	for name, g := range halves {
		out[name] = len(g.reads())
	}
	return out
}

// writeOf is a person's commit of one file onto repo's main, as a tool asks
// one: its own operation, valid by gitlet.EngineWrite.Validate, and made for
// nobody, as a serving node reads every write it decodes — so what arrives is
// what was sent.
func writeOf(repo string) gitlet.EngineWrite {
	text := "one\n"
	return gitlet.EngineWrite{Kind: gitlet.WriteCommitFiles, RepoID: repo,
		Principal:  gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"},
		WrittenFor: types.ForNobody(),
		OpID:       statelog.NewOpID(time.Now(), ""), Fresh: statelog.Freshness{Level: statelog.ReadLinearizable},
		Ref: "refs/heads/main", ExpectedHead: "1111111111111111111111111111111111111111", Message: "one",
		Files: []gitlet.FileOp{{Op: gitlet.FileOpPut, Path: "README.md", Content: &text}}}
}

// seatWriteOf is a seat's commit onto its own branch of repo, naming its turn:
// one a node behind the company's configuration judges no grant for.
func seatWriteOf(repo string) gitlet.EngineWrite {
	w := writeOf(repo)
	w.Principal = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev",
		Run: gitlet.RunProvenance{TurnID: "019a0c2e-0000-7000-8000-00000000d00d"}}
	w.Ref, w.ExpectedHead, w.Base = "refs/heads/seats/dev/a", "", "1111111111111111111111111111111111111111"
	return w
}

// writesAsked is how many engine writes each half has been asked, by node.
func writesAsked(halves map[string]*fakeGit) map[string]int {
	out := map[string]int{}
	for name, g := range halves {
		out[name] = len(g.writes())
	}
	return out
}

// previewOf is a person's merge preview of one commit of repo into another, as
// a comparison asks one.
func previewOf(repo string) gitlet.MergePreviewQuery {
	return gitlet.MergePreviewQuery{RepoID: repo,
		Asker: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"},
		Base:  "1111111111111111111111111111111111111111", Head: "2222222222222222222222222222222222222222"}
}

// previewsAsked is how many merge previews each half has been asked, by node.
func previewsAsked(halves map[string]*fakeGit) map[string]int {
	out := map[string]int{}
	for name, g := range halves {
		out[name] = len(g.previews())
	}
	return out
}

// A GIT OPERATION IS ASKED OF ITS REPOSITORY'S HOME (ADR-0089): for a thousand
// repositories, the only Gitlet node asked, every node answering, is the first
// of rendezvous.Order(repository, live Gitlet nodes), from a node holding
// nothing and from a Gitlet node answering in process, for a read, an engine
// write and a merge preview alike: the home's copy is the one kept warm. Never
// the node that answered last (a keyed order has no sticky node), never the
// asking Gitlet node out of turn, which would warm a copy of every repository
// its seats touch.
//
// Mutation: order by the asking node's id rather than the repository's — most
// operations answered by a node that is not their home, red.
func TestAGitOperationIsAskedOfItsRepositorysHome(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c"}
	halves := f.gitletNodes(t, holders)
	routers := map[string]*Router{
		"a node that holds nothing":          f.client,
		"a Gitlet node, its half in process": f.gitRouter(t, "git-b", gitHalf{node: halves["git-b"]}),
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

			before = writesAsked(halves)
			w, err := r.Code().GitWrite(t.Context(), writeOf(repo))
			if err != nil || w.Commit != home {
				t.Fatalf("%s: the write to %s = (%s, %v), want it built by its home %s", name, repo, w.Commit,
					err, home)
			}
			after = writesAsked(halves)
			for _, n := range holders {
				want := before[n]
				if n == home {
					want++
				}
				if after[n] != want {
					t.Fatalf("%s: writing %s asked %s %d times, want only its home %s asked, once", name,
						repo, n, after[n]-before[n], home)
				}
			}
			if asked := halves[home].writes()[after[home]-1]; !asked.w.Home {
				t.Fatalf("%s: the home %s was asked %s's write not stamped home", name, home, repo)
			}

			before = previewsAsked(halves)
			p, err := r.Code().MergePreview(t.Context(), previewOf(repo))
			if err != nil || p.State != gitlet.MergeClean || p.RepoID != repo {
				t.Fatalf("%s: the preview of %s = (%+v, %v), want it answered clean", name, repo, p, err)
			}
			after = previewsAsked(halves)
			for _, n := range holders {
				want := before[n]
				if n == home {
					want++
				}
				if after[n] != want {
					t.Fatalf("%s: previewing %s asked %s %d times, want only its home %s asked, once", name,
						repo, n, after[n]-before[n], home)
				}
			}
			if asked := halves[home].previews()[after[home]-1]; !asked.Home {
				t.Fatalf("%s: the home %s was asked %s's preview not stamped home", name, home, repo)
			}
		}
	}
}

// HOME IS THE READS' FIRST GITLET NODE: for two hundred repositories over five
// Gitlet nodes, the home every router names (on a node holding nothing, and on
// a Gitlet node with its half in process) is the node a quiet fleet's walk asks
// first, stamped home, and HomeOf names a node for exactly those repositories.
// No demotion: a home that answered nothing is still the home. A placement that
// cannot say who serves git is an error, never "this node" or "home to none",
// and a fleet with no Gitlet node is ErrNoGitletNode.
//
// Mutations: key Home on the asking node's id — most homes are not the node the
// reads ask first, red; order Home with the walk's demotions — the suspect home
// is no longer named, red.
func TestHomeIsTheReadsFirstGitletNode(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c", "git-d", "git-e"}
	halves := f.gitletNodes(t, holders)
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
			"a Gitlet node, its half in process": ownHalf} {
			home, err := r.Home(repo)
			if err != nil || home != read.Rev {
				t.Fatalf("%s: Home(%s) = (%s, %v), and the read's first Gitlet node, which answered it, is %s",
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
		t.Fatalf("two hundred repositories are homed on %v, want every Gitlet node home to some", homes)
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

	// NO DEMOTION: a home remembered silent keeps its repositories, though the
	// walk asks it last.
	repo := repoID(3)
	home, err := f.client.Home(repo)
	if err != nil {
		t.Fatal(err)
	}
	f.client.markSuspect(home)
	if order := f.client.keyedOrder(opCodeGitRead.spec, repo, holders, false, nil); order[0] == home {
		t.Fatalf("the premise: the walk still asks the suspect home %s first: %v", home, order)
	}
	if again, err := f.client.Home(repo); err != nil || again != home {
		t.Fatalf("Home(%s) once its home %s went silent = (%s, %v), want the home still", repo, home, again, err)
	}

	// UNKNOWN IS NOT AN ANSWER.
	f.git.set(func(p *fakePlacement) { p.err = errors.New("the presence view answers unknown") })
	if home, err := ownHalf.Home(repo); err == nil || errors.Is(err, ErrNoGitletNode) {
		t.Fatalf("Home with the git placement unknown = (%s, %v), want the placement's error", home, err)
	}
	if own, err := ownHalf.HomeOf([]string{repo}); err == nil || own != nil {
		t.Fatalf("HomeOf with the git placement unknown = (%v, %v), want no answer and an error", own, err)
	}
	none := newFleet(t)
	if home, err := none.client.Home(repo); !errors.Is(err, ErrNoGitletNode) {
		t.Fatalf("Home on a fleet with no Gitlet node = (%s, %v), want ErrNoGitletNode", home, err)
	}
	if own, err := none.client.HomeOf([]string{repo}); !errors.Is(err, ErrNoGitletNode) || own != nil {
		t.Fatalf("HomeOf on a fleet with no Gitlet node = (%v, %v), want ErrNoGitletNode", own, err)
	}
}

// HOMES ARE JUDGED AGAINST ONE READING: HomeOf over a thousand repositories
// reads who serves git ONCE (the change work's backstop pass pays one presence
// read, never one a repository) and names exactly the repositories Home names
// this node for; asked of none, it reads nothing and names none.
//
// Mutation: HomeOf over Home, and the placement is read a thousand times.
func TestHomeOfJudgesEveryRepositoryAgainstOneReading(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c", "git-d", "git-e"}
	halves := f.gitletNodes(t, holders)
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

// A REPOSITORY'S GIT ORDER IS EVERY LIVE GITLET NODE, ITS HOME FIRST, THE SAME
// ON EVERY NODE: GitOrder names each Gitlet node once, the asker's own built
// half included, Home first; a node remembered silent keeps its place, since a
// check's claim is ranked by this order on every node alike; an unknown
// placement answers unknown, a fleet with none ErrNoGitletNode.
//
// Mutations: order with the walk's demotions — the suspect home falls from
// first, red; leave out this node's own half — git-c is missing, red.
func TestAGitOrderIsEveryGitletNodeItsHomeFirst(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c", "git-d", "git-e"}
	halves := f.gitletNodes(t, holders)
	f.git.set(func(p *fakePlacement) { p.nodes = []string{"git-a", "git-b", "git-d", "git-e"} })
	ownHalf := f.gitRouter(t, "git-c", gitHalf{node: halves["git-c"]})
	for i := range 50 {
		repo := repoID(i)
		order, err := ownHalf.GitOrder(repo)
		if err != nil {
			t.Fatalf("GitOrder(%s): %v", repo, err)
		}
		if got := slices.Sorted(slices.Values(order)); !slices.Equal(got, holders) {
			t.Fatalf("GitOrder(%s) = %v, want every Gitlet node once, git-c's own half among them", repo, order)
		}
		home, err := ownHalf.Home(repo)
		if err != nil || order[0] != home {
			t.Fatalf("GitOrder(%s) = %v, and its home is (%s, %v)", repo, order, home, err)
		}
		order[0] = "scribbled"
		if again, _ := ownHalf.GitOrder(repo); again[0] != home {
			t.Fatalf("a caller's edit of one answer reached the next: %v", again)
		}
	}
	repo := repoID(3)
	before, err := ownHalf.GitOrder(repo)
	if err != nil {
		t.Fatal(err)
	}
	ownHalf.markSuspect(before[0])
	if after, err := ownHalf.GitOrder(repo); err != nil || !slices.Equal(after, before) {
		t.Fatalf("GitOrder(%s) once its home went silent = (%v, %v), want %v unchanged", repo, after, err, before)
	}
	f.git.set(func(p *fakePlacement) { p.err = errors.New("the presence view answers unknown") })
	if order, err := ownHalf.GitOrder(repo); err == nil || errors.Is(err, ErrNoGitletNode) || order != nil {
		t.Fatalf("GitOrder with the git placement unknown = (%v, %v), want the placement's error", order, err)
	}
	if order, err := newFleet(t).client.GitOrder(repo); !errors.Is(err, ErrNoGitletNode) || order != nil {
		t.Fatalf("GitOrder on a fleet with no Gitlet node = (%v, %v), want ErrNoGitletNode", order, err)
	}
}

// A SCATTER'S ORDERS ARE ONE READING: GitOrders over a thousand repositories
// reads who serves git once and answers each GitOrder's order; asked of none it
// reads nothing; an unknown placement answers unknown for all.
//
// Mutations: read the placement per repository — read a thousand times, red;
// order with the walk's demotions — a suspect home falls from first, red.
func TestAScattersGitOrdersAreOneReading(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	halves := f.gitletNodes(t, []string{"git-a", "git-b", "git-c", "git-d"})
	ownHalf := f.gitRouter(t, "git-b", gitHalf{node: halves["git-b"]})
	repos := make([]string, 1000)
	for i := range repos {
		repos[i] = repoID(i)
	}
	first, err := ownHalf.GitOrder(repos[0])
	if err != nil {
		t.Fatal(err)
	}
	ownHalf.markSuspect(first[0])
	before := f.git.readsSoFar()
	orders, err := ownHalf.GitOrders(repos)
	if err != nil {
		t.Fatalf("GitOrders of a thousand repositories: %v", err)
	}
	if reads := f.git.readsSoFar() - before; reads != 1 {
		t.Errorf("GitOrders of a thousand repositories read who serves git %d times, want once", reads)
	}
	for _, repo := range repos {
		want, err := ownHalf.GitOrder(repo)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(orders[repo], want) {
			t.Fatalf("GitOrders names %s %v, and GitOrder %v", repo, orders[repo], want)
		}
	}
	before = f.git.readsSoFar()
	if none, err := ownHalf.GitOrders(nil); err != nil || len(none) != 0 || f.git.readsSoFar() != before {
		t.Errorf("GitOrders of no repository = (%v, %v) after %d reads, want none and no read", none, err,
			f.git.readsSoFar()-before)
	}
	f.git.set(func(p *fakePlacement) { p.err = errors.New("the presence view answers unknown") })
	if orders, err := ownHalf.GitOrders(repos[:2]); err == nil || orders != nil {
		t.Fatalf("GitOrders with the git placement unknown = (%v, %v), want the placement's error", orders, err)
	}
}

// MAINTAINERS ARE JUDGED AGAINST ONE LISTING: MaintainerOf over a thousand
// repositories, each at its own charge, reads the git listing ONCE (a
// maintainer's pass pays one presence read, never one a repository) and names
// exactly the repositories Maintainer names this node for; asked of none, it
// reads nothing and names none.
//
// Mutation: MaintainerOf over Maintainer, and the listing is read a thousand
// times.
func TestMaintainerOfJudgesEveryRepositoryAgainstOneListing(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c", "git-d", "git-e"}
	f.git.set(func(p *fakePlacement) {
		p.nodes = slices.Clone(holders)
		p.said = map[string]int64{"git-a": 8 * gib, "git-b": 8 * gib, "git-c": 64 * gib, "git-d": 8 * gib,
			"git-e": 8 * gib}
	})
	large := f.gitRouter(t, "git-c", gitHalf{node: &fakeGit{name: "git-c"}})
	charges := make(map[string]int64, 1000)
	for i := range 1000 {
		// Work every node holds, and work only the large node holds, in
		// turn: the set is judged at each repository's own charge.
		charges[repoID(i)] = []int64{4 * gib, 40 * gib}[i%2]
	}
	before := f.git.readsSoFar()
	own, err := large.MaintainerOf(charges)
	if err != nil {
		t.Fatalf("MaintainerOf a thousand repositories: %v", err)
	}
	if reads := f.git.readsSoFar() - before; reads != 1 {
		t.Errorf("MaintainerOf a thousand repositories read the git listing %d times, want once", reads)
	}
	for repo, charge := range charges {
		m, err := large.Maintainer(repo, charge)
		if err != nil {
			t.Fatal(err)
		}
		if own[repo] != (m == "git-c") {
			t.Fatalf("MaintainerOf names %s %t at %d GiB, and Maintainer names %s", repo, own[repo], charge/gib, m)
		}
	}
	if len(own) <= len(charges)/2 || len(own) == len(charges) {
		t.Fatalf("the premise: git-c maintains %d of %d repositories, want every 40 GiB one and some, not all, "+
			"of the rest", len(own), len(charges))
	}
	before = f.git.readsSoFar()
	if none, err := large.MaintainerOf(nil); err != nil || len(none) != 0 || f.git.readsSoFar() != before {
		t.Errorf("MaintainerOf no repository = (%v, %v) after %d reads of the git listing, want none and no read",
			none, err, f.git.readsSoFar()-before)
	}
}

// THE MAINTAINER IS THE FIRST NODE IN ORDER THAT HOLDS THE CHARGE: over five
// Gitlet nodes advertising {8, 8, 64, 8, 8} GiB, work every node holds (4 GiB)
// is the home's, work only the large node holds (40 GiB) is that node's, work
// none holds (100 GiB) the home's, whose admission refuses it. A node SAYING it
// has nothing (Gitlet not built) is passed over as too small. NOTHING MOVES
// PAST A HOME ON MISSING INFORMATION: a node NOT SAYING (no figure in its row)
// is passed over as a runner-up but keeps what it is home to, so work never
// goes to a node holding no copy only to be refused after a cold sync. git-c's
// MaintainerOf agrees with Maintainer for every repository.
//
// Mutations: walk by budget / keep a runner-up not saying / pass over a home
// not saying / answer the first node whatever it holds — each red.
func TestTheMaintainerIsTheFirstNodeInOrderThatHoldsTheCharge(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c", "git-d", "git-e"}
	f.git.set(func(p *fakePlacement) { p.nodes = slices.Clone(holders) })
	advertised := map[string]int64{"git-a": 8 * gib, "git-b": 8 * gib, "git-c": 64 * gib, "git-d": 8 * gib,
		"git-e": 8 * gib}
	// says has every Gitlet node advertise its budget but as change leaves
	// it: a node it deletes is not saying.
	says := func(change func(said map[string]int64)) {
		said := maps.Clone(advertised)
		change(said)
		f.git.set(func(p *fakePlacement) { p.said = said })
	}
	large := f.gitRouter(t, "git-c", gitHalf{node: &fakeGit{name: "git-c"}})
	// home is the repository's home as the router names it: the first of
	// its rendezvous order over the Gitlet nodes (TestHomeIsTheReadsFirstGitletNode).
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
			is, err := large.MaintainerOf(map[string]int64{repo: charge})
			if err != nil || is[repo] != (got == "git-c") {
				t.Fatalf("%s: git-c's MaintainerOf(%s, %d GiB) = (%v, %v), and the maintainer is %s", arm,
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

	// THE PREMISES of the quiet arms: repositories git-b ranks ahead of the
	// large node without being home (what a quiet runner-up kept in place
	// would take), and repositories git-b and git-a are each home to.
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

// A MAINTAINER NOBODY CAN READ IS UNKNOWN, NEVER A GUESS: with the git listing
// unreadable, Maintainer and MaintainerOf answer its error, never "this node"
// or "nobody", so a node that cannot tell compacts and publishes nothing that pass. A listing
// with no Gitlet node is ErrNoGitletNode, even on a node whose own git half is
// built: its peers name nobody either.
//
// Mutation: answer an unreadable listing by this node's own git half —
// MaintainerOf names the repository, red.
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
	if m, err := own.Maintainer(repo, 1); err == nil || errors.Is(err, ErrNoGitletNode) ||
		!strings.Contains(err.Error(), "background work") {
		t.Errorf("Maintainer with the git listing unknown = (%s, %v), want the listing's error", m, err)
	}
	if is, err := own.MaintainerOf(map[string]int64{repo: 1}); err == nil || is != nil {
		t.Errorf("MaintainerOf with the git listing unknown = (%v, %v), want no answer and an error", is, err)
	}

	none := newFleet(t)
	if m, err := none.client.Maintainer(repo, 1); !errors.Is(err, ErrNoGitletNode) {
		t.Errorf("Maintainer on a fleet with no Gitlet node = (%s, %v), want ErrNoGitletNode", m, err)
	}
	if is, err := none.client.MaintainerOf(map[string]int64{repo: 1}); is != nil || !errors.Is(err, ErrNoGitletNode) {
		t.Errorf("MaintainerOf on a fleet with no Gitlet node = (%v, %v), want no answer and ErrNoGitletNode", is, err)
	}
	unlisted := none.gitRouter(t, "git-z", gitHalf{node: &fakeGit{name: "git-z"}})
	if home, err := unlisted.Home(repo); err != nil || home != "git-z" {
		t.Fatalf("the premise: a node whose git half is built is its own home on no listing (%s, %v)", home, err)
	}
	if is, err := unlisted.MaintainerOf(map[string]int64{repo: 1}); is != nil || !errors.Is(err, ErrNoGitletNode) {
		t.Errorf("a node no listing carries, on a listing with no Gitlet node, maintains (%v, %v), want no answer "+
			"and ErrNoGitletNode", is, err)
	}
}

// A NODE IS LISTED WHERE ITS GIT LISTING CARRIES IT, AND ONLY THERE (what a
// maintainer's loops wait on before their first pass): one it does not name is
// not, its own git half built — the node Maintainer never answers, though Home
// places it; an unreadable listing is unknown, never "not listed".
//
// Mutation: answer Listed by this node's own git half — the unlisted node is
// listed, red.
func TestANodeIsListedOnlyWhereItsGitListingCarriesIt(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.git.set(func(p *fakePlacement) { p.nodes = []string{"git-a", "git-b"} })
	carried := f.gitRouter(t, "git-a", gitHalf{node: &fakeGit{name: "git-a"}})
	if listed, err := carried.Listed(); err != nil || !listed {
		t.Errorf("a node its git listing carries is listed (%v, %v), want (true, nil)", listed, err)
	}
	unlisted := f.gitRouter(t, "git-z", gitHalf{node: &fakeGit{name: "git-z"}})
	homed := false
	for i := range 64 {
		if home, err := unlisted.Home(repoID(i)); err == nil && home == "git-z" {
			homed = true
			break
		}
	}
	if !homed {
		t.Fatal("the premise: Home places a node whose git half is built in its own order, home to one " +
			"repository in three")
	}
	if listed, err := unlisted.Listed(); err != nil || listed {
		t.Errorf("a node its git listing does not carry, its git half built, is listed (%v, %v), want "+
			"(false, nil)", listed, err)
	}

	f.git.set(func(p *fakePlacement) { p.err = errors.New("the presence view answers unknown") })
	if listed, err := carried.Listed(); err == nil || listed {
		t.Errorf("a node whose git listing cannot be read is listed (%v, %v), want (false, an error)", listed,
			err)
	}
}

// shiftingListing is who serves git on a fleet whose presence view lists again
// between ANY two reads: each read, of the holders or the listing, is answered
// from the current listing and the script's next is put in its place.
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

// A MAINTAINER IS WEIGHED OFF THE LISTING THAT NAMES IT. With the view listing
// again between any two reads (git-a in one listing, gone from the next), each
// maintainer question reads the listing ONCE, and 40 GiB work goes to the large
// node for all thousand repositories, never git-a: ordered off one listing and
// weighed off the next, git-a would read as a home not saying and keep it.
// A Gitlet node the listing does not carry is GONE, never "not saying", even
// with its own half built (draining, or not yet listed): it maintains nothing.
//
// Mutations: weigh each node off its own read of the listing — git-a keeps the
// work, red; place this node by its own git half — the unlisted node maintains
// what it would be home to, red.
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
			if is, err := unlisted.MaintainerOf(map[string]int64{repo: charge}); err != nil || is[repo] {
				t.Fatalf("git-f, which no listing carries, takes %s's %d GiB work (%v, %v)", repo, charge/gib, is,
					err)
			}
		}
	}
	if ahead == 0 {
		t.Fatal("the premise: git-f, were it listed, would be home to none of the thousand repositories")
	}
}

// A GITLET NODE THAT IS SYNCING SENDS THE ASKER TO THE RUNNER-UP, and is not
// held against it: a home still downloading says `behind`, the walk asks the
// runner-up, and the next read asks the home first again. Demoted like a node
// that ran nothing, every repository it was filling would move to its runner-up
// and fill there too.
//
// Mutation: read `behind` as `no_backend` — the second read asks the runner-up
// first, red.
func TestAGitletNodeThatIsSyncingSendsTheAskerToTheRunnerUp(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c"}
	halves := f.gitletNodes(t, holders)
	repo := repoID(7)
	order := rendezvous.Order(repo, holders)
	home, runnerUp := halves[order[0]], halves[order[1]]
	home.set(func(g *fakeGit) { g.unready = gitlet.UnreadySyncing })

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

// ONLY THE EFFECTIVE HOME IS ASKED TO FILL (ADR-0089): over four Gitlet nodes,
// only the walk's first attempt is stamped home (allowed to start a download);
// once the home is silent the router asks it last, so its runner-up's attempt
// is stamped. A home's death costs one download per repository read, never one
// per Gitlet node a walk reaches. The git placement is told the home went
// unanswered, as the data placement is of a silent data node.
//
// Mutations: stamp every attempt home — later attempts stamped, red; tell only
// the data placement — the git placement never told, red.
func TestOnlyTheEffectiveHomeIsAskedToFill(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c", "git-d"}
	halves := f.gitletNodes(t, holders)
	repo := repoID(11)
	order := rendezvous.Order(repo, holders)
	for _, n := range order[:3] {
		halves[n].set(func(g *fakeGit) { g.unready = gitlet.UnreadySyncing })
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

	// THE HOME GOES SILENT, the rest holding no copy: that walk asks them
	// unstamped and none fills; the next asks the runner-up first, stamped.
	f.nodes[order[0]].set(func(n *fakeNode) { n.silent = true })
	for _, n := range order[1:] {
		halves[n].set(func(g *fakeGit) { g.unready, g.held = "", map[string]bool{} })
	}
	var unserved *ErrUnserved
	if _, err := f.client.Code().GitRead(t.Context(), treeOf(repo)); !errors.As(err, &unserved) {
		t.Fatalf("a read whose home is silent and whose other nodes hold no copy = %v, want it unserved", err)
	}
	// THE GIT PLACEMENT IS TOLD, so it lists again rather than wait out the
	// silent node's heartbeat.
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

// seatTreeOf is a seat's read of repo's root at a commit, which a node behind
// the company's configuration judges no grant for.
func seatTreeOf(repo string) gitlet.GitQuery {
	q := treeOf(repo)
	q.Asker = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	return q
}

// stamped is every node whose half has been asked a read stamped home.
func stamped(halves map[string]*fakeGit) []string {
	var out []string
	for name, g := range halves {
		if slices.ContainsFunc(g.reads(), func(q gitlet.GitQuery) bool { return q.Home }) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// A HOME THAT JUDGES NO SEAT HANDS THE FILL TO ITS RUNNER-UP (ADR-0089). A
// home behind the company's configuration, still live in the git placement,
// refuses every seat's read `behind_config` before syncing; the others hold no
// copy, so a fill kept on the first node would leave every seat read unserved
// (`not_held`) until it recovered. Instead the runner-up is stamped, fills and
// serves; the next seat read asks the home first again (not demoted); a
// person's read, judging no chart, is the home's; the third node never fills;
// and the asker remembers the home's posture for its data operations.
//
// Mutation: stamp the walk's first node alone — the seat read is unserved, red.
func TestAHomeThatJudgesNoSeatHandsTheFillToItsRunnerUp(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c"}
	halves := f.gitletNodes(t, holders)
	repo := repoID(23)
	order := rendezvous.Order(repo, holders)
	home, runnerUp, third := halves[order[0]], halves[order[1]], halves[order[2]]
	home.set(func(g *fakeGit) { g.unready, g.seatsOnly = gitlet.UnreadyBehindConfig, true })
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
// home may give, and a home without Gitlet, over two nodes holding no copy:
// where no serving copy is coming on the home (behind the configuration for a
// seat's read, its own failure, Gitlet not built) the runner-up is stamped,
// fills and serves; where one may be (syncing, at its share, its chart moments
// away) the others are asked unstamped and the read is unserved, since a second
// download would double the fill. A reason this build does not know keeps the
// fill where it is.
//
// Mutations: let `syncing` pass the fill — its row served, red; keep the fill
// past a home without Gitlet — that row unserved, red.
func TestTheFillMovesOnlyPastANodeBringingNoCopyUp(t *testing.T) {
	t.Parallel()
	cases := map[string]gitlet.UnreadyReason{"without Gitlet": "", "an unknown reason": "drowsy"}
	for _, why := range gitlet.UnreadyReasons() {
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
			if name == "without Gitlet" {
				idle = order[:1]
			}
			halves := f.gitletNodes(t, holders, idle...)
			if why != "" {
				halves[order[0]].set(func(g *fakeGit) { g.unready = why })
			}
			for _, n := range order[1:] {
				halves[n].set(func(g *fakeGit) { g.held = map[string]bool{} })
			}
			passes := name == "without Gitlet" || why.PassesFill()

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
			case name == "without Gitlet":
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

// A NODE WITHOUT GITLET RUNS NO GIT READ: it answers `no_backend` (never
// silence, which reads as a node gone), so the walk asks the runner-up and asks
// that node LAST for git reads for [suspectFor], its runner-up the effective
// home. A Gitlet node without Gitlet leaves itself out of its own order, saying
// why.
//
// Mutation: forget a `no_backend` for a git read — the second read asks the
// node without Gitlet first again, red.
func TestANodeWithoutGitletRunsNoGitRead(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c"}
	halves := f.gitletNodes(t, holders, "git-a")
	var homedOnA []string
	for i := 0; len(homedOnA) < 2; i++ {
		if repo := repoID(i); rendezvous.Order(repo, holders)[0] == "git-a" {
			homedOnA = append(homedOnA, repo)
		}
	}
	first, second := homedOnA[0], homedOnA[1]
	a, err := f.client.Code().GitRead(t.Context(), treeOf(first))
	if want := rendezvous.Order(first, holders)[1]; err != nil || a.Rev != want {
		t.Fatalf("a read whose home does not run Gitlet = (%s, %v), want its runner-up %s's answer", a.Rev, err,
			want)
	}
	idle := f.nodes["git-a"]
	idle.mu.Lock()
	asked := idle.requests
	idle.mu.Unlock()
	if asked != 1 {
		t.Fatalf("the node without Gitlet was asked %d requests, want the one read", asked)
	}

	runnerUp := rendezvous.Order(second, holders)[1]
	before := len(halves[runnerUp].reads())
	if a, err := f.client.Code().GitRead(t.Context(), treeOf(second)); err != nil || a.Rev != runnerUp {
		t.Fatalf("the next read homed on the node without Gitlet = (%s, %v), want %s's", a.Rev, err, runnerUp)
	}
	idle.mu.Lock()
	asked = idle.requests
	idle.mu.Unlock()
	if asked != 1 {
		t.Errorf("the node without Gitlet was asked again (%d requests) inside the window it ran "+
			"nothing for git reads", asked)
	}
	if reads := halves[runnerUp].reads(); len(reads) != before+1 || !reads[before].Home {
		t.Errorf("the runner-up, first once the node without Gitlet is asked last, was asked %+v after "+
			"%d reads, want one stamped home", reads, before)
	}

	// THIS NODE, A GITLET NODE WITHOUT GITLET, AND NOBODY ELSE.
	alone := newFleet(t)
	alone.git.set(func(p *fakePlacement) { p.nodes = []string{"git-z"} })
	_, err = alone.gitRouter(t, "git-z", gitHalf{}).Code().GitRead(t.Context(), treeOf(first))
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "git-z: does not run Gitlet") {
		t.Errorf("a read on the only Gitlet node, which does not run Gitlet = %v, want it unserved saying so", err)
	}
}

// A GIT READ IS NEVER ASKED ON THE SHARED LANE: a Gitlet node answers a git
// read or merge preview on its git lane alone and a data operation on the
// shared lane alone; each asked on the other is unserved, naming its lane, with
// nothing run. The server keeps the separation, so a git read's sync never
// holds a slot replicated reads and a push's resolve need.
func TestAGitReadIsNeverAskedOnTheSharedLane(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	halves := f.gitletNodes(t, []string{"git-a"})
	args, err := json.Marshal(treeOf(repoID(1)))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := json.Marshal(previewOf(repoID(1)))
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
		{opCodeMergePreview.spec.name, preview, Subject("git-a"), "the git lane"},
		{opTasks.spec.name, json.RawMessage(`{}`), laneGitReads.subject("git-a"), "the shared lane"},
	} {
		rep := askSignedOn(t, f.start(t), tc.subject, request{Op: tc.op, Args: tc.args, From: "agent-9"})
		if rep.Unserved != unservedNoBackend || !strings.Contains(rep.Detail, tc.want) {
			t.Errorf("%s asked on %s answered %+v, want it unserved naming %s", tc.op, tc.subject, rep, tc.want)
		}
	}
	if got := halves["git-a"].reads(); len(got) != 0 || len(halves["git-a"].previews()) != 0 ||
		f.nodes["git-a"].askedFor("tasks") {
		t.Errorf("an operation asked on another lane ran: reads %v, previews %v, %v", got,
			halves["git-a"].previews(), f.nodes["git-a"].asked)
	}
}

// THE WIRE CARRIES NO UNREADY GIT ERROR: a Gitlet node whose copy cannot answer
// in budget answers `behind` with the reason in the detail and no error, so the
// asker moves on without demoting it, and a walk nobody served is ErrUnserved,
// never a gitlet.ErrGitUnready a caller could take for the repository's answer
// (the type is registered anyway, as every gitlet error is). Its REASON crosses
// as reply.Unready, which tells the walk who may fill; a seat's read refused
// for the node's configuration says `behind_config`, as every operation does.
func TestTheWireCarriesNoUnreadyGitError(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	halves := f.gitletNodes(t, []string{"git-a"})
	halves["git-a"].set(func(g *fakeGit) { g.unready = gitlet.UnreadySyncing })
	args, err := json.Marshal(treeOf(repoID(2)))
	if err != nil {
		t.Fatal(err)
	}
	rep := askSignedOn(t, f.start(t), laneGitReads.subject("git-a"),
		request{Op: opCodeGitRead.spec.name, Args: args, From: "agent-9"})
	if rep.Unserved != unservedBehind || rep.Err != nil || !strings.Contains(rep.Detail, "git-a is syncing") ||
		rep.Unready != gitlet.UnreadySyncing {
		t.Fatalf("a Gitlet node whose copy is syncing answered %+v, want `behind` naming why, its reason beside "+
			"it, and no error", rep)
	}
	halves["git-a"].set(func(g *fakeGit) { g.unready = gitlet.UnreadyBehindConfig })
	rep = askSignedOn(t, f.start(t), laneGitReads.subject("git-a"),
		request{Op: opCodeGitRead.spec.name, Args: args, From: "agent-9"})
	if rep.Unserved != unservedBehindConfig || rep.Err != nil || rep.Unready != gitlet.UnreadyBehindConfig {
		t.Fatalf("a Gitlet node behind its configuration answered %+v, want `behind_config` with its reason", rep)
	}
	halves["git-a"].set(func(g *fakeGit) { g.unready = gitlet.UnreadySyncing })
	for name, r := range map[string]*Router{
		"a node that holds nothing":   f.client,
		"the Gitlet node, in process": f.gitRouter(t, "git-a", gitHalf{node: halves["git-a"]}),
	} {
		_, err := r.Code().GitRead(t.Context(), treeOf(repoID(2)))
		var (
			unserved *ErrUnserved
			unready  *gitlet.ErrGitUnready
		)
		if !errors.As(err, &unserved) || errors.As(err, &unready) || !strings.Contains(unserved.Detail, "syncing") {
			t.Errorf("%s: a read only a syncing node was asked = %v, want ErrUnserved naming why and no "+
				"unready error", name, err)
		}
	}
}

// A GITLET NODE AT ITS SHARE IS HEARD INSIDE ITS ASKER'S ATTEMPT. With its
// seat git-work share held and an attempt shorter than the share's wait plus
// the reply's margin, the read is served until a reply's margin before the
// attempt ends, so its `behind` (at its share) reaches the asker while it
// listens: the walk names the share and no placement is told it went
// unanswered. Served to the deadline itself, a busy node reads as a gone one.
//
// Mutation: serve the git arm until the request's deadline — the placements
// told the node went unanswered, red.
//
// In a synctest bubble ([inBubble]), for
// [TestANodeThatLagsItsReadsBarrierIsHeardSayingSo]'s reason.
func TestAGitletNodeAtItsShareIsHeardInsideItsAskersAttempt(t *testing.T) {
	t.Parallel()
	inBubble(t, testAGitletNodeAtItsShareIsHeardInsideItsAskersAttempt)
}

func testAGitletNodeAtItsShareIsHeardInsideItsAskersAttempt(t *testing.T) {
	f := newFleet(t)
	halves := f.gitletNodes(t, []string{"git-a"})
	share := gitlet.NewGitShare("git-a", 2, nil)
	unit, err := share.Take(t.Context(), gitlet.SeatWorkRead)
	if err != nil {
		t.Fatal(err)
	}
	defer unit()
	halves["git-a"].set(func(g *fakeGit) { g.share = share })
	timing := Production()
	timing.GitReadAttempt = minReadAttempt + 500*time.Millisecond
	if timing.GitReadAttempt >= gitlet.GitShareWait+gitlet.ReplyMargin {
		t.Fatalf("the premise: a %v attempt must end before the share's own wait and the margin would",
			timing.GitReadAttempt)
	}
	r := f.routerTimed(t, "agent-2", nil, timing)

	begun := time.Now()
	_, err = r.Code().GitRead(t.Context(), treeOf(repoID(5)))
	waited := time.Since(begun)
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || unserved.Silent() || !strings.Contains(unserved.Detail, "share") {
		t.Fatalf("a read of a Gitlet node at its share, at a %v attempt = %v, want it unserved with the node "+
			"heard saying it is at its share", timing.GitReadAttempt, err)
	}
	if waited >= timing.GitReadAttempt {
		t.Errorf("the walk took %v, its whole %v attempt: the node's answer came too late to hear", waited,
			timing.GitReadAttempt)
	}
	for name, p := range map[string]*fakePlacement{"gitlet": f.git, "data": f.placement} {
		p.mu.Lock()
		told := slices.Clone(p.unanswered)
		p.mu.Unlock()
		if slices.Contains(told, "git-a") {
			t.Errorf("the %s placement was told git-a went unanswered (%v): a node heard at its share is busy, "+
				"not gone", name, told)
		}
	}
}

// A GIT READ IN PROCESS IS HELD TO THE ATTEMPT A REMOTE ONE IS: a Gitlet
// node's own seats' reads get the remote git attempt, served until the reply's
// margin before it, so the half sees the same time left whichever node asked.
// Under its caller's context alone, a read from the home would wait out a busy
// lane or slow sync that every remote asker is answered unready for.
//
// Mutation: run the in-process read under its caller's context — its half
// sees no deadline, red.
//
// In a synctest bubble ([inBubble]): the request carries its wait, counted from
// its arrival on the node's clock, so on the machine's clock the instant it is
// served until moves with the trip and the wait's rounding to a millisecond.
func TestAGitReadInProcessIsHeldToTheAttemptARemoteOneIs(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		f := newFleet(t)
		halves := f.gitletNodes(t, []string{"git-a"})
		half := halves["git-a"]
		attempt := Production().GitReadAttempt
		for name, r := range map[string]*Router{
			"asked of the Gitlet node by another": f.client,
			"asked in process by the Gitlet node": f.gitRouter(t, "git-a", gitHalf{node: half}),
		} {
			begun := time.Now()
			if _, err := r.Code().GitRead(t.Context(), treeOf(repoID(9))); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			deadlines := half.readDeadlines()
			if got := deadlines[len(deadlines)-1]; !servedAnAttempt(got, begun, attempt) {
				t.Errorf("%s: the git half was served until %v after the read began (the zero time for no "+
					"deadline: %v), want a git read's attempt of %v less the reply's margin", name,
					got.Sub(begun), got.IsZero(), attempt)
			}
		}
	})
}

// A NODE BEHIND THE COMPANY'S CONFIGURATION DECIDES NO GRANT (M3-O8's hard
// arm). A SEAT's read is judged by the serving node's chart, so a data node
// behind (shed or stuck) refuses it `behind_config`, even asked LAST as the
// only node: the revision it failed may have revoked the grant. A person's
// read judges no chart and is served there as the last resort. A SEAT'S PUSH
// is judged by the COMMITTER's chart: one gated on the node's posture hands it
// on, to the current runner-up or, alone, to unserved with its log untouched;
// a person's push reaches its log. In process alike.
//
// Mutations: drop the hard arm, or skip it for a request told to answer anyway
// — the seat's read runs on the node behind, red; drop the committer's gate
// (gitlet.CommitterOptions.Current) — the seat's push reaches its log, red.
func TestANodeBehindTheConfigurationDecidesNoGrant(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	seat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	person := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"}
	for _, tc := range []struct {
		op  string
		run func(r *Router, asker gitlet.Principal) error
	}{
		{opCodeRead.spec.name, func(r *Router, asker gitlet.Principal) error {
			_, err := r.Code().Read(ctx, gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRepos})
			return err
		}},
		{opCodeResolveRef.spec.name, func(r *Router, asker gitlet.Principal) error {
			_, err := r.Code().ResolveRef(ctx, gitlet.RefQuery{Repo: "app", Refs: []string{"main"}, Asker: asker})
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
	push := func(asker gitlet.Principal, ref string) gitlet.Proposal {
		pushes++
		p := proposalTo(repo, fmt.Sprintf("019a0c2e-0000-7000-8000-%012x", pushes))
		p.Principal, p.Commands[0].Ref = asker, ref
		return p
	}
	// A seat's push git receives is its coding box's, naming the executor run
	// and its harness beside the turn that launched it.
	seatPush := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev",
		Run: gitlet.RunProvenance{TurnID: "019a0c2e-0000-7000-8000-00000000d00d",
			RunID: "019a0c2e-0000-7000-8000-00000000b0c5", Harness: "claude-code"}}
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
		got.Answer.Refused != gitlet.RefusalBusy || log.appends() != 1 {
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
		got.Answer.Refused != gitlet.RefusalBusy || ownLog.appends() != 1 {
		t.Errorf("this node's own committer behind the configuration, its person's push = (%+v, %v), its "+
			"log asked %d times; want it decided there", got, err, ownLog.appends())
	}
}

// gatedCommitter makes n's code half a REAL committer, gated as the engine
// gates one (behind the configuration exactly while n is) and judging a seat's
// push by [platform]'s chart, over a log whose every append is a counted
// conflict: a push that reached it was decided on n, one that did not was
// handed on before any decide.
func gatedCommitter(t *testing.T, n *fakeNode) *refusingLog {
	t.Helper()
	log := &refusingLog{}
	c, err := gitlet.NewCommitter(gitlet.CommitterOptions{
		Publisher: log, DB: codeRows(t, statelog.Position{Stream: codeStream, Generation: 1, Seq: 1}).Reader(),
		NodeID: n.name, Limits: gitlet.DefaultLimits,
		Retention: func() time.Duration { return gitlet.DefaultDeletedRetention },
		SeatRules: func() (gitlet.SeatRules, bool) {
			return gitlet.SeatRules{Roster: platform{}, Zone: time.UTC}, true
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
	n.set(func(n *fakeNode) { n.codeHalf = gitlet.Served{Committer: c} })
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

// EVERY OPERATION THAT JUDGES A GRANT IS DECLARED: every operation on a
// domain's copy whose arguments carry the gitlet.Principal it judges (a read's
// Asker, a lifecycle request's Actor) declares judgesGrant, so one added later
// is held by the hard arm, and none naming no such principal does. A git
// operation declares none: its Gitlet node answers a seat itself
// (gitlet.UnreadyBehindConfig); declared, it would refuse persons' reads too.
// A READ's declaration judges a SEAT alone (a person's read consults no chart);
// a lifecycle request's judges EVERY actor, a person's create's owner included.
//
// Mutations: drop code.resolve_ref's or code.lifecycle's declaration, or
// declare code.git_read — each red.
func TestEveryOperationThatJudgesAGrantIsDeclared(t *testing.T) {
	t.Parallel()
	principal := reflect.TypeFor[gitlet.Principal]()
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
			t.Errorf("%s is a git operation and declares that it judges a grant: its Gitlet node judges a "+
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
	seat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	person := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-founder", Seat: "founder"}
	for _, tc := range []struct {
		spec *opSpec
		args any
		want bool
	}{
		{opCodeRead.spec, gitlet.ReadQuery{Asker: seat, Kind: gitlet.ReadRepos}, true},
		{opCodeRead.spec, gitlet.ReadQuery{Asker: person, Kind: gitlet.ReadRepos}, false},
		{opCodeRead.spec, gitlet.ReadQuery{Kind: gitlet.ReadIntroduced}, false},
		{opCodeRead.spec, gitlet.ReadQuery{Kind: gitlet.ReadRunPushes}, false},
		{opCodeResolveRef.spec, gitlet.RefQuery{Asker: seat}, true},
		{opCodeResolveRef.spec, gitlet.RefQuery{Asker: person}, false},
		// A SEAT'S WRITE OF A CHANGE, which its grant decides whether it may
		// make; a person's consults no chart.
		{opCodeChange.spec, gitlet.ChangeProposal{Asker: seat}, true},
		{opCodeChange.spec, gitlet.ChangeProposal{Asker: person}, false},
		// A LANDING, the home's own write naming no asker, judges no grant.
		{opCodeChange.spec, gitlet.ChangeProposal{Landed: &gitlet.ChangeLanded{}}, false},
		// EVERY ENQUEUE, whoever asks (its gate counts approvals, owners' lead
		// chains and a seat's merge authority by the chart), and a seat's
		// dequeue; a person's dequeue consults no chart.
		{opCodeQueue.spec, gitlet.QueueProposal{Asker: seat, Enqueue: &gitlet.Enqueue{}}, true},
		{opCodeQueue.spec, gitlet.QueueProposal{Asker: person, Enqueue: &gitlet.Enqueue{}}, true},
		{opCodeQueue.spec, gitlet.QueueProposal{Asker: seat, Dequeue: &gitlet.Dequeue{}}, true},
		{opCodeQueue.spec, gitlet.QueueProposal{Asker: person, Dequeue: &gitlet.Dequeue{}}, false},
		// EVERY MERGE, whoever asks: its gate counts approvals, owners' lead
		// chains and a seat's merge authority by the chart.
		{opCodeMerge.spec, gitlet.MergeProposal{Asker: seat}, true},
		{opCodeMerge.spec, gitlet.MergeProposal{Asker: person}, true},
		{opCodeLifecycle.spec, gitlet.LifecycleRequest{Kind: gitlet.LifecycleHold, Actor: seat}, true},
		{opCodeLifecycle.spec, gitlet.LifecycleRequest{Kind: gitlet.LifecycleCreate, Actor: person}, true},
		// A WRITE OF A CHANGE'S CONVERSATION THE CHART JUDGES: a seat's, and an
		// ask of reviewers by anybody, each reviewer judged by the chart; a
		// person's comment, review, answer and withdrawal consult none.
		{opCodeChecks.spec, gitlet.ChecksRequest{Asker: seat, Rerun: &gitlet.CheckRerun{}}, true},
		{opCodeChecks.spec, gitlet.ChecksRequest{Asker: person, Rerun: &gitlet.CheckRerun{}}, false},
		{opCodeChecks.spec, gitlet.ChecksRequest{Asker: person, Report: &gitlet.CheckReported{}}, false},
		{opCodeChecks.spec, gitlet.ChecksRequest{Queued: &gitlet.CheckQueued{}}, false},
		{opCodeConversation.spec, gitlet.ConversationWrite{Asker: seat, Comment: &gitlet.Comment{}}, true},
		{opCodeConversation.spec, gitlet.ConversationWrite{Asker: person, Comment: &gitlet.Comment{}}, false},
		{opCodeConversation.spec, gitlet.ConversationWrite{Asker: seat, Review: &gitlet.Review{}}, true},
		{opCodeConversation.spec, gitlet.ConversationWrite{Asker: person, Review: &gitlet.Review{}}, false},
		{opCodeConversation.spec, gitlet.ConversationWrite{Asker: seat, FindingResolve: &gitlet.FindingResolve{}}, true},
		{opCodeConversation.spec, gitlet.ConversationWrite{Asker: person, FindingResolve: &gitlet.FindingResolve{}},
			false},
		{opCodeConversation.spec, gitlet.ConversationWrite{Asker: person, Reviewers: &gitlet.ReviewerRequest{
			Request: []gitlet.Reviewer{{Handle: "dev"}}}}, true},
		{opCodeConversation.spec, gitlet.ConversationWrite{Asker: person, Reviewers: &gitlet.ReviewerRequest{
			Withdraw: []string{"dev"}}}, false},
		// A REVISION'S FACTS, the home's own write naming no asker, whose path
		// owners the chart resolves; and A MERGEABILITY, the home's too, whose
		// readiness the chart judges (whose approvals count, who maintains).
		{opCodeConversation.spec, gitlet.ConversationWrite{Revise: &gitlet.Revise{}}, true},
		{opCodeConversation.spec, gitlet.ConversationWrite{Mergeability: &gitlet.Mergeability{}}, true},
		{opCodeConversation.spec, gitlet.ConversationWrite{Mergeability: &gitlet.Mergeability{Refresh: true}}, true},
		// A PERSON'S MARK OF THEIR OWN CODE INBOX, held to the handle by the
		// principal alone: no chart; a seat's is refused by the writer.
		{opCodeMark.spec, gitlet.MarkWrite{Asker: person, Handle: "founder"}, false},
		{opCodeMark.spec, gitlet.MarkWrite{Asker: seat, Handle: "dev"}, false},
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

// PRODUCTION'S GIT READ ATTEMPT HOLDS A GIT READ'S WAITS at their whole
// budgets: a unit of the seat git-work share, the sync to the asker's floor, a
// linearizable re-sync of a missing id, the read's own git and the reply's
// margin, 2 + 2 + 2 + 10 + 1 = 17 s, never a data read's ten seconds. Each term
// is spelled from its own constant, so one raised without the attempt fails
// here rather than as a healthy home heard silent.
func TestProductionsGitReadAttemptHoldsAGitReadsWaits(t *testing.T) {
	t.Parallel()
	waits := gitlet.GitShareWait + 2*statelog.ReadBudget + gitproc.ReadDeadline + gitlet.ReplyMargin
	if got := Production().GitReadAttempt; got != waits || waits != 17*time.Second {
		t.Errorf("production's git read attempt is %v, want the %v a git read's waits take at their whole "+
			"budgets with the reply's margin (17s)", got, waits)
	}
	r := newFleet(t).client
	if got := r.wholeAttempt(opCodeGitRead.spec, exchange{}); got != Production().GitReadAttempt {
		t.Errorf("a router gives a git read %v, want its git read attempt %v", got, Production().GitReadAttempt)
	}
	if got := r.wholeAttempt(opCodeRead.spec, exchange{}); got != Production().ReadAttempt {
		t.Errorf("a router gives a read of the rows %v, want its read attempt %v", got, Production().ReadAttempt)
	}
}

// A ROUTER NEEDS A GIT PLACEMENT: without one every git read would read as a
// fleet with no Gitlet node, a wiring mistake posing as a deployment fact, so
// it is refused by name; an empty placement says the fleet has none. An
// unanswered git read names the GITLET nodes, a data operation the data nodes.
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
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "no node with the gitlet role is live") {
		t.Errorf("a read with no Gitlet node live = %v, want it unserved saying so", err)
	}
	// AND IT NAMES THE GITLET NODES, never the data nodes: whoever reads it
	// has to find which nodes could not answer.
	if err == nil || !unserved.Git || !strings.Contains(err.Error(), "no Gitlet node serves") ||
		strings.Contains(err.Error(), "data node") {
		t.Errorf("a git read nobody answered says %q, want it to name the Gitlet nodes", err)
	}
	// A DATA OPERATION IS NOT ASKED OF THE GITLET NODES: the placements are two.
	if _, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); !errors.As(err, &unserved) ||
		unserved.Git || !strings.Contains(err.Error(), "no data node serves") {
		t.Errorf("a tracker read with no data node = %v, want it unserved, naming the data nodes", err)
	}
}

// AN ENGINE WRITE THAT RAN NOTHING MOVES ON WITH THE SAME WRITE (ADR-0089,
// ADR-0092): idempotent by its push id, derived from its operation, the
// runner-up is asked the very write.
//   - HOME RAN NOTHING (gitlet.ErrNotServing): asked stamped home, since the
//     home brings no copy up, and the home is asked writes LAST for 30 s;
//   - HOME WENT SILENT (perhaps landed): asked unstamped, since the silent node
//     may be filling, and the home is asked last;
//   - HOME AT ITS SHARE: asked unstamped, and the home is not demoted.
//
// Mutations: answer gitlet.ErrNotServing (no ranNothing) or an unready home's
// gitlet.ErrGitUnready (no gitUnready) as the answer — the walk ends on it,
// red; declare it a once-write — a silent home's ends ErrOutcomeUnknown, red.
func TestAnEngineWriteThatRanNothingMovesOnWithTheSameWrite(t *testing.T) {
	t.Parallel()
	holders := []string{"git-a", "git-b", "git-c"}
	for _, tc := range []struct {
		name string
		// fail has the home run none of a write; mend ends it.
		fail, mend func(f *fleet, home *fakeGit, node *fakeNode)
		// stamped is whether the runner-up is asked the write stamped home,
		// and demoted whether the next write is the runner-up's.
		stamped, demoted bool
	}{
		{
			name: "ran nothing",
			fail: func(_ *fleet, home *fakeGit, _ *fakeNode) {
				home.set(func(g *fakeGit) {
					g.ranNothing = fmt.Errorf("%w: %s's Gitlet is stopping", gitlet.ErrNotServing, g.name)
				})
			},
			mend:    func(_ *fleet, home *fakeGit, _ *fakeNode) { home.set(func(g *fakeGit) { g.ranNothing = nil }) },
			stamped: true, demoted: true,
		},
		{
			name:    "went silent",
			fail:    func(_ *fleet, _ *fakeGit, node *fakeNode) { node.set(func(n *fakeNode) { n.silent = true }) },
			mend:    func(_ *fleet, _ *fakeGit, node *fakeNode) { node.set(func(n *fakeNode) { n.silent = false }) },
			demoted: true,
		},
		{
			name: "is at its share",
			fail: func(_ *fleet, home *fakeGit, _ *fakeNode) {
				home.set(func(g *fakeGit) { g.unready = gitlet.UnreadyShare })
			},
			mend: func(_ *fleet, home *fakeGit, _ *fakeNode) { home.set(func(g *fakeGit) { g.unready = "" }) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t)
			halves := f.gitletNodes(t, holders)
			repo := repoID(31)
			order := rendezvous.Order(repo, holders)
			tc.fail(f, halves[order[0]], f.nodes[order[0]])

			w := writeOf(repo)
			res, err := f.client.Code().GitWrite(t.Context(), w)
			if err != nil || res.Commit != order[1] || res.PushID != gitlet.WritePushID(w.OpID) {
				t.Fatalf("a write whose home %s = (%+v, %v), want the runner-up %s's answer under the write's own "+
					"push", tc.name, res, err, order[1])
			}
			asked := halves[order[1]].writes()
			if len(asked) != 1 {
				t.Fatalf("the runner-up was asked %d writes, want the one its home ran nothing of", len(asked))
			}
			sent := w
			sent.Home = tc.stamped
			if !reflect.DeepEqual(asked[0].w, sent) {
				t.Errorf("the runner-up was asked %+v, want the very write its home was asked — the same operation, "+
					"the same push — stamped home %t: %+v", asked[0].w, tc.stamped, sent)
			}
			if got := halves[order[2]].writes(); len(got) != 0 {
				t.Errorf("the third node was asked %d writes, want none: the runner-up answered", len(got))
			}

			tc.mend(f, halves[order[0]], f.nodes[order[0]])
			want := order[0]
			if tc.demoted {
				want = order[1]
			}
			if next, err := f.client.Code().GitWrite(t.Context(), writeOf(repo)); err != nil || next.Commit != want {
				t.Errorf("the next write, its home having %s = (%+v, %v), want it built by %s", tc.name, next, err,
					want)
			}
		})
	}
}

// AN ENGINE WRITE THE ONE RULE REFUSES IS ASKED OF NO GITLET NODE: the router
// holds it to gitlet.EngineWrite.Validate, so a malformed write (a branch git
// refuses, a seat's write naming no turn, a payload past its bound as the wire
// spells it: a revert's message, or a commit message of '<', six bytes on the
// wire) is gitlet.ErrInvalid at once, naming the field, costing no node a trip.
//
// Mutations: carry it unvalidated — the node builds it, red; measure the
// message by its length — the '<' commit and the revert are asked, red.
func TestAnEngineWriteTheOneRuleRefusesIsAskedOfNoNode(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	halves := f.gitletNodes(t, []string{"git-a", "git-b"})
	badRef := writeOf(repoID(4))
	badRef.Ref = "refs/heads/a..b"
	noTurn := seatWriteOf(repoID(4))
	noTurn.Principal.Run = gitlet.RunProvenance{}
	longRevert := writeOf(repoID(4))
	longRevert.Kind, longRevert.Files = gitlet.WriteRevert, nil
	longRevert.Commit = "2222222222222222222222222222222222222222"
	longRevert.Message = strings.Repeat("x", gitlet.MaxCommitPayloadBytes)
	angled := writeOf(repoID(4))
	angled.Message = strings.Repeat("<", gitlet.MaxCommitPayloadBytes/2)
	for name, w := range map[string]gitlet.EngineWrite{
		"a branch git refuses":                        badRef,
		"a seat's write naming no turn":               noTurn,
		"a revert whose message is past the bound":    longRevert,
		"a commit whose message of '<' is half of it": angled,
	} {
		if _, err := f.client.Code().GitWrite(t.Context(), w); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s = %v, want gitlet.ErrInvalid before any Gitlet node is asked", name, err)
		}
	}
	if got := writesAsked(halves); got["git-a"] != 0 || got["git-b"] != 0 {
		t.Errorf("a write the one rule refuses was asked of the Gitlet nodes: %v", got)
	}
}

// A GIT WRITE IS NEVER ANSWERED ON ANOTHER LANE: an engine write runs on the
// git writes' lane alone, a read by object id on the reads' lane alone; each
// asked on another is unserved, naming its lane, nothing run, so a write never
// holds a slot a seat's read, a push's proposal or a fetch's resolve needs.
func TestAGitWriteIsNeverAnsweredOnAnotherLane(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	half := f.gitletNodes(t, []string{"git-a"})["git-a"]
	write, err := json.Marshal(writeOf(repoID(1)))
	if err != nil {
		t.Fatal(err)
	}
	read, err := json.Marshal(treeOf(repoID(1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		op      string
		args    json.RawMessage
		subject string
		want    string
	}{
		{opCodeGitWrite.spec.name, write, Subject("git-a"), "the git-writes lane"},
		{opCodeGitWrite.spec.name, write, ProposalSubject("git-a"), "the git-writes lane"},
		{opCodeGitWrite.spec.name, write, laneGitReads.subject("git-a"), "the git-writes lane"},
		{opCodeGitRead.spec.name, read, laneGitWrites.subject("git-a"), "the git lane"},
	} {
		rep := askSignedOn(t, f.start(t), tc.subject, request{Op: tc.op, Args: tc.args, From: "agent-9"})
		if rep.Unserved != unservedNoBackend || !strings.Contains(rep.Detail, tc.want) {
			t.Errorf("%s asked on %s answered %+v, want it unserved naming %s", tc.op, tc.subject, rep, tc.want)
		}
	}
	if len(half.writes()) != 0 || len(half.reads()) != 0 {
		t.Errorf("an operation asked on another lane ran: %d writes, %d reads", len(half.writes()),
			len(half.reads()))
	}
	rep := askSignedOn(t, f.start(t), laneGitWrites.subject("git-a"),
		request{Op: opCodeGitWrite.spec.name, Args: write, From: "agent-9"})
	if rep.Unserved != "" || rep.Err != nil || len(half.writes()) != 1 {
		t.Errorf("a write asked on its own lane answered %+v and ran %d times, want it built once", rep,
			len(half.writes()))
	}
}

// PRODUCTION'S GIT WRITE ATTEMPT HOLDS AN ENGINE WRITE'S WAITS at their whole
// budgets: a share unit, three reads (sync to the floor, whether a moved head
// is its own push, the sync every refusal is judged again at), the build's
// git, the upload at the pace floor, the proposal's attempt and the reply's
// margin, 2 + 6 + 10 + 30 + 33 + 1 = 82 s, never a data write's sixty. An
// attempt that cannot hold the proposal after the margin is refused naming the
// field; a nanosecond longer is taken. Each term is spelled from its constant,
// so one raised alone fails here rather than as a healthy home heard silent.
func TestProductionsGitWriteAttemptHoldsAWritesWaits(t *testing.T) {
	t.Parallel()
	proposal := statelog.ReadBudget + gitlet.CommitBudget + gitlet.ReplyMargin
	waits := gitlet.GitShareWait + 3*statelog.ReadBudget + gitlet.WriteBuildDeadline + objstore.MiBPace + proposal +
		gitlet.ReplyMargin
	if got := Production().GitWriteAttempt; got != waits || waits != 82*time.Second {
		t.Errorf("production's git write attempt is %v, want the %v an engine write's waits take at their whole "+
			"budgets with the reply's margin (82s)", got, waits)
	}
	r := newFleet(t).client
	if got := r.wholeAttempt(opCodeGitWrite.spec, exchange{}); got != Production().GitWriteAttempt {
		t.Errorf("a router gives an engine write %v, want its git write attempt %v", got,
			Production().GitWriteAttempt)
	}
	if got := r.wholeAttempt(opCodeChange.spec, exchange{}); got != Production().WriteAttempt {
		t.Errorf("a router gives a write on a data node %v, want its write attempt %v", got,
			Production().WriteAttempt)
	}
	build := func(attempt time.Duration) error {
		timing := Production()
		timing.GitWriteAttempt = attempt
		_, err := NewRouter(RouterOptions{Self: "agent-1", Queue: &recordingAsker{}, Placement: &fakePlacement{},
			Git: &fakePlacement{}, Session: NewSession(), Timing: timing, Envelopes: fleetEnvelopes})
		return err
	}
	bound := proposal + gitlet.ReplyMargin
	if err := build(bound); err == nil || !strings.Contains(err.Error(), "Timing.GitWriteAttempt") ||
		!strings.Contains(err.Error(), "must hold the proposal it makes") {
		t.Errorf("a router whose git write attempt is the proposal's own attempt and the reply's margin (%v) "+
			"answered %v, want a refusal naming Timing.GitWriteAttempt: the Gitlet node would answer every write "+
			"busy before its upload", bound, err)
	}
	for _, attempt := range []time.Duration{bound + time.Nanosecond, Production().GitWriteAttempt} {
		if err := build(attempt); err != nil {
			t.Errorf("a router whose git write attempt is %v, over the %v a write's proposal needs, answered %v",
				attempt, bound, err)
		}
	}
}

// AN ENGINE WRITE'S ATTEMPT FOLLOWS ITS KIND'S BUILD (M5-D11): the build term
// (a push-landing write's ten seconds) becomes the kind's own, so merge_change
// (thirty) gets 102 s, rebase_change (forty) 112 s, every other kind 82 s, read
// off the write on every attempt, the home's included.
//
// Mutation: give every write the class's attempt → the merge's 82 s, red.
func TestAnEngineWritesAttemptFollowsItsKind(t *testing.T) {
	t.Parallel()
	prod := Production()
	r := newFleet(t).client
	for _, kind := range gitlet.EngineWriteKinds() {
		want := map[gitlet.EngineWriteKind]time.Duration{gitlet.WriteMergeChange: 102 * time.Second,
			gitlet.WriteRebaseChange: 112 * time.Second}[kind]
		if want == 0 {
			want = 82 * time.Second
		}
		if got := prod.GitWriteAttemptFor(kind); got != want {
			t.Errorf("production gives a %s %v, want %v: the git write attempt with its build term the kind's",
				kind, got, want)
		}
		w := gitlet.EngineWrite{Kind: kind, RepoID: uuid.Must(uuid.NewV7()).String()}
		x, err := exchangesOf(opCodeGitWrite.spec, opCodeGitWrite, nil, w, nil, w.RepoID)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.wholeAttempt(opCodeGitWrite.spec, x); got != want {
			t.Errorf("a router gives a %s %v on one Gitlet node, want %v", kind, got, want)
		}
		if got := r.wholeAttempt(opCodeGitWrite.spec, *x.home); got != want {
			t.Errorf("a router gives a %s %v on its walk's home, want %v", kind, got, want)
		}
	}
}

// A GIT WRITE IS HELD TO ITS OWN ATTEMPT, WHATEVER ITS CALLER ALLOWS: its node
// fits every stage into the git write attempt, so a node silent through it is
// wedged and the runner-up builds the same push; under the caller's deadline
// (a turn's, minutes away) a wedged home would hold the tool call until the
// turn gave up. The half is served the attempt less the reply's margin, peer
// and in process alike, under no caller deadline or a longer one; a SHORTER
// one stays the caller's.
//
// Mutation: give it the caller's deadline (budgetFor) — the write under a
// five-minute caller is served nearly five minutes, red.
//
// In a synctest bubble ([inBubble]): the request carries its wait, counted from
// its arrival on the node's clock, so on the machine's clock the instant it is
// served until moves with the trip and the wait's rounding to a millisecond.
func TestAGitWriteIsHeldToItsOwnAttempt(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		f := newFleet(t)
		half := f.gitletNodes(t, []string{"git-a"})["git-a"]
		attempt := Production().GitWriteAttempt
		for name, r := range map[string]*Router{
			"asked of the Gitlet node by another": f.client,
			"asked in process by the Gitlet node": f.gitRouter(t, "git-a", gitHalf{node: half}),
		} {
			for _, tc := range []struct {
				// caller is the caller's deadline (zero: none); callers is
				// whether it, not the attempt, bounds what the half is served.
				caller  time.Duration
				callers bool
			}{
				{0, false},
				{5 * time.Minute, false},
				{20 * time.Second, true},
			} {
				ctx, cancel := t.Context(), context.CancelFunc(func() {})
				if tc.caller > 0 {
					ctx, cancel = context.WithTimeout(ctx, tc.caller)
				}
				begun := time.Now()
				_, err := r.Code().GitWrite(ctx, writeOf(repoID(9)))
				callerDeadline := deadlineOf(ctx)
				cancel()
				if err != nil {
					t.Fatalf("%s, its caller's deadline %v: %v", name, tc.caller, err)
				}
				writes := half.writes()
				got := writes[len(writes)-1].deadline
				if tc.callers {
					if want := callerDeadline.Add(-gitlet.ReplyMargin); !got.Equal(want) {
						t.Errorf("%s, its caller's deadline %v: the git half was served until %v, want its "+
							"caller's deadline less the reply's margin, %v", name, tc.caller, got, want)
					}
					continue
				}
				if !servedAnAttempt(got, begun, attempt) {
					t.Errorf("%s, its caller's deadline %v: the git half was served until %v after the write "+
						"began (the zero time for no deadline: %v), want the router's git write attempt of %v "+
						"less the reply's margin", name, tc.caller, got.Sub(begun), got.IsZero(), attempt)
				}
			}
		}
	})
}
