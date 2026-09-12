package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// capacityFixture is one node's half of the handshake: an engine with an
// identity, a mode and a coordination store, and nothing else.
//
// The two halves this file exercises — admission and acknowledgement — run
// BEFORE anything else in the constructor and touch nothing but the fleet
// store, which is what makes them testable without a broker.
func capacityFixture(t *testing.T, id string, mode statelog.MaintenanceMode) (
	*Engine, *coordmem.Fleet) {

	t.Helper()
	fleet := coordmem.NewFleet()
	return &Engine{
		id:          id,
		incarnation: id + ":boot-1",
		mode:        mode,
		backends:    &Backends{Fleet: fleet},
	}, fleet
}

// TestANormalNodeWritesItsAdmissionBeforeItReadsTheOperation.
//
// The order IS the guarantee. A node that read the operation absent and then
// started publishing has a gap in which a coordinator can take the exclusion;
// writing first means whichever write lands first wins and the loser can see
// that it did. So the admission must be durable even on the path that then
// refuses — and must be withdrawn again before the refusal returns, or a node
// that correctly declined to start would block every later operation for ever.
func TestANormalNodeWritesItsAdmissionBeforeItReadsTheOperation(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeNormal)

	if err := e.admit(ctx, []string{"CREWLET_TRACKER_LOG"}); err != nil {
		t.Fatalf("a fleet with no operation refused a normal boot: %v", err)
	}
	admissions, err := fleet.Admissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(admissions) != 1 || admissions[0].NodeID != "node-1" ||
		admissions[0].Incarnation != e.incarnation {
		t.Fatalf("the node started without a durable admission: %+v", admissions)
	}

	// NOW ONE IS OPEN. The node must refuse, and must not leave the
	// admission behind: it is not publishing.
	_, _, err = fleet.OpenMaintenance(ctx, coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1",
		TargetMaxBytes: 1 << 33, Phase: coord.PhaseOpened, Attempt: 1,
		Participants: []string{"node-1"}, EnteredAt: time.Now().UTC(), By: "ops-3",
	})
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := capacityFixture(t, "node-2", statelog.ModeNormal)
	e2.backends.Fleet = fleet
	err = e2.admit(ctx, []string{"CREWLET_TRACKER_LOG"})
	var excluded *ErrExcluded
	if !errors.As(err, &excluded) {
		t.Fatalf("a normal boot during an open operation was permitted: %v", err)
	}
	if excluded.OperationID != "op-1" || excluded.By != "ops-3" {
		t.Errorf("the refusal does not name the operation: %+v", excluded)
	}
	admissions, err = fleet.Admissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range admissions {
		if a.NodeID == "node-2" {
			t.Fatal("the refused node left its admission behind — it is not " +
				"publishing, and the key would block every later operation")
		}
	}
}

// TestAnUnreadableOperationIsNotAnAbsentOne.
//
// Reading a coordination failure as "no maintenance" is the exact admission
// the two-sided handshake exists to make impossible: the node would start
// publishing into a fleet mid-window on the one failure that most plausibly
// accompanies one.
func TestAnUnreadableOperationIsNotAnAbsentOne(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeNormal)
	e.backends.Fleet = &blindFleet{fleetStore: fleet, readErr: errors.New("store unreachable")}

	err := e.admit(ctx, []string{"CREWLET_TRACKER_LOG"})
	if err == nil {
		t.Fatal("a node started publishing on a coordination store it could " +
			"not read — an unreadable operation was treated as an absent one")
	}
	if !strings.Contains(err.Error(), "CREWLET_TRACKER_LOG") {
		t.Errorf("the refusal does not name the stream it could not read: %v", err)
	}
	admissions, _ := fleet.Admissions(ctx)
	if len(admissions) != 0 {
		t.Fatalf("the node kept an admission it is not publishing behind: %+v",
			admissions)
	}
}

