package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY LOG COUNTS THE LIVE DATA NODES, AND ONE THAT HAS NOT REPORTED COUNTS AT
// ZERO.
//
// The trim may not remove a record a node that applies the log has not
// applied, and every data node applies every log from boot. So a data node
// still joining — no position yet — counts at zero on every log and blocks it:
// the tail it is about to replay is exactly what the trim must keep.
func TestEveryLogCountsTheLiveDataNodes(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.core.Load().log
	e.stopRetention()
	s.publishPositions(t.Context())
	r := &retention{fleet: e.backends.Fleet, state: s, nodeID: e.id,
		holders: fixedHolders{{NodeID: e.id}, {NodeID: "node-joining"}}}
	shared, err := r.read(t.Context())
	if err != nil {
		t.Fatalf("read the tick's inputs: %v", err)
	}
	if len(s.running()) == 0 {
		t.Fatal("the premise: the node runs no log")
	}
	for _, running := range s.running() {
		var ids []string
		for _, n := range shared.counted(running, nil) {
			ids = append(ids, n.NodeID)
			if n.NodeID == "node-joining" && (n.Seq != 0 || n.Generation != 0) {
				t.Errorf("%s counts the joining data node at %d@%d, want zero — it "+
					"has reported nothing, and the tail it is about to replay is what "+
					"the trim must keep", running.domain.Name(), n.Seq, n.Generation)
			}
		}
		if want := slices.Sorted(slices.Values([]string{e.id, "node-joining"})); !slices.Equal(ids, want) {
			t.Errorf("%s counts %v, want every live data node %v", running.domain.Name(), ids, want)
		}
	}
}

// THE HOLDERS ARE THE LIVE DATA NODES, LISTED AFRESH: the estate is held whole
// by every data node and by nothing else, so the counted set's live half is the
// presence the trim has always counted — data nodes only, since a node without
// `data` applies no log — the same nodes the router asks.
func TestTheHoldersAreTheLiveDataNodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := coordmem.New()
	claimPresences(t, backend, map[string][]string{
		"data-a": {"data", "seats"}, "agent-1": {"seats"},
	})
	e := &Engine{backends: &Backends{Coord: backend}}
	holders := e.holdersOf()
	if _, byPresence := holders.(presenceHolders); !byPresence {
		t.Fatalf("the holders are answered by %T, want a fresh presence listing", holders)
	}
	live, err := holders.LiveData(ctx)
	if err != nil {
		t.Fatalf("LiveData: %v", err)
	}
	if len(live) != 1 || live[0].NodeID != "data-a" {
		t.Errorf("the estate is held by %v, want the one live data node", live)
	}
}

