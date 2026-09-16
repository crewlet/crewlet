package statelog_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

var maintenanceAt = time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)

// operation is a capacity operation in a named phase, with two participants
// baselined and neither restarted.
func operation(phase coord.MaintenancePhase) coord.MaintenanceOperation {
	return coord.MaintenanceOperation{
		Stream:            "CREWLET_TRACKER_LOG",
		OperationID:       "op-1",
		TargetMaxBytes:    8 << 30,
		OriginalMaxBytes:  4 << 30,
		Phase:             phase,
		Attempt:           1,
		Participants:      []string{"node-1", "node-2"},
		WriteIncarnations: map[string]string{"node-1": "node-1:old", "node-2": "node-2:old"},
		EnteredAt:         maintenanceAt,
		By:                "ops-3",
	}
}

// sealed is the acknowledgement set that satisfies the barrier: both
// participants, this operation, this attempt, seal mode, fresh incarnations.
func sealed(attempt int) []coord.MaintenanceAck {
	return []coord.MaintenanceAck{
		{NodeID: "node-1", OperationID: "op-1", Attempt: attempt,
			Incarnation: "node-1:new", Mode: "seal"},
		{NodeID: "node-2", OperationID: "op-1", Attempt: attempt,
			Incarnation: "node-2:new", Mode: "seal"},
	}
}

// TestNoPhaseReachesConfirmedWithoutSealEvidence is the property the whole
// transition table exists for.
//
// The failure it replaces is a recovery paragraph reading "applied leads to
// confirm and clear" — written before the seal existed and left in place
// after. A recovery following it would bypass the protection on exactly the
// path that crashed, which is the one path that needed it.
func TestNoPhaseReachesConfirmedWithoutSealEvidence(t *testing.T) {
	t.Parallel()
	// EVERY PHASE against evidence that establishes everything EXCEPT a
	// seal: a baseline, a matching read-back and a passed verify.
	generous := statelog.PhaseEvidence{
		Now:              maintenanceAt,
		BaselineWritten:  true,
		Observed:         true,
		ObservedMaxBytes: 8 << 30,
		Verified:         true,
	}
	for _, phase := range coord.MaintenancePhases {
		if phase == coord.PhaseSealed || phase == coord.PhaseConfirmed {
			// Sealed is where the seal already happened, and
			// confirmed is terminal. Neither is a bypass.
			continue
		}
		next, err := statelog.PermitPhase(operation(phase), generous)
		if err != nil {
			t.Fatalf("PermitPhase from %s: %v", phase, err)
		}
		if next == coord.PhaseConfirmed {
			t.Fatalf("phase %s advanced straight to confirmed on evidence that "+
				"contains no seal — a request the broker had already queued "+
				"would still be in flight, and the exclusion is about to be "+
				"released", phase)
		}
	}
}

