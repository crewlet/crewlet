package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
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
	}, jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom)
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

// A TARGET THE LOG ALREADY HOLDS MORE THAN IS REFUSED BEFORE THE WINDOW OPENS.
//
// A resize is decided against the usage the log is at, which is the whole
// reason it runs with nothing publishing, and nothing decided against it: a
// target under what the log held was carried through three restarts to a log
// that refused every append the moment it applied. Lowering a ceiling is a real
// gesture (a log created larger than its budget allows is the common case), so
// the line is drawn at the usage and nowhere else.
func TestATargetTheLogAlreadyExceedsIsRefused(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	current := jetstream.LogStats{Bytes: 5 << 30, MaxBytes: 8 << 30}
	for _, target := range []uint64{4 << 30, 5 << 30} {
		_, err := e.openCapacity(ctx, CapacityRequest{
			Stream: "CREWLET_PAGES_LOG", TargetMaxBytes: target, By: "ops-3",
		}, current, unstatedRoom)
		if err == nil {
			t.Fatalf("a %d-byte target on a log holding %d bytes was accepted",
				target, current.Bytes)
		}
		if !strings.Contains(err.Error(), "holds 5368709120 bytes") {
			t.Errorf("the refusal does not say what the log holds: %v", err)
		}
		if op, found, _ := fleet.Maintenance(ctx, "CREWLET_PAGES_LOG"); found {
			t.Fatalf("a window was opened anyway: %+v", op)
		}
	}

	// LOWERING PAST THE USAGE IS NOT LOWERING PAST THE CEILING: a target
	// under the old ceiling and above what the log holds is the gesture
	// that reclaims a reservation.
	op, err := e.openCapacity(ctx, CapacityRequest{
		Stream: "CREWLET_PAGES_LOG", TargetMaxBytes: 6 << 30, By: "ops-3",
	}, current, unstatedRoom)
	if err != nil {
		t.Fatalf("a target above the usage and under the ceiling was refused: %v", err)
	}
	if op.OriginalMaxBytes != current.MaxBytes {
		t.Errorf("the window recorded an original ceiling of %d, want %d",
			op.OriginalMaxBytes, current.MaxBytes)
	}
}

// A TARGET ONLY THE GATE RESERVE IS ABOVE THE USAGE IS A FULL LOG TOO.
//
// Ordinary writes on a log that claims identity are refused at the target less
// its gate reserve, so a target that clears the usage by less than the reserve
// was carried through three restarts to a log that refused every ordinary
// append the moment it applied — which is what refusing a target at the usage
// exists to prevent, measured against the wrong line. A log that keeps no
// reserve is still measured against its whole ceiling, and the target the
// refusal names is exact: it opens, and a byte less does not.
func TestATargetOnlyTheGateReserveClearsIsRefused(t *testing.T) {
	ctx := context.Background()
	current := jetstream.LogStats{Bytes: 15 << 30, MaxBytes: 32 << 30}
	const target = 16 << 30 // a sixteenth of it is the reserve: 15 GiB left

	open := func(stream string, target uint64) error {
		e, _ := capacityFixture(t, "node-1", statelog.ModeMaintenance)
		_, err := e.openCapacity(ctx, CapacityRequest{
			Stream: stream, TargetMaxBytes: target, By: "ops-3",
		}, current, unstatedRoom)
		return err
	}
	for _, domain := range []statelog.Domain{tracker.Domain{}, pages.Domain{}} {
		stream := domain.Stream().Name
		err := open(stream, target)
		if err == nil {
			t.Fatalf("a %d-byte target on %s, holding %d bytes, was accepted — "+
				"its ordinary writes are held to %d and would all be refused",
				uint64(target), stream, current.Bytes,
				statelog.OrdinaryCeiling(target, true))
		}
		least := smallestTargetAbove(current.Bytes, true)
		for _, want := range []string{
			"holds 16106127360 bytes", "ordinary writes are held to 16106127360",
			fmt.Sprintf("at least %d bytes", least),
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal on %s does not say %q: %v", stream, want, err)
			}
		}
		if err := open(stream, least); err != nil {
			t.Errorf("the target the refusal named, %d, was refused on %s: %v",
				least, stream, err)
		}
		if err := open(stream, least-1); err == nil {
			t.Errorf("a byte under the target the refusal named was accepted on "+
				"%s, so the number it tells an operator to type is not the "+
				"least that works", stream)
		}
	}
	if err := open(search.Domain{}.Stream().Name, target); err != nil {
		t.Errorf("the vector changelog keeps no reserve, and a target above what "+
			"it holds was refused: %v", err)
	}
}

