package engine

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY DECLARED TABLE IS READ BY THE PASSES THIS ENGINE STARTS.
//
// The declarations are one list (internal/objstore/references) and the
// estates the passes read them through are this engine's. A consumer
// declaring a table in a domain the engine hands no estate for would have the
// passes refuse to start at boot, logged and nothing more, and every data
// node would stop repairing and collecting — so the mismatch fails here, at
// build time, instead.
func TestThePassesReadEveryDeclaredTable(t *testing.T) {
	t.Parallel()
	n := &native{trackerReader: &tracker.Reader{}}
	if _, err := upkeep.Sources(references.All, n.objectEstates()...); err != nil {
		t.Fatalf("the passes cannot be built from the declared tables: %v", err)
	}
}

// A runtime without a tracker has no files, so no estate the passes read — and
// the engine starts no passes rather than passes over nothing.
func TestARuntimeWithNoTrackerOffersTheObjectStoreNoEstate(t *testing.T) {
	t.Parallel()
	if got := (&native{}).objectEstates(); len(got) != 0 {
		t.Fatalf("a runtime with no tracker offered %d estates", len(got))
	}
}

// holdersFixture claims the given leases on a fresh in-memory backend.
func holdersFixture(t *testing.T, leases map[string]map[string]any) *coordmemory.Backend {
	t.Helper()
	b := coordmemory.New()
	for resource, meta := range leases {
		if _, err := b.TryAcquire(t.Context(), resource, coord.AcquireOptions{
			Owner: resource + ":1", TTL: time.Minute, Ungated: true, Meta: meta,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// THE MAP IS MAINTAINED FROM THE OBJECTS LEASES AND NOTHING ELSE.
//
// Membership used to be read off node PRESENCE, which a shutdown drain gives up
// at its first step while the node still serves every chunk — so a drain longer
// than the grace moved the node's whole share away and back. A presence lease
// is therefore no member at all; an objects lease is, weighed and labelled as
// it says, and counted unhealthy exactly when its store reports itself failed.
func TestTheMapIsMaintainedFromTheObjectsLeasesAlone(t *testing.T) {
	t.Parallel()
	done := objstore.ObjectsRepair{Epoch: 5, Completed: true, Pending: 0, At: time.Now()}
	partial := objstore.ObjectsRepair{Epoch: 5, Completed: false, Pending: 0, At: time.Now()}
	backend := holdersFixture(t, map[string]map[string]any{
		// Presence, of a node that also holds objects and of one that
		// holds none: neither is a member on that account.
		coord.NodeResource("a"):     {"roles": []any{"data"}},
		coord.NodeResource("ghost"): {"roles": []any{"data"}},
		coord.ObjectsResource("a"): objstore.ObjectsMeta{
			Weight: 2, Labels: map[string]string{"zone": "z1"},
			Health: &objstore.ObjectsHealth{State: string(disk.HealthOK)},
			Repair: &done,
		}.Encode(),
		coord.ObjectsResource("failed"): objstore.ObjectsMeta{
			Weight: 1,
			Health: &objstore.ObjectsHealth{State: string(disk.HealthFailed), Detail: "probe: EIO"},
		}.Encode(),
		coord.ObjectsResource("full"): objstore.ObjectsMeta{
			Weight: 1, Health: &objstore.ObjectsHealth{State: string(disk.HealthFull)},
			Repair: &partial,
		}.Encode(),
		// No readable weight: offers nothing.
		coord.ObjectsResource("weightless"): {"health": map[string]any{"state": "ok"}},
	})

	got, err := objectHolders(t.Context(), backend)
	if err != nil {
		t.Fatal(err)
	}
	byNode := map[string]upkeep.Presence{}
	for _, p := range got {
		byNode[p.Node] = p
	}
	if len(byNode) != 3 {
		t.Fatalf("members = %v, want a, failed and full — presence and a weightless "+
			"lease are not members", slices.Sorted(maps.Keys(byNode)))
	}
	a := byNode["a"]
	if a.Weight != 2 || a.Labels["zone"] != "z1" || a.Unhealthy {
		t.Errorf("a = %+v, want weight 2 in z1 and healthy", a)
	}
	if a.Repair != (upkeep.RepairReport{Epoch: 5, Pending: 0}) {
		t.Errorf("a completed repair at epoch 5 reads as %+v", a.Repair)
	}
	if f := byNode["failed"]; !f.Unhealthy || f.Detail != "probe: EIO" {
		t.Errorf("a failed store reads as %+v, want unhealthy with its detail", f)
	}
	// A FULL STORE STILL SERVES what it holds; only a failed one is counted
	// absent. And a pass that did not reach every group claims nothing.
	if f := byNode["full"]; f.Unhealthy || f.Repair != (upkeep.RepairReport{}) {
		t.Errorf("a full store with an incomplete pass reads as %+v", f)
	}
}

// THE MAP TAKES NOTHING FROM A COMPANY NO ACTIVATION NAMED, and from one that
// was activated it takes the replica count — the default when it names none —
// and the failure domain, stamped with the activation's instant so a duty
// holder a revision behind cannot set the map back.
func TestTheMapTakesTheCompanyStampedWithItsActivation(t *testing.T) {
	t.Parallel()
	cfg := parseCompany(t, companyWithoutSandboxDoc)
	e := &Engine{}
	e.epoch.current.Store(&Company{Config: cfg})
	if _, ok := e.objectsCompany(); ok {
		t.Fatal("a company no activation named was applied to the map")
	}

	activated := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	e.epoch.current.Store(&Company{Config: cfg, ActivatedAt: activated})
	got, ok := e.objectsCompany()
	if !ok {
		t.Fatal("an activated company was not applied")
	}
	want := membership.Company{
		Epoch:    uint64(configplane.ActivationStamp(activated)),
		Replicas: config.DefaultObjectReplicas,
		// Named, so a refusal of it names the field to change.
		Block: "objects",
	}
	if got != want {
		t.Errorf("company = %+v, want %+v", got, want)
	}

	zoned := *cfg
	zoned.Objects = config.Objects{Replicas: 5, FailureDomain: "zone"}
	e.epoch.current.Store(&Company{Config: &zoned, ActivatedAt: activated.Add(time.Second)})
	got, _ = e.objectsCompany()
	if got.Replicas != 5 || got.FailureDomain != "zone" || got.Epoch <= want.Epoch {
		t.Errorf("a later revision naming 5 copies across zones reads as %+v", got)
	}
}

// A FAILED PASS IS RETRIED SOON, THEN LESS OFTEN, NEVER LESS OFTEN THAN THE
// PASS RUNS ANYWAY: thirty seconds, doubling, capped at the interval of the
// pass being retried — ten minutes for a repair, an hour for a collection.
func TestAFailedPassBacksOff(t *testing.T) {
	t.Parallel()
	for ceiling, want := range map[time.Duration][]time.Duration{
		upkeep.RepairInterval: {30 * time.Second, time.Minute, 2 * time.Minute,
			4 * time.Minute, 8 * time.Minute, upkeep.RepairInterval, upkeep.RepairInterval},
		upkeep.CollectInterval: {30 * time.Second, time.Minute, 2 * time.Minute,
			4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute,
			upkeep.CollectInterval, upkeep.CollectInterval},
	} {
		var got []time.Duration
		var last time.Duration
		for range want {
			last = nextPassRetry(last, ceiling)
			got = append(got, last)
		}
		if !slices.Equal(got, want) {
			t.Errorf("retries capped at %v = %v, want %v", ceiling, got, want)
		}
	}
}

// storedAbsence is how many ticks the stored map has counted node absent.
func storedAbsence(t *testing.T, store *coordmemory.Fleet, node string) int {
	t.Helper()
	rec, found, err := store.ObjectMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return 0
	}
	state, err := objstore.DecodeMapState(rec.Value)
	if err != nil {
		t.Fatal(err)
	}
	return state.Absence[node].Ticks
}

// THE MAP DUTY COUNTS AN ABSENCE ONLY A WHOLE TICK APART, so a member gone is
// removed a whole grace after the last tick that saw it — because the duty
// paces on what its TICK found in the store and on nothing else. It was keyed
// on this node's cache of the map, and a cache that lagged or refused the map
// the tick had just read had the duty tick every second against it, removing
// a member forty seconds after it went quiet.
func TestTheMapDutyCountsAnAbsenceOnlyATickApart(t *testing.T) {
	t.Parallel()
	store := coordmemory.NewFleet()
	var live []upkeep.Presence
	m, err := upkeep.NewMaintainer(upkeep.MaintainerOptions{Store: store,
		Live:    func(context.Context) ([]upkeep.Presence, error) { return live, nil },
		Company: func() (membership.Company, bool) { return membership.Company{Epoch: 1, Replicas: 2}, true },
		// NO OBSERVER, and no cache anywhere: the duty is handed its
		// tick and nothing else to pace by.
	})
	if err != nil {
		t.Fatal(err)
	}
	duty := mapDuty{tick: m.Tick}

	// NO MAP YET: the poll, so a fresh fleet can store files within a
	// second of its first data node — the tick that writes the map
	// included, since it found none either.
	if wait := duty.turn(t.Context()); wait != objectMapUnplacedPoll {
		t.Fatalf("a tick with no data node waits %v, want the poll", wait)
	}
	live = []upkeep.Presence{{Node: "data-a", Weight: 1}, {Node: "data-b", Weight: 1}}
	if wait := duty.turn(t.Context()); wait != objectMapUnplacedPoll {
		t.Fatalf("the tick that wrote the first map waits %v, want the poll", wait)
	}

	// A VIRTUAL CLOCK, advanced by exactly what each turn answered. The
	// tick at zero is the last to see data-b.
	var clock time.Duration
	wait := duty.turn(t.Context())
	if wait != objectMapInterval {
		t.Fatalf("a tick over a map waits %v, want %v", wait, objectMapInterval)
	}
	const lastSeen = time.Duration(0)
	clock += wait
	live = live[:1]
	var counted []time.Duration
	removed := time.Duration(-1)
	for range 3 * membership.OutTicks {
		before := storedAbsence(t, store, "data-b")
		wait := duty.turn(t.Context())
		after := storedAbsence(t, store, "data-b")
		switch {
		case after > before:
			if n := len(counted); n > 0 && clock-counted[n-1] < membership.TickInterval {
				t.Fatalf("absence counted %v after the last count, want at least %v",
					clock-counted[n-1], membership.TickInterval)
			}
			counted = append(counted, clock)
		case after < before:
			removed = clock
		}
		if removed >= 0 {
			break
		}
		clock += wait
	}
	if removed < 0 {
		t.Fatalf("data-b was never removed; counted %d ticks", len(counted))
	}
	if gone := removed - lastSeen; gone != membership.OutGrace {
		t.Fatalf("data-b was removed %v after the last tick that saw it, want the grace, %v",
			gone, membership.OutGrace)
	}
}

// THE DUTY'S PACE IS WHAT ITS OWN TICK FOUND: the tick interval after a tick
// that found a map, whatever else happened to it; the poll only after one that
// read the store cleanly and found none; and the interval after a turn that
// did not hold the duty or a tick that failed before it could say — neither
// counted anything, and neither is a reason to ask the store every second.
func TestTheMapDutyPacesOnWhatItsTickFound(t *testing.T) {
	t.Parallel()
	failed := errors.New("the store did not answer")
	for name, tc := range map[string]struct {
		claim  func(context.Context) (bool, error)
		result upkeep.TickResult
		err    error
		ticks  bool
		want   time.Duration
	}{
		"found a map":                      {result: upkeep.TickResult{Mapped: true}, ticks: true, want: objectMapInterval},
		"found a map, then failed":         {result: upkeep.TickResult{Mapped: true}, err: failed, ticks: true, want: objectMapInterval},
		"found none":                       {ticks: true, want: objectMapUnplacedPoll},
		"failed before it read the store":  {err: failed, ticks: true, want: objectMapInterval},
		"held the duty and found a map":    {claim: dutyAnswers(true, nil), result: upkeep.TickResult{Mapped: true}, ticks: true, want: objectMapInterval},
		"held the duty and found none":     {claim: dutyAnswers(true, nil), ticks: true, want: objectMapUnplacedPoll},
		"did not hold the duty":            {claim: dutyAnswers(false, nil), want: objectMapInterval},
		"could not ask whether it held it": {claim: dutyAnswers(false, failed), want: objectMapInterval},
	} {
		ticked := false
		duty := mapDuty{claim: tc.claim, tick: func(context.Context) (upkeep.TickResult, error) {
			ticked = true
			return tc.result, tc.err
		}}
		if got := duty.turn(t.Context()); got != tc.want || ticked != tc.ticks {
			t.Errorf("%s: waited %v having ticked %v, want %v having ticked %v",
				name, got, ticked, tc.want, tc.ticks)
		}
	}
}

// dutyAnswers is a duty claim that answers mine and err.
func dutyAnswers(mine bool, err error) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return mine, err }
}

// A LOOP WAITS EXACTLY WHAT ITS RUN ANSWERED, every time — the link between a
// duty's pace and when it next runs.
func TestALoopWaitsWhatItsRunAnswered(t *testing.T) {
	t.Parallel()
	answers := []time.Duration{time.Second, 15 * time.Second, 3 * time.Second, time.Second}
	next := 0 // touched only by the loop's goroutine
	waits := make(chan time.Duration)
	l := startLoop(t.Context(), func(ctx context.Context, d time.Duration) {
		select {
		case waits <- d:
		case <-ctx.Done():
		}
	}, func(context.Context) time.Duration {
		d := answers[next%len(answers)]
		next++
		return d
	})
	for i, want := range answers {
		if got := <-waits; got != want {
			t.Fatalf("wait %d = %v, want %v", i, got, want)
		}
	}
	l.stop()
}

// fakePasses is a data node's passes as the schedule drives them: each pass
// runs at the epoch set, completes unless told to fail, and a completed
// collection keeps the strays set.
type fakePasses struct {
	epoch                 uint64
	repairErr, collectErr error
	strays                int
	status                upkeep.Status
	ran                   []string
}

func (f *fakePasses) Repair(context.Context) error {
	f.ran = append(f.ran, "repair")
	st := upkeep.RepairStatus{Epoch: f.epoch, Completed: f.repairErr == nil, At: time.Now()}
	f.status.Repair = st
	if st.Completed {
		f.status.Repaired = st
	}
	return f.repairErr
}

func (f *fakePasses) Collect(context.Context) error {
	f.ran = append(f.ran, "collect")
	st := upkeep.CollectStatus{Epoch: f.epoch, Completed: f.collectErr == nil, At: time.Now()}
	if st.Completed {
		st.Strays = f.strays
		f.status.Collected = st
	}
	f.status.Collect = st
	return f.collectErr
}

func (f *fakePasses) Status() upkeep.Status { return f.status }

// scheduled is a pass schedule over fake passes on a virtual clock, whose
// fleet reports itself settled at an epoch exactly when settledAt says so.
type scheduled struct {
	*passSchedule
	passes    *fakePasses
	clock     time.Time
	settledAt map[uint64]bool
	asked     int

	// members is the map's members; this node, data-a, is one.
	members []placement.Member
}

func newScheduled() *scheduled {
	s := &scheduled{passes: &fakePasses{}, clock: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
		settledAt: map[uint64]bool{},
		members:   []placement.Member{{Node: "data-a", Weight: 1}, {Node: "data-b", Weight: 1}}}
	s.passSchedule = &passSchedule{self: "data-a", node: s.passes,
		now: func() time.Time { return s.clock },
		settled: func(_ context.Context, m objplacement.Map) (bool, error) {
			s.asked++
			return s.settledAt[m.Epoch], nil
		}}
	return s
}

// at polls at epoch after advancing the clock by d, answering the passes the
// poll ran.
func (s *scheduled) at(t *testing.T, d time.Duration, epoch uint64) []string {
	t.Helper()
	s.clock = s.clock.Add(d)
	s.passes.epoch = epoch
	before := len(s.passes.ran)
	s.poll(t.Context(), objplacement.Map{Epoch: epoch, Members: s.members}, true)
	return s.passes.ran[before:]
}

// A MAP CHANGE IS COLLECTED ONCE THE FLEET HAS SETTLED AT IT, within a poll of
// the fleet settling rather than on the hour: that is the moment every copy the
// epoch moved away is held by every member it now belongs to, and a member
// taken out holds its whole share of them — its decommission waits on this
// pass. Asked only once this node's own repair has completed at the epoch, run
// once per epoch, and never beside a repair: a repair that is due goes first.
func TestAMapChangeIsCollectedOnceTheFleetSettles(t *testing.T) {
	t.Parallel()
	s := newScheduled()
	poll := objectEpochPoll
	if ran := s.at(t, 0, 5); !slices.Equal(ran, []string{"repair"}) {
		t.Fatalf("the first poll ran %v, want a repair", ran)
	}
	if ran := s.at(t, poll, 5); !slices.Equal(ran, []string{"collect"}) {
		t.Fatalf("the next ran %v, want the collection a node that never collected owes", ran)
	}
	s.asked = 0

	// THE MAP MOVES while this node's repair keeps failing: the fleet is
	// never asked, since this node is part of what settled means.
	s.passes.repairErr = errors.New("a member did not answer")
	if ran := s.at(t, poll, 6); !slices.Equal(ran, []string{"repair"}) {
		t.Fatalf("a new epoch ran %v, want a repair", ran)
	}
	s.settledAt[6] = true
	if ran := s.at(t, poll, 6); len(ran) != 0 || s.asked != 0 {
		t.Fatalf("with this node's repair incomplete a poll ran %v and asked the fleet %d times",
			ran, s.asked)
	}

	// ITS RETRY COMPLETES, and on the same poll the fleet is already
	// settled: the repair goes first, the collection on the next poll.
	s.passes.repairErr = nil
	s.settledAt[6] = false
	if ran := s.at(t, objectPassRetryFloor, 6); !slices.Equal(ran, []string{"repair"}) {
		t.Fatalf("the retry poll ran %v, want the repair alone", ran)
	}
	if ran := s.at(t, poll, 6); len(ran) != 0 || s.asked != 1 {
		t.Fatalf("an unsettled fleet: ran %v having asked %d times, want nothing after one ask",
			ran, s.asked)
	}
	s.settledAt[6] = true
	if ran := s.at(t, poll, 6); !slices.Equal(ran, []string{"collect"}) {
		t.Fatalf("the poll after the fleet settled ran %v, want the collection", ran)
	}
	for range 5 {
		if ran := s.at(t, poll, 6); len(ran) != 0 {
			t.Fatalf("a settled epoch already collected ran %v again", ran)
		}
	}

	// WITH NOBODY TO ASK, the hour is all there is.
	alone := newScheduled()
	alone.settled = nil
	alone.at(t, 0, 5)
	alone.at(t, poll, 5)
	collected := alone.clock
	if ran := alone.at(t, poll, 6); !slices.Equal(ran, []string{"repair"}) {
		t.Fatalf("a new epoch ran %v, want a repair", ran)
	}
	for !slices.Contains(alone.at(t, poll, 6), "collect") {
		if alone.clock.Sub(collected) > 2*upkeep.CollectInterval {
			t.Fatal("a node with no fleet to ask never collected again")
		}
	}
	if since := alone.clock.Sub(collected); since < upkeep.CollectInterval {
		t.Fatalf("a node with no fleet to ask collected again %v after the last, before the hour", since)
	}
}

// THE SETTLED COLLECTION IS OWED UNTIL IT LEAVES NO STRAY: one that failed, or
// kept strays because a member did not confirm at that moment, is retried on
// the repair's backoff — thirty seconds, doubling — rather than an hour later,
// since those strays are what a decommission is waiting on. It ends at the
// first pass that keeps none.
func TestASettledCollectionIsOwedUntilItLeavesNoStray(t *testing.T) {
	t.Parallel()
	s := newScheduled()
	s.at(t, 0, 5)
	s.at(t, objectEpochPoll, 5)
	s.settledAt[6] = true
	s.at(t, objectEpochPoll, 6)

	s.passes.collectErr = errors.New("the log did not answer")
	if ran := s.at(t, objectEpochPoll, 6); !slices.Equal(ran, []string{"collect"}) {
		t.Fatalf("the settled poll ran %v", ran)
	}
	if ran := s.at(t, objectPassRetryFloor-time.Second, 6); len(ran) != 0 {
		t.Fatalf("a failed settled collection was retried before its backoff: %v", ran)
	}
	s.passes.collectErr, s.passes.strays = nil, 3
	if ran := s.at(t, time.Second, 6); !slices.Equal(ran, []string{"collect"}) {
		t.Fatalf("the retry after thirty seconds ran %v", ran)
	}
	if ran := s.at(t, time.Minute-time.Second, 6); len(ran) != 0 {
		t.Fatalf("a pass that kept strays was retried before its doubled backoff: %v", ran)
	}
	s.passes.strays = 0
	if ran := s.at(t, time.Second, 6); !slices.Equal(ran, []string{"collect"}) {
		t.Fatalf("the retry after a minute ran %v", ran)
	}
	for range 5 {
		if ran := s.at(t, 2*time.Minute, 6); slices.Contains(ran, "collect") {
			t.Fatalf("a settled epoch whose collection kept no stray ran %v again", ran)
		}
	}

	// A NODE THAT KEEPS EVERY COPY BY RULE is owed one pass by a settled
	// epoch and no more: a retry could only count the same strays. That is a
	// node the map does not hold, and a member on probation — which the
	// collection itself keeps whole ([upkeep.KeepsEveryCopy]), and which was
	// retried every thirty seconds doubling for as long as its probation ran
	// while this read "the map holds it" — unless it is out as well, when it
	// sheds like any out member and is owed the retries.
	for name, tc := range map[string]struct {
		members []placement.Member
		retried bool
	}{
		"outside the map": {members: []placement.Member{{Node: "data-b", Weight: 1}}},
		"on probation": {members: []placement.Member{
			{Node: "data-a", Weight: 1, Probation: true}, {Node: "data-b", Weight: 1}}},
		"on probation and out": {retried: true, members: []placement.Member{
			{Node: "data-a", Weight: 1, Probation: true, Out: true}, {Node: "data-b", Weight: 1}}},
	} {
		keeps := newScheduled()
		keeps.members = tc.members
		keeps.passes.strays = 7
		keeps.settledAt[5] = true
		keeps.at(t, 0, 5)
		if ran := keeps.at(t, objectEpochPoll, 5); !slices.Equal(ran, []string{"collect"}) {
			t.Fatalf("%s: the settled poll ran %v", name, ran)
		}
		retried := false
		for range 5 {
			retried = retried || slices.Contains(keeps.at(t, objectPassRetryFloor, 5), "collect")
		}
		if retried != tc.retried {
			t.Errorf("%s: a pass that kept strays was retried: %v, want %v", name, retried, tc.retried)
		}
	}
}

// THE LEASE COUNTS STRAYS ONLY FROM A COLLECTION THAT WALKED EVERY SLOT AT THE
// EPOCH THIS NODE PLACES BY, and says nothing otherwise — because `crewlet
// objects status` stops a member taken out on a zero, and both other zeros are
// lies: a pass that stopped short never looked, and a member taken out at
// epoch N held no strays at N-1, when its share was still its own.
func TestTheLeaseCountsStraysOnlyFromACompleteCollectionAtThisEpoch(t *testing.T) {
	t.Parallel()
	collected := func(epoch uint64, strays int) upkeep.CollectStatus {
		return upkeep.CollectStatus{Epoch: epoch, Completed: true, Strays: strays, At: time.Now()}
	}
	for name, tc := range map[string]struct {
		status upkeep.Status
		placed bool
		want   *int
	}{
		"a complete pass at this epoch": {status: upkeep.Status{Collected: collected(7, 0)},
			placed: true, want: new(0)},
		"a complete pass at this epoch keeping strays": {
			status: upkeep.Status{Collected: collected(7, 12)}, placed: true, want: new(12)},
		"only a pass before the map moved": {status: upkeep.Status{Collected: collected(6, 0)},
			placed: true},
		"a pass at this epoch that stopped short": {status: upkeep.Status{
			Collect: upkeep.CollectStatus{Epoch: 7, At: time.Now(), Error: "the log did not answer"}},
			placed: true},
		"no collection yet": {placed: true},
		"no map yet":        {status: upkeep.Status{Collected: collected(7, 0)}},
	} {
		var out objstore.ObjectsMeta
		fillPassesMeta(tc.status, 7, tc.placed, &out)
		switch {
		case tc.want == nil && out.Strays != nil:
			t.Errorf("%s: the lease counts %d strays, want none reported", name, *out.Strays)
		case tc.want != nil && (out.Strays == nil || *out.Strays != *tc.want):
			t.Errorf("%s: the lease counts %v strays, want %d", name, out.Strays, *tc.want)
		}
	}
}

// THE LEASE CARRIES EVERYTHING THE SCRUB KNOWS, and carries it as soon as it
// knows anything. The unreadable count and the error are what every surface
// renders — `/fleet`, the CLI's SCRUB line, the dashboard's notice — and they
// were never copied into the lease, so each showed 0 and no error off a disk
// failing chunk by chunk. And a scrub whose FIRST walk the disk refused has an
// error and no cycle, which a report keyed on the cycle left absent: the one
// failure read as "no scrub yet".
func TestTheLeaseCarriesWhatTheScrubFound(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		scrub upkeep.ScrubStatus
		want  *objstore.ObjectsScrub
	}{
		"no scrub yet": {},
		"a cycle under way with unreadable chunks": {
			scrub: upkeep.ScrubStatus{CycleStarted: started, Progress: 0.25, Verified: 90,
				Rotten: 1, Unreadable: 3, LastUnreadable: started.Add(time.Hour)},
			want: &objstore.ObjectsScrub{CycleStarted: started, Progress: 0.25, Verified: 90,
				Rotten: 1, Unreadable: 3},
		},
		"a cycle the disk stopped": {
			scrub: upkeep.ScrubStatus{CycleStarted: started, Progress: 0.5, Verified: 40,
				Error: "walk the store: input/output error"},
			want: &objstore.ObjectsScrub{CycleStarted: started, Progress: 0.5, Verified: 40,
				Error: "walk the store: input/output error"},
		},
		"a first walk the disk refused": {
			scrub: upkeep.ScrubStatus{Error: "walk the store: permission denied"},
			want:  &objstore.ObjectsScrub{Error: "walk the store: permission denied"},
		},
	} {
		var out objstore.ObjectsMeta
		fillPassesMeta(upkeep.Status{Scrub: tc.scrub}, 7, true, &out)
		switch {
		case tc.want == nil && out.Scrub != nil:
			t.Errorf("%s: the lease reports a scrub %+v, want none", name, *out.Scrub)
		case tc.want != nil && (out.Scrub == nil || *out.Scrub != *tc.want):
			t.Errorf("%s: the lease reports the scrub %+v, want %+v", name, out.Scrub, *tc.want)
		}
		// And what it reports survives the wire a peer reads it from.
		if tc.want == nil {
			continue
		}
		back, ok := objstore.ObjectsFromMeta(objstore.ObjectsMeta{Weight: 1, Scrub: out.Scrub}.Encode())
		if !ok || back.Scrub == nil || *back.Scrub != *tc.want {
			t.Errorf("%s: a peer reads the scrub back as %+v, want %+v", name, back.Scrub, *tc.want)
		}
	}
}

