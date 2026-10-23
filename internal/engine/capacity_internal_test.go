package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
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
// started publishing has a gap in which a coordinator can open a window and
// send its request;
// writing first means whichever write lands first wins and the loser can see
// that it did. So the admission must be durable even on the path that then
// refuses — and must be withdrawn again before the refusal returns, or a node
// that correctly declined to start would block every later operation for ever.
func TestANormalNodeWritesItsAdmissionBeforeItReadsTheOperation(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	acks, _ := fleet.MaintenanceAcks(ctx, "CREWLET_TRACKER_LOG")
	if len(acks) != 0 {
		t.Fatalf("a publishing node acknowledged a capacity operation, which "+
			"is the one claim its mode cannot make — its process is exactly "+
			"the one that may hold an outstanding request: %+v", acks)
	}
}

// nodeOn is one process of the node id on a shared fleet store, as a boot of
// it in mode, holding profile, would be: process names its incarnation, which
// every process mints afresh.
func nodeOn(fleet coord.Fleet, id, process string, mode statelog.MaintenanceMode,
	profile placement.NodeProfile) *Engine {

	profile.ID = id
	return &Engine{
		id: id, incarnation: id + ":" + process, mode: mode, profile: profile,
		backends: &Backends{Fleet: fleet},
	}
}

// dataNode and seatsNode are the two profiles a capacity window tells apart:
// one that holds the estate, and so admits itself to publish, and one that
// does not.
var (
	dataNode  = placement.NodeProfile{Roles: placement.Roles(placement.RoleData, placement.RoleSeats)}
	seatsNode = placement.NodeProfile{Roles: placement.Roles(placement.RoleSeats)}
)

// THE ADMISSION A CRASHED PROCESS LEFT IS GONE BY THE BOOT THAT ENTERS A
// WINDOW, and nothing else is touched.
//
// An admission says one process may be publishing. A crashed process never ran
// the orderly stop that withdraws it, and the key has no age, so a node
// restarted into a maintenance mode (which admits nothing) left the dead
// process's claim standing for good. Each case boots a new incarnation of the
// node while nobody holds the node's presence (no lease store here), which is
// the evidence that the old one is gone: into a maintenance mode it retires
// the claim; in service a data node's own
// admission replaces it, as it always did, and a node without data leaves it
// to the window's first restart, which then retires it. That last row is
// pinned on purpose: a boot in service makes no coordination read for a key
// that is almost never there, so the bounds a deployment sizes a boot by
// count no such step.
//
// Mutation: make supersede return at once and the two maintenance cases, and
// every case's window restart, fail.
func TestABootRetiresTheAdmissionItsNodesCrashedProcessLeft(t *testing.T) {
	t.Parallel()
	streams := []string{"CREWLET_TRACKER_LOG"}
	for _, tc := range []struct {
		name    string
		mode    statelog.MaintenanceMode
		profile placement.NodeProfile
		// holder is the process whose admission node-a holds once the
		// restart has booted: "" for none.
		holder string
	}{
		{"restarted into maintenance mode", statelog.ModeMaintenance, dataNode, ""},
		{"restarted into seal mode", statelog.ModeSeal, dataNode, ""},
		{"restarted in normal mode with data", statelog.ModeNormal, dataNode, "restarted"},
		{"restarted in normal mode without data", statelog.ModeNormal, seatsNode, "crashed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fleet := coordmem.NewFleet()
			crashed := nodeOn(fleet, "node-a", "crashed", statelog.ModeNormal, dataNode)
			if err := crashed.handshake(ctx, streams); err != nil {
				t.Fatalf("the first boot was refused: %v", err)
			}
			// KILLED: nothing withdraws its admission. And a live peer's
			// stands beside it, which no other node's boot may touch.
			if err := fleet.PutAdmission(ctx, coord.Admission{
				NodeID: "node-b", Incarnation: "node-b:live",
			}); err != nil {
				t.Fatal(err)
			}

			restarted := nodeOn(fleet, "node-a", "restarted", tc.mode, tc.profile)
			if err := restarted.handshake(ctx, streams); err != nil {
				t.Fatalf("the restart was refused: %v", err)
			}
			admissions, err := fleet.Admissions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held := map[string]string{}
			for _, a := range admissions {
				held[a.NodeID] = a.Incarnation
			}
			want := ""
			if tc.holder != "" {
				want = "node-a:" + tc.holder
			}
			if held["node-a"] != want {
				t.Errorf("node-a holds an admission as %q after the restart, want %q: "+
					"a crashed process's claim left by a maintenance boot is a "+
					"publisher a window sees that no longer exists", held["node-a"], want)
			}
			if held["node-b"] != "node-b:live" {
				t.Errorf("node-a's boot withdrew node-b's admission, a claim about a "+
					"process it knows nothing of (%+v)", admissions)
			}

			// THE WINDOW'S FIRST RESTART, into maintenance mode, leaves
			// node-a holding nothing whatever the boot before it left.
			window := nodeOn(fleet, "node-a", "window", statelog.ModeMaintenance, tc.profile)
			if err := window.handshake(ctx, streams); err != nil {
				t.Fatalf("the window's restart was refused: %v", err)
			}
			if held, found, err := heldAdmission(ctx, fleet, "node-a"); err != nil || found {
				t.Errorf("node-a still holds %+v after restarting into maintenance "+
					"mode (err %v)", held, err)
			}
			if held, _, _ := heldAdmission(ctx, fleet, "node-b"); held.Incarnation != "node-b:live" {
				t.Errorf("node-a's boot withdrew node-b's admission (%+v)", held)
			}
		})
	}
}

// A BOOT THAT CANNOT READ THE ADMISSIONS STILL BOOTS INTO A MAINTENANCE MODE,
// AND SAYS WHAT IT LEFT.
//
// The node publishes nothing there, so refusing its boot over a key it could
// not clear would cost the window the participant it needs to hear from: it
// still acknowledges the window. The key does what it always did, standing in
// the window's way by name, and the boot's log says so.
//
// Process-wide logging, so not parallel. Mutation: return from supersede before
// its warning, or drop the acknowledgement from the handshake, and this fails.
func TestAnUnreadableAdmissionDoesNotRefuseAMaintenanceBoot(t *testing.T) {
	logs := captureEngineLogs(t)
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	window := coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "capacity:open",
		TargetMaxBytes: 1 << 33, Phase: coord.PhaseOpened, Attempt: 1,
		Participants: []string{"node-a"},
	}
	if _, _, err := fleet.OpenMaintenance(ctx, window); err != nil {
		t.Fatal(err)
	}
	blind := &blindFleet{fleetStore: fleet, admissionsErr: errors.New("store unreachable")}
	e := nodeOn(blind, "node-a", "restarted", statelog.ModeMaintenance, dataNode)
	if err := e.handshake(ctx, []string{"CREWLET_TRACKER_LOG"}); err != nil {
		t.Fatalf("a maintenance-mode boot was refused over an admission it could "+
			"not read: %v", err)
	}
	acks, err := fleet.MaintenanceAcks(ctx, "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 1 || acks[0].Incarnation != "node-a:restarted" ||
		acks[0].OperationID != window.OperationID {
		t.Errorf("acknowledgements %+v: want node-a:restarted's, for the open "+
			"window, whatever the admission read did", acks)
	}
	if lines := logs.about("admission_not_superseded", "node", "node-a"); len(lines) != 1 {
		t.Errorf("admission_not_superseded = %v: a boot that left a claim it "+
			"could not read has to say so", lines)
	}
}

// A MAINTENANCE BOOT WITHDRAWS ANOTHER PROCESS'S ADMISSION ONLY WHEN THAT
// PROCESS IS NOT RUNNING.
//
// A new incarnation of a node is not on its own proof that the one the
// admission names is gone: a second process under a live node's id boots
// exactly like a restart, and withdrawing on its word baselined a window past
// a publisher that was still running. So the boot reads who holds the node's
// presence, which a running process renews in every mode. Held by the
// process the admission names, the claim stays; lapsed, or held by another
// process of the node, it goes; and a presence nobody can read leaves it,
// since nobody can say its process is gone.
//
// Mutation: drop the presence check from supersede and the first row fails;
// treat an unreadable presence as nobody's and the last row fails.
func TestAMaintenanceBootLeavesTheAdmissionOfAProcessStillRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// present is the process holding node-a's presence lease: "" for
		// none.
		present string
		blind   bool
		// stays is whether node-a:live's admission survives the boot.
		stays bool
	}{
		{name: "the process it names holds the presence", present: "node-a:live", stays: true},
		{name: "nobody holds the presence", present: "", stays: false},
		{name: "another process holds the presence", present: "node-a:other", stays: false},
		{name: "the presence can't be read", present: "node-a:live", blind: true, stays: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fleet := coordmem.NewFleet()
			backend := coordmem.New()
			live := nodeOn(fleet, "node-a", "live", statelog.ModeNormal, dataNode)
			if err := live.handshake(ctx, []string{"CREWLET_TRACKER_LOG"}); err != nil {
				t.Fatal(err)
			}
			if tc.present != "" {
				if _, _, err := backend.TryAcquire(ctx, coord.NodeResource("node-a"),
					coord.AcquireOptions{Owner: tc.present, TTL: time.Minute}); err != nil {
					t.Fatal(err)
				}
			}
			twin := nodeOn(fleet, "node-a", "twin", statelog.ModeMaintenance, dataNode)
			twin.backends.Coord = backend
			if tc.blind {
				twin.backends.Coord = blindPresence{Backend: backend}
			}
			if err := twin.handshake(ctx, []string{"CREWLET_TRACKER_LOG"}); err != nil {
				t.Fatalf("the maintenance boot was refused: %v", err)
			}
			held, found, err := heldAdmission(ctx, fleet, "node-a")
			if err != nil {
				t.Fatal(err)
			}
			if found != tc.stays || (found && held.Incarnation != "node-a:live") {
				t.Errorf("node-a holds %+v (found %v) after the boot, want it held "+
					"by node-a:live: %v", held, found, tc.stays)
			}
		})
	}
}