// A TARGET UNDER THE FLOOR IS REFUSED ON EVERY LOG, whatever it holds.
//
// Tier A refuses an explicit ceiling below a gibibyte and the division of the
// broker's budget never scales one below it; the capacity verb was the one way
// round both. And on a log that claims identity the floor is what the gate
// reserve is sized against, so a smaller ceiling keeps too small a reserve for
// the appends in flight it has to absorb.
func TestATargetUnderTheFloorIsRefused(t *testing.T) {
	ctx := context.Background()
	current := jetstream.LogStats{Bytes: 1 << 20, MaxBytes: 4 << 30}
	for _, domain := range registeredDomains() {
		stream := domain.Stream().Name
		e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
		_, err := e.openCapacity(ctx, CapacityRequest{
			Stream: stream, TargetMaxBytes: uint64(MinDomainCeiling) - 1, By: "ops-3",
		}, current, unstatedRoom)
		if err == nil {
			t.Fatalf("a target a byte under the floor was accepted on %s", stream)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("at least %d bytes", MinDomainCeiling)) {
			t.Errorf("the refusal on %s does not name the floor: %v", stream, err)
		}
		if op, found, _ := fleet.Maintenance(ctx, stream); found {
			t.Fatalf("a window was opened anyway on %s: %+v", stream, op)
		}
		e, _ = capacityFixture(t, "node-1", statelog.ModeMaintenance)
		if _, err := e.openCapacity(ctx, CapacityRequest{
			Stream: stream, TargetMaxBytes: uint64(MinDomainCeiling), By: "ops-3",
		}, current, unstatedRoom); err != nil {
			t.Errorf("a target at the floor was refused on %s: %v", stream, err)
		}
	}
}

// unstatedRoom is a broker that states no limit this node can read, which
// holds no target back.
var unstatedRoom = jetstream.StorageBudget{Limit: -1, Source: jetstream.BudgetUnstated}

// A RAISE THE BROKER CANNOT RESERVE IS REFUSED BEFORE THE WINDOW OPENS.
//
// The broker refuses that update when the window applies it, and an apply that
// returned an error is an unknown only the seal retires: a raise past the
// broker's room cost the fleet its restarts, and its attempts, to learn a
// number the node could read before any of them. So the room is decided with
// the usage, and only where it is stated.
func TestARaiseTheBrokerCannotReserveIsRefused(t *testing.T) {
	ctx := context.Background()
	current := jetstream.LogStats{Bytes: 1 << 30, MaxBytes: 4 << 30}
	room := jetstream.StorageBudget{Limit: 10 << 30, Committed: 8 << 30,
		Source: jetstream.BudgetServerStore}

	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	_, err := e.openCapacity(ctx, CapacityRequest{
		Stream: "CREWLET_PAGES_LOG", TargetMaxBytes: 6<<30 + 1, By: "ops-3",
	}, current, room)
	if err == nil {
		t.Fatal("a raise one byte past what the broker has left was accepted")
	}
	for _, want := range []string{
		"reserves 2147483649 more", "the broker has 2147483648 left",
		"at most 6442450944 bytes",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if op, found, _ := fleet.Maintenance(ctx, "CREWLET_PAGES_LOG"); found {
		t.Fatalf("a window was opened anyway: %+v", op)
	}

	for name, tc := range map[string]struct {
		target uint64
		room   jetstream.StorageBudget
	}{
		"a raise of exactly what is left": {6 << 30, room},
		// UNSTATED HOLDS NOTHING BACK: a limit this node cannot read is
		// the broker's to apply, not a guess to refuse on.
		"a raise against an unstated limit": {64 << 30, unstatedRoom},
		// LOWERING RESERVES NOTHING, so a spent broker does not refuse it:
		// it is how a log gives a reservation back.
		"a lowering on a spent broker": {2 << 30, jetstream.StorageBudget{
			Limit: 8 << 30, Committed: 8 << 30, Source: jetstream.BudgetServerStore}},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := capacityFixture(t, "node-1", statelog.ModeMaintenance)
			if _, err := e.openCapacity(ctx, CapacityRequest{
				Stream: "CREWLET_PAGES_LOG", TargetMaxBytes: tc.target, By: "ops-3",
			}, current, tc.room); err != nil {
				t.Fatalf("a %d-byte target was refused: %v", tc.target, err)
			}
		})
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
	}, jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom)
	if err != nil {
		t.Fatalf("the same target did not resume the open operation: %v", err)
	}
	if held.OperationID != "op-1" || held.Phase != coord.PhaseApplied {
		t.Fatalf("the resume did not return the open operation: %+v", held)
	}

	_, err = e.openCapacity(ctx, CapacityRequest{
		Stream: "CREWLET_TRACKER_LOG", TargetMaxBytes: 1 << 34, By: "ops-4",
	}, jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom)
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
	}, jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom)
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
	}, jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom)
	if err == nil {
		t.Fatal("a create whose outcome was never established reported success")
	}
	if !strings.Contains(err.Error(), "may already be excluded") {
		t.Errorf("the refusal does not say the fleet may already be excluded: %v", err)
	}
}