// TestEveryPhaseResumesAfterACrash walks the whole table: what each phase
// advances to, and what it does when nothing has been established.
func TestEveryPhaseResumesAfterACrash(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		from coord.MaintenancePhase
		ev   statelog.PhaseEvidence
		want coord.MaintenancePhase
		why  string
	}{
		{
			name: "opened with nothing established stays opened",
			from: coord.PhaseOpened,
			ev:   statelog.PhaseEvidence{Now: maintenanceAt},
			want: coord.PhaseOpened,
			why: "no configuration request has been issued, so a resume is a " +
				"resumed activation rather than a recovery",
		},
		{
			name: "opened with a baseline advances",
			from: coord.PhaseOpened,
			ev:   statelog.PhaseEvidence{Now: maintenanceAt, BaselineWritten: true},
			want: coord.PhaseBaselined,
		},
		{
			name: "baselined with a matching read-back is applied",
			from: coord.PhaseBaselined,
			ev: statelog.PhaseEvidence{Now: maintenanceAt, Observed: true,
				ObservedMaxBytes: 8 << 30},
			want: coord.PhaseApplied,
		},
		{
			name: "baselined with a read-back that does not match cannot advance",
			from: coord.PhaseBaselined,
			ev: statelog.PhaseEvidence{Now: maintenanceAt, Observed: true,
				ObservedMaxBytes: 4 << 30},
			want: coord.PhaseBaselined,
			why: "the ceiling is not the target, so nothing has been applied — " +
				"and an accepted request is not an applied one",
		},
		{
			name: "baselined reaches the seal on a verified stop",
			from: coord.PhaseBaselined,
			ev:   statelog.PhaseEvidence{Now: maintenanceAt, Stopped: true},
			want: coord.PhaseSealing,
			why: "a crash here must assume a request is in flight, and the only " +
				"thing that retires one is the barrier — so the seal has to be " +
				"reachable from here or retry and abandonment are both behind " +
				"a door nothing can open",
		},
		{
			name: "applied reaches the seal on a verified stop",
			from: coord.PhaseApplied,
			ev:   statelog.PhaseEvidence{Now: maintenanceAt, Stopped: true},
			want: coord.PhaseSealing,
		},
		{
			name: "sealing with a full acknowledgement set is sealed",
			from: coord.PhaseSealing,
			ev:   statelog.PhaseEvidence{Now: maintenanceAt, Acks: sealed(1)},
			want: coord.PhaseSealed,
		},
		{
			name: "sealing with no acknowledgements stays sealing",
			from: coord.PhaseSealing,
			ev:   statelog.PhaseEvidence{Now: maintenanceAt},
			want: coord.PhaseSealing,
		},
		{
			name: "sealed with a passed verify is confirmed",
			from: coord.PhaseSealed,
			ev:   statelog.PhaseEvidence{Now: maintenanceAt, Verified: true},
			want: coord.PhaseConfirmed,
		},
		{
			name: "sealed without a verify stays sealed",
			from: coord.PhaseSealed,
			ev:   statelog.PhaseEvidence{Now: maintenanceAt},
			want: coord.PhaseSealed,
		},
		{
			name: "confirmed is terminal",
			from: coord.PhaseConfirmed,
			ev:   statelog.PhaseEvidence{Now: maintenanceAt, Verified: true},
			want: coord.PhaseConfirmed,
			why:  "the one remaining write is the delete",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := statelog.PermitPhase(operation(c.from), c.ev)
			if err != nil {
				t.Fatalf("PermitPhase: %v", err)
			}
			if got != c.want {
				t.Fatalf("%s → %s, want %s%s", c.from, got, c.want,
					detail(c.why))
			}
		})
	}
}

func detail(why string) string {
	if why == "" {
		return ""
	}
	return " — " + why
}

// TestAnUnknownPhaseIsNotAdvancedPast: a build that cannot read a phase must
// not step over it, because the phase it would skip is the one carrying the
// evidence rule it does not have.
func TestAnUnknownPhaseIsNotAdvancedPast(t *testing.T) {
	t.Parallel()
	op := operation("sealing_v2")
	if _, err := statelog.PermitPhase(op, statelog.PhaseEvidence{
		Now: maintenanceAt, Verified: true, Stopped: true, Acks: sealed(1),
	}); err == nil {
		t.Fatal("an operation in a phase this build does not know was advanced")
	}
}

// TestASameAttemptRecordBlocksTheSeal is the schedule the journal exists for:
// two coordinators working ONE attempt of one operation. Coordinator A wrote a
// record, lost its lease, and B resumed — both are attempt 1.
//
// A predicate that refused only on records from an EARLIER attempt exempts A's
// record, which is exactly the request it was built to catch.
func TestASameAttemptRecordBlocksTheSeal(t *testing.T) {
	t.Parallel()
	op := operation(coord.PhaseSealing)
	op.Journal = []coord.JournalRecord{{
		Attempt: 1, By: "node-1:a", State: coord.JournalIssued, At: maintenanceAt,
	}}
	held, missing := statelog.SealHolds(op, sealed(1))
	if held {
		t.Fatal("the seal held with an unresolved write attempt from its own " +
			"attempt — the request the journal exists to catch is the one a " +
			"strictly-earlier comparison passes straight over")
	}
	if !strings.Contains(missing, "unresolved") {
		t.Fatalf("the seal refused with %q, which does not name the record", missing)
	}
}

