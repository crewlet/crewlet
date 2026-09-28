package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// fakeMember is a member's embedded broker as the fleet-broker surface asks it:
// a metadata group it reports, and the removals it was asked to commit.
type fakeMember struct {
	mu      sync.Mutex
	group   jetstream.MetaGroup
	groupOK error
	removed []string
	refuse  error
}

func (f *fakeMember) MetaGroup() (jetstream.MetaGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.group, f.groupOK
}

func (f *fakeMember) RemovePeer(_ context.Context, peer string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return f.refuse
	}
	var kept []jetstream.MetaPeer
	removed := ""
	for _, p := range f.group.Peers {
		if p.Peer == peer {
			removed = p.Name
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == len(f.group.Peers) {
		// WHAT THE GROUP ITSELF ANSWERS a voter it does not count.
		return jetstream.ErrNotAMetaPeer
	}
	f.removed = append(f.removed, removed)
	f.group.Peers = kept
	return nil
}

// brokerFleet is a fleet of engines sharing one in-memory broker and one lease
// store, each with only what the fleet-broker surface reads.
type brokerFleet struct {
	broker *memory.Broker
	leases *coordmem.Backend
}

func newBrokerFleet(t *testing.T) *brokerFleet {
	t.Helper()
	return &brokerFleet{broker: memory.NewBroker(), leases: coordmem.New()}
}

// node builds one node's engine: its profile, a live presence lease advertising
// it, and — on a member given one — its embedded broker, serving its subject.
func (f *brokerFleet) node(t *testing.T, profile placement.NodeProfile, member brokerMembership) *Engine {
	t.Helper()
	ctx := t.Context()
	q := f.broker.Client()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	e := &Engine{id: profile.ID, profile: profile,
		backends: &Backends{Queue: q, Coord: f.leases, broker: member}}
	f.present(t, profile)
	if err := e.serveFleetBroker(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.stopServingFleetBroker(context.WithoutCancel(ctx)) })
	return e
}

// present gives a node a live presence lease carrying its profile.
func (f *brokerFleet) present(t *testing.T, profile placement.NodeProfile) {
	t.Helper()
	if _, _, err := f.leases.TryAcquire(t.Context(), coord.NodeResource(profile.ID),
		coord.AcquireOptions{Owner: profile.ID + ":boot-1", TTL: time.Minute,
			Meta: profile.Meta()}); err != nil {
		t.Fatal(err)
	}
}

func member(id string, roles ...placement.NodeRole) placement.NodeProfile {
	return placement.NodeProfile{ID: id, Roles: placement.Roles(roles...), Broker: placement.BrokerMember}
}

func groupOf(names ...string) jetstream.MetaGroup {
	g := jetstream.MetaGroup{Cluster: "acme", Leader: names[0]}
	for _, n := range names {
		g.Peers = append(g.Peers, jetstream.MetaPeer{Name: n, Peer: jetstream.PeerIDOf(n),
			Leader: n == names[0], Current: true})
	}
	return g
}

// THE TWO RECORDS OF THE BROKER'S MEMBERSHIP ARE HELD AGAINST EACH OTHER, and
// every disagreement is named: a voter no live node is (the member that is gone
// for good, which every election goes on counting), a voter whose node came
// back as a leaf, a node advertising a member the group does not count, and a
// presence that does not say. A leaf reads the group through a member.
func TestTheBrokerListingNamesEveryDisagreement(t *testing.T) {
	fleet := newBrokerFleet(t)
	a := &fakeMember{group: groupOf("node-a", "node-b", "node-c", "node-e")}
	fleet.node(t, member("node-a", placement.RoleData), a)
	fleet.present(t, member("node-b", placement.RoleData))
	fleet.present(t, member("node-d", placement.RoleData))
	fleet.present(t, placement.NodeProfile{ID: "node-e",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf})
	fleet.present(t, placement.NodeProfile{ID: "old-1", Roles: placement.Roles(placement.RoleData)})
	leaf := fleet.node(t, placement.NodeProfile{ID: "leaf-1",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf}, nil)

	view, err := leaf.FleetBroker().List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if view.Group == nil || view.GroupFrom != "node-a" {
		t.Fatalf("the leaf read the group from %q (%v): it must ask a live member",
			view.GroupFrom, view.GroupError)
	}
	kinds := map[string]string{}
	for _, n := range view.Nodes {
		kinds[n.Node] = n.Kind
	}
	if kinds["old-1"] != "unknown" || kinds["leaf-1"] != "leaf" || kinds["node-a"] != "member" {
		t.Errorf("advertised kinds %v", kinds)
	}
	var got []string
	for _, f := range view.Findings {
		got = append(got, string(f.Kind)+":"+f.Node)
	}
	want := []string{"dead_member:node-c", "dead_member:node-e", "not_in_group:node-d",
		"unknown_kind:old-1"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("findings %v, want %v", got, want)
	}
}