// SETTLED IS READ FROM THE OBJECTS LEASES, the maintainer's own reading: every
// member the map places on holding one that reports a completed repair at the
// map's epoch with nothing pending.
func TestTheFleetIsSettledAsItsLeasesSay(t *testing.T) {
	t.Parallel()
	m := objplacement.Map{Epoch: 4, Members: []placement.Member{
		{Node: "a", Weight: 1}, {Node: "b", Weight: 1}, {Node: "gone", Weight: 1, Out: true}}}
	lease := func(epoch uint64, completed bool, pending int) map[string]any {
		return objstore.ObjectsMeta{Weight: 1, Repair: &objstore.ObjectsRepair{
			Epoch: epoch, Completed: completed, Pending: pending, At: time.Now()}}.Encode()
	}
	for name, tc := range map[string]struct {
		b    map[string]any
		want bool
	}{
		"every member repaired here":      {lease(4, true, 0), true},
		"one still repairing":             {lease(4, false, 0), false},
		"one finished at the older epoch": {lease(3, true, 0), false},
		"one with chunks pending":         {lease(4, true, 2), false},
	} {
		e := &Engine{backends: &Backends{Coord: holdersFixture(t, map[string]map[string]any{
			coord.ObjectsResource("a"): lease(4, true, 0),
			coord.ObjectsResource("b"): tc.b,
		})}}
		got, err := e.objectsSettled()(t.Context(), m)
		if err != nil || got != tc.want {
			t.Errorf("%s: settled = %v, %v; want %v", name, got, err, tc.want)
		}
	}
	if (&Engine{backends: &Backends{}}).objectsSettled() != nil {
		t.Error("an engine with no coordination store asks a fleet it cannot read")
	}
}