// TestObservingTheTargetIsNotRetirement: a live request is not dead because
// somebody else wrote the same number.
func TestObservingTheTargetIsNotRetirement(t *testing.T) {
	t.Parallel()
	op := operation(coord.PhaseSealing)
	op.Journal = []coord.JournalRecord{{
		Attempt: 1, By: "node-1:a", State: coord.JournalIssued, At: maintenanceAt,
	}}
	// A read-back that matches the target exactly, and no barrier.
	op.ObservedMaxBytes = op.TargetMaxBytes
	if held, _ := statelog.SealHolds(op, sealed(1)); held {
		t.Fatal("the seal held because the ceiling equals the target — another " +
			"request may have set it while this one is still in flight")
	}

	// THE BARRIER is what retires it.
	after := statelog.Retire(op, statelog.BarrierEvidence{
		Incarnations:     map[string]string{"node-1": "node-1:new", "node-2": "node-2:new"},
		ObservedMaxBytes: op.TargetMaxBytes,
		At:               maintenanceAt,
	})
	if after.Journal[0].State != coord.JournalRetired {
		t.Fatalf("the barrier left the record %q", after.Journal[0].State)
	}
	if !strings.Contains(after.Journal[0].Evidence, "node-1:new") {
		t.Fatalf("the retirement records %q, which does not name the "+
			"incarnations that established it", after.Journal[0].Evidence)
	}
	if held, missing := statelog.SealHolds(after, sealed(1)); !held {
		t.Fatalf("the seal still refuses after the barrier retired every "+
			"record: %s", missing)
	}
}

// TestRetirementLeavesAResolvedRecordAlone: a completed record is not
// rewritten by a later barrier, because its evidence is its own read-back.
func TestRetirementLeavesAResolvedRecordAlone(t *testing.T) {
	t.Parallel()
	op := operation(coord.PhaseSealing)
	op.Journal = []coord.JournalRecord{{
		Attempt: 1, By: "node-1:a", State: coord.JournalCompleted,
		At: maintenanceAt, ResolvedAt: maintenanceAt,
	}}
	after := statelog.Retire(op, statelog.BarrierEvidence{At: maintenanceAt.Add(time.Hour)})
	if after.Journal[0].State != coord.JournalCompleted {
		t.Fatalf("a completed record became %q", after.Journal[0].State)
	}
}

// TestTheSealNeedsARestartRatherThanAnAcknowledgement: the whole barrier is
// that the process holding a request is gone, so an ack carrying the
// incarnation it was baselined at proves the opposite of what is needed.
func TestTheSealNeedsARestartRatherThanAnAcknowledgement(t *testing.T) {
	t.Parallel()
	op := operation(coord.PhaseSealing)
	stale := sealed(1)
	stale[0].Incarnation = op.WriteIncarnations["node-1"]
	held, missing := statelog.SealHolds(op, stale)
	if held {
		t.Fatal("the seal held on an acknowledgement from the same process that " +
			"was baselined — that process may still be holding the request")
	}
	if !strings.Contains(missing, "did not restart") {
		t.Fatalf("the refusal says %q, which does not name what is wrong", missing)
	}
}

// TestAnAcknowledgementFromAPublishingModeIsNotEvidence: a node that started
// its publishers has not established that it cannot have issued the request.
func TestAnAcknowledgementFromAPublishingModeIsNotEvidence(t *testing.T) {
	t.Parallel()
	op := operation(coord.PhaseSealing)
	wrong := sealed(1)
	wrong[1].Mode = string(statelog.ModeMaintenance)
	if held, _ := statelog.SealHolds(op, wrong); held {
		t.Fatal("the seal held on an acknowledgement from a mode that may write " +
			"configuration — the barrier is established by processes that " +
			"demonstrably cannot have issued what it is retiring")
	}
}

