package partmap

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
)

// joinedAndServing is a settled fleet of two that a third node has joined: the
// tick that named its first join, then the node acting until its lease says it
// serves that partition — with no tick since, so only a convergence has read
// its word. It answers the fleet and the partition the join was for.
func joinedAndServing(t *testing.T) (*sim, int) {
	t.Helper()
	s := settled(t, smallLayout, 2, "data-00", "data-01")
	s.add("data-02", 1, nil)
	s.tick()
	joining := -1
	for g := range s.state.Map.Partitions {
		if h := holderOf(&s.state.Map.Partitions[g], "data-02"); h != nil && h.State == Joining {
			joining = g
		}
	}
	if joining < 0 || s.joins("data-02") != MaxJoinsPerNode {
		t.Fatalf("the premise: the tick named %d join(s) on the new node, want the ration's %d",
			s.joins("data-02"), MaxJoinsPerNode)
	}
	id := s.state.Map.Partitions[joining].ID
	for range 3 {
		s.act()
	}
	if got := s.nodes["data-02"].meta.Partitions[id]; got != PartServing {
		t.Fatalf("the premise: the joiner's lease says %s of %s, want serving", got, id)
	}
	return s, joining
}

// joins is how many partitions node is joining.
func (s *sim) joins(node string) int {
	n := 0
	for g := range s.state.Map.Partitions {
		if h := holderOf(&s.state.Map.Partitions[g], node); h != nil && h.State == Joining {
			n++
		}
	}
	return n
}

// A CONVERGENCE BETWEEN TICKS ANSWERS A NODE'S WORD, AND NAMES NO JOIN.
//
// A joiner whose lease says it serves is promoted by the convergence the
// maintainer runs when a lease changes, rather than a tick later — and that
// pass names no next join for it, though the target wants more: the ration
// stays one transfer per tick, so the join is the next tick's, which names it.
func TestAConvergenceBetweenTicksAnswersANodesWordAndNamesNoJoin(t *testing.T) {
	t.Parallel()
	s, g := joinedAndServing(t)
	next, changed := Converge(s.state, s.live())
	if !changed {
		t.Fatal("a joiner's lease says it serves and the convergence changed nothing")
	}
	if h := holderOf(&next.Map.Partitions[g], "data-02"); h == nil || h.State != Serving {
		t.Fatalf("the joiner that says it serves is %+v, want serving", h)
	}
	if next.Map.Epoch != s.state.Map.Epoch+1 {
		t.Errorf("the convergence wrote epoch %d over %d, want the next", next.Map.Epoch, s.state.Map.Epoch)
	}
	if h := holderOf(&next.Map.Partitions[g], "data-02"); h.Since != next.Map.Epoch {
		t.Errorf("the promoted holder is stamped %d, want the epoch that promoted it %d", h.Since, next.Map.Epoch)
	}
	s.state = next
	if got := s.joins("data-02"); got != 0 {
		t.Fatalf("the convergence named %d join(s) on the node it promoted: a join is the "+
			"tick's, and the ration one transfer per tick", got)
	}
	// THE CONTROL: the tick after it does name one, so the join above was
	// withheld rather than not wanted.
	s.tick()
	if got := s.joins("data-02"); got != MaxJoinsPerNode {
		t.Errorf("the next tick named %d join(s) on the node, want the ration's %d", got, MaxJoinsPerNode)
	}
}

// A CONVERGENCE COUNTS NO ABSENCE, AND CHANGES NO MEMBER.
//
// Absence is counted in ticks, so a pass run on every lease change must not
// count it at all: a member gone is a member for exactly as many ticks as it
// was before, however many convergences ran — and the copies, the label and
// every share are the tick's as well.
func TestAConvergenceCountsNoAbsenceAndChangesNoMember(t *testing.T) {
	t.Parallel()
	s, _ := joinedAndServing(t)
	s.nodes["data-01"].down = true
	before := s.state.Clone()
	next, _ := Converge(s.state, s.live())
	if !reflect.DeepEqual(next.State, before.State) {
		t.Errorf("a convergence changed the membership state:\n%+v\nwant\n%+v", next.State, before.State)
	}
	if !samePlacement(next.Map, before.Map) || !reflect.DeepEqual(next.Balance, before.Balance) {
		t.Error("a convergence changed what the map places")
	}
	if !next.Map.Draw().Holds("data-01") {
		t.Error("a convergence removed a member that is gone")
	}
}

// NO MAP, NO CONVERGENCE: a first map is the tick's, the one place a map is
// created.
func TestAConvergenceCreatesNoMap(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 2, "data-00", "data-01")
	if next, changed := Converge(s.state, s.live()); changed || next.Map.Generation != uuid.Nil {
		t.Errorf("a convergence with no map wrote one: %+v", next.Map)
	}
}

