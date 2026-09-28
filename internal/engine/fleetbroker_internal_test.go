package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
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

// wedgedMember is a member whose process takes a request and never answers
// it — the one an asker cannot tell from a member that is about to: its
// requests are delivered, and nothing comes back until the case ends.
type wedgedMember struct{ release chan struct{} }

func newWedgedMember(t *testing.T) *wedgedMember {
	return &wedgedMember{release: make(chan struct{})}
}

func (w *wedgedMember) MetaGroup() (jetstream.MetaGroup, error) {
	<-w.release
	return jetstream.MetaGroup{}, jetstream.ErrNoMetaGroup
}

func (w *wedgedMember) RemovePeer(context.Context, string) error {
	<-w.release
	return jetstream.ErrNoMetaGroup
}

// wedged builds a member that is wedged, releasing it before its subject is
// withdrawn — a withdrawal waits for the answers in flight.
func (f *brokerFleet) wedged(t *testing.T, id string) {
	t.Helper()
	w := newWedgedMember(t)
	f.node(t, member(id, placement.RoleData), w)
	t.Cleanup(func() { close(w.release) })
}

// brokerFleet is a fleet of engines sharing one in-memory broker and one lease
// store, each with only what the fleet-broker surface reads.
type brokerFleet struct {
	broker  *memory.Broker
	leases  *coordmem.Backend
	engines map[string]*Engine
}

func newBrokerFleet(t *testing.T) *brokerFleet {
	t.Helper()
	return &brokerFleet{broker: memory.NewBroker(), leases: coordmem.New(),
		engines: map[string]*Engine{}}
}

// engine is a node [brokerFleet.node] built.
func (f *brokerFleet) engine(id string) *Engine { return f.engines[id] }

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
	f.engines[profile.ID] = e
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

// nameless is a voter as a member reports one it has not heard from since it
// started: counted, by its peer id, with no name.
func nameless(g jetstream.MetaGroup, node string) jetstream.MetaGroup {
	for i, p := range g.Peers {
		if p.Name == node {
			g.Peers[i].Name = ""
		}
	}
	return g
}

