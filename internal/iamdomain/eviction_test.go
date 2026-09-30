package iamdomain_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE'S EVICTION IS WRITTEN ON THIS LOG, BY THE DEPLOYMENT, AT ITS OWN
// POSITION — and a retry of it after a readmission is refused rather than
// answered as though it still held.
//
// Nothing wrote one: this log had an applier, a fence and a table for the gate
// and no writer, so the engine's eviction lifted the tracker's pin on its trim
// and left this one's where it was. And the record, when there was one to
// write, stated the positions it took effect at as FIELDS — which a writer
// cannot know, since only the broker says where a record lands.
//
// Mutation: take the grant off [iamdomain.Writer.mayOperate] and the
// administrator's eviction lands; state the boundary from anything but the
// record's own position and the row names another; drop the standing and the
// stale retry answers applied.
func TestANodeGateIsTheDeploymentsAndLandsAtItsOwnPosition(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const node = "node-z"

	// ADMINISTERING PEOPLE CONFERS NO SAY OVER A NODE.
	admin := rig.writer.As(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iamdomain.AdminGrant}))
	if _, err := admin.EvictNode(rig.t.Context(), statelog.NewOpID(time.Now(), "evict-"+node),
		node); !errors.Is(err, iamdomain.ErrRefused) {
		t.Fatalf("an eviction by a people administrator answered %v, want %v",
			err, iamdomain.ErrRefused)
	}

	operator := rig.writer.As(principalNamed("ops:fleet", iam.KindMachine,
		[]iam.Grant{iam.GrantFleetOperate}))
	gate := func(op string, readmit bool) (statelog.Result, error) {
		var result statelog.Result
		err := rig.during(func() error {
			var err error
			if readmit {
				result, err = operator.ReadmitNode(rig.t.Context(), op, node)
			} else {
				result, err = operator.EvictNode(rig.t.Context(), op, node)
			}
			return err
		})
		return result, err
	}
	evictOp := statelog.NewOpID(time.Now(), "evict-"+node)
	evicted, err := gate(evictOp, false)
	if err != nil || evicted.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the eviction answered %+v (%v), want applied", evicted, err)
	}
	row := standingOf(t, rig, node)
	if row.Back || row.From != uint64(evicted.Position.Packed()) || row.By != "ops:fleet" {
		t.Fatalf("the eviction row reads %+v, want %s out from its own position "+
			"%d, by ops:fleet", row, node, evicted.Position.Packed())
	}
	if on, found := evictedOnLog(t, rig, node); !found || !on {
		t.Errorf("the log's own reading of %s is evicted %t (found %t), want "+
			"evicted", node, on, found)
	}

	readmitted, err := gate(statelog.NewOpID(time.Now(), "readmit-"+node), true)
	if err != nil || readmitted.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the readmission answered %+v (%v), want applied", readmitted, err)
	}
	row = standingOf(t, rig, node)
	if !row.Back || row.Readmitted != uint64(readmitted.Position.Packed()) ||
		row.From != uint64(evicted.Position.Packed()) {
		t.Fatalf("after the readmission the row reads %+v, want it kept and "+
			"back from %d", row, readmitted.Position.Packed())
	}
	if on, found := evictedOnLog(t, rig, node); !found || on {
		t.Errorf("the log's own reading of %s is evicted %t (found %t), want "+
			"readmitted", node, on, found)
	}

	// THE EVICTION'S RETRY, after the readmission inverted it, is not a
	// gesture that still holds.
	_, err = gate(evictOp, false)
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) || refused.Reason != statelog.ReasonSuperseded {
		t.Errorf("the eviction retried after its readmission answered %v, want "+
			"it refused as superseded", err)
	}
}

// standingOf is node's one eviction row on this log, as the trim lists it.
func standingOf(t *testing.T, rig *writeRig, node string) statelog.EvictionRow {
	t.Helper()
	rows, err := iamdomain.Domain{}.Evictions(t.Context(), rig.db)
	if err != nil {
		t.Fatalf("list the evictions: %v", err)
	}
	var found []statelog.EvictionRow
	for _, row := range rows {
		if row.NodeID == node {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the evictions list %d rows for %s, want one", len(found), node)
	}
	return found[0]
}

// evictedOnLog is node's standing read straight off the log, through the
// verified reader every standing read is handed — the probe half of the
// contract.
func evictedOnLog(t *testing.T, rig *writeRig, node string) (bool, bool) {
	t.Helper()
	evicted, found, err := statelog.EvictedOnLog(t.Context(), iamdomain.Domain{},
		standingLog{VerifiedLog: statelog.VerifiedLog{Log: rig.log, Verifier: rig.verifier},
			last: rig.log}, node)
	if err != nil {
		t.Fatalf("read %s's standing off the log: %v", node, err)
	}
	return evicted, found
}

// standingLog is the rig's log as a standing read takes it: the per-subject
// probe, and every record read back opened out of its signed frame.
type standingLog struct {
	statelog.VerifiedLog
	last interface {
		LastSeq(ctx context.Context, subject string) (uint64, bool, error)
	}
}

func (l standingLog) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	return l.last.LastSeq(ctx, subject)
}