// TestPriorAttemptsAreInvalidatedByTheNumber, which is why they are not
// deleted: a stale acknowledgement is refused by arithmetic rather than by a
// cleanup somebody has to remember.
func TestPriorAttemptsAreInvalidatedByTheNumber(t *testing.T) {
	t.Parallel()
	op := operation(coord.PhaseSealing)
	op.Attempt = 2
	if held, missing := statelog.SealHolds(op, sealed(1)); held {
		t.Fatalf("attempt 2 sealed on attempt 1's acknowledgements (%s)", missing)
	}
	if held, missing := statelog.SealHolds(op, sealed(2)); !held {
		t.Fatalf("attempt 2 did not seal on its own acknowledgements: %s", missing)
	}
}

// TestAnExcludedParticipantIsTheOnlyWaiver: an operator's assertion that a
// process is stopped is the one thing that stands in for its acknowledgement.
func TestAnExcludedParticipantIsTheOnlyWaiver(t *testing.T) {
	t.Parallel()
	op := operation(coord.PhaseSealing)
	op.Excluded = []string{"node-2"}
	only := sealed(1)[:1]
	if held, missing := statelog.SealHolds(op, only); !held {
		t.Fatalf("an excluded participant still blocked the seal: %s", missing)
	}
	op.Excluded = nil
	if held, _ := statelog.SealHolds(op, only); held {
		t.Fatal("a participant that neither acknowledged nor was excluded was " +
			"waived — which is the absence check the handshake replaced")
	}
}

// TestAMismatchStartsANumberedAttemptFromTheWritableMode.
//
// Every node is in seal mode, which refuses configuration writes, and phases
// advance only forwards — so a retry that tried to re-apply in place would be
// refused by the very prohibition the barrier depends on. A numbered attempt
// is what makes the promised retry executable.
func TestAMismatchStartsANumberedAttemptFromTheWritableMode(t *testing.T) {
	t.Parallel()
	op := operation(coord.PhaseSealed)
	next, err := statelog.NextAttempt(op, 4<<30, maintenanceAt)
	if err != nil {
		t.Fatalf("NextAttempt: %v", err)
	}
	if next.Attempt != 2 {
		t.Fatalf("the retry is attempt %d, want 2", next.Attempt)
	}
	if next.Phase != coord.PhaseOpened {
		t.Fatalf("the retry starts at %s, want opened — the fleet restarts into "+
			"the writable maintenance mode and takes a fresh baseline", next.Phase)
	}
	if next.TargetMaxBytes != op.TargetMaxBytes {
		t.Fatalf("the retry targets %d and the operation targets %d: the target "+
			"is chosen once, and a second choice is a decision nothing fences",
			next.TargetMaxBytes, op.TargetMaxBytes)
	}
	if len(next.WriteIncarnations) != 0 {
		t.Fatalf("the retry kept the previous attempt's baseline (%v) — an "+
			"incarnation from before compared against an ack from now is "+
			"satisfied by a restart that already happened",
			next.WriteIncarnations)
	}
	// AND THE WRITABLE MODE IS THE ONE THAT MAY ISSUE IT.
	if !statelog.ModeMaintenance.WritesConfiguration() {
		t.Fatal("maintenance mode cannot write configuration, so the retry has " +
			"nowhere legal to run")
	}
	if statelog.ModeSeal.WritesConfiguration() {
		t.Fatal("seal mode may write configuration, which destroys the evidence " +
			"an acknowledgement from it is supposed to be")
	}
}

// TestTheAttemptBudgetSurvivesCoordinatorFailure: the count is on the RECORD,
// so a takeover resumes it. Held in the coordinator, each takeover would reset
// it and the advertised bound would be unbounded.
func TestTheAttemptBudgetSurvivesCoordinatorFailure(t *testing.T) {
	t.Parallel()
	op := operation(coord.PhaseSealed)
	op.Attempt = statelog.ControlRaiseAttempts
	blocked, err := statelog.NextAttempt(op, 4<<30, maintenanceAt)
	if err == nil {
		t.Fatalf("attempt %d was allowed past the budget of %d",
			op.Attempt, statelog.ControlRaiseAttempts)
	}
	if blocked.Blocked != "capacity_operation_unsealed" {
		t.Fatalf("the exhausted operation is blocked by %q", blocked.Blocked)
	}
	if !strings.Contains(err.Error(), "maintenance status") {
		t.Fatalf("the refusal does not say where to look: %v", err)
	}
}