// THE MAP DUTY'S LOOP IS STOPPED BEFORE THE DUTY IS GIVEN BACK, so a node that
// stops never exits holding it.
//
// The loop claims the duty afresh on every turn, and a claim of a released
// lease simply succeeds. Stopped after the release, a turn in flight across it
// took the duty straight back under this incarnation, and the node exited
// owning the placement map for the lease's whole TTL — nobody maintaining it,
// and on a new fleet no first map, so every upload refused.
//
// DETERMINISTIC IN BOTH ORDERS. The loop here makes the duty's own claim from
// a turn that stays in flight: it polls the lease, claims it the moment it
// reads it given back, and looks for its stop only AFTER each read. A teardown
// that releases first is therefore always seen, however short the gap before
// the stop, and one that stops first always ends the turn with the duty still
// held, for the release to give back.
func TestTheMapDutyIsNotTakenBackAsTheNodeStops(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, companyWithoutSandboxDoc))
	leases := e.backends.Coord
	resource := coord.WorkerResource(objectMapDuty)
	eventually(t, "the map duty", func() bool {
		lease := held(t, leases, resource)
		return lease != nil && lease.Owner == e.incarnation
	})

	e.objects.maintainer.stop()
	claim := e.workerDuty(objectMapDuty, objectMapDutyTTL)
	claimed := make(chan error, 1)
	e.objects.maintainer = startLoop(t.Context(), sleep, func(ctx context.Context) time.Duration {
		// THE READ AND THE CLAIM OUTLIVE THE STOP: they are the turn
		// already in flight, which a stop waits out rather than aborts.
		inFlight := context.WithoutCancel(ctx)
		for {
			lease, err := leases.Get(inFlight, resource)
			if err == nil && lease == nil {
				_, err := claim(inFlight)
				select {
				case claimed <- err:
				default:
				}
				return time.Millisecond
			}
			if ctx.Err() != nil {
				return 0
			}
			time.Sleep(time.Millisecond)
		}
	})

	e.Stop(context.Background())
	select {
	case err := <-claimed:
		if err != nil {
			t.Fatalf("the turn in flight could not ask for the duty: %v", err)
		}
	default:
	}
	if lease := held(t, leases, resource); lease != nil && lease.Owner == e.incarnation {
		t.Errorf("the map duty is held by %s after it stopped: no node maintains "+
			"the placement map until the lease lapses", lease.Owner)
	}
}

