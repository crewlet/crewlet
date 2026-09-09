package coordtest

import (
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// maintenanceCases certify the capacity window: the one record whose existence
// is the exclusion, the admissions that block taking it, and the
// acknowledgements that seal it.
//
// Every case here names a schedule that admitted a publisher while a resize
// was unresolved, which is the whole failure the mode exists to prevent.
var maintenanceCases = []fleetCase{
	{"two coordinators cannot both open a window", func(h *fleetHarness) {
		// FIRST-WRITER-WINS, and the loser has to be able to tell "my
		// own earlier attempt won" from "somebody else's operation is
		// here": the id is minted once and reused across create
		// attempts, because an unknown create establishes nothing and
		// is retried.
		mine := window("op-1")
		got, won, err := h.f.OpenMaintenance(h.ctx, mine)
		if err != nil || !won {
			h.t.Fatalf("the first open won=%v (err %v)", won, err)
		}
		if got.Revision == 0 {
			h.t.Fatal("the opened operation came back with no revision, so no " +
				"later mutation can be conditional on anything")
		}

		theirs := window("op-2")
		held, won, err := h.f.OpenMaintenance(h.ctx, theirs)
		if err != nil {
			h.t.Fatalf("the second open: %v", err)
		}
		if won {
			h.t.Fatal("two coordinators both opened a window on one stream — " +
				"both believe they have exclusion and both may issue a " +
				"configuration request")
		}
		if held.OperationID != "op-1" {
			h.t.Fatalf("the loser was told %q holds the key, want op-1", held.OperationID)
		}

		// A RETRY OF MY OWN CREATE reports my own id, which is what
		// makes an unknown create safe to repeat.
		held, won, err = h.f.OpenMaintenance(h.ctx, mine)
		if err != nil || won {
			h.t.Fatalf("re-opening won=%v (err %v)", won, err)
		}
		if held.OperationID != "op-1" {
			h.t.Fatalf("a retry of my own create reports %q", held.OperationID)
		}
	}},

	{"a stale coordinator's write is refused by the revision", func(h *fleetHarness) {
		// NOT BY THE OPERATION ID. A successor RESUMES the same id, so
		// an id comparison checks the one quantity guaranteed not to
		// change — and a coordinator that read `opened`, lost its lease
		// and came back would commit over a successor that has since
		// reached `baselined` and possibly issued a request.
		opened, _, err := h.f.OpenMaintenance(h.ctx, window("op-1"))
		if err != nil {
			h.t.Fatalf("OpenMaintenance: %v", err)
		}
		stale := opened

		successor := opened
		successor.Phase = coord.PhaseBaselined
		successor.WriteIncarnations = map[string]string{"node-1": "node-1:a"}
		if err := h.f.UpdateMaintenance(h.ctx, successor); err != nil {
			h.t.Fatalf("the successor's write: %v", err)
		}

		stale.Phase = coord.PhaseConfirmed
		err = h.f.UpdateMaintenance(h.ctx, stale)
		if !errors.Is(err, coord.ErrMaintenanceMoved) {
			h.t.Fatalf("a stale coordinator's write returned %v, want a moved "+
				"refusal — it holds the same operation id as the successor, so "+
				"an id check would have let it through", err)
		}

		held, found, err := h.f.Maintenance(h.ctx, opened.Stream)
		if err != nil || !found {
			h.t.Fatalf("Maintenance: found=%v err=%v", found, err)
		}
		if held.Phase != coord.PhaseBaselined {
			h.t.Fatalf("the record is in phase %s — the stale write landed", held.Phase)
		}
	}},

	{"an interrupted activation is not mistaken for completed cleanup", func(h *fleetHarness) {
		// THE EXCLUSION IS THE RECORD. Held as two, a crash after the
		// exclusion and before the operation is byte-identical to a
		// finished resize whose cleanup was interrupted — and any rule
		// that reads one reads the other, releasing the exclusion while
		// a coordinator is still preparing.
		opened, _, err := h.f.OpenMaintenance(h.ctx, window("op-1"))
		if err != nil {
			h.t.Fatalf("OpenMaintenance: %v", err)
		}
		// Nothing else happened: no lease, no acknowledgement.
		held, found, err := h.f.Maintenance(h.ctx, opened.Stream)
		if err != nil {
			h.t.Fatalf("Maintenance: %v", err)
		}
		if !found {
			h.t.Fatal("an interrupted activation left no record at all, so a " +
				"booting node reads no maintenance and starts publishing while " +
				"a coordinator is preparing one")
		}
		if held.Phase != coord.PhaseOpened {
			h.t.Fatalf("the interrupted activation is in phase %s, want opened — "+
				"which is what tells a successor to resume rather than to "+
				"conclude the resize finished", held.Phase)
		}
	}},

	{"closing is conditional, so an interrupted delete cannot take a successor's window",
		func(h *fleetHarness) {
			opened, _, err := h.f.OpenMaintenance(h.ctx, window("op-1"))
			if err != nil {
				h.t.Fatalf("OpenMaintenance: %v", err)
			}
			stale := opened.Revision

			moved := opened
			moved.Phase = coord.PhaseBaselined
			if err := h.f.UpdateMaintenance(h.ctx, moved); err != nil {
				h.t.Fatalf("UpdateMaintenance: %v", err)
			}
			if err := h.f.CloseMaintenance(h.ctx, opened.Stream, "op-1", stale); !errors.Is(
				err, coord.ErrMaintenanceMoved) {
				h.t.Fatalf("a delete at a stale revision returned %v, want a "+
					"moved refusal — an unconditional one commits against "+
					"whatever the record has since become", err)
			}
			if _, found, _ := h.f.Maintenance(h.ctx, opened.Stream); !found {
				h.t.Fatal("the stale delete released the exclusion anyway")
			}

			current, _, err := h.f.Maintenance(h.ctx, opened.Stream)
			if err != nil {
				h.t.Fatalf("Maintenance: %v", err)
			}
			if err := h.f.CloseMaintenance(h.ctx, opened.Stream, "op-1",
				current.Revision); err != nil {
				h.t.Fatalf("closing at the current revision: %v", err)
			}
			if _, found, _ := h.f.Maintenance(h.ctx, opened.Stream); found {
				h.t.Fatal("the window survived its own close")
			}
		}},

	{"an operation that says nothing is refused", func(h *fleetHarness) {
		for _, c := range []struct {
			name string
			op   coord.MaintenanceOperation
		}{
			{"no stream", coord.MaintenanceOperation{
				OperationID: "op-1", Phase: coord.PhaseOpened, Attempt: 1,
				TargetMaxBytes: 1, Participants: []string{"node-1"}}},
			{"no id", coord.MaintenanceOperation{
				Stream: "S", Phase: coord.PhaseOpened, Attempt: 1,
				TargetMaxBytes: 1, Participants: []string{"node-1"}}},
			{"an unknown phase", coord.MaintenanceOperation{
				Stream: "S", OperationID: "op-1", Phase: "later", Attempt: 1,
				TargetMaxBytes: 1, Participants: []string{"node-1"}}},
			{"attempt zero", coord.MaintenanceOperation{
				Stream: "S", OperationID: "op-1", Phase: coord.PhaseOpened,
				TargetMaxBytes: 1, Participants: []string{"node-1"}}},
			{"no participants", coord.MaintenanceOperation{
				Stream: "S", OperationID: "op-1", Phase: coord.PhaseOpened,
				Attempt: 1, TargetMaxBytes: 1}},
		} {
			if _, _, err := h.f.OpenMaintenance(h.ctx, c.op); err == nil {
				h.t.Errorf("an operation with %s was written", c.name)
			}
		}
	}},

	{"an admission round-trips and is withdrawn only by its own incarnation",
		func(h *fleetHarness) {
			// A POSITIVE RECORD FROM EACH SIDE. A coordinator checking
			// for the ABSENCE of a publisher is back to check-then-act:
			// a node that read the operation absent and started in the
			// interval leaves this key, where a silence would have let
			// the coordinator proceed.
			if err := h.f.PutAdmission(h.ctx, coord.Admission{
				NodeID: "node-1", Incarnation: "node-1:a",
			}); err != nil {
				h.t.Fatalf("PutAdmission: %v", err)
			}
			admissions, err := h.f.Admissions(h.ctx)
			if err != nil || len(admissions) != 1 {
				h.t.Fatalf("Admissions = %v (err %v)", admissions, err)
			}
			if admissions[0].At.IsZero() {
				h.t.Fatal("the admission carries no instant")
			}

			// A LATE CLEANUP against the crashed process's identity
			// must not remove the NEWER one's.
			if err := h.f.PutAdmission(h.ctx, coord.Admission{
				NodeID: "node-1", Incarnation: "node-1:b",
			}); err != nil {
				h.t.Fatalf("the replacement process's admission: %v", err)
			}
			if err := h.f.ForgetAdmission(h.ctx, "node-1", "node-1:a"); err != nil {
				h.t.Fatalf("ForgetAdmission: %v", err)
			}
			admissions, err = h.f.Admissions(h.ctx)
			if err != nil || len(admissions) != 1 {
				h.t.Fatalf("a stale withdrawal removed a live admission: %v (err %v)",
					admissions, err)
			}
			if err := h.f.ForgetAdmission(h.ctx, "node-1", "node-1:b"); err != nil {
				h.t.Fatalf("withdrawing its own: %v", err)
			}
			if admissions, _ := h.f.Admissions(h.ctx); len(admissions) != 0 {
				h.t.Fatalf("%d admission(s) survive their own withdrawal", len(admissions))
			}
		}},

	{"an acknowledgement round-trips and never reads back as anything else",
		func(h *fleetHarness) {
			// FOUR OTHER KEY CLASSES SHARE THIS BUCKET, so the filters
			// are what make it safe. An acknowledgement decoded as a
			// positions row is a node that has applied nothing, and a
			// positions row decoded as an acknowledgement seals a
			// barrier that never ran.
			if err := h.f.PutPositions(h.ctx, coord.NodePositions{
				NodeID:  "node-1",
				Domains: map[string]coord.DomainPosition{"tracker": {Seq: 90}},
			}); err != nil {
				h.t.Fatalf("PutPositions: %v", err)
			}
			if _, _, err := h.f.OpenMaintenance(h.ctx, window("op-1")); err != nil {
				h.t.Fatalf("OpenMaintenance: %v", err)
			}
			if err := h.f.PutAdmission(h.ctx, coord.Admission{
				NodeID: "node-2", Incarnation: "node-2:a",
			}); err != nil {
				h.t.Fatalf("PutAdmission: %v", err)
			}
			ack := coord.MaintenanceAck{
				NodeID: "node-1", OperationID: "op-1", Attempt: 1,
				Incarnation: "node-1:new", Mode: "seal",
			}
			if err := h.f.PutMaintenanceAck(h.ctx, ack); err != nil {
				h.t.Fatalf("PutMaintenanceAck: %v", err)
			}

			acks, err := h.f.MaintenanceAcks(h.ctx)
			if err != nil || len(acks) != 1 {
				h.t.Fatalf("MaintenanceAcks = %v (err %v)", acks, err)
			}
			if acks[0].Incarnation != "node-1:new" || acks[0].Mode != "seal" {
				h.t.Fatalf("the acknowledgement came back %+v", acks[0])
			}
			// AND EVERY OTHER CLASS STILL READS ITS OWN.
			if nodes, err := h.f.Positions(h.ctx); err != nil || len(nodes) != 1 {
				h.t.Fatalf("the positions listing sees %d row(s) (err %v)", len(nodes), err)
			}
			if admissions, err := h.f.Admissions(h.ctx); err != nil || len(admissions) != 1 {
				h.t.Fatalf("the admission listing sees %d row(s) (err %v)",
					len(admissions), err)
			}
			if _, found, err := h.f.Maintenance(h.ctx, "CREWLET_TRACKER_LOG"); err != nil || !found {
				h.t.Fatalf("the operation is not readable beside them (err %v)", err)
			}
		}},

	{"an acknowledgement that establishes nothing is refused", func(h *fleetHarness) {
		for _, c := range []struct {
			name string
			ack  coord.MaintenanceAck
		}{
			{"no node", coord.MaintenanceAck{
				OperationID: "op-1", Attempt: 1, Incarnation: "x"}},
			{"no operation", coord.MaintenanceAck{
				NodeID: "node-1", Attempt: 1, Incarnation: "x"}},
			{"attempt zero", coord.MaintenanceAck{
				NodeID: "node-1", OperationID: "op-1", Incarnation: "x"}},
			{"no incarnation", coord.MaintenanceAck{
				NodeID: "node-1", OperationID: "op-1", Attempt: 1}},
		} {
			if err := h.f.PutMaintenanceAck(h.ctx, c.ack); err == nil {
				h.t.Errorf("an acknowledgement with %s was written", c.name)
			}
		}
	}},
}

// window is a capacity operation as a coordinator first writes it.
func window(id string) coord.MaintenanceOperation {
	return coord.MaintenanceOperation{
		Stream:           "CREWLET_TRACKER_LOG",
		OperationID:      id,
		TargetMaxBytes:   8 << 30,
		OriginalMaxBytes: 4 << 30,
		Phase:            coord.PhaseOpened,
		Attempt:          1,
		Participants:     []string{"node-1", "node-2"},
		EnteredAt:        time.Now().UTC(),
		By:               "ops-3",
	}
}