// TestOnlyAPeerAtALaterGenerationHasReanchoredTheStream.
//
// ONE DEFINITION, because the permission check counts these and the refusal
// names them: if the two disagreed, a reanchor would refuse naming nobody, or
// permit while naming someone.
func TestOnlyAPeerAtALaterGenerationHasReanchoredTheStream(t *testing.T) {
	rows := []coord.NodePositions{
		{NodeID: "self", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 4, AppliedThrough: 900},
		}},
		// A PEER STILL AT THIS NODE'S GENERATION is on the stream this
		// node's rows came from — however much it applied there — and is
		// the most-caught-up rule's to weigh, never a refusal of its own:
		// counted here, every peer on a lost stream refused every reanchor.
		{NodeID: "on-the-lost-stream", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 3, AppliedThrough: 900},
		}},
		{NodeID: "behind-a-reanchor", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 2, AppliedThrough: 900},
		}},
		// A PEER THAT HAS ALREADY RE-ANCHORED this stream holds the fleet's
		// history in that generation, whatever it has applied since.
		{NodeID: "already-reanchored", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 4, AppliedThrough: 12},
		}},
		{NodeID: "reanchored-onto-an-empty-log", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 4, AppliedThrough: 0},
		}},
		{NodeID: "runs-another-domain", Domains: map[string]coord.DomainPosition{
			"vectors": {Generation: 9, AppliedThrough: 900},
		}},
	}
	got := reanchoredPeers(rows, "tracker", 3, "self", nil)
	want := []string{"already-reanchored", "reanchored-onto-an-empty-log"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("re-anchored peers = %v, want exactly %v: this node is not its own "+
			"peer, a peer at this node's generation or an earlier one has not "+
			"re-anchored the stream, and one at a LATER generation has — a second "+
			"independent reanchor would keep a different prefix under the same "+
			"number", got, want)
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

// budgetHost is a broker that answers the two capacity questions and nothing
// else.
//
// The PROVISIONING half of [domainHost] is unreachable from every caller here
// by construction — a refusal creates nothing — so those stubs panic rather
// than pretending to work, which is what stops a later change quietly
// depending on one.
//
// The embedded [queue.EventQueue] is NIL and deliberately so. It is what lets
// this sit in the engine's own Queue slot, which is where [Engine.capacityHost]
// reads its broker from — so these cases exercise the path production takes
// rather than a field a test set for it, which is the arrangement under which
// the constructor's omission was invisible. Every verb it contributes panics
// on a nil interface, exactly as the named stubs below do on purpose.
type budgetHost struct {
	queue.EventQueue
	room jetstream.StorageBudget
	err  error
}

func (h budgetHost) GrowthBudget(context.Context) (jetstream.StorageBudget, error) {
	return h.room, h.err
}

func (h budgetHost) StreamBudget(context.Context) (jetstream.StorageBudget, error) {
	return h.room, h.err
}

func (budgetHost) Clustered() jsprovision.Clustered { return false }

func (budgetHost) DomainStreamCeiling(context.Context, string) (int64, bool, error) {
	panic("a capacity refusal must not read a stream's ceiling")
}

func (budgetHost) EnsureDomainStream(context.Context, jetstream.DomainStream) error {
	panic("a capacity refusal must not provision anything")
}

func (budgetHost) DomainLog(context.Context, string) (*jetstream.DomainLog, error) {
	panic("a capacity refusal must not open a log")
}

func (budgetHost) DomainConsumer(context.Context, string, string, uint64) (*jetstream.DomainConsumer, error) {
	panic("a capacity refusal must not open a consumer")
}

// capacityNode is an engine holding ONE domain and a broker that answers about
// capacity and nothing else.
//
// Everything the refusals below reach runs before the first broker call on the
// log itself, which is what makes them testable without one: the target's
// shape is decided before the exclusion is opened, and that ordering is the
// point rather than an implementation detail.
func capacityNode(t *testing.T, host budgetHost) (*Engine, *coordmem.Fleet, string) {
	t.Helper()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	domain := tracker.Domain{}
	e.backends.Queue = host
	e.native.Store(&native{log: &stateLog{
		order:   []string{domain.Name()},
		domains: map[string]*runningDomain{domain.Name(): {domain: domain}},
		volume:  t.TempDir(),
	}})
	return e, fleet, domain.Stream().Name
}

// A TARGET PAST int64 IS THE UNBOUNDED SETTING WEARING A LARGE NUMBER.
//
// A stream's ceiling is an int64 on the wire, so a target above that wraps
// NEGATIVE — and a negative MaxBytes is how JetStream spells "unbounded". It
// is the same stream a target of zero would have produced, which this verb
// already refuses by name, reached from the opposite-looking input.
func TestACapacityTargetPastInt64IsRefused(t *testing.T) {
	ctx := context.Background()
	e, fleet, stream := capacityNode(t, budgetHost{room: unstatedRoom})

	_, err := e.SetCapacity(ctx, CapacityRequest{
		Stream: stream, TargetMaxBytes: math.MaxUint64, By: "ops-3"})
	if err == nil {
		t.Fatal("a target past int64 was accepted, which sets the log " +
			"unbounded while reporting a ceiling of eighteen exabytes")
	}
	if !strings.Contains(err.Error(), "unbounded") {
		t.Errorf("the refusal does not say what the number would actually "+
			"do:\n%v", err)
	}
	if _, found, ferr := fleet.Maintenance(ctx, stream); ferr != nil || found {
		t.Errorf("an operation was opened anyway (found=%t, err=%v): the "+
			"exclusion is the expensive half and it is taken after this check",
			found, ferr)
	}
}

// A REFUSED CONFIGURATION REQUEST IS NOT AN UNKNOWN ONE.
//
// The apply wrapped every failure as "the outcome is unknown and the operation
// stays open", which is right for a transport error — the request may have
// landed — and wrong for a broker that checked the ceiling against its limit
// and declined to propose it. That one wrote nothing, and the number it
// compared against is readable, so reporting it as an unknown costs the
// operator both facts: what went wrong, and that waiting for the seal will not
// tell them anything more. The journal record stays `issued` either way, which
// is why even this half says to abandon rather than to walk away.
func TestARefusedCeilingIsReportedAsARefusalAndNotAsAnUnknown(t *testing.T) {
	ctx := context.Background()
	e, _, stream := capacityNode(t, budgetHost{room: jetstream.StorageBudget{
		Limit: 8 << 30, Committed: 7 << 30, Source: jetstream.BudgetServerStore}})
	op := coord.MaintenanceOperation{Stream: stream, TargetMaxBytes: 7 << 30}

	// THE SHAPE [jetstream.DomainLog.SetMaxBytes] PRODUCES, which names
	// the sentinel over the broker's own numberless answer.
	refused := e.applyFailure(ctx, op, fmt.Errorf("jetstream: set %q's ceiling "+
		"to %d: %w: %w", stream, op.TargetMaxBytes, jetstream.ErrInsufficientStorage,
		&natsjs.APIError{ErrorCode: 10047, Code: 500,
			Description: "insufficient storage resources available"}))
	if strings.Contains(refused.Error(), "the outcome is unknown") {
		t.Errorf("a refusal the broker answered is reported as a request that "+
			"may still be in flight:\n%v", refused)
	}
	// THE NUMBERS THE BROKER COMPARED, read at the refusal: what is left,
	// what is already reserved, the limit, and the field that sets it.
	for _, needle := range []string{
		"REFUSED", "abandon",
		strconv.FormatInt(1<<30, 10), // left to reserve
		strconv.FormatInt(7<<30, 10), // already reserved
		strconv.FormatInt(8<<30, 10), // the limit
		"stream.store_max_bytes",     // the lever
	} {
		if !strings.Contains(refused.Error(), needle) {
			t.Errorf("the refusal does not mention %q:\n%v", needle, refused)
		}
	}

	// AND THE OTHER HALF STAYS AN UNKNOWN. A transport error says
	// nothing about whether the request landed, and the barrier is what
	// resolves that.
	lost := e.applyFailure(ctx, op, errors.New("nats: timeout"))
	if !strings.Contains(lost.Error(), "the outcome is unknown") {
		t.Errorf("a lost request is reported as something other than an "+
			"unknown, which is the one thing the journal record is for:\n%v", lost)
	}
	if strings.Contains(lost.Error(), "REFUSED") {
		t.Errorf("a lost request is reported as a refusal:\n%v", lost)
	}
}

// AND A NODE THAT CANNOT READ THE ROOM STILL REPORTS THE REFUSAL.
//
// The clause is context, not the finding: the broker already said it refused,
// and an unreadable budget must not turn that answer back into an unknown the
// fleet has to seal to retire. An external broker whose account states no
// limit is the same case — "the limit is -1" is not a sentence.
func TestARefusalWithNoReadableRoomIsStillARefusal(t *testing.T) {
	ctx := context.Background()
	for name, host := range map[string]budgetHost{
		"a budget that cannot be read":  {err: errors.New("no responders")},
		"a broker that states no limit": {room: unstatedRoom},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, stream := capacityNode(t, host)
			err := e.applyFailure(ctx, coord.MaintenanceOperation{
				Stream: stream, TargetMaxBytes: 7 << 30},
				fmt.Errorf("%w: refused", jetstream.ErrInsufficientStorage))
			if strings.Contains(err.Error(), "the outcome is unknown") {
				t.Errorf("a refusal became an unknown because the clause "+
					"beside it could not be built:\n%v", err)
			}
			if !strings.Contains(err.Error(), "REFUSED") {
				t.Errorf("the refusal is no longer named:\n%v", err)
			}
		})
	}
}