// THE ROUTED DATA NODES ARE THE ROUTER'S OWN PLACEMENT: every live data node
// the WATCHED view names, sorted — the set a keyed operation's rendezvous order
// is computed over — and never a node without `data`; and a view that has not
// listed is UNKNOWN, never "none", since a caller waiting for the fleet's
// routing to be whole must not read an unlisted view as a fleet of nobody.
func TestTheRoutedDataNodesAreTheRoutersPlacement(t *testing.T) {
	t.Parallel()
	backend := coordmem.New()
	claimPresences(t, backend, map[string][]string{
		"data-b": {"data"}, "data-a": {"data", "seats"}, "agent-1": {"seats"},
	})
	view, err := coord.NewLeaseView(backend, coord.ClassNode,
		coord.ViewOptions{Every: time.Hour, Trust: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{dataView: view}
	if nodes, err := e.RoutedDataNodes(); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("a view that has never listed routes to %v (%v), want unknown", nodes, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = view.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, 5*time.Second, "the view to list", func() bool { return !view.ListedAt().IsZero() })
	nodes, err := e.RoutedDataNodes()
	if err != nil {
		t.Fatalf("RoutedDataNodes: %v", err)
	}
	placed, err := e.estatePlacement().Holders()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"data-a", "data-b"}; !slices.Equal(nodes, want) || !slices.Equal(nodes, placed) {
		t.Errorf("the router asks %v by RoutedDataNodes and %v by its placement, want both %v",
			nodes, placed, want)
	}
}

// THE GIT PLACEMENT IS THE LIVE GIT NODES: every node the router's WATCHED view
// names whose profile has the git role, sorted — the set a repository's git
// order is computed over (ADR-0059) — a node that declares no roles among
// them, since such a node has every role, and never a data node that lists its
// roles without git or a node with seats alone; off the same view as the data
// placement, so the two cannot disagree about who is live, and UNKNOWN before
// the view has listed, never a fleet of no git node.
//
// Mutation: read the data role for the git one — the data node listing no git
// role is named a git node, red.
func TestTheGitPlacementIsTheLiveGitNodes(t *testing.T) {
	t.Parallel()
	backend := coordmem.New()
	claimPresences(t, backend, map[string][]string{
		"git-b": {"data", "git", "ingress"}, "every-a": nil, "data-c": {"data", "ingress"},
		"agent-1": {"seats"},
	})
	view, err := coord.NewLeaseView(backend, coord.ClassNode,
		coord.ViewOptions{Every: time.Hour, Trust: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{dataView: view}
	if nodes, err := e.gitPlacement().Holders(); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("a view that has never listed names the git nodes %v (%v), want unknown", nodes, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = view.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, 5*time.Second, "the view to list", func() bool { return !view.ListedAt().IsZero() })
	nodes, err := e.gitPlacement().Holders()
	if err != nil {
		t.Fatalf("the git placement: %v", err)
	}
	if want := []string{"every-a", "git-b"}; !slices.Equal(nodes, want) {
		t.Errorf("the git placement names %v, want every live node with the git role %v", nodes, want)
	}
}

// WHAT EACH GIT NODE ADVERTISES IS READ OFF THE ROUTER'S OWN WATCHED VIEW, as
// its presence row says it beside its status: a budget the row names, across
// the store's JSON round trip, is said — a beat that carried no status at all
// included, since the figure is not in the status; a zero is said as zero, a
// node with nothing to give its background work; a row that names no figure is
// a node NOT SAYING (absent is not zero), whatever its status says, and so is
// a node the view names no lease of; a view that has never listed is UNKNOWN,
// never a fleet in which nobody says anything — which is what a node with no
// view, and so no fleet, answers.
//
// Mutations: read a row that names no figure as a node with nothing to give —
// the quiet node said, red; read the figure out of the status — the node whose
// beat dropped its status says nothing, red; read a view that has never listed
// as nobody saying — no error, red.
func TestTheGitBudgetsAreWhatEachGitNodesPresenceSays(t *testing.T) {
	t.Parallel()
	const budget = int64(64)<<30 + 12_345
	backend := coordmem.New()
	gitRoles := []string{"data", "git", "ingress"}
	claimMeta(t, backend, map[string]map[string]any{
		"git-large": {"roles": gitRoles, coord.GitBackgroundKey: budget},
		"git-cold": {"roles": gitRoles, coord.GitBackgroundKey: int64(0),
			coord.StatusKey: coord.NodeStatus{InFlight: 1}.Meta()},
		"git-quiet": {"roles": gitRoles, coord.StatusKey: coord.NodeStatus{InFlight: 1}.Meta()},
	})
	view, err := coord.NewLeaseView(backend, coord.ClassNode,
		coord.ViewOptions{Every: time.Hour, Trust: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	budgets := (&Engine{dataView: view}).gitBudgets()
	if got, said, err := budgets.BackgroundBudget("git-large"); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("a view that has never listed says git-large gives %d (said %v, %v), want unknown", got, said,
			err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = view.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, 5*time.Second, "the view to list", func() bool { return !view.ListedAt().IsZero() })
	for node, want := range map[string]struct {
		budget int64
		said   bool
	}{
		"git-large": {budget, true}, "git-cold": {0, true}, "git-quiet": {0, false}, "git-gone": {0, false},
	} {
		got, said, err := budgets.BackgroundBudget(node)
		if err != nil || got != want.budget || said != want.said {
			t.Errorf("%s gives its background work %d (said %v, %v), want %d (said %v)", node, got, said, err,
				want.budget, want.said)
		}
	}
	if got, said, err := (&Engine{}).gitBudgets().BackgroundBudget("git-large"); got != 0 || said || err != nil {
		t.Errorf("a node with no view says git-large gives %d (said %v, %v), want nobody saying", got, said, err)
	}
}

// THE ROUTER A NODE BUILDS PLACES A REPOSITORY'S MAINTENANCE BY WHAT EACH GIT
// NODE ADVERTISES: over three git nodes whose presence rows say 8, 64 and 8
// GiB — the large one's beat carrying no status, as a beat whose status read
// overran carries none — the router [Engine.newRouter] builds — on a node
// holding nothing, as a seats node's is — places work only the large node
// holds on it for every one of two hundred repositories, and work every node
// holds on each repository's home.
//
// Mutation: hand newRouter's router git budgets in which nobody says anything —
// the 40 GiB work stays with each home, red.
func TestTheRoutersMaintainerWeighsWhatEachGitNodeAdvertises(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	backend := coordmem.New()
	gitRoles := []string{"data", "git", "ingress"}
	claimMeta(t, backend, map[string]map[string]any{
		"git-a": {"roles": gitRoles, coord.GitBackgroundKey: 8 * gib,
			coord.StatusKey: coord.NodeStatus{}.Meta()},
		"git-b": {"roles": gitRoles, coord.GitBackgroundKey: 64 * gib},
		"git-c": {"roles": gitRoles, coord.GitBackgroundKey: 8 * gib,
			coord.StatusKey: coord.NodeStatus{}.Meta()},
		"agent-1": {"roles": []string{"seats"}, coord.StatusKey: coord.NodeStatus{}.Meta()},
	})
	view, err := coord.NewLeaseView(backend, coord.ClassNode,
		coord.ViewOptions{Every: time.Hour, Trust: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = view.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, 5*time.Second, "the view to list", func() bool { return !view.ListedAt().IsZero() })
	e := &Engine{dataView: view, profile: placement.NodeProfile{ID: "agent-1",
		Roles: placement.Roles(placement.RoleSeats)}}
	r, err := e.newRouter(silentAsker{}, "agent-1", routerEnvelopes(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		repo := fmt.Sprintf("019a0c2e-0000-7000-8000-%012x", i)
		if m, err := r.Maintainer(repo, 40*gib); err != nil || m != "git-b" {
			t.Fatalf("Maintainer(%s, 40 GiB) = (%s, %v), want git-b, the one node advertising room for it",
				repo, m, err)
		}
		home, err := r.Home(repo)
		if err != nil {
			t.Fatal(err)
		}
		if m, err := r.Maintainer(repo, 4*gib); err != nil || m != home {
			t.Fatalf("Maintainer(%s, 4 GiB) = (%s, %v), want its home %s", repo, m, err, home)
		}
	}
}

// claimMeta claims a presence lease for every node, carrying its meta.
func claimMeta(t testing.TB, backend coord.Backend, metas map[string]map[string]any) {
	t.Helper()
	for id, meta := range metas {
		if _, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":1", TTL: time.Hour, Meta: meta,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// claimPresences claims a presence lease for every node, with its roles.
func claimPresences(t testing.TB, backend coord.Backend, roles map[string][]string) {
	t.Helper()
	for id, r := range roles {
		if _, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":1", TTL: time.Hour, Meta: map[string]any{"roles": r},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// fixedHolders answers the live data nodes from a list a case wrote.
type fixedHolders []statelog.Presence

func (f fixedHolders) LiveData(context.Context) ([]statelog.Presence, error) {
	return slices.Clone(f), nil
}