// THE LEASE KEY MOVES WITH WHAT A CONVERGENCE READS, AND WITH NOTHING ELSE.
//
// The duty converges again when the key moves, so a key that moved on every
// renewal — a lease's free space, its detail, the index it is building —
// would converge on every beat of every node, and one that missed a partition
// state, an epoch, the store's health or the layout would leave a node's word
// unanswered until the tick.
func TestTheLeaseKeyMovesOnlyWithWhatAConvergenceReads(t *testing.T) {
	t.Parallel()
	s, _ := joinedAndServing(t)
	live := s.live()
	key := LeaseKey(live)
	edit := func(change func(*Meta)) string {
		copied := slices.Clone(live)
		m := copied[0].Meta
		m.Partitions = maps.Clone(m.Partitions)
		change(&m)
		copied[0].Meta = m
		return LeaseKey(copied)
	}
	for name, change := range map[string]func(*Meta){
		"free space": func(m *Meta) { m.FreeBytes += 1 << 30 },
		"detail":     func(m *Meta) { m.Detail = "renewed" },
		"building":   func(m *Meta) { m.Building = []string{"tracker.000"} },
		"labels":     func(m *Meta) { m.Labels = map[string]string{"zone": "b"} },
	} {
		if edit(change) != key {
			t.Errorf("the key moved with the lease's %s, which no convergence reads", name)
		}
	}
	for name, change := range map[string]func(*Meta){
		"a partition's state": func(m *Meta) {
			for p := range m.Partitions {
				m.Partitions[p] = PartDraining
				break
			}
		},
		"the map epoch acted on": func(m *Meta) { m.MapEpoch++ },
		"the map acted on":       func(m *Meta) { m.MapGeneration = uuid.New() },
		"the store's health":     func(m *Meta) { m.Healthy = no() },
		"the layout":             func(m *Meta) { m.Layout = layoutNo(7) },
	} {
		if edit(change) == key {
			t.Errorf("the key did not move with %s, which a convergence reads", name)
		}
	}
	// NOT SAYING IS NOT HOLDING NOTHING: a lease listing no partition says
	// it has gone from every one, and a lease that does not say says nothing.
	empty := edit(func(m *Meta) { m.Partitions = map[string]PartitionState{} })
	if unsaid := edit(func(m *Meta) { m.Partitions = nil }); unsaid == empty {
		t.Error("the key reads a lease that says nothing of what it holds as one holding nothing")
	}
	if LeaseKey(live[1:]) == key {
		t.Error("the key did not move when a lease lapsed")
	}
}

// THE MAINTAINER'S CONVERGENCE WRITES THE STORED MAP BY COMPARE-AND-SET, ON A
// TICK'S TERMS.
//
// It writes what [Converge] answers when the map is read and the leases
// listed; it writes nothing again once the map says it; and a listing it could
// not take, a race it lost and a fleet with no map each leave the store as it
// was.
func TestTheMaintainerConvergesTheStoredMapOnATicksTerms(t *testing.T) {
	t.Parallel()
	s, g := joinedAndServing(t)
	store := coordmemory.NewFleet()
	raw, err := s.state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.CreateEstateMap(t.Context(), raw); err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	listed := func() ([]Presence, error) { return s.live(), nil }
	unchanged := func(what string) {
		t.Helper()
		if rec, _, _ := store.EstateMap(t.Context()); string(rec.Value) != string(raw) {
			t.Fatalf("%s changed the stored map", what)
		}
	}

	unread := maintainerOver(t, store, smallLayout,
		func() ([]Presence, error) { return nil, errors.New("coordination timed out") },
		company(2, "", 1), noProvision)
	if wrote, err := unread.Converge(t.Context()); err == nil || wrote {
		t.Errorf("a convergence whose lease listing failed answered (%v, %v)", wrote, err)
	}
	unchanged("a convergence that could not list the leases")

	lost := &losingStore{MapStore: store}
	racing := maintainerOver(t, lost, smallLayout, listed, company(2, "", 1), noProvision)
	if wrote, err := racing.Converge(t.Context()); err != nil || wrote || lost.lost != 1 {
		t.Errorf("a lost race answered (%v, %v) after %d write(s)", wrote, err, lost.lost)
	}
	unchanged("a convergence that lost its race")

	m := maintainerOver(t, store, smallLayout, listed, company(2, "", 1), noProvision)
	if wrote, err := m.Converge(t.Context()); err != nil || !wrote {
		t.Fatalf("the convergence answered (%v, %v), want it written", wrote, err)
	}
	rec, _, err := store.EstateMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := DecodeMapState(rec.Value)
	if err != nil {
		t.Fatal(err)
	}
	if h := holderOf(&stored.Map.Partitions[g], "data-02"); h == nil || h.State != Serving {
		t.Errorf("the stored map holds the joiner as %+v, want serving", h)
	}
	if wrote, err := m.Converge(t.Context()); err != nil || wrote {
		t.Errorf("a second convergence with nothing changed answered (%v, %v)", wrote, err)
	}

	none := maintainerOver(t, coordmemory.NewFleet(), smallLayout, listed, company(2, "", 1), noProvision)
	if wrote, err := none.Converge(t.Context()); err != nil || wrote {
		t.Errorf("a convergence over no map answered (%v, %v), want nothing", wrote, err)
	}
}