// TestOnlyANormalNodeAdmitsAndOnlyAMaintenanceNodeAcknowledges: the two halves
// are exclusive, and each is the other's evidence.
func TestOnlyANormalNodeAdmitsAndOnlyAMaintenanceNodeAcknowledges(t *testing.T) {
	ctx := context.Background()
	streams := []string{"CREWLET_TRACKER_LOG"}

	for _, mode := range []statelog.MaintenanceMode{
		statelog.ModeMaintenance, statelog.ModeSeal,
	} {
		e, fleet := capacityFixture(t, "node-1", mode)
		if err := e.admit(ctx, streams); err != nil {
			t.Fatalf("%s: admit: %v", mode, err)
		}
		admissions, _ := fleet.Admissions(ctx)
		if len(admissions) != 0 {
			t.Fatalf("%s wrote an admission — the key means `this process may "+
				"be publishing`, and that mode starts no publisher: %+v",
				mode, admissions)
		}
	}

	e, fleet := capacityFixture(t, "node-1", statelog.ModeNormal)
	// WITH AN OPERATION OPEN, which is the only state in which an
	// acknowledgement is written at all — a normal-mode node that stayed
	// silent merely because there was nothing to answer would prove
	// nothing about the mode.
	if _, _, err := fleet.OpenMaintenance(ctx, coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1",
		TargetMaxBytes: 1 << 33, Phase: coord.PhaseOpened, Attempt: 1,
		Participants: []string{"node-1"}, EnteredAt: time.Now().UTC(), By: "ops-3",
	}); err != nil {
		t.Fatal(err)
	}
	e.acknowledge(ctx, streams)
	acks, _ := fleet.MaintenanceAcks(ctx)
	if len(acks) != 0 {
		t.Fatalf("a publishing node acknowledged a capacity operation, which "+
			"is the one claim its mode cannot make — its process is exactly "+
			"the one that may hold an outstanding request: %+v", acks)
	}
}

// TestAnAcknowledgementCarriesTheModeItWasMadeIn.
//
// The seal is established only from SEAL mode, because that mode cannot write
// configuration — so the process making the claim cannot be the one holding
// the request it is retiring. An ack that did not carry its mode would let a
// maintenance-mode node's restart satisfy the barrier.
func TestAnAcknowledgementCarriesTheModeItWasMadeIn(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeSeal)

	// NO OPERATION IS NOT A FAILURE — a fleet restarted into maintenance
	// mode before the verb ran is the ordinary first step.
	e.acknowledge(ctx, []string{"CREWLET_TRACKER_LOG"})
	if acks, _ := fleet.MaintenanceAcks(ctx); len(acks) != 0 {
		t.Fatalf("an acknowledgement was written against no operation: %+v", acks)
	}

	if _, _, err := fleet.OpenMaintenance(ctx, coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1",
		TargetMaxBytes: 1 << 33, Phase: coord.PhaseBaselined, Attempt: 2,
		Participants: []string{"node-1"}, EnteredAt: time.Now().UTC(), By: "ops-3",
	}); err != nil {
		t.Fatal(err)
	}
	e.acknowledge(ctx, []string{"CREWLET_TRACKER_LOG"})
	acks, err := fleet.MaintenanceAcks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 1 {
		t.Fatalf("acks = %+v, want exactly this node's", acks)
	}
	got := acks[0]
	if got.Mode != string(statelog.ModeSeal) {
		t.Errorf("the ack does not say which mode made it (%q) — the barrier "+
			"filters on exactly that", got.Mode)
	}
	if got.Attempt != 2 {
		t.Errorf("the ack is against attempt %d rather than the operation's "+
			"current one: a restart for an earlier attempt is not evidence "+
			"for this one", got.Attempt)
	}
	if got.OperationID != "op-1" || got.Incarnation != e.incarnation {
		t.Errorf("the ack does not identify what restarted: %+v", got)
	}
}

// TestTheBarrierRefusesEveryWayAnAcknowledgementCanBeTheWrongOne.
//
// One table over the four ways an ack fails to be evidence, because each of
// them looks like a satisfied barrier from a distance and each would confirm
// an operation whose request may still be outstanding.
func TestTheBarrierRefusesEveryWayAnAcknowledgementCanBeTheWrongOne(t *testing.T) {
	op := coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1", Attempt: 2,
		Participants:      []string{"node-1", "node-2"},
		WriteIncarnations: map[string]string{"node-1": "node-1:old", "node-2": "node-2:old"},
	}
	good := func(node string) coord.MaintenanceAck {
		return coord.MaintenanceAck{
			NodeID: node, OperationID: "op-1", Attempt: 2,
			Incarnation: node + ":new", Mode: string(statelog.ModeSeal),
		}
	}
	bend := func(node string, f func(*coord.MaintenanceAck)) []coord.MaintenanceAck {
		other := good("node-2")
		mine := good(node)
		f(&mine)
		return []coord.MaintenanceAck{mine, other}
	}

	cases := []struct {
		name string
		acks []coord.MaintenanceAck
		want bool
	}{
		{"every participant restarted for this attempt",
			[]coord.MaintenanceAck{good("node-1"), good("node-2")}, true},
		{"one never acknowledged at all",
			[]coord.MaintenanceAck{good("node-2")}, false},
		{"one acknowledged the previous attempt",
			bend("node-1", func(a *coord.MaintenanceAck) { a.Attempt = 1 }), false},
		{"one acknowledged a different operation",
			bend("node-1", func(a *coord.MaintenanceAck) { a.OperationID = "op-0" }), false},
		{"one acknowledged from the mode that can still write configuration",
			bend("node-1", func(a *coord.MaintenanceAck) {
				a.Mode = string(statelog.ModeMaintenance)
			}), false},
		{"one is the same process that was baselined",
			bend("node-1", func(a *coord.MaintenanceAck) { a.Incarnation = "node-1:old" }),
			false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restarted, incarnations := barrierEvidence(op, tc.acks)
			if restarted != tc.want {
				t.Fatalf("the barrier reports restarted=%v, want %v", restarted, tc.want)
			}
			if !tc.want {
				return
			}
			if len(incarnations) != 2 {
				t.Fatalf("the barrier held but named %d incarnations", len(incarnations))
			}
			// THE SAME EVIDENCE THE SEAL PREDICATE READS: a retirement
			// established from a different reading than the seal would
			// retire a record the seal then refuses to accept.
			out := statelog.Retire(op, statelog.BarrierEvidence{
				Incarnations: incarnations, ObservedMaxBytes: 1 << 33,
				At: time.Now().UTC(),
			})
			if held, missing := statelog.SealHolds(out, tc.acks); !held {
				t.Fatalf("the barrier held but the seal does not: %s", missing)
			}
		})
	}
}