// A CRASHED NODE RESTARTED INTO MAINTENANCE MODE NO LONGER HOLDS THE WINDOW
// SHUT, which is the restart the refusal tells the operator to make.
//
// node-a crashes in normal mode, holding its admission. While that process
// might still be running, its admission stops the window sending anything:
// that is what the key is evidence of. Restarted into maintenance mode, as the
// refusal says, it used to leave the dead process's claim behind, so the
// window stayed shut and its refusal sent the operator to make the same
// restart again, for ever.
func TestACrashedNodeRestartedIntoMaintenanceLetsTheWindowProceed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	streams := []string{"CREWLET_TRACKER_LOG"}

	crashed := nodeOn(fleet, "node-a", "crashed", statelog.ModeNormal, dataNode)
	if err := crashed.handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	coordinator := nodeOn(fleet, "node-b", "boot-1", statelog.ModeMaintenance, dataNode)
	if err := coordinator.handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	op := openWindow(t, coordinator)
	if _, err := coordinator.advanceCapacity(ctx, nil, op); err == nil ||
		!strings.Contains(err.Error(), "node-a") {
		t.Fatalf("the window went past opened while node-a's process might still "+
			"be publishing (err %v)", err)
	}

	restarted := nodeOn(fleet, "node-a", "restarted", statelog.ModeMaintenance, dataNode)
	if err := restarted.handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	next, err := coordinator.advanceCapacity(ctx, nil, op)
	if err != nil {
		t.Fatalf("node-a restarted into maintenance mode as it was told to, and "+
			"the window is still refused: %v", err)
	}
	if next.Phase != coord.PhaseBaselined {
		t.Errorf("the window stands at %s, want baselined", next.Phase)
	}
}

// openWindow opens a capacity window on the tracker's log from e, as `crewlet
// retention set-capacity` does, and fails the test if it does not open.
func openWindow(t *testing.T, e *Engine) coord.MaintenanceOperation {
	t.Helper()
	op, err := e.openCapacity(context.Background(), CapacityRequest{
		Stream: "CREWLET_TRACKER_LOG", TargetMaxBytes: 1 << 33, By: "ops-3",
	}, jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom)
	if err != nil {
		t.Fatalf("the window did not open: %v", err)
	}
	if op.Phase != coord.PhaseOpened {
		t.Fatalf("the window opened in phase %s, want opened", op.Phase)
	}
	return op
}

// TestAnAcknowledgementCarriesTheModeItWasMadeIn.
//
// The seal is established only from SEAL mode, because that mode cannot write
// configuration — so the process making the claim cannot be the one holding
// the request it is retiring. An ack that did not carry its mode would let a
// maintenance-mode node's restart satisfy the barrier.
func TestAnAcknowledgementCarriesTheModeItWasMadeIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeSeal)

	// NO OPERATION IS NOT A FAILURE — a fleet restarted into maintenance
	// mode before the verb ran is the ordinary first step — and the boot
	// still records which process it is, in which mode, bound to nothing.
	e.acknowledge(ctx, []string{"CREWLET_TRACKER_LOG"})
	acks, err := fleet.MaintenanceAcks(ctx, "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 1 || !acks[0].Windowless() || acks[0].Mode != string(statelog.ModeSeal) ||
		acks[0].Incarnation != e.incarnation {
		t.Fatalf("a boot with no window open recorded %+v, want one windowless "+
			"acknowledgement of this process in seal mode", acks)
	}

	if _, _, err := fleet.OpenMaintenance(ctx, coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1",
		TargetMaxBytes: 1 << 33, Phase: coord.PhaseBaselined, Attempt: 2,
		Participants: []string{"node-1"}, EnteredAt: time.Now().UTC(), By: "ops-3",
	}); err != nil {
		t.Fatal(err)
	}
	e.acknowledge(ctx, []string{"CREWLET_TRACKER_LOG"})
	acks, err = fleet.MaintenanceAcks(ctx, "CREWLET_TRACKER_LOG")
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

// A BOOT THAT COULD NOT READ A STREAM RECORDS NO WINDOWLESS RESTART: a window
// may be open there, and an acknowledgement saying none was is a claim this
// boot cannot make. It records nothing, and the window's baseline names it.
func TestABootThatCouldNotReadAWindowRecordsNoWindowlessRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	e := nodeOn(&blindFleet{fleetStore: fleet, readErr: errors.New("store unreachable")},
		"node-1", "boot-1", statelog.ModeMaintenance, dataNode)
	e.acknowledge(ctx, []string{"CREWLET_TRACKER_LOG"})
	if acks, _ := fleet.MaintenanceAcks(ctx, "CREWLET_TRACKER_LOG"); len(acks) != 0 {
		t.Fatalf("a boot that could not read the window recorded %+v", acks)
	}
}