// TestAbandonFromOpenedClearsWithoutASeal, and from anywhere else it seals:
// abandoning changes what the operation is trying to reach, never the barrier
// it must cross. Reading a status page retires nobody's outstanding request.
func TestAbandonFromOpenedClearsWithoutASeal(t *testing.T) {
	t.Parallel()
	phase, clears, err := statelog.AbandonPhase(operation(coord.PhaseOpened))
	if err != nil {
		t.Fatalf("AbandonPhase: %v", err)
	}
	if !clears || phase != coord.PhaseOpened {
		t.Fatalf("abandoning from opened gave (%s, clears=%v), want an outright "+
			"clear — no configuration request has been issued, so there is "+
			"nothing to retire", phase, clears)
	}
	for _, from := range []coord.MaintenancePhase{
		coord.PhaseBaselined, coord.PhaseApplied,
		coord.PhaseSealing, coord.PhaseSealed,
	} {
		phase, clears, err := statelog.AbandonPhase(operation(from))
		if err != nil {
			t.Fatalf("AbandonPhase from %s: %v", from, err)
		}
		if clears || phase != coord.PhaseSealing {
			t.Fatalf("abandoning from %s gave (%s, clears=%v), want a seal — a "+
				"paused coordinator's request is outstanding whether or not a "+
				"person has looked at it", from, phase, clears)
		}
	}
}

// TestAnUnknownDeleteIsResolvedByReadingNotByAssuming, in its three arms.
//
// A conditional delete whose response was lost MAY have committed, so neither
// "the record is unchanged" nor its opposite is safe to assume.
func TestAnUnknownDeleteIsResolvedByReadingNotByAssuming(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		found bool
		op    coord.MaintenanceOperation
		want  statelog.DeleteOutcome
		why   string
	}{
		{
			name: "the record is still there at this operation", found: true,
			op: operation(coord.PhaseConfirmed), want: statelog.DeleteRetry,
			why: "the delete was never issued or never committed, and a " +
				"successor completes it",
		},
		{
			name: "the key is absent", found: false,
			want: statelog.DeleteDone,
			why:  "the delete committed and its response was lost",
		},
		{
			name: "the key holds a LATER operation", found: true,
			op: coord.MaintenanceOperation{
				Stream: "CREWLET_TRACKER_LOG", OperationID: "op-2",
				Phase: coord.PhaseOpened, Attempt: 1,
			},
			want: statelog.DeleteNotMine,
			why: "reading this as \"my delete failed, retry\" deletes the NEXT " +
				"operation's record and silently releases a fleet that is " +
				"still in maintenance",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := statelog.ResolveDelete("op-1", c.op, c.found); got != c.want {
				t.Fatalf("ResolveDelete = %q, want %q — %s", got, c.want, c.why)
			}
		})
	}
}

// TestAnAdmissionBlocksTheExclusion: a positive record from each side is what
// makes either order safe. A node that slipped between the coordinator's check
// and its own start leaves a durable key rather than a silence.
func TestAnAdmissionBlocksTheExclusion(t *testing.T) {
	t.Parallel()
	admissions := []coord.Admission{
		{NodeID: "node-2", Incarnation: "node-2:x"},
		{NodeID: "node-1", Incarnation: "node-1:x"},
	}
	blocking := statelog.AdmissionBlocks(admissions, nil)
	if len(blocking) != 2 || blocking[0] != "node-1" {
		t.Fatalf("AdmissionBlocks = %v, want both nodes in order", blocking)
	}
	if got := statelog.AdmissionBlocks(admissions, []string{"node-1", "node-2"}); len(got) != 0 {
		t.Fatalf("excluded nodes still blocked: %v", got)
	}
}

// TestNoModeButNormalPublishes is the static claim maintenance rests on.
func TestNoModeButNormalPublishes(t *testing.T) {
	t.Parallel()
	for _, mode := range statelog.MaintenanceModes {
		if got, want := mode.Publishes(), mode == statelog.ModeNormal; got != want {
			t.Fatalf("mode %q publishes = %v, want %v", mode, got, want)
		}
	}
}