// TestAnExcludedParticipantIsTheOnlyThingThatWaivesAnAcknowledgement.
func TestAnExcludedParticipantIsTheOnlyThingThatWaivesAnAcknowledgement(t *testing.T) {
	op := coord.MaintenanceOperation{
		OperationID: "op-1", Attempt: 1,
		Participants:      []string{"node-1", "node-2"},
		WriteIncarnations: map[string]string{"node-1": "node-1:old", "node-2": "node-2:old"},
	}
	acks := []coord.MaintenanceAck{{
		NodeID: "node-1", OperationID: "op-1", Attempt: 1,
		Incarnation: "node-1:new", Mode: string(statelog.ModeSeal),
	}}
	if restarted, _ := barrierEvidence(op, acks); restarted {
		t.Fatal("the barrier held with a participant that never came back")
	}
	op.Excluded = []string{"node-2"}
	restarted, incarnations := barrierEvidence(op, acks)
	if !restarted {
		t.Fatal("an operator's exclusion did not waive the acknowledgement")
	}
	if _, named := incarnations["node-2"]; named {
		t.Error("the excluded node was given an incarnation it never wrote")
	}
}

// TestABaselineOfNothingIsRefusedRatherThanRecorded.
//
// An empty baseline compares unequal to every later acknowledgement, so a
// participant baselined at one satisfies the seal without ever restarting.
func TestABaselineOfNothingIsRefusedRatherThanRecorded(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	op := coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1", Attempt: 1,
		Participants: []string{"node-1", "node-2"},
	}

	_, _, err := e.capacityIncarnations(ctx, op)
	if err == nil {
		t.Fatal("a participant that never restarted was baselined at nothing, " +
			"which the seal then satisfies without it ever coming back")
	}
	if !strings.Contains(err.Error(), "node-2") {
		t.Errorf("the refusal does not name who has not restarted: %v", err)
	}

	// EITHER an acknowledgement or an exclusion resolves it — and nothing
	// else does.
	if err := fleet.PutMaintenanceAck(ctx, coord.MaintenanceAck{
		NodeID: "node-2", OperationID: "op-1", Attempt: 1,
		Incarnation: "node-2:boot-9", Mode: string(statelog.ModeMaintenance),
	}); err != nil {
		t.Fatal(err)
	}
	incarnations, participants, err := e.capacityIncarnations(ctx, op)
	if err != nil {
		t.Fatalf("capacityIncarnations: %v", err)
	}
	if incarnations["node-2"] != "node-2:boot-9" {
		t.Errorf("the baseline is %q rather than what node-2 acknowledged as",
			incarnations["node-2"])
	}
	// THIS NODE'S OWN IDENTITY IS NOT READ BACK FROM THE STORE: it is the
	// coordinator, it has not written an ack, and it knows what it is.
	if incarnations["node-1"] != e.incarnation {
		t.Errorf("the coordinator baselined itself as %q", incarnations["node-1"])
	}
	if len(participants) != 2 {
		t.Errorf("participants = %v", participants)
	}
}