// A FLEET ON AN EXTERNAL CLUSTER LISTS NO MEMBERSHIP OF ITS OWN, and a removal
// is refused naming whose cluster it is — never an answer about a broker this
// fleet does not run.
func TestAnExternalBrokerIsNotThisFleetsToList(t *testing.T) {
	fleet := newBrokerFleet(t)
	client := fleet.node(t, placement.NodeProfile{ID: "node-a",
		Roles: placement.Roles(placement.RoleData), Broker: placement.BrokerClient}, nil)
	view, err := client.FleetBroker().List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !view.External || view.Group != nil {
		t.Fatalf("an external cluster's fleet answered external=%v group=%v", view.External, view.Group)
	}
	if _, err := client.FleetBroker().Remove(t.Context(), BrokerRemoval{Node: "node-b"}); !errors.Is(err, ErrExternalBroker) {
		t.Fatalf("a removal on an external cluster answered %v", err)
	}
}

// A MEMBER THAT HOLDS A LIVE PRESENCE IS NOT REMOVED WITHOUT FORCE: it is still
// running, and a running member removed from the group rejoins as a voter at
// its next restart. Gone, it is removed through a live member's system account —
// a leaf's gesture carried by a member, never by the member being removed while
// another will do — and the metadata group's own refusal travels back to the
// asker as the sentinel a caller branches on.
func TestAMemberIsRemovedOnlyOnceItIsGoneAndThroughAnotherMember(t *testing.T) {
	fleet := newBrokerFleet(t)
	a := &fakeMember{group: groupOf("node-a", "node-b", "node-c")}
	b := &fakeMember{group: groupOf("node-a", "node-b", "node-c")}
	fleet.node(t, member("node-a", placement.RoleData), a)
	fleet.node(t, member("node-b", placement.RoleData), b)
	leaf := fleet.node(t, placement.NodeProfile{ID: "leaf-1",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf}, nil)

	var live *BrokerMemberLive
	if _, err := leaf.FleetBroker().Remove(t.Context(), BrokerRemoval{Node: "node-b"}); !errors.As(err, &live) {
		t.Fatalf("removing a member with a live presence answered %v", err)
	}
	if len(a.removed)+len(b.removed) != 0 {
		t.Fatal("a refused removal reached a member anyway")
	}

	done, err := leaf.FleetBroker().Remove(t.Context(), BrokerRemoval{Node: "node-c", By: "ops"})
	if err != nil {
		t.Fatalf("removing a member that is gone: %v", err)
	}
	if done.By != "node-a" || !slices.Equal(a.removed, []string{"node-c"}) {
		t.Fatalf("carried by %q (a removed %v): the first live member by id carries it",
			done.By, a.removed)
	}
	if done.Group == nil || len(done.Group.Peers) != 2 {
		t.Errorf("the answer does not carry the group after the commit: %+v", done.Group)
	}

	// FORCED, a running member is removed — by the OTHER member.
	done, err = leaf.FleetBroker().Remove(t.Context(), BrokerRemoval{Node: "node-a", Force: true})
	if err != nil {
		t.Fatalf("a forced removal: %v", err)
	}
	if done.By != "node-b" {
		t.Fatalf("the member being removed carried its own removal while another was live")
	}

	// THE GROUP'S REFUSAL CROSSES THE WIRE AS ITSELF.
	if _, err := leaf.FleetBroker().Remove(t.Context(), BrokerRemoval{Node: "ghost"}); !errors.Is(err, jetstream.ErrNotAMetaPeer) {
		t.Fatalf("a name the group does not list answered %v", err)
	}
}

// A MEMBER THAT CANNOT CARRY IT IS PASSED OVER, and with nobody who can the
// gesture says so rather than reporting a removal nobody made.
func TestARemovalWithNoMemberToCarryItSaysSo(t *testing.T) {
	fleet := newBrokerFleet(t)
	solo := &fakeMember{groupOK: jetstream.ErrNoMetaGroup, refuse: jetstream.ErrNoMetaGroup}
	fleet.node(t, member("node-a", placement.RoleData), solo)
	leaf := fleet.node(t, placement.NodeProfile{ID: "leaf-1",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf}, nil)
	if _, err := leaf.FleetBroker().Remove(t.Context(), BrokerRemoval{Node: "node-c"}); !errors.Is(err, ErrNoBrokerMember) {
		t.Fatalf("a removal no member could carry answered %v", err)
	}
	view, err := leaf.FleetBroker().List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if view.Group != nil || view.GroupError == "" {
		t.Fatalf("a group nobody could read was answered as %+v (%q) — an unread "+
			"group must never read as an empty one", view.Group, view.GroupError)
	}
}

// EVERY FINDING KIND IS ONE THIS BUILD NAMES, and none is valid by accident.
func TestTheBrokerFindingKindsAreClosed(t *testing.T) {
	for _, k := range BrokerFindingKinds() {
		if !k.Valid() {
			t.Errorf("%q is listed and not valid", k)
		}
	}
	if BrokerFindingKind("dead").Valid() || BrokerFindingKind("").Valid() {
		t.Error("a kind this build does not name reads as valid")
	}
}
