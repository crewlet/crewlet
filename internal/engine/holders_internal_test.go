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
// ZERO: the trim may not remove a record a node applying the log has not
// applied, and every data node applies every log from boot, so a live data node
// with no position row yet holds every log at zero.
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

// THE HOLDERS ARE THE LIVE DATA NODES, LISTED AFRESH: every data node and
// nothing else holds the estate whole (a node without `data` applies no log),
// so the counted set's live half is the nodes the router asks.
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

// THE ROUTED DATA NODES ARE THE ROUTER'S OWN PLACEMENT: every live data node the
// WATCHED view names, sorted (the set a keyed operation's rendezvous order is
// computed over), never a node without `data`; and an unlisted view is UNKNOWN,
// never "none", or a caller waiting for whole routing reads a fleet of nobody.
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

// THE GIT PLACEMENT IS THE LIVE GITLET NODES: every node the router's WATCHED
// view names with the gitlet role, sorted (the set a repository's git order is
// computed over, ADR-0089) — a node declaring no roles has every role; a data
// node listing roles without gitlet, or a seats-only node, is out. It reads the
// data placement's view, so the two cannot disagree on who is live, and is
// UNKNOWN before the view has listed, never a fleet of no Gitlet node.
//
// Mutation: read the data role for the gitlet one — the data node listing no
// gitlet role is named a Gitlet node, red.
func TestTheGitPlacementIsTheLiveGitletNodes(t *testing.T) {
	t.Parallel()
	backend := coordmem.New()
	claimPresences(t, backend, map[string][]string{
		"git-b": {"data", "gitlet", "ingress"}, "every-a": nil, "data-c": {"data", "ingress"},
		"agent-1": {"seats"},
	})
	view, err := coord.NewLeaseView(backend, coord.ClassNode,
		coord.ViewOptions{Every: time.Hour, Trust: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{dataView: view}
	if nodes, err := e.gitPlacement().Holders(); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("a view that has never listed names the Gitlet nodes %v (%v), want unknown", nodes, err)
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
		t.Errorf("the git placement names %v, want every live node with the gitlet role %v", nodes, want)
	}
}

// WHAT EACH GITLET NODE ADVERTISES IS READ OFF THE ROUTER'S WATCHED VIEW, IN ONE
// LISTING: exactly the git placement's nodes (no-roles nodes in, a node whose
// roles leave gitlet out never, whatever its row says), each with the budget
// its presence row names — across the JSON round trip, from a beat with no
// status too (the figure is not in the status), zero as zero, and no entry for
// a row naming none (absent is NOT SAYING, not zero). An unlisted view is
// UNKNOWN; a node with no view lists nobody.
//
// Mutations, each red: a row naming no figure read as zero (the quiet node says
// zero); the figure read out of the status (the statusless beat says nothing);
// every node of the view listed (the data node with a figure appears); an
// unlisted view read as nobody (no error).
func TestTheGitListingIsWhatEachGitletNodesPresenceSays(t *testing.T) {
	t.Parallel()
	const budget = int64(64)<<30 + 12_345
	backend := coordmem.New()
	gitletRoles := []string{"data", "gitlet", "ingress"}
	claimMeta(t, backend, map[string]map[string]any{
		"git-large": {"roles": gitletRoles, coord.GitBackgroundKey: budget},
		"git-cold": {"roles": gitletRoles, coord.GitBackgroundKey: int64(0),
			coord.StatusKey: coord.NodeStatus{InFlight: 1}.Meta()},
		"git-quiet": {"roles": gitletRoles, coord.StatusKey: coord.NodeStatus{InFlight: 1}.Meta()},
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
		t.Errorf("the git listing names %v and the git placement %v, want both every live Gitlet node %v",
			listing.Nodes, holders, want)
	}
	said := map[string]int64{"every-a": 8 << 30, "git-cold": 0, "git-large": budget}
	if !maps.Equal(listing.Budgets, said) {
		t.Errorf("the git listing says %v, want %v: a figure where a Gitlet node's row names one, zero included, "+
			"and none for a node not saying or a node that is no Gitlet node", listing.Budgets, said)
	}
	if listing, err := (&Engine{}).gitPlacement().Listing(); err != nil || len(listing.Nodes) != 0 ||
		len(listing.Budgets) != 0 {
		t.Errorf("a node with no view lists %+v (%v), want no Gitlet node and nobody saying anything", listing, err)
	}
}

// THE ROUTER A NODE BUILDS PLACES A REPOSITORY'S MAINTENANCE BY WHAT EACH GITLET
// NODE ADVERTISES: with Gitlet nodes saying 8, 64 and 8 GiB (the large one's
// beat carrying no status, as an overrun status read leaves it), the router
// [Engine.newRouter] builds on a seats node places work only the large node
// holds on it for all two hundred repositories, and work every node holds on
// each repository's home.
//
// Mutation: answer newRouter's git placement's listing with no figure — the
// 40 GiB work stays with each home, red.
func TestTheRoutersMaintainerWeighsWhatEachGitletNodeAdvertises(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	backend := coordmem.New()
	gitletRoles := []string{"data", "gitlet", "ingress"}
	claimMeta(t, backend, map[string]map[string]any{
		"git-a": {"roles": gitletRoles, coord.GitBackgroundKey: 8 * gib,
			coord.StatusKey: coord.NodeStatus{}.Meta()},
		"git-b": {"roles": gitletRoles, coord.GitBackgroundKey: 64 * gib},
		"git-c": {"roles": gitletRoles, coord.GitBackgroundKey: 8 * gib,
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