// TestEveryMachineThatCouldHoldARequestMustAcknowledge.
//
// The participant set is about whose PROCESS could hold an outstanding
// request, so it is every node the fleet has a position for, union everything
// holding a presence lease, union this node — not the retention counted set,
// which is about whose position pins the trim. A node evicted from that is
// still a machine that can run a publisher.
func TestEveryMachineThatCouldHoldARequestMustAcknowledge(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-coordinator", statelog.ModeMaintenance)
	backend := coordmem.New()
	e.backends.Coord = backend

	if err := fleet.PutPositions(ctx, coord.NodePositions{
		NodeID: "node-with-a-position", At: time.Now().UTC(),
		Domains: map[string]coord.DomainPosition{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.TryAcquire(ctx, coord.NodeResource("node-holding-a-lease"),
		coord.AcquireOptions{Owner: "node-holding-a-lease:boot-1", TTL: time.Minute,
			Preferred: "node-holding-a-lease"}); err != nil {
		t.Fatal(err)
	}

	got, err := e.capacityParticipants(ctx)
	if err != nil {
		t.Fatalf("capacityParticipants: %v", err)
	}
	want := []string{"node-coordinator", "node-holding-a-lease", "node-with-a-position"}
	if len(got) != len(want) {
		t.Fatalf("participants = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("participants = %v, want %v", got, want)
		}
	}
}

// TestAnUnreadablePositionRegisterRefusesRatherThanShrinkingTheSet: a
// participant set read from a store that failed is a set with nodes silently
// missing, and every one of them is a machine that may be publishing.
func TestAnUnreadablePositionRegisterRefusesRatherThanShrinkingTheSet(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	e.backends.Fleet = &blindFleet{fleetStore: fleet, positionsErr: errors.New("store unreachable")}

	if _, err := e.capacityParticipants(ctx); err == nil {
		t.Fatal("the participant set was formed from a register that could not " +
			"be read — every node missing from it may be publishing")
	}
}

// TestAnAdmissionBlocksTakingTheExclusion is the coordinator's half of the
// handshake: a node that slipped between the check and its own start left a
// durable record, and that record is what a silence could never be.
func TestAnAdmissionBlocksTakingTheExclusion(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	if err := fleet.PutAdmission(ctx, coord.Admission{
		NodeID: "node-9", Incarnation: "node-9:boot-1", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := e.openCapacity(ctx, CapacityRequest{
		Stream: "CREWLET_TRACKER_LOG", TargetMaxBytes: 1 << 33, By: "ops-3",
	}, 1<<32)
	if err == nil {
		t.Fatal("the exclusion was taken while a node held an admission")
	}
	if !strings.Contains(err.Error(), "node-9") {
		t.Errorf("the refusal does not name who is still admitted: %v", err)
	}
	if op, found, _ := fleet.Maintenance(ctx, "CREWLET_TRACKER_LOG"); found {
		t.Fatalf("a window was opened anyway: %+v", op)
	}
}

// TestAnOpenOperationIsResumedAtItsOwnTargetAndNeverRetargeted.
//
// The target is chosen once for the life of an operation: a verify compares
// the observed ceiling against it, so a target that moved mid-window would
// make a mismatch unreadable — nobody could tell an unapplied request from a
// changed mind.
func TestAnOpenOperationIsResumedAtItsOwnTargetAndNeverRetargeted(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	if _, _, err := fleet.OpenMaintenance(ctx, coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1",
		TargetMaxBytes: 1 << 33, OriginalMaxBytes: 1 << 32,
		Phase: coord.PhaseApplied, Attempt: 1,
		Participants: []string{"node-1"}, EnteredAt: time.Now().UTC(), By: "ops-3",
	}); err != nil {
		t.Fatal(err)
	}

	held, err := e.openCapacity(ctx, CapacityRequest{
		Stream: "CREWLET_TRACKER_LOG", TargetMaxBytes: 1 << 33, By: "ops-4",
	}, 1<<32)
	if err != nil {
		t.Fatalf("the same target did not resume the open operation: %v", err)
	}
	if held.OperationID != "op-1" || held.Phase != coord.PhaseApplied {
		t.Fatalf("the resume did not return the open operation: %+v", held)
	}

	_, err = e.openCapacity(ctx, CapacityRequest{
		Stream: "CREWLET_TRACKER_LOG", TargetMaxBytes: 1 << 34, By: "ops-4",
	}, 1<<32)
	if err == nil {
		t.Fatal("an open operation was retargeted, which makes a later " +
			"mismatch unreadable")
	}
	if !strings.Contains(err.Error(), "already open") {
		t.Errorf("the refusal does not say an operation holds the stream: %v", err)
	}
}

// TestAnUnknownCreateRetriesWithTheSameIdRatherThanOpeningASecondWindow.
//
// An unknown create establishes nothing: the request may never have been
// received and may still be outstanding. A retry with a fresh id would open a
// SECOND window on a stream that may already have one — and the loser of that
// race is an exclusion nobody can find.
func TestAnUnknownCreateRetriesWithTheSameIdRatherThanOpeningASecondWindow(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	flaky := &blindFleet{fleetStore: fleet, openFailures: 2}
	e.backends.Fleet = flaky

	op, err := e.openCapacity(ctx, CapacityRequest{
		Stream: "CREWLET_TRACKER_LOG", TargetMaxBytes: 1 << 33, By: "ops-3",
	}, 1<<32)
	if err != nil {
		t.Fatalf("openCapacity: %v", err)
	}
	if len(flaky.openedIDs) != 3 {
		t.Fatalf("the create was attempted %d times, want 3", len(flaky.openedIDs))
	}
	for _, id := range flaky.openedIDs {
		if id != op.OperationID {
			t.Fatalf("a retry minted a fresh id (%s vs %s) — the first create "+
				"may still be outstanding, so this opens a second window on "+
				"one stream", id, op.OperationID)
		}
	}
}

// TestAnUnknownCreateThatNeverResolvesRefusesRatherThanReportingNoWindow.
func TestAnUnknownCreateThatNeverResolvesRefusesRatherThanReportingNoWindow(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	e.backends.Fleet = &blindFleet{fleetStore: fleet, openFailures: 99}

	_, err := e.openCapacity(ctx, CapacityRequest{
		Stream: "CREWLET_TRACKER_LOG", TargetMaxBytes: 1 << 33, By: "ops-3",
	}, 1<<32)
	if err == nil {
		t.Fatal("a create whose outcome was never established reported success")
	}
	if !strings.Contains(err.Error(), "may already be excluded") {
		t.Errorf("the refusal does not say the fleet may already be excluded: %v", err)
	}
}

// TestOnlyAPeerOnTheLiveStreamHoldsHistoryAReanchorWouldDiscard.
//
// ONE DEFINITION, because the permission check counts these and the refusal
// names them: if the two disagreed, a reanchor would refuse naming nobody, or
// permit while naming someone.
func TestOnlyAPeerOnTheLiveStreamHoldsHistoryAReanchorWouldDiscard(t *testing.T) {
	rows := []coord.NodePositions{
		{NodeID: "self", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 3, AppliedThrough: 900},
		}},
		{NodeID: "hydrated", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 3, AppliedThrough: 900},
		}},
		{NodeID: "on-the-old-stream", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 2, AppliedThrough: 900},
		}},
		{NodeID: "applied-nothing", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 3, AppliedThrough: 0},
		}},
		{NodeID: "runs-another-domain", Domains: map[string]coord.DomainPosition{
			"vectors": {Generation: 3, AppliedThrough: 900},
		}},
	}
	got := hydratedPeers(rows, "tracker", 3, "self")
	if len(got) != 1 || got[0] != "hydrated" {
		t.Fatalf("hydrated peers = %v, want exactly [hydrated]: this node is "+
			"not its own peer, a peer at another generation is on the stream "+
			"being replaced, and one that has applied nothing holds no history",
			got)
	}
}