// A GENERATION RECORD NAMES WHO MOVED THIS LOG AS THE PRINCIPAL THEY ARE, AND
// CARRIES THE STAMP OF THE GENERATION IT OPENS.
//
// The record is the identity estate's [statelog.GenerationEncoder] half, and
// nothing wrote one: this log had an applier for it and no encoder, so a
// reanchor could not name it at all. Two things about it are easy to get
// wrong and silent when they are. The stamp — the generation it opens and the
// node that wrote it — is what the eviction gate and a reader of the log's
// openers read. And the author's kind is this domain's PRINCIPAL kind, read
// back from the kind the reanchor verb states: an operator acting under their
// own login is an unbound person or a machine, and which one is the login's
// shape — every operator recorded as a machine made an administrator outside
// the org chart read as a service account.
//
// Mutation: drop the login-shape split and the administrator is a machine;
// stamp the record with anything but the facts' generation and writer and the
// envelope names another.
func TestAGenerationRecordNamesItsAuthorAndCarriesItsStamp(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		facts statelog.GenerationFacts
		actor string
		kind  iam.Kind
	}{
		"a person acting as their seat": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: string(iam.ActorHuman)},
			actor: "eng", kind: iam.KindPerson,
		},
		"an administrator in no seat": {
			facts: statelog.GenerationFacts{By: "jane.doe",
				ByKind: string(iam.ActorOperator)},
			actor: "jane.doe", kind: iam.KindPerson,
		},
		"a Tier A token": {
			facts: statelog.GenerationFacts{By: "token:ops",
				ByKind: string(iam.ActorOperator)},
			actor: "token:ops", kind: iam.KindMachine,
		},
		"a service account named by its principal kind": {
			facts: statelog.GenerationFacts{By: "ci:release",
				ByKind: string(iam.KindMachine)},
			actor: "ci:release", kind: iam.KindMachine,
		},
		"a person named by their principal kind": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: string(iam.KindPerson)},
			actor: "eng", kind: iam.KindPerson,
		},
		"a seat": {
			facts: statelog.GenerationFacts{By: "eng", ByKind: string(iam.ActorAgent)},
			actor: "eng", kind: iam.KindSeat,
		},
		"the engine": {
			facts: statelog.GenerationFacts{By: "engine", ByKind: string(iam.ActorSystem)},
			actor: "engine", kind: iam.KindEngine,
		},
		"a person's login with no kind stated": {
			facts: statelog.GenerationFacts{By: "jane.doe"},
			actor: "jane.doe", kind: iam.KindPerson,
		},
		"a token with no kind stated": {
			facts: statelog.GenerationFacts{By: "token:ops"},
			actor: "token:ops", kind: iam.KindMachine,
		},
		// NOBODY NAMED IS THE NODE, as its own writer records itself —
		// never an empty author column.
		"nobody named": {
			facts: statelog.GenerationFacts{},
			actor: "node-a", kind: iam.KindMachine,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			facts := tc.facts
			facts.Generation, facts.Writer, facts.At = 2, "node-a", at
			encoder := iamdomain.GenerationRecord{}
			record, keeps, err := encoder.GenerationRecord(facts)
			if err != nil || !keeps {
				t.Fatalf("GenerationRecord = (keeps %v, %v), want a record", keeps, err)
			}
			subject, keeps := encoder.GenerationSubject(2)
			if !keeps || record.Subject != subject {
				t.Errorf("the record is published on %v, and generation 2's "+
					"subject is %v (kept %v)", record.Subject, subject, keeps)
			}
			if record.OpID != facts.OpID() {
				t.Errorf("the record's operation is %q, want the facts' own %q",
					record.OpID, facts.OpID())
			}
			env, err := iamdomain.Domain{}.Envelope(record.Payload)
			if err != nil {
				t.Fatalf("decode the envelope: %v", err)
			}
			if env.Gen != 2 || env.Writer != "node-a" || env.OpID != record.OpID ||
				env.V != iamdomain.BaseRecordVersion {
				t.Errorf("the envelope reads generation %d, writer %q, op %q, "+
					"version %d — want the generation it opens, the node that "+
					"wrote it, its own operation and the base version a peer "+
					"never defers", env.Gen, env.Writer, env.OpID, env.V)
			}
			decoded, err := iamdomain.Decode(record.Payload)
			if err != nil {
				t.Fatalf("decode the record: %v", err)
			}
			if decoded.Actor != tc.actor || decoded.ActorKind != tc.kind {
				t.Errorf("the record is authored (%q, %s), want (%q, %s)",
					decoded.Actor, decoded.ActorKind, tc.actor, tc.kind)
			}
			generation, err := iamdomain.DecodeGeneration(decoded.Mutation)
			if err != nil {
				t.Fatalf("decode the generation: %v", err)
			}
			if generation.By != tc.actor || generation.Generation != 2 {
				t.Errorf("the generation names %q opening %d, want %q opening 2",
					generation.By, generation.Generation, tc.actor)
			}
		})
	}
}
