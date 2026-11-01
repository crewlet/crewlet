package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
// applied, and every data node applies every log from boot. So a live data
// node whose row has not landed — no position yet — counts at zero on every
// log and blocks it: what it has still to apply is exactly what the trim must
// keep.
func TestEveryLogCountsTheLiveDataNodes(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.core.Load().log
	e.stopRetention()
	s.publishPositions(t.Context())
	r := &retention{fleet: e.backends.Fleet, state: s, nodeID: e.id,
		holders: fixedHolders{{NodeID: e.id}, {NodeID: "node-unreported"}}}
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
			if n.NodeID == "node-unreported" && (n.Seq != 0 || n.Generation != 0) {
				t.Errorf("%s counts the live data node with no row at %d@%d, want "+
					"zero — it has reported nothing, and everything it has still to "+
					"apply is what the trim must keep", running.domain.Name(), n.Seq,
					n.Generation)
			}
		}
		if want := slices.Sorted(slices.Values([]string{e.id, "node-unreported"})); !slices.Equal(ids, want) {
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
// order is computed over (ADR-0082) — a node that declares no roles among
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

// WHAT EACH GIT NODE ADVERTISES IS READ OFF THE ROUTER'S OWN WATCHED VIEW, ONE
// LISTING FOR THE NODES AND THEIR FIGURES: the listing names exactly the git
// nodes the git placement names — a node that declares no roles among them,
// since such a node has every role, and never one whose roles leave git out,
// whatever its row says — and beside each, what its presence row says beside
// its status: a budget the row names, across the store's JSON round trip, said
// — a beat that carried no status at all included, since the figure is not in
// the status; a zero said as zero, a node with nothing to give its background
// work; and no entry for a row that names no figure, a node NOT SAYING (absent
// is not zero), whatever its status says. A view that has never listed is
// UNKNOWN, never a fleet of no git node; a node with no view has no fleet, and
// its listing names nobody.
//
// Mutations: read a row that names no figure as a node with nothing to give —
// the quiet node says zero, red; read the figure out of the status — the node
// whose beat dropped its status says nothing, red; list every node of the view
// rather than its git nodes — the data node that writes a figure is listed,
// red; read a view that has never listed as nobody — no error, red.
func TestTheGitListingIsWhatEachGitNodesPresenceSays(t *testing.T) {
	t.Parallel()
	const budget = int64(64)<<30 + 12_345
	backend := coordmem.New()
	gitRoles := []string{"data", "git", "ingress"}
	claimMeta(t, backend, map[string]map[string]any{
		"git-large": {"roles": gitRoles, coord.GitBackgroundKey: budget},
		"git-cold": {"roles": gitRoles, coord.GitBackgroundKey: int64(0),
			coord.StatusKey: coord.NodeStatus{InFlight: 1}.Meta()},
		"git-quiet": {"roles": gitRoles, coord.StatusKey: coord.NodeStatus{InFlight: 1}.Meta()},
		"every-a":   {coord.GitBackgroundKey: int64(8) << 30},
		"data-c":    {"roles": []string{"data", "ingress"}, coord.GitBackgroundKey: budget},
	})
	view, err := coord.NewLeaseView(backend, coord.ClassNode,
		coord.ViewOptions{Every: time.Hour, Trust: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	git := (&Engine{dataView: view}).gitPlacement()
	if listing, err := git.Listing(); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("a view that has never listed answers the git listing %+v (%v), want unknown", listing, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = view.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, 5*time.Second, "the view to list", func() bool { return !view.ListedAt().IsZero() })
	listing, err := git.Listing()
	if err != nil {
		t.Fatalf("the git listing: %v", err)
	}
	holders, err := git.Holders()
	if err != nil {
		t.Fatalf("the git placement: %v", err)
	}
	want := []string{"every-a", "git-cold", "git-large", "git-quiet"}
	if !slices.Equal(listing.Nodes, want) || !slices.Equal(holders, want) {
		t.Errorf("the git listing names %v and the git placement %v, want both every live git node %v",
			listing.Nodes, holders, want)
	}
	said := map[string]int64{"every-a": 8 << 30, "git-cold": 0, "git-large": budget}
	if !maps.Equal(listing.Budgets, said) {
		t.Errorf("the git listing says %v, want %v: a figure where a git node's row names one, zero included, "+
			"and none for a node not saying or a node that is no git node", listing.Budgets, said)
	}
	if listing, err := (&Engine{}).gitPlacement().Listing(); err != nil || len(listing.Nodes) != 0 ||
		len(listing.Budgets) != 0 {
		t.Errorf("a node with no view lists %+v (%v), want no git node and nobody saying anything", listing, err)
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
// Mutation: answer newRouter's git placement's listing with no figure in it —
// nobody saying anything, the 40 GiB work stays with each home, red.
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