// THE OBJECTS READING IS SAFE BESIDE THE TEARDOWN, which does not wait for it:
// an operator's retention query reaches it from the API, and the API may still
// be answering one while the engine stops. The teardown used to clear the disk
// under it — a data race, and a reader between its nil check and its call
// through the pointer dereferenced nil and took the process down mid-shutdown.
// A -race run is what sees it, and every package here runs under one.
//
// AND AFTER THE TEARDOWN IT STILL ANSWERS, with the closed store's own account.
func TestTheObjectsReadingIsSafeBesideTheTeardown(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, companyWithoutSandboxDoc))
	eventually(t, "the object passes", func() bool { return e.objects.passes.Load() != nil })

	stop := make(chan struct{})
	reads := make(chan int)
	go func() {
		n := 0
		defer func() { reads <- n }()
		for {
			select {
			case <-stop:
				return
			default:
			}
			var r statelog.Reading
			e.objectsReading(time.Now(), &r)
			n++
		}
	}()
	e.Stop(context.Background())
	close(stop)
	if n := <-reads; n == 0 {
		t.Fatal("the premise: nothing read the objects reading while the engine stopped")
	}

	if e.objects.disk == nil {
		t.Fatal("the teardown cleared the store's disk under its readers")
	}
	var r statelog.Reading
	e.objectsReading(time.Now(), &r)
	if want := e.objects.disk.Health(); r.ObjectsHealth != want.State ||
		r.ObjectsHealthDetail != want.Detail {
		t.Errorf("a stopped node reads health %q (%q), and its store says %q (%q)",
			r.ObjectsHealth, r.ObjectsHealthDetail, want.State, want.Detail)
	}
}