// A LOG WITH NO WINDOW OPEN IS RECORDED AS SUCH WHATEVER ANOTHER LOG HOLDS.
//
// A boot reads each log's window on its own, and the restart it records for a
// log with no window open is what that log's next window baselines from. It
// used to be one record per node, written only when no log had a window, so a
// fleet that restarted while the tracker's window was open wrote nothing for
// the pages log, and a window opened there next found no restart recorded for
// any node but its coordinator: one more fleet-wide restart, for nothing.
//
// Mutation: withhold a log's windowless record whenever another log has a
// window open and the pages window's baseline refuses node-b.
func TestARestartBesideAnOpenWindowRecordsTheOtherLogs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	streams := []string{"CREWLET_TRACKER_LOG", "CREWLET_PAGES_LOG"}
	for _, id := range []string{"node-a", "node-b"} {
		if err := fleet.PutPositions(ctx, coord.NodePositions{
			NodeID: id, At: time.Now().UTC(), Domains: map[string]coord.DomainPosition{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// THE TRACKER'S WINDOW IS OPEN BEFORE THE FLEET RESTARTS, as it is when
	// the verb runs from the first node up.
	openWindow(t, nodeOn(fleet, "node-a", "first", statelog.ModeMaintenance, dataNode))

	booted := map[string]*Engine{}
	for _, id := range []string{"node-a", "node-b"} {
		booted[id] = nodeOn(fleet, id, "maintenance", statelog.ModeMaintenance, dataNode)
		if err := booted[id].handshake(ctx, streams); err != nil {
			t.Fatal(err)
		}
	}
	op, err := booted["node-a"].openCapacity(ctx, CapacityRequest{
		Stream: "CREWLET_PAGES_LOG", TargetMaxBytes: 1 << 33, By: "ops-3",
	}, jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom)
	if err != nil {
		t.Fatalf("the pages window did not open: %v", err)
	}
	applied, err := booted["node-a"].driveCapacity(ctx, &ceilingLog{max: 1 << 32}, op)
	if err != nil {
		t.Fatalf("the pages window's baseline found no restart recorded beside "+
			"the tracker's open window: %v", err)
	}
	if applied.Phase != coord.PhaseApplied {
		t.Fatalf("the pages window stands at %s, want applied", applied.Phase)
	}
	if got := applied.WriteIncarnations["node-b"]; got != booted["node-b"].incarnation {
		t.Errorf("node-b is recorded as %q, want the process it restarted as (%s)",
			got, booted["node-b"].incarnation)
	}
}

// A LOG THE BOOT COULDN'T READ COSTS THAT LOG ITS RECORD AND NO OTHER: the
// window that may be open there is one this boot can say nothing about, and
// every log it did read still records the restart, bound or windowless.
//
// Mutation: withhold every windowless record once any log was unreadable and
// the pages log records nothing.
func TestAnUnreadableLogCostsOnlyItsOwnRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	e := nodeOn(&blindFleet{fleetStore: fleet, readErr: errors.New("store unreachable"),
		readErrOn: "CREWLET_TRACKER_LOG"}, "node-1", "boot-1", statelog.ModeMaintenance, dataNode)
	e.acknowledge(ctx, []string{"CREWLET_TRACKER_LOG", "CREWLET_PAGES_LOG"})
	if acks, _ := fleet.MaintenanceAcks(ctx, "CREWLET_TRACKER_LOG"); len(acks) != 0 {
		t.Errorf("a log the boot couldn't read was recorded: %+v", acks)
	}
	acks, err := fleet.MaintenanceAcks(ctx, "CREWLET_PAGES_LOG")
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 1 || !acks[0].Windowless() || acks[0].Incarnation != e.incarnation {
		t.Fatalf("the log the boot did read recorded %+v, want this process's "+
			"windowless restart", acks)
	}
}

// THE CAPACITY PROCEDURE RUNS IN THE RESTARTS IT NAMES, on a fleet with a node
// that crashed in service.
//
// This is the whole of what went wrong, end to end. node-a crashed holding its
// admission; every node is restarted into maintenance mode, as the procedure's
// first step says, before anybody runs the verb; the verb runs from node-b.
// The window used to refuse to open over node-a's admission and tell the
// operator to restart node-a into the mode it was in. With that fixed, the
// baseline found no acknowledgement from anyone but the coordinator, since a
// boot with no window open recorded nothing, and sent the operator to restart
// the whole fleet again. Now the verb baselines and applies in the one call,
// and after the one seal restart the verb run again seals, verifies and
// confirms the window, which closes.
//
// Mutation: drop the windowless acknowledgement and the baseline refuses
// node-a and node-c; drop the boot's admission retirement and it refuses
// node-a's admission.
func TestTheCapacityProcedureRunsInTheRestartsItNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	streams := []string{"CREWLET_TRACKER_LOG"}
	nodes := []string{"node-a", "node-b", "node-c"}

	// IN SERVICE: three data nodes, each admitted and each with a position.
	for _, id := range nodes {
		e := nodeOn(fleet, id, "service", statelog.ModeNormal, dataNode)
		if err := e.handshake(ctx, streams); err != nil {
			t.Fatal(err)
		}
		if err := fleet.PutPositions(ctx, coord.NodePositions{
			NodeID: id, At: time.Now().UTC(), Domains: map[string]coord.DomainPosition{},
		}); err != nil {
			t.Fatal(err)
		}
		// node-a CRASHES; the other two stop cleanly and give theirs back.
		if id != "node-a" {
			if err := e.withdraw(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	// RESTART 1: every node into maintenance mode, before the verb.
	maintenance := map[string]*Engine{}
	for _, id := range nodes {
		maintenance[id] = nodeOn(fleet, id, "maintenance", statelog.ModeMaintenance, dataNode)
		if err := maintenance[id].handshake(ctx, streams); err != nil {
			t.Fatal(err)
		}
	}

	// THE VERB, from node-b: it opens the window, baselines it and applies
	// the target.
	resized := &ceilingLog{max: 1 << 32}
	op := openWindow(t, maintenance["node-b"])
	applied, err := maintenance["node-b"].driveCapacity(ctx, resized, op)
	if err != nil {
		t.Fatalf("the first run of the verb did not apply the target: %v", err)
	}
	if applied.Phase != coord.PhaseApplied {
		t.Fatalf("the window stands at %s, want applied", applied.Phase)
	}
	for _, id := range nodes {
		if got := applied.WriteIncarnations[id]; got != maintenance[id].incarnation {
			t.Errorf("%s is baselined as %q, want the process it restarted as (%s)",
				id, got, maintenance[id].incarnation)
		}
	}

	// THE WINDOWLESS RECORDS ARE NOT A SEAL, before the seal restart.
	acks, err := fleet.MaintenanceAcks(ctx, "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatal(err)
	}
	if held, _ := statelog.SealHolds(applied, acks); held {
		t.Fatal("the seal held on the maintenance restarts that preceded the apply")
	}

	// RESTART 2: every node into seal mode, and the verb run again seals,
	// verifies and confirms the window, which closes.
	seal := map[string]*Engine{}
	for _, id := range nodes {
		seal[id] = nodeOn(fleet, id, "seal", statelog.ModeSeal, dataNode)
		if err := seal[id].handshake(ctx, streams); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := seal["node-a"].driveCapacity(ctx, resized, applied); err != nil {
		t.Fatalf("the verb run from seal mode: %v", err)
	}
	if held, found, err := fleet.Maintenance(ctx, op.Stream); err != nil || found {
		t.Fatalf("the window is still open after every node restarted into "+
			"seal mode (found=%t, err=%v): %+v", found, err, held)
	}
	if resized.sets != 1 {
		t.Errorf("the procedure sent %d configuration requests, want the one", resized.sets)
	}
}

// A SEAL STILL WAITING ON A NODE IS AN ANSWER, NOT A FAULT.
//
// Running the verb from seal mode before every node has restarted into it is
// the ordinary way to ask what the seal still waits on. The seal wrote the
// window back on every pass whether or not anything had moved, so the drive
// never saw it settle and answered that the operation "didn't settle in 7
// transitions, more than the table has phases": an invariant breach, which the
// route reports as a fault, for a window doing exactly what it should. A pass
// that moves nothing writes nothing.
//
// Mutation: make the seal write the window whatever it found and the first run
// from seal mode fails with the breach.
func TestASealStillWaitingOnANodeIsAnAnswerNotAFault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	streams := []string{"CREWLET_TRACKER_LOG"}
	for _, id := range []string{"node-a", "node-b"} {
		e := nodeOn(fleet, id, "maintenance", statelog.ModeMaintenance, dataNode)
		if err := e.handshake(ctx, streams); err != nil {
			t.Fatal(err)
		}
		if err := fleet.PutPositions(ctx, coord.NodePositions{
			NodeID: id, At: time.Now().UTC(), Domains: map[string]coord.DomainPosition{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := nodeOn(fleet, "node-a", "maintenance", statelog.ModeMaintenance, dataNode)
	resized := &ceilingLog{max: 1 << 32}
	applied, err := coordinator.driveCapacity(ctx, resized, openWindow(t, coordinator))
	if err != nil || applied.Phase != coord.PhaseApplied {
		t.Fatalf("the verb from maintenance mode left %s (err %v)", applied.Phase, err)
	}

	// ONLY node-a restarts into seal mode.
	sealing := nodeOn(fleet, "node-a", "seal", statelog.ModeSeal, dataNode)
	if err := sealing.handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	pending, err := sealing.driveCapacity(ctx, resized, applied)
	if err != nil {
		t.Fatalf("a seal waiting on node-b was reported as a fault: %v", err)
	}
	if pending.Phase != coord.PhaseSealing {
		t.Fatalf("the window stands at %s, want sealing", pending.Phase)
	}
	again, err := sealing.driveCapacity(ctx, resized, pending)
	if err != nil {
		t.Fatalf("asking again: %v", err)
	}
	if again.Revision != pending.Revision {
		t.Errorf("asking again rewrote a window nothing had moved (revision %d "+
			"to %d)", pending.Revision, again.Revision)
	}

	// AND node-b's restart is what moves it.
	if err := nodeOn(fleet, "node-b", "seal", statelog.ModeSeal, dataNode).
		handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	if _, err := sealing.driveCapacity(ctx, resized, again); err != nil {
		t.Fatalf("the verb once node-b restarted: %v", err)
	}
	if _, found, err := fleet.Maintenance(ctx, applied.Stream); err != nil || found {
		t.Fatalf("the window is still open once every node restarted into seal "+
			"mode (found=%t, err=%v)", found, err)
	}
}

// WINDOWS ON TWO LOGS RUN IN THE SAME RESTARTS, and each seals on its own
// acknowledgements.
//
// A fleet resizing two logs restarts once into maintenance mode, runs the
// verb for each, restarts once into seal mode and runs the verb for each
// again. Every boot acknowledges each log's window. The acknowledgements were
// kept one per node, so the boot's second write replaced its first, and the
// window whose acknowledgement went first read "acknowledged another
// operation" from every node for as long as the other was open: no restart
// could seal it.
//
// Mutation: key an acknowledgement by its node alone and the tracker's window
// never closes.
func TestWindowsOnTwoLogsSealInTheSameRestarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	streams := []string{"CREWLET_TRACKER_LOG", "CREWLET_PAGES_LOG"}
	nodes := []string{"node-a", "node-b"}

	maintenance := map[string]*Engine{}
	for _, id := range nodes {
		maintenance[id] = nodeOn(fleet, id, "maintenance", statelog.ModeMaintenance, dataNode)
		if err := maintenance[id].handshake(ctx, streams); err != nil {
			t.Fatal(err)
		}
		if err := fleet.PutPositions(ctx, coord.NodePositions{
			NodeID: id, At: time.Now().UTC(), Domains: map[string]coord.DomainPosition{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	logs := map[string]*ceilingLog{}
	applied := map[string]coord.MaintenanceOperation{}
	for _, stream := range streams {
		logs[stream] = &ceilingLog{max: 1 << 32}
		op, err := maintenance["node-a"].openCapacity(ctx, CapacityRequest{
			Stream: stream, TargetMaxBytes: 1 << 33, By: "ops-3",
		}, jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom)
		if err != nil {
			t.Fatalf("the window on %s did not open: %v", stream, err)
		}
		if applied[stream], err = maintenance["node-a"].driveCapacity(ctx, logs[stream], op); err != nil {
			t.Fatalf("the verb on %s: %v", stream, err)
		}
		if applied[stream].Phase != coord.PhaseApplied {
			t.Fatalf("the window on %s stands at %s, want applied", stream,
				applied[stream].Phase)
		}
	}

	seal := map[string]*Engine{}
	for _, id := range nodes {
		seal[id] = nodeOn(fleet, id, "seal", statelog.ModeSeal, dataNode)
		if err := seal[id].handshake(ctx, streams); err != nil {
			t.Fatal(err)
		}
	}
	for _, stream := range streams {
		if _, err := seal["node-b"].driveCapacity(ctx, logs[stream], applied[stream]); err != nil {
			t.Fatalf("the verb on %s from seal mode: %v", stream, err)
		}
		if held, found, err := fleet.Maintenance(ctx, stream); err != nil || found {
			t.Errorf("the window on %s is still open at %s after every node "+
				"restarted into seal mode (found=%t, err=%v)", stream, held.Phase,
				found, err)
		}
	}
}

// ceilingLog is a log a capacity window drives end to end with no broker: its
// ceiling is what the last request set, and onSet, when given, runs as each
// request leaves and may refuse it.
type ceilingLog struct {
	mu    sync.Mutex
	max   uint64
	sets  int
	onSet func(maxBytes uint64) error
}

func (l *ceilingLog) SetMaxBytes(_ context.Context, maxBytes uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sets++
	if l.onSet != nil {
		if err := l.onSet(maxBytes); err != nil {
			return err
		}
	}
	l.max = maxBytes
	return nil
}

func (l *ceilingLog) Stats(context.Context) (jetstream.LogStats, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return jetstream.LogStats{MaxBytes: l.max}, nil
}

// TestTheBarrierRefusesEveryWayAnAcknowledgementCanBeTheWrongOne.
//
// One table over the four ways an ack fails to be evidence, because each of
// them looks like a satisfied barrier from a distance and each would confirm
// an operation whose request may still be outstanding.
func TestTheBarrierRefusesEveryWayAnAcknowledgementCanBeTheWrongOne(t *testing.T) {
	t.Parallel()
	op := coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1", Attempt: 2,
		Participants:      []string{"node-1", "node-2"},
		WriteIncarnations: map[string]string{"node-1": "node-1:old", "node-2": "node-2:old"},
	}
	good := func(node string) coord.MaintenanceAck {
		return coord.MaintenanceAck{
			Stream: "CREWLET_TRACKER_LOG", NodeID: node, OperationID: "op-1", Attempt: 2,
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
		{"one acknowledged another log's window",
			bend("node-1", func(a *coord.MaintenanceAck) { a.Stream = "CREWLET_PAGES_LOG" }),
			false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			incarnations, missing := statelog.SealRestarts(op, tc.acks)
			if restarted := len(missing) == 0; restarted != tc.want {
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
	t.Parallel()
	op := coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1", Attempt: 1,
		Participants:      []string{"node-1", "node-2"},
		WriteIncarnations: map[string]string{"node-1": "node-1:old", "node-2": "node-2:old"},
	}
	acks := []coord.MaintenanceAck{{
		Stream: "CREWLET_TRACKER_LOG", NodeID: "node-1", OperationID: "op-1", Attempt: 1,
		Incarnation: "node-1:new", Mode: string(statelog.ModeSeal),
	}}
	if _, missing := statelog.SealRestarts(op, acks); len(missing) == 0 {
		t.Fatal("the barrier held with a participant that never came back")
	}
	op.Excluded = []string{"node-2"}
	incarnations, missing := statelog.SealRestarts(op, acks)
	if len(missing) > 0 {
		t.Fatal("an operator's exclusion did not waive the acknowledgement")
	}
	if _, named := incarnations["node-2"]; named {
		t.Error("the excluded node was given an incarnation it never wrote")
	}
}

// WHO EXCLUDED A NODE AND WHO ABANDONED THE WINDOW ARE ON THE WINDOW.
//
// An exclusion is the one fact the seal takes on somebody's word, and an
// abandonment the decision the fleet goes on carrying out until the seal
// restart; both used to be said only in the serving node's log, while the
// window named the person who OPENED it — so the one record every node reads
// showed its opener as the author of whatever it had become. Mutation: record
// either gesture without its party and its half goes red; check the
// abandonment's confirmation after the update, or not at all, and the refused
// abandonment changes the window.
func TestWhoExcludedANodeAndWhoAbandonedTheWindowAreOnTheWindow(t *testing.T) {
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	if _, _, err := fleet.OpenMaintenance(ctx, coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1",
		TargetMaxBytes: 1 << 33, OriginalMaxBytes: 1 << 32,
		Phase: coord.PhaseApplied, Attempt: 1,
		Participants: []string{"node-1", "node-2"}, EnteredAt: time.Now().UTC(),
		By: "ops-3",
	}); err != nil {
		t.Fatal(err)
	}
	sre := iam.Actor{Name: "dana.sre", Kind: iam.ActorOperator,
		OperatorID: "session:0192f00d-0000-7000-8000-0000000000dd"}
	lead := iam.Actor{Name: "ana.admin", Kind: iam.ActorOperator,
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}

	op, err := e.ExcludeParticipant(ctx, "CREWLET_TRACKER_LOG", "node-2", sre)
	if err != nil {
		t.Fatalf("exclude: %v", err)
	}
	want := coord.MaintenanceParty{By: sre.Name, OperatorID: sre.OperatorID}
	if got := op.ExcludedBy["node-2"]; got != want {
		t.Errorf("node-2's exclusion is recorded as %+v, want %+v", got, want)
	}

	// A CONFIRMATION NAMING ANOTHER OPERATION ABANDONS NOTHING, and says
	// which operation is open.
	if held, err := e.AbandonCapacity(ctx, "CREWLET_TRACKER_LOG", "op-0", lead); err == nil {
		t.Fatalf("an abandonment confirming op-0 ran against op-1: %+v", held)
	} else if mismatch := (*ConfirmMismatchError)(nil); !errors.As(err, &mismatch) ||
		mismatch.Open != "op-1" {
		t.Fatalf("a confirmation naming another operation was refused as %v, want "+
			"the mismatch naming op-1", err)
	}
	if held, _, _ := fleet.Maintenance(ctx, "CREWLET_TRACKER_LOG"); held.Phase != coord.PhaseApplied ||
		held.AbandonedBy.By != "" {
		t.Fatalf("a refused abandonment changed the window: %+v", held)
	}

	op, err = e.AbandonCapacity(ctx, "CREWLET_TRACKER_LOG", "op-1", lead)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if op.Phase != coord.PhaseSealing {
		t.Fatalf("an applied window abandoned into %s, want sealing — the "+
			"record would not be there to carry who abandoned it", op.Phase)
	}
	if want := (coord.MaintenanceParty{By: lead.Name, OperatorID: lead.OperatorID}); op.AbandonedBy != want {
		t.Errorf("the abandonment is recorded as %+v, want %+v", op.AbandonedBy, want)
	}
	// AND THE EXCLUSION SURVIVES IT: a later gesture on the window keeps
	// whose word waived node-2's acknowledgement.
	if got := op.ExcludedBy["node-2"]; got.By != sre.Name {
		t.Errorf("the abandon lost the exclusion's author: %+v", op.ExcludedBy)
	}

	// AND THE RETENTION REPORT SAYS IT, which is the banner `crewlet
	// retention status` leads with while the window holds the fleet.
	r := &retention{fleet: fleet, state: runsLogs(&stateLog{},
		&runningLog{domain: worklet.Domain{}, spec: worklet.Domain{}.Stream()})}
	row := r.openMaintenance(ctx)
	if row == nil {
		t.Fatal("the report shows no window while one is open")
	}
	if row.AbandonedBy != op.AbandonedBy {
		t.Errorf("the report names %+v as having abandoned the window, want %+v",
			row.AbandonedBy, op.AbandonedBy)
	}
}

// THE REPORT AT OPENED SAYS WHAT THE BASELINE WAITS ON.
//
// `crewlet retention status` and the dashboard's banner say who a window is
// waiting on. At opened they said every participant, because they counted
// acknowledgements bound to this window and no node writes one before the
// verb runs, and they never named the admission actually holding it. So at
// opened the report names the participants with no restart recorded and the
// admissions; past it, the seal's acknowledgements, and no admission, since
// no step reads one there.
//
// Mutation: count bound acknowledgements at opened, or drop the admissions,
// and the opened half fails; keep the admissions past opened and the
// baselined half does.
func TestTheReportAtOpenedSaysWhatTheBaselineWaitsOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	window := coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "capacity:now",
		TargetMaxBytes: 1 << 33, Phase: coord.PhaseOpened, Attempt: 1,
		Participants: []string{"node-1", "node-2", "node-3"},
		EnteredAt:    time.Now().UTC(),
	}
	op, _, err := fleet.OpenMaintenance(ctx, window)
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.PutMaintenanceAck(ctx, coord.MaintenanceAck{
		Stream: "CREWLET_TRACKER_LOG", NodeID: "node-1", OperationID: "capacity:before", Attempt: 2,
		Incarnation: "node-1:2", Mode: string(statelog.ModeMaintenance),
	}); err != nil {
		t.Fatal(err)
	}
	if err := fleet.PutAdmission(ctx, coord.Admission{
		NodeID: "node-3", Incarnation: "node-3:1",
	}); err != nil {
		t.Fatal(err)
	}
	r := &retention{fleet: fleet, state: runsLogs(&stateLog{},
		&runningLog{domain: worklet.Domain{}, spec: worklet.Domain{}.Stream()})}

	row := r.openMaintenance(ctx)
	if row == nil {
		t.Fatal("the report shows no window while one is open")
	}
	if !slices.Equal(row.ParticipantsMissing, []string{"node-2", "node-3"}) {
		t.Errorf("at opened the report waits on %v, want node-2 and node-3, the "+
			"nodes with no restart recorded", row.ParticipantsMissing)
	}
	if !slices.Equal(row.AdmissionsBlocking, []string{"node-3"}) {
		t.Errorf("at opened the report names admissions %v, want node-3's",
			row.AdmissionsBlocking)
	}

	op.Phase = coord.PhaseBaselined
	op.WriteIncarnations = map[string]string{"node-1": "node-1:2",
		"node-2": "node-2:1", "node-3": "node-3:1"}
	if err := fleet.UpdateMaintenance(ctx, op); err != nil {
		t.Fatal(err)
	}
	row = r.openMaintenance(ctx)
	if !slices.Equal(row.ParticipantsMissing, []string{"node-1", "node-2", "node-3"}) {
		t.Errorf("past opened the report waits on %v, want every node the seal "+
			"has no acknowledgement from", row.ParticipantsMissing)
	}
	if len(row.AdmissionsBlocking) != 0 {
		t.Errorf("past opened the report names admissions %v, which no step reads",
			row.AdmissionsBlocking)
	}
}

// toldRefusal is err's told sentence ([statelog.Told]) as an error, failing the
// case where err tells its operator nothing or carries the package it was
// written in: the retention routes answer the sentence alone, so a refusal an
// operator acts on that isn't told is answered as a fault with its remedy in
// this node's log, and an `engine:` in it is a package a person reads.
func toldRefusal(t *testing.T, err error) error {
	t.Helper()
	said, told := statelog.Tell(err)
	if !told {
		t.Fatalf("%v tells its operator nothing, so the route answers it as a fault", err)
	}
	if strings.Contains(said, "engine:") || strings.Contains(said, "statelog:") {
		t.Errorf("it tells %q, carrying the package it was written in", said)
	}
	return errors.New(said)
}

// TestABaselineOfNothingIsRefusedRatherThanRecorded.
//
// An empty baseline compares unequal to every later acknowledgement, so a
// participant baselined at one satisfies the seal without ever restarting.
func TestABaselineOfNothingIsRefusedRatherThanRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	op := coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1", Attempt: 1,
		Participants: []string{"node-1", "node-2"},
	}

	_, _, refusal := e.capacityIncarnations(ctx, op)
	if refusal == nil {
		t.Fatal("a participant that never restarted was baselined at nothing, " +
			"which the seal then satisfies without it ever coming back")
	}
	err := toldRefusal(t, refusal)
	if !strings.Contains(err.Error(), "node-2") {
		t.Errorf("the refusal does not name who has not restarted: %v", err)
	}
	// THE COMMAND IT NAMES IS ONE THE CLI RUNS, for this window's stream
	// and this node, and the sentence reads right for one node: it named a
	// `--exclude` flag nothing parses, and said `"node-2" have not
	// restarted`. And it says no restart is RECORDED, since a node that did
	// restart and could not write its acknowledgement is the same refusal.
	// Mutation: put any of them back and this fails.
	for _, want := range []string{
		"no restart into a maintenance mode is recorded for \"node-2\"",
		"`crewlet retention maintenance exclude -stream CREWLET_TRACKER_LOG " +
			"-node node-2 -confirm node-2`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "--exclude") || strings.Contains(err.Error(), "these nodes") {
		t.Errorf("the refusal names a flag the CLI lacks or reads wrong for one node: %v", err)
	}

	// EITHER an acknowledgement or an exclusion resolves it — and nothing
	// else does.
	if err := fleet.PutMaintenanceAck(ctx, coord.MaintenanceAck{
		Stream: "CREWLET_TRACKER_LOG", NodeID: "node-2", OperationID: "op-1", Attempt: 1,
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
	t.Parallel()
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
	if _, _, err := backend.TryAcquire(ctx, coord.NodeResource("node-holding-a-lease"),
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

// A NODE THAT CAME UP WHILE THE WINDOW WAITED TAKES PART IN IT, and one the
// window recorded stays in it.
//
// A window waits at `opened` for as long as an admission or a missing
// acknowledgement holds it, and its participants were read once, as it opened.
// A broker member that came up in that time was never waited for: the seal
// passed without its restart, and its broker could still hold the request the
// window sent. Read again at the baseline and joined with what the window
// recorded, it is asked for, and a node recorded at the open is not dropped
// for having gone quiet since.
//
// Mutation: baseline over the recorded set alone and the first refusal goes
// missing; over the set read now alone and the second does.
func TestANodeThatCameUpWhileTheWindowWaitedTakesPart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	streams := []string{"CREWLET_TRACKER_LOG"}
	fleet := coordmem.NewFleet()
	coordinator := nodeOn(fleet, "node-1", "boot-1", statelog.ModeMaintenance, dataNode)
	op := openWindow(t, coordinator)
	if !slices.Equal(op.Participants, []string{"node-1"}) {
		t.Fatalf("participants = %v, want node-1 alone", op.Participants)
	}

	// NODE-7 COMES UP while the window waits, a data node publishing its
	// position.
	if err := fleet.PutPositions(ctx, coord.NodePositions{
		NodeID: "node-7", At: time.Now().UTC(), Domains: map[string]coord.DomainPosition{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.advanceCapacity(ctx, nil, op); err == nil ||
		!strings.Contains(err.Error(), "node-7") {
		t.Fatalf("the window baselined without node-7, which came up while it "+
			"waited (err %v)", err)
	}
	late := nodeOn(fleet, "node-7", "boot-1", statelog.ModeMaintenance, dataNode)
	if err := late.handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	next, err := coordinator.advanceCapacity(ctx, nil, op)
	if err != nil {
		t.Fatalf("the window still refuses with node-7 acknowledged: %v", err)
	}
	if !slices.Contains(next.Participants, "node-7") ||
		next.WriteIncarnations["node-7"] != late.incarnation {
		t.Errorf("node-7 is not baselined as the process it acknowledged as: "+
			"participants %v, incarnations %v", next.Participants, next.WriteIncarnations)
	}

	// AND A NODE THE WINDOW RECORDED IS NOT DROPPED for being nowhere now.
	recorded := coord.MaintenanceOperation{
		Stream: "CREWLET_PAGES_LOG", OperationID: "op-2", TargetMaxBytes: 1 << 33,
		Phase: coord.PhaseOpened, Attempt: 1, Participants: []string{"node-1", "node-9"},
		EnteredAt: time.Now().UTC(), By: "ops-3",
	}
	held, _, err := fleet.OpenMaintenance(ctx, recorded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.advanceCapacity(ctx, nil, held); err == nil ||
		!strings.Contains(err.Error(), "node-9") {
		t.Fatalf("the window dropped node-9, which it recorded at the open and "+
			"nobody has shown stopped (err %v)", err)
	}
}

// TestAnUnreadablePositionRegisterRefusesRatherThanShrinkingTheSet: a
// participant set read from a store that failed is a set with nodes silently
// missing, and every one of them is a machine that may be publishing.
func TestAnUnreadablePositionRegisterRefusesRatherThanShrinkingTheSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	e.backends.Fleet = &blindFleet{fleetStore: fleet, positionsErr: errors.New("store unreachable")}

	if _, err := e.capacityParticipants(ctx); err == nil {
		t.Fatal("the participant set was formed from a register that could not " +
			"be read — every node missing from it may be publishing")
	}
}

// blindPresence is a lease store that cannot list the presence leases.
type blindPresence struct{ coord.Backend }

func (b blindPresence) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	if class == coord.ClassNode {
		return nil, errors.New("coordination timed out")
	}
	return b.Backend.ListLive(ctx, class)
}

// AN UNREADABLE PRESENCE LISTING REFUSES RATHER THAN SHRINKING THE SET, for the
// register's reason: every presence it could not read names a node that may be
// publishing the estate's records, or a broker member holding a request.
func TestAnUnreadablePresenceListingRefusesRatherThanShrinkingTheSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, _ := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	e.backends.Coord = blindPresence{Backend: coordmem.New()}
	if _, err := e.capacityParticipants(ctx); err == nil {
		t.Fatal("the participant set was formed without the presence leases it " +
			"could not list — every node missing from it may be publishing")
	}
}

// AN ADMISSION WHOSE PROCESS ISN'T RUNNING STOPS THE WINDOW'S FIRST REQUEST,
// and the refusal names the way past it and the way out.
//
// node-9 holds an admission and nobody holds its presence: a process that
// stopped without giving it back. The window opens, since it has sent nothing
// and is where the operator's way past is recorded, and what it refuses is its
// FIRST REQUEST: naming the node, the restart, the exclusion for that node,
// and the abandonment that closes the window, because an open window refuses
// every node that starts in service while it waits.
//
// Mutation: drop the admission check from the baseline and this goes red;
// check it without the window's exclusions and
// TestAnExcludedNodesAdmissionNoLongerStopsTheWindow does.
func TestAnAdmissionStopsTheWindowsFirstRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	if err := fleet.PutAdmission(ctx, coord.Admission{
		NodeID: "node-9", Incarnation: "node-9:boot-1", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	op := openWindow(t, e)
	_, refusal := e.advanceCapacity(ctx, nil, op)
	if refusal == nil {
		t.Fatal("the window baselined while a node held an admission")
	}
	err := toldRefusal(t, refusal)
	// THE COMMANDS IT NAMES ARE ONES THE CLI RUNS, for this window's stream
	// and this node, and the sentence reads right for one node.
	for _, want := range []string{
		`"node-9" holds an admission`, "-mode maintenance",
		"`crewlet retention maintenance exclude -stream CREWLET_TRACKER_LOG " +
			"-node node-9 -confirm node-9`",
		"`crewlet retention maintenance abandon -stream CREWLET_TRACKER_LOG " +
			"-confirm " + op.OperationID + "`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "these nodes") || strings.Contains(err.Error(), " hold an") {
		t.Errorf("the refusal reads wrong for one node: %v", err)
	}
	held, found, err := fleet.Maintenance(ctx, "CREWLET_TRACKER_LOG")
	if err != nil || !found {
		t.Fatalf("the window is gone (found %t, err %v)", found, err)
	}
	if held.Phase != coord.PhaseOpened || len(held.Journal) != 0 ||
		len(held.WriteIncarnations) != 0 {
		t.Fatalf("the window moved past opened, so a request may be on its way, "+
			"while a node is admitted: %+v", held)
	}
}

// stuckClose is a coordination store that cannot close a window.
type stuckClose struct{ *fleetStore }

func (stuckClose) CloseMaintenance(context.Context, string, string, uint64) error {
	return errors.New("store unreachable")
}

// A SERVING NODE'S ADMISSION LEAVES NO WINDOW BEHIND.
//
// node-9 is running in service: it holds an admission and its process holds
// the node's presence. A window left open over it waited at opened while the
// fleet went on publishing, refused every node that restarted in service and
// told it to restart into maintenance mode, taking a serving node out of
// service. So the window is closed again as soon as it is opened and the verb
// is refused, as it was before the window opened at all; and so it is when
// nobody can read the admissions. A close that does not land leaves
// the window, and the refusal says how to close it.
//
// Mutation: keep the window whatever the presence says, and the first row
// fails; keep it when the admissions can't be read, and the second does.
func TestAServingNodesAdmissionLeavesNoWindowBehind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		blind       bool
		stuck       bool
		wantWindow  bool
		wantRefusal []string
	}{
		{name: "the node is running",
			wantRefusal: []string{`"node-9" is running and holds an admission`,
				"no window was opened", "-mode maintenance"}},
		{name: "nobody can read the admissions", blind: true,
			wantRefusal: []string{"couldn't tell", "no window was opened"}},
		{name: "the window can't be closed again", stuck: true, wantWindow: true,
			wantRefusal: []string{"still open",
				"`crewlet retention maintenance abandon -stream CREWLET_TRACKER_LOG -confirm "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
			backend := coordmem.New()
			e.backends.Coord = backend
			if tc.blind {
				e.backends.Fleet = &blindFleet{fleetStore: fleet,
					admissionsErr: errors.New("store unreachable")}
			}
			if tc.stuck {
				e.backends.Fleet = stuckClose{fleetStore: fleet}
			}
			serving := nodeOn(fleet, "node-9", "boot-1", statelog.ModeNormal, dataNode)
			if err := serving.handshake(ctx, []string{"CREWLET_TRACKER_LOG"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := backend.TryAcquire(ctx, coord.NodeResource("node-9"),
				coord.AcquireOptions{Owner: "node-9:boot-1", TTL: time.Minute}); err != nil {
				t.Fatal(err)
			}

			op, err := e.openCapacity(ctx, CapacityRequest{
				Stream: "CREWLET_TRACKER_LOG", TargetMaxBytes: 1 << 33, By: "ops-3",
			}, jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom)
			if err == nil {
				t.Fatalf("a window opened over a node that may be publishing: %+v", op)
			}
			said := toldRefusal(t, err)
			for _, want := range tc.wantRefusal {
				if !strings.Contains(said.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			_, found, readErr := fleet.Maintenance(ctx, "CREWLET_TRACKER_LOG")
			if readErr != nil {
				t.Fatal(readErr)
			}
			if found != tc.wantWindow {
				t.Errorf("a window stands after the refusal: %t, want %t", found, tc.wantWindow)
			}
			if (op.OperationID != "") != tc.wantWindow {
				t.Errorf("the refusal carries operation %+v, want one only while a "+
					"window stands", op)
			}
			// THE SERVING NODE'S NEXT BOOT IN SERVICE IS ADMITTED, as it
			// was before the window: nothing was left to refuse it.
			if !tc.wantWindow {
				again := nodeOn(fleet, "node-9", "boot-2", statelog.ModeNormal, dataNode)
				if err := again.handshake(ctx, []string{"CREWLET_TRACKER_LOG"}); err != nil {
					t.Errorf("a boot in service was refused after the window "+
						"was closed again: %v", err)
				}
			}
		})
	}
}

// A NODE RUNNING AT THE BASELINE IS NAMED AS RUNNING, WITH THE WAY OUT.
//
// The window opened with nothing holding it, and node-9 then admitted itself
// and took its presence before it read the window: the one order the open
// cannot see. The baseline reads it as a process that may be publishing,
// sends nothing, and names the restart and the abandonment that closes the
// window, since the window refuses every boot in service while it stands.
func TestANodeRunningAtTheBaselineIsNamedWithTheWayOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	backend := coordmem.New()
	e.backends.Coord = backend
	op := openWindow(t, e)
	if err := fleet.PutAdmission(ctx, coord.Admission{
		NodeID: "node-9", Incarnation: "node-9:boot-1", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.TryAcquire(ctx, coord.NodeResource("node-9"),
		coord.AcquireOptions{Owner: "node-9:boot-1", TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	_, refusal := e.advanceCapacity(ctx, nil, op)
	if refusal == nil {
		t.Fatal("the window baselined over a running node's admission")
	}
	err := toldRefusal(t, refusal)
	for _, want := range []string{
		`"node-9" is running and holds an admission, so it may be publishing`,
		"Nothing has been sent", "refuses every node that starts in service",
		"`crewlet retention maintenance abandon -stream CREWLET_TRACKER_LOG -confirm " +
			op.OperationID + "`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "exclude") {
		t.Errorf("the refusal offers to exclude a node that is running: %v", err)
	}
}

// A NODE RESTARTED INSIDE ITS CRASHED PROCESS'S LEASE IS RETIRED ONCE IT HOLDS
// THE PRESENCE.
//
// node-a crashed in service and was restarted into maintenance mode at once,
// so its boot found the crashed process still holding the node's presence and
// left the admission: it could not tell the crash from a duplicate. A window
// opened then sees a process that may be publishing and is closed again. Once
// the old lease lapses and the restarted process holds the presence, the same
// evidence says the crashed one is gone, and the window withdraws the
// admission and goes on without anybody excluding a node that is running.
//
// Mutation: drop the withdrawal of a replaced admission and the window stays
// shut on the second open.
func TestARestartInsideItsCrashedProcesssLeaseIsRetiredByTheWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	backend := coordmem.New()
	streams := []string{"CREWLET_TRACKER_LOG"}

	crashed := nodeOn(fleet, "node-a", "crashed", statelog.ModeNormal, dataNode)
	if err := crashed.handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	lease, _, err := backend.TryAcquire(ctx, coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a:crashed", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	restarted := nodeOn(fleet, "node-a", "restarted", statelog.ModeMaintenance, dataNode)
	restarted.backends.Coord = backend
	if err := restarted.handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	if held, _, _ := heldAdmission(ctx, fleet, "node-a"); held.Incarnation != "node-a:crashed" {
		t.Fatalf("the premise failed: node-a holds %+v after a restart inside "+
			"the crashed process's lease", held)
	}

	coordinator := nodeOn(fleet, "node-b", "boot-1", statelog.ModeMaintenance, dataNode)
	coordinator.backends.Coord = backend
	if err := coordinator.handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	request := CapacityRequest{Stream: "CREWLET_TRACKER_LOG", TargetMaxBytes: 1 << 33, By: "ops-3"}
	if _, err := coordinator.openCapacity(ctx, request,
		jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom); err == nil {
		t.Fatal("a window opened while the process node-a's admission names " +
			"still held its presence")
	}

	// THE CRASHED PROCESS'S LEASE LAPSES, and the restarted one takes it.
	if _, err := backend.Release(ctx, coord.NodeResource("node-a"), "node-a:crashed",
		lease.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.TryAcquire(ctx, coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a:restarted", TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	op, err := coordinator.openCapacity(ctx, request,
		jetstream.LogStats{MaxBytes: 1 << 32}, unstatedRoom)
	if err != nil {
		t.Fatalf("the window was refused once node-a's restarted process held "+
			"its presence: %v", err)
	}
	if _, found, _ := heldAdmission(ctx, fleet, "node-a"); found {
		t.Error("node-a's crashed process still holds an admission the window " +
			"could see it no longer has")
	}
	// AND THE RESTARTED PROCESS ACKNOWLEDGES THE WINDOW, as its next boot
	// would, so the baseline has a process to record for it.
	if err := fleet.PutMaintenanceAck(ctx, coord.MaintenanceAck{
		Stream: "CREWLET_TRACKER_LOG", NodeID: "node-a", OperationID: op.OperationID, Attempt: op.Attempt,
		Incarnation: "node-a:restarted", Mode: string(statelog.ModeMaintenance),
		At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	next, err := coordinator.advanceCapacity(ctx, nil, op)
	if err != nil {
		t.Fatalf("the baseline refused once node-a was retired: %v", err)
	}
	if next.Phase != coord.PhaseBaselined {
		t.Errorf("the window stands at %s, want baselined", next.Phase)
	}
}

// slipFleet is a coordination store a node admits itself through at the worst
// moment there is: after the coordinator could have read the admissions and
// before its window's record lands, so the node finds no window and starts.
type slipFleet struct {
	*fleetStore
	slip coord.Admission
}

func (f *slipFleet) OpenMaintenance(ctx context.Context, op coord.MaintenanceOperation) (
	coord.MaintenanceOperation, bool, error) {

	if err := f.fleetStore.PutAdmission(ctx, f.slip); err != nil {
		return coord.MaintenanceOperation{}, false, err
	}
	return f.fleetStore.OpenMaintenance(ctx, op)
}

// A NODE ADMITTED JUST BEFORE THE WINDOW EXISTED IS SEEN BY ITS FIRST REQUEST.
//
// The coordinator read the admissions and then wrote the window. A node that
// wrote its admission between the two and read the window before the create
// saw nothing either way: it started publishing, and the window baselined and
// resized the log under it. Read after the window's record lands, as the
// handshake needs, the admission is seen.
func TestAnAdmissionWrittenBeforeTheWindowLandedIsSeen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	e.backends.Fleet = &slipFleet{fleetStore: fleet, slip: coord.Admission{
		NodeID: "node-9", Incarnation: "node-9:boot-1", At: time.Now().UTC(),
	}}

	op := openWindow(t, e)
	if _, err := e.advanceCapacity(ctx, nil, op); err == nil ||
		!strings.Contains(err.Error(), "node-9") {
		t.Fatalf("the window baselined over a node that admitted itself just "+
			"before the window landed, and is publishing (err %v)", err)
	}
}

// stuckAdmissions is a coordination store that cannot withdraw an admission.
type stuckAdmissions struct{ *fleetStore }

func (stuckAdmissions) ForgetAdmission(context.Context, string, string) error {
	return errors.New("store unreachable")
}

// AN EXCLUDED NODE'S ADMISSION STOPS COUNTING EVEN WHERE IT COULD NOT BE
// WITHDRAWN. The exclusion is the operator's word that the process is
// stopped, recorded on the window, and the window's first request reads it
// there: one that went on counting the key a store blip left would refuse
// the very node it was told is gone. And the key it left is said in the log,
// which is the whole of what it leaves behind: the exclusion once read the
// admissions through a helper that answered a failed read as none, so a store
// blip withdrew nothing and said nothing.
//
// Process-wide logging, so not parallel. Mutation: swallow the withdrawal's
// failure and the log half fails.
func TestAnExcludedNodesAdmissionNoLongerStopsTheWindow(t *testing.T) {
	logs := captureEngineLogs(t)
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
	if err := fleet.PutAdmission(ctx, coord.Admission{
		NodeID: "node-9", Incarnation: "node-9:boot-1", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	e.backends.Fleet = stuckAdmissions{fleetStore: fleet}
	op := openWindow(t, e)
	excluded, err := e.ExcludeParticipant(ctx, op.Stream, "node-9", iam.Actor{Name: "dana.sre"})
	if err != nil {
		t.Fatalf("the exclusion was refused: %v", err)
	}
	if _, found, _ := heldAdmission(ctx, fleet, "node-9"); !found {
		t.Fatal("the premise failed: node-9's admission was withdrawn")
	}
	if lines := logs.about("excluded_admission_not_withdrawn", "node", "node-9"); len(lines) != 1 {
		t.Errorf("excluded_admission_not_withdrawn = %v: an exclusion that left "+
			"the node's admission has to say so", lines)
	}
	if _, err := e.advanceCapacity(ctx, nil, excluded); err != nil {
		t.Fatalf("an excluded node's admission still stops the window: %v", err)
	}
}

// A DEAD NODE'S ADMISSION IS WITHDRAWN BY EXCLUDING IT.
//
// node-a crashed holding its admission and will never boot again, so nothing
// retires its claim on its behalf. The window used to refuse to open over it,
// and an exclusion is a gesture on an open window, so the fleet could never
// resize that log again. Now the window opens, stops before its first request,
// and the exclusion — the operator's word that the process is stopped,
// recorded with their name — withdraws the admission and waives the dead
// node's acknowledgement in the same breath.
func TestADeadNodesAdmissionIsWithdrawnByExcludingIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	streams := []string{"CREWLET_TRACKER_LOG"}

	crashed := nodeOn(fleet, "node-a", "crashed", statelog.ModeNormal, dataNode)
	if err := crashed.handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	// A DATA NODE PUBLISHES ITS POSITION, which is what makes it a
	// participant whose acknowledgement the window waits for.
	if err := fleet.PutPositions(ctx, coord.NodePositions{
		NodeID: "node-a", At: time.Now().UTC(), Domains: map[string]coord.DomainPosition{},
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := nodeOn(fleet, "node-b", "boot-1", statelog.ModeMaintenance, dataNode)
	if err := coordinator.handshake(ctx, streams); err != nil {
		t.Fatal(err)
	}
	op := openWindow(t, coordinator)
	if !slices.Contains(op.Participants, "node-a") {
		t.Fatalf("participants = %v, want node-a among them", op.Participants)
	}
	if _, err := coordinator.advanceCapacity(ctx, nil, op); err == nil {
		t.Fatal("the window baselined over a dead node's admission")
	}

	sre := iam.Actor{Name: "dana.sre", OperatorID: "session:0192f00d-0000-7000-8000-0000000000dd"}
	excluded, err := coordinator.ExcludeParticipant(ctx, op.Stream, "node-a", sre)
	if err != nil {
		t.Fatalf("the exclusion was refused: %v", err)
	}
	if party := excluded.ExcludedBy["node-a"]; party.By != sre.Name {
		t.Errorf("the window records node-a as excluded by %+v, want %s", party, sre.Name)
	}
	if _, found, err := heldAdmission(ctx, fleet, "node-a"); err != nil || found {
		t.Fatalf("node-a's admission outlived its exclusion (found %t, err %v)", found, err)
	}
	next, err := coordinator.advanceCapacity(ctx, nil, excluded)
	if err != nil {
		t.Fatalf("the window still refuses with node-a excluded: %v", err)
	}
	if next.Phase != coord.PhaseBaselined {
		t.Errorf("the window stands at %s, want baselined", next.Phase)
	}
	if _, baselined := next.WriteIncarnations["node-a"]; baselined {
		t.Errorf("an excluded node was baselined: %+v", next.WriteIncarnations)
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
	t.Parallel()
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
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a",
	}, current, unstatedRoom)
	if err != nil {
		t.Fatalf("a target above the usage and under the ceiling was refused: %v", err)
	}
	if op.OriginalMaxBytes != current.MaxBytes {
		t.Errorf("the window recorded an original ceiling of %d, want %d",
			op.OriginalMaxBytes, current.MaxBytes)
	}
	// WHO OPENED IT, and through what: the operation is the one record of
	// who is restarting the fleet three times, and one that named only
	// the author could not tell their own gesture from their token's.
	if op.By != "ops-3" || op.OperatorID != "pat:0192f00d-0000-7000-8000-00000000000a" {
		t.Errorf("the window records %q through %q, want ops-3 through the "+
			"token it was opened with", op.By, op.OperatorID)
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
//
// EVERY REGISTERED LOG, each on the side its claim puts it: the identity
// estate keeps a reserve as the tracker's and the knowledge base's logs do,
// and a list of two left it unasked.
func TestATargetOnlyTheGateReserveClearsIsRefused(t *testing.T) {
	t.Parallel()
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
	reserved := 0
	for _, domain := range registeredDomains() {
		stream := domain.Stream().Name
		reservation, field, _ := capacityBounds(stream)
		if !domain.ClaimsIdentity() {
			// A LOG THAT KEEPS NO RESERVE is measured against its whole
			// ceiling, which clears what it holds.
			if reservation.Kept {
				t.Errorf("%s claims no identity and keeps a gate reserve", stream)
			}
			if err := open(stream, target); err != nil {
				t.Errorf("%s keeps no reserve, and a target above what it holds "+
					"was refused: %v", stream, err)
			}
			continue
		}
		reserved++
		if ordinary := reservation.Ordinary(target); !reservation.Kept ||
			ordinary > current.Bytes {
			t.Fatalf("%s holds its ordinary writes to %d of a %d-byte target, so a "+
				"log holding %d bytes is not the case this asks about", stream,
				ordinary, uint64(target), current.Bytes)
		}
		err := open(stream, target)
		if err == nil {
			t.Fatalf("a %d-byte target on %s, holding %d bytes, was accepted — "+
				"its ordinary writes are held to %d and would all be refused",
				uint64(target), stream, current.Bytes,
				reservation.Ordinary(target))
		}
		least := smallestTargetAbove(current.Bytes, reservation, uint64(field.Floor))
		for _, want := range []string{
			fmt.Sprintf("holds %d bytes", current.Bytes),
			fmt.Sprintf("ordinary writes are held to %d", reservation.Ordinary(target)),
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
	if reserved < 7 {
		t.Fatalf("%d registered log(s) keep a gate reserve, want at least the "+
			"tracker's, the knowledge base's, the identity estate's, the "+
			"artifacts log's, the artifact datastore's, the assistant's and chat's", reserved)
	}
}

// A TARGET UNDER A LOG'S FLOOR IS REFUSED, AND ITS FLOOR IS ITS FIELD'S.
//
// Tier A refuses an explicit ceiling below its field's floor; the capacity
// verb was the one way round it. And the floor differs by log: the verb held
// every log to a gibibyte, calling it the floor Tier A enforces, while Tier A
// takes the identity estate's and the usage log's down to 64 MiB — so a
// target Tier A would accept on either was refused with a reason that was not
// true of it. The floor each case expects is read from Tier A's own
// validation of the field, not from the register the verb reads, so the two
// cannot agree by sharing a mistake.
func TestATargetUnderTheFloorIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	current := jetstream.LogStats{Bytes: 1 << 20, MaxBytes: 4 << 30}
	keys := streamKeys(t)
	for _, domain := range registeredDomains() {
		stream := domain.Stream().Name
		_, field, known := capacityBounds(stream)
		if !known {
			t.Fatalf("%s is registered and the capacity verb reads no bounds for it", stream)
		}
		floor := tierAFloor(t, keys, field.Field, field.Bytes)
		e, fleet := capacityFixture(t, "node-1", statelog.ModeMaintenance)
		_, err := e.openCapacity(ctx, CapacityRequest{
			Stream: stream, TargetMaxBytes: uint64(floor) - 1, By: "ops-3",
		}, current, unstatedRoom)
		if err == nil {
			t.Fatalf("a target a byte under the floor was accepted on %s", stream)
		}
		for _, want := range []string{fmt.Sprintf("at least %d bytes", floor), field.Field} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal on %s does not say %q: %v", stream, want, err)
			}
		}
		if op, found, _ := fleet.Maintenance(ctx, stream); found {
			t.Fatalf("a window was opened anyway on %s: %+v", stream, op)
		}
		e, _ = capacityFixture(t, "node-1", statelog.ModeMaintenance)
		if _, err := e.openCapacity(ctx, CapacityRequest{
			Stream: stream, TargetMaxBytes: uint64(floor), By: "ops-3",
		}, current, unstatedRoom); err != nil {
			t.Errorf("a target at %s's floor of %d was refused: %v", stream, floor, err)
		}
	}
}

// tierAFloor is the smallest value Tier A's own validation accepts for the
// stream field named field — found by asking it, not by reading a constant —
// bisected below accepted, a value it takes (the one an unset field derives,
// which [TestEveryRegisteredDomainIsSizedFromTierA] holds it to taking).
func tierAFloor(t *testing.T, keys map[string][]int, field string, accepted int64) int64 {
	t.Helper()
	index, found := keys[strings.TrimPrefix(field, "stream.")]
	if !found {
		t.Fatalf("%s is not a Tier A stream key", field)
	}
	accepts := func(v int64) bool {
		boot := testBootstrap(t)
		boot.Stream.StoreDir = t.TempDir()
		reflect.ValueOf(&boot.Stream).Elem().FieldByIndex(index).SetInt(v)
		return boot.Validate() == nil
	}
	// BISECTED between a byte, which no field accepts, and a value this
	// one does.
	low, high := int64(1), accepted
	if accepts(low) || !accepts(high) {
		t.Fatalf("%s accepts %v at 1 byte and %v at %d, so it has no floor "+
			"between them", field, accepts(low), accepts(high), high)
	}
	for high-low > 1 {
		mid := low + (high-low)/2
		if accepts(mid) {
			high = mid
		} else {
			low = mid
		}
	}
	return high
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	readErr error
	// readErrOn narrows readErr to one log's window, where it is set.
	readErrOn     string
	positionsErr  error
	admissionsErr error
	openFailures  int
	openedIDs     []string
}

func (b *blindFleet) Maintenance(ctx context.Context, stream string) (
	coord.MaintenanceOperation, bool, error) {

	if b.readErr != nil && (b.readErrOn == "" || b.readErrOn == stream) {
		return coord.MaintenanceOperation{}, false, b.readErr
	}
	return b.fleetStore.Maintenance(ctx, stream)
}

func (b *blindFleet) Admissions(ctx context.Context) ([]coord.Admission, error) {
	if b.admissionsErr != nil {
		return nil, b.admissionsErr
	}
	return b.fleetStore.Admissions(ctx)
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
	domain := worklet.Domain{}
	e.backends.Queue = host
	s := &stateLog{volume: t.TempDir()}
	runsLogs(s, &runningLog{domain: domain, spec: domain.Stream()})
	e.core.Store(&core{log: s})
	return e, fleet, domain.Stream().Name
}

// A TARGET PAST int64 IS THE UNBOUNDED SETTING WEARING A LARGE NUMBER.
//
// A stream's ceiling is an int64 on the wire, so a target above that wraps
// NEGATIVE — and a negative MaxBytes is how JetStream spells "unbounded". It
// is the same stream a target of zero would have produced, which this verb
// already refuses by name, reached from the opposite-looking input.
func TestACapacityTargetPastInt64IsRefused(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		"the broker refused", "abandon",
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
	if strings.Contains(lost.Error(), "the broker refused") {
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
	t.Parallel()
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
			if !strings.Contains(err.Error(), "the broker refused") {
				t.Errorf("the refusal is no longer named:\n%v", err)
			}
		})
	}
}

// WHO TAKES PART IN A CAPACITY SEAL IS DECIDED BY THE ESTATE AND BY THE
// BROKER, asked apart. The seal's proof is that every broker process restarted
// and every publisher was admitted, so a live node takes part when it holds the
// estate — every data node does, whatever its broker — or when its broker is a
// member (it queues requests) — and when its presence does not say what its
// broker is, or what its roles are, because leaving out a node that may be a
// member or a publisher could pass a seal it should hold. A leaf or a client
// of an external cluster that holds no data does neither.
func TestWhoTakesPartInACapacitySealIsTheEstateAndTheBrokerMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, fleet := capacityFixture(t, "node-coordinator", statelog.ModeMaintenance)
	backend := coordmem.New()
	e.backends.Coord = backend
	for id, meta := range map[string]map[string]any{
		"data-member":      {"roles": []string{"data", "seats"}, "broker": "member"},
		"data-client":      {"roles": []string{"data"}, "broker": "client"},
		"data-client-idle": {"roles": []string{"data"}, "broker": "client"},
		"dataless-member":  {"roles": []string{"seats"}, "broker": "member"},
		"an-older-build":   {"roles": []string{"seats"}},
		"a-newer-kind":     {"roles": []string{"seats"}, "broker": "observer"},
		"stateless-leaf":   {"roles": []string{"seats"}, "broker": "leaf"},
		"stateless-client": {"roles": []string{"seats"}, "broker": "client"},
		"estate-on-a-leaf": {"roles": []string{"data"}, "broker": "leaf"},
		"unreadable-roles": {"roles": "every one", "broker": "client"},
	} {
		if _, _, err := backend.TryAcquire(ctx, coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":boot-1", TTL: time.Minute, Meta: meta,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.capacityParticipants(ctx)
	if err != nil {
		t.Fatalf("capacityParticipants: %v", err)
	}
	want := []string{"a-newer-kind", "an-older-build", "data-client", "data-client-idle",
		"data-member", "dataless-member", "estate-on-a-leaf", "node-coordinator",
		"unreadable-roles"}
	if !slices.Equal(got, want) {
		t.Fatalf("participants = %v, want %v", got, want)
	}

	// AND A NODE THAT HOLDS NO DATA ADMITS NOTHING, whatever its broker: it
	// publishes to no state log, and an admission it wrote would be a
	// publisher a capacity operation waited on for ever.
	for _, broker := range []placement.BrokerKind{placement.BrokerLeaf, placement.BrokerMember} {
		stateless, _ := capacityFixture(t, "agent-1", statelog.ModeNormal)
		stateless.backends.Fleet = fleet
		stateless.profile = placement.NodeProfile{
			ID: "agent-1", Roles: placement.Roles(placement.RoleSeats), Broker: broker,
		}
		if err := stateless.admit(ctx, []string{"CREWLET_TRACKER_LOG"}); err != nil {
			t.Fatalf("admit: %v", err)
		}
	}
	admissions, err := fleet.Admissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(admissions) != 0 {
		t.Fatalf("a node that publishes to no state log recorded %+v", admissions)
	}
}

// TestADatalessMemberMustAcknowledgeASeal: a broker member that holds no data
// is a participant, and restarted into seal mode it acknowledges — so the seal
// holds once it has, and not before.
//
// Tier A refuses a broker member without `data`, so this node's own
// configuration never produces the pairing — but [Engine.capacityParticipants]
// reads every OTHER node's presence, not this node's configuration, and a
// presence advertising one is a participant it cannot rule out, so it is
// waited for. The profile is built here to certify the engine's half of the
// handshake against that presence. Left out of the participants, its broker
// could still hold a queued request to resize the log after the seal passed;
// named but unable to acknowledge, it would wedge every seal for ever.
//
// ITS PARTICIPATION IS FOUND FROM WHAT A SEAL-MODE NODE LEAVES BEHIND, never
// from a lease the case writes for it: such a node has no position and no
// admission, so the presence its seat host renews — in every mode, claiming no
// seat — is the one record there is. Each node here boots that host exactly as
// the engine does, behind the engine's own claim gate.
func TestADatalessMemberMustAcknowledgeASeal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	coordinator, fleet := capacityFixture(t, "data-a", statelog.ModeSeal)
	backend := coordmem.New()
	coordinator.backends.Coord = backend
	profiles := map[string]placement.NodeProfile{
		"data-a": {ID: "data-a", Roles: placement.Roles(placement.RoleData, placement.RoleSeats),
			Broker: placement.BrokerMember},
		"broker-1": {ID: "broker-1", Roles: placement.Roles(placement.RoleSeats),
			Broker: placement.BrokerMember},
		"agent-1": {ID: "agent-1", Roles: placement.Roles(placement.RoleSeats),
			Broker: placement.BrokerLeaf},
	}
	coordinator.profile = profiles["data-a"]
	nodes := map[string]*Engine{"data-a": coordinator}
	for _, id := range []string{"broker-1", "agent-1"} {
		node, _ := capacityFixture(t, id, statelog.ModeSeal)
		node.backends.Fleet = fleet
		node.profile = profiles[id]
		nodes[id] = node
	}
	for id, node := range nodes {
		if held := bootSeatHost(t, node, backend); len(held) != 0 {
			t.Fatalf("%s booted into seal mode holding %v", id, held)
		}
	}
	participants, err := coordinator.capacityParticipants(ctx)
	if err != nil {
		t.Fatalf("capacityParticipants: %v", err)
	}
	if want := []string{"broker-1", "data-a"}; !slices.Equal(participants, want) {
		t.Fatalf("participants = %v, want %v: a member that holds no data holds "+
			"queued requests all the same, and a leaf holds none", participants, want)
	}
	op := coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-1", TargetMaxBytes: 1 << 33,
		Phase: coord.PhaseBaselined, Attempt: 1, Participants: participants,
		WriteIncarnations: map[string]string{"data-a": "data-a:old", "broker-1": "broker-1:old"},
		EnteredAt:         time.Now().UTC(), By: "ops-3",
	}
	if _, _, err := fleet.OpenMaintenance(ctx, op); err != nil {
		t.Fatal(err)
	}

	// EVERY NODE RESTARTS INTO SEAL MODE, each acknowledging as its own
	// profile says it must — the leaf included, which must write nothing.
	coordinator.acknowledge(ctx, []string{op.Stream})
	acks, err := fleet.MaintenanceAcks(ctx, "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatal(err)
	}
	if _, missing := statelog.SealRestarts(op, acks); len(missing) == 0 {
		t.Fatal("the seal held before the dataless member restarted into seal mode")
	}
	for _, id := range []string{"broker-1", "agent-1"} {
		nodes[id].acknowledge(ctx, []string{op.Stream})
	}
	acks, err = fleet.MaintenanceAcks(ctx, "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatal(err)
	}
	for _, ack := range acks {
		if ack.NodeID == "agent-1" {
			t.Errorf("a leaf that holds no data acknowledged: its broker queues " +
				"nothing and it is no participant, so the record is noise at best")
		}
	}
	incarnations, missing := statelog.SealRestarts(op, acks)
	if len(missing) > 0 {
		t.Fatalf("the seal does not hold with every participant restarted into "+
			"seal mode: the dataless member never acknowledged (acks: %+v)", acks)
	}
	if incarnations["broker-1"] != "broker-1:boot-1" {
		t.Errorf("the dataless member's evidence is %q, want the incarnation it "+
			"restarted as", incarnations["broker-1"])
	}
}

// bootSeatHost runs a node's seat host as the engine builds it — its profile
// advertised, behind the engine's own claim gate — over a company of two
// seats, and reports what the first sweep claimed. What it leaves in the lease
// store is what that node's boot leaves.
func bootSeatHost(t *testing.T, e *Engine, backend coord.Backend) []string {
	t.Helper()
	host, err := seat.New(seat.Config{
		Backend: backend, Owner: e.incarnation, NodeID: e.id, Profile: e.profile,
		Seats: func() []placement.Seat {
			return []placement.Seat{{Handle: "ceo"}, {Handle: "cto"}}
		},
		Ready: e.seatsAdmitted,
	})
	if err != nil {
		t.Fatal(err)
	}
	host.Start(t.Context())
	t.Cleanup(func() { host.Stop(context.WithoutCancel(t.Context())) })
	return host.Held()
}