// fleetStore is an alias so blindFleet can embed the memory fleet without the
// field name shadowing [coord.Fleet]'s own Fleet method.
type fleetStore = coordmem.Fleet

// blindFleet fails specific reads, so the paths that must not read a failure
// as an absence are reachable.
type blindFleet struct {
	*fleetStore
	readErr      error
	positionsErr error
	openFailures int
	openedIDs    []string
}

func (b *blindFleet) Maintenance(ctx context.Context, stream string) (
	coord.MaintenanceOperation, bool, error) {

	if b.readErr != nil {
		return coord.MaintenanceOperation{}, false, b.readErr
	}
	return b.fleetStore.Maintenance(ctx, stream)
}

func (b *blindFleet) Positions(ctx context.Context) ([]coord.NodePositions, error) {
	if b.positionsErr != nil {
		return nil, b.positionsErr
	}
	return b.fleetStore.Positions(ctx)
}

func (b *blindFleet) OpenMaintenance(ctx context.Context, op coord.MaintenanceOperation) (
	coord.MaintenanceOperation, bool, error) {

	b.openedIDs = append(b.openedIDs, op.OperationID)
	if b.openFailures > 0 {
		b.openFailures--
		return coord.MaintenanceOperation{}, false, errors.New("store unreachable")
	}
	return b.fleetStore.OpenMaintenance(ctx, op)
}