// THE PASSES NEVER START ONCE THEY HAVE BEEN STOPPED. The native runtime that
// starts them may be brought up by an apply on its own goroutine, as late as
// the teardown; with the disk kept once closed, only the stop's own flag keeps
// a late start from running passes over a closed store that nothing would
// ever end.
func TestThePassesNeverStartOnceStopped(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, companyWithoutSandboxDoc))
	e.Stop(context.Background())

	refs, err := upkeep.Sources(references.All, refusedEstate{name: tracker.ObjectEstate{}.Name()})
	if err != nil {
		t.Fatalf("the references: %v", err)
	}
	if err := e.startObjectPasses(t.Context(), refs); err != nil {
		t.Fatalf("startObjectPasses on a stopped node: %v", err)
	}
	if e.objects.passes.Load() != nil {
		e.stopObjectPasses()
		t.Fatal("passes started over the store of a node that had stopped them")
	}
}

// refusedEstate is a domain whose barrier never answers, so a pass started
// over it fails at its pin rather than reading anything.
type refusedEstate struct{ name string }

func (r refusedEstate) Name() string { return r.name }

func (refusedEstate) Barrier(context.Context) (statelog.Position, error) {
	return statelog.Position{}, errors.New("the estate is not answering")
}

func (refusedEstate) Read(context.Context, statelog.Position, func(*sql.Tx) error) (bool, error) {
	return false, errors.New("the estate is not answering")
}