// A NODE THAT HOLDS NO DATA IS NOT A PARTICIPANT, AND ADMITS NOTHING. It
// publishes to no state log — its seats write through a data node, which is
// the publisher this handshake is about — so a participant set that named it
// would wait for an acknowledgement it has no reason to give, and an admission
// it wrote would be a publisher a capacity operation waited on for ever.
func TestAStatelessNodeHasNoPartInACapacityOperation(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-coordinator", statelog.ModeMaintenance)
	backend := coordmem.New()
	e.backends.Coord = backend
	for id, roles := range map[string][]string{
		"data-a": {"data", "seats"}, "agent-1": {"seats"},
	} {
		if _, err := backend.TryAcquire(ctx, coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":boot-1", TTL: time.Minute, Meta: map[string]any{"roles": roles},
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.capacityParticipants(ctx)
	if err != nil {
		t.Fatalf("capacityParticipants: %v", err)
	}
	if want := []string{"data-a", "node-coordinator"}; !slices.Equal(got, want) {
		t.Fatalf("participants = %v, want %v", got, want)
	}

	stateless, _ := capacityFixture(t, "agent-1", statelog.ModeNormal)
	stateless.backends.Fleet = fleet
	stateless.boot = &config.Bootstrap{Node: config.Node{Roles: []string{"seats"}}}
	if err := stateless.admit(ctx, []string{"CREWLET_TRACKER_LOG"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	admissions, err := fleet.Admissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(admissions) != 0 {
		t.Fatalf("a node that publishes to no state log recorded %+v", admissions)
	}
}