// THE TWO RECORDS OF THE BROKER'S MEMBERSHIP ARE HELD AGAINST EACH OTHER, and
// every disagreement is named: a voter no live node is (the member that is gone
// for good, which every election goes on counting), a voter whose node came
// back as a leaf, a node advertising a member the group does not count, and a
// presence that does not say. A leaf reads the group through a member.
//
// BY PEER ID: a voter the answering member cannot name is still the live node
// whose id hashes to it — a leaf now, so a dead member by its node id — and a
// voter nobody can name and no live node is, is a dead member by its peer id
// alone. A live member counted under a name the answer does not carry is
// counted, not "not in the group".
func TestTheBrokerListingNamesEveryDisagreement(t *testing.T) {
	fleet := newBrokerFleet(t)
	group := groupOf("node-a", "node-b", "node-c", "node-e", "node-f", "node-g")
	group = nameless(nameless(nameless(group, "node-e"), "node-f"), "node-b")
	a := &fakeMember{group: group}
	fleet.node(t, member("node-a", placement.RoleData), a)
	fleet.present(t, member("node-b", placement.RoleData))
	fleet.present(t, member("node-d", placement.RoleData))
	fleet.present(t, placement.NodeProfile{ID: "node-e",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf})
	fleet.present(t, placement.NodeProfile{ID: "node-g",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerClient})
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
		got = append(got, string(f.Kind)+":"+f.Node+"@"+f.Peer)
	}
	want := []string{
		"dead_member:@" + jetstream.PeerIDOf("node-f"),
		"dead_member:node-c@" + jetstream.PeerIDOf("node-c"),
		"dead_member:node-e@" + jetstream.PeerIDOf("node-e"),
		"dead_member:node-g@" + jetstream.PeerIDOf("node-g"),
		"not_in_group:node-d@",
		"unknown_kind:old-1@",
	}
	slices.Sort(got)
	slices.Sort(want)
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
	fleet.present(t, placement.NodeProfile{ID: "old-1", Roles: placement.Roles(placement.RoleData)})
	leaf := fleet.node(t, placement.NodeProfile{ID: "leaf-1",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf}, nil)

	var live *BrokerMemberLive
	for _, req := range []BrokerRemoval{
		{Node: "node-b"}, {Peer: jetstream.PeerIDOf("node-b")},
		// A NODE THAT DOES NOT SAY WHAT ITS BROKER IS may be a member,
		// so it is refused as one.
		{Node: "old-1"},
	} {
		if _, err := leaf.FleetBroker().Remove(t.Context(), req); !errors.As(err, &live) {
			t.Fatalf("removing %+v, whose node holds a live presence, answered %v", req, err)
		}
	}
	if len(a.removed)+len(b.removed) != 0 {
		t.Fatal("a refused removal reached a member anyway")
	}

	done, err := leaf.FleetBroker().Remove(t.Context(), BrokerRemoval{Node: "node-c", By: "ops"})
	if err != nil {
		t.Fatalf("removing a member that is gone: %v", err)
	}
	if done.By != "node-a" || !slices.Equal(a.removed, []string{"node-c"}) ||
		done.Node != "node-c" || done.Peer != jetstream.PeerIDOf("node-c") {
		t.Fatalf("carried by %q (a removed %v), answered %+v: the first live member "+
			"by id carries it", done.By, a.removed, done)
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

// A VOTER NOBODY CAN NAME IS REMOVED BY ITS PEER ID — the one the listing
// shows it by — and by its NODE ID too, which hashes to the same peer: what the
// group counts is the id, and a name nobody has heard is no obstacle to
// computing it.
func TestAVoterNobodyCanNameIsRemovedByItsPeerID(t *testing.T) {
	fleet := newBrokerFleet(t)
	a := &fakeMember{group: nameless(nameless(
		groupOf("node-a", "node-b", "node-c", "node-d"), "node-c"), "node-d")}
	fleet.node(t, member("node-a", placement.RoleData), a)
	leaf := fleet.node(t, placement.NodeProfile{ID: "leaf-1",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf}, nil)

	done, err := leaf.FleetBroker().Remove(t.Context(),
		BrokerRemoval{Peer: jetstream.PeerIDOf("node-c")})
	if err != nil {
		t.Fatalf("remove a nameless voter by its peer id: %v", err)
	}
	if done.Peer != jetstream.PeerIDOf("node-c") || done.Node != "" {
		t.Errorf("answered %+v, want the peer id and no node id nobody knows", done)
	}
	if _, err := leaf.FleetBroker().Remove(t.Context(), BrokerRemoval{Node: "node-d"}); err != nil {
		t.Fatalf("remove a nameless voter by the node id it was: %v", err)
	}
	if a.group.Counts(jetstream.PeerIDOf("node-c")) || a.group.Counts(jetstream.PeerIDOf("node-d")) {
		t.Fatalf("the group still counts a removed voter: %+v", a.group.Peers)
	}
	for _, req := range []BrokerRemoval{
		{}, {Node: "node-c", Peer: jetstream.PeerIDOf("node-c")},
		{Peer: "node-c"}, {Node: "Not A Node"},
	} {
		if _, err := leaf.FleetBroker().Remove(t.Context(), req); err == nil {
			t.Errorf("a removal naming %+v was accepted", req)
		}
	}
}

// A NODE ALIVE AS A LEAF OR A CLIENT UNDER A VOTER'S NAME IS REMOVED WITHOUT
// FORCE. The member it was is gone for good — its broker runs no JetStream now
// and never rejoins the group — so the live-lease refusal, which exists
// because a running MEMBER rejoins at its next restart, has nothing to protect,
// and the command the listing prints for it must work as printed.
func TestANodeAliveAsALeafUnderAVotersNameIsRemovedWithoutForce(t *testing.T) {
	fleet := newBrokerFleet(t)
	a := &fakeMember{group: groupOf("node-a", "node-b", "node-e", "node-g")}
	fleet.node(t, member("node-a", placement.RoleData), a)
	fleet.present(t, member("node-b", placement.RoleData))
	fleet.present(t, placement.NodeProfile{ID: "node-e",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf})
	fleet.present(t, placement.NodeProfile{ID: "node-g",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerClient})
	for _, node := range []string{"node-e", "node-g"} {
		done, err := fleet.engine("node-a").FleetBroker().Remove(t.Context(),
			BrokerRemoval{Node: node})
		if err != nil {
			t.Fatalf("remove %s, alive but no longer a member: %v", node, err)
		}
		if done.Node != node {
			t.Errorf("answered %+v", done)
		}
	}
	if !slices.Equal(a.removed, []string{"node-e", "node-g"}) {
		t.Fatalf("removed %v", a.removed)
	}
}

// A MEMBER THAT CANNOT CARRY A REMOVAL HERE MOVES IT ON: this node is a member
// whose broker has no metadata group (its JetStream switched off), which says
// nothing about the group, so the next member carries it rather than the
// operator being told the removal failed.
func TestAMemberThatCannotCarryItHereAsksTheNext(t *testing.T) {
	fleet := newBrokerFleet(t)
	here := &fakeMember{groupOK: jetstream.ErrNoMetaGroup, refuse: jetstream.ErrNoMetaGroup}
	b := &fakeMember{group: groupOf("node-a", "node-b", "node-c")}
	local := fleet.node(t, member("node-a", placement.RoleData), here)
	fleet.node(t, member("node-b", placement.RoleData), b)
	done, err := local.FleetBroker().Remove(t.Context(), BrokerRemoval{Node: "node-c"})
	if err != nil {
		t.Fatalf("a removal this member could not carry answered %v", err)
	}
	if done.By != "node-b" || !slices.Equal(b.removed, []string{"node-c"}) {
		t.Fatalf("carried by %q, b removed %v", done.By, b.removed)
	}
	view, err := local.FleetBroker().List(t.Context())
	if err != nil || view.GroupFrom != "node-b" {
		t.Fatalf("the listing read the group from %q (%v, %v)", view.GroupFrom,
			view.GroupError, err)
	}
}

// THE ONLY LIVE MEMBER IS NEVER ASKED TO REMOVE ITSELF, forced or not: it could
// not see the group drop it, and it loses JetStream the moment the change
// commits — so a removal it carried could only ever be reported as not made.
// The gesture says who would have to carry it instead.
func TestTheOnlyLiveMemberIsNeverAskedToRemoveItself(t *testing.T) {
	fleet := newBrokerFleet(t)
	a := &fakeMember{group: groupOf("node-a", "node-b", "node-c")}
	fleet.node(t, member("node-a", placement.RoleData), a)
	leaf := fleet.node(t, placement.NodeProfile{ID: "leaf-1",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf}, nil)
	_, err := leaf.FleetBroker().Remove(t.Context(), BrokerRemoval{Node: "node-a", Force: true})
	if !errors.Is(err, ErrNoBrokerMember) || !strings.Contains(err.Error(), "the only live member is node-a") {
		t.Fatalf("removing the only live member answered %v", err)
	}
	if len(a.removed) != 0 {
		t.Fatalf("the member carried its own removal: %v", a.removed)
	}
}

// A CARRIER THAT DOES NOT ANSWER ENDS THE REMOVAL, reported as an outcome
// nobody knows: it may have proposed the change before it went silent, and the
// group commits a proposal whoever is listening. The next member is not asked,
// and the gesture is over inside ONE wait — never one wait per member — which
// is what the command line's and the dashboard's own waits are sized on.
func TestASilentCarrierEndsTheRemovalAsUnknown(t *testing.T) {
	fleet := newBrokerFleet(t)
	b := &fakeMember{group: groupOf("node-a", "node-b", "node-c")}
	// node-a TAKES THE REQUEST AND NEVER ANSWERS, first in the order
	// members are asked in: a process wedged inside its lease.
	fleet.wedged(t, "node-a")
	fleet.node(t, member("node-b", placement.RoleData), b)
	leaf := fleet.node(t, placement.NodeProfile{ID: "leaf-1",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf}, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := leaf.FleetBroker().Remove(ctx, BrokerRemoval{Node: "node-c"})
	if !errors.Is(err, ErrRemovalOutcomeUnknown) {
		t.Fatalf("a removal whose carrier went silent answered %v", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the gesture took %s against a 300ms deadline", took)
	}
	if len(b.removed) != 0 {
		t.Fatalf("a second member proposed the removal after the first went silent: %v", b.removed)
	}
}

// A LISTING IS NOT HELD UP BY A SILENT MEMBER. Every member is asked at once
// and the first report of the group answers, so a member that died inside its
// lease costs nothing while another answers — asked one after another, it
// would spend the whole wait of the command line and the dashboard, which is
// ten seconds each.
func TestAListingIsNotHeldUpByASilentMember(t *testing.T) {
	fleet := newBrokerFleet(t)
	fleet.wedged(t, "node-a")
	fleet.node(t, member("node-b", placement.RoleData),
		&fakeMember{group: groupOf("node-a", "node-b")})
	leaf := fleet.node(t, placement.NodeProfile{ID: "leaf-1",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf}, nil)
	began := time.Now()
	view, err := leaf.FleetBroker().List(t.Context())
	if err != nil || view.GroupFrom != "node-b" {
		t.Fatalf("the listing read the group from %q (%q, %v)", view.GroupFrom,
			view.GroupError, err)
	}
	if took := time.Since(began); took >= brokerReadWait {
		t.Fatalf("the listing took %s: it waited on the silent member", took)
	}
}

// A LISTING NOBODY COULD ANSWER SAYS WHO WAS ASKED, within one wait.
func TestAnUnreadGroupSaysWhyWithinOneWait(t *testing.T) {
	fleet := newBrokerFleet(t)
	fleet.wedged(t, "node-a")
	fleet.present(t, member("node-b", placement.RoleData))
	leaf := fleet.node(t, placement.NodeProfile{ID: "leaf-1",
		Roles: placement.Roles(placement.RoleSeats), Broker: placement.BrokerLeaf}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	view, err := leaf.FleetBroker().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Group != nil || !strings.Contains(view.GroupError, "node-a did not answer") ||
		!strings.Contains(view.GroupError, "node-b did not answer") {
		t.Fatalf("an unread group was answered as %+v (%q)", view.Group, view.GroupError)
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
