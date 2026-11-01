package executordomain_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE EXECUTOR DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, from its
// declaration through its envelope, determinism, idempotency, versioned fields,
// stamp, evictions and node gates, exactly as every other domain is.
func TestTheExecutorDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  executordomain.Domain{},
			Applier: executordomain.NewApplier(),
			// Migrate is nil: these tables ship in the replicated estate's
			// own migration (0073), so a fresh store already has them.
			Encode: encodeSuiteRecord,
			// THE VERSIONED-FIELD TABLE is empty: every field this domain
			// writes is in the base format.
			Fields:     executordomain.VersionedFields(),
			Kinds:      kindNames(),
			Rows:       executordomain.NewRows,
			Write:      suiteWrite,
			EncodeGate: encodeSuiteGate,
		}
	})
}

// THE EXECUTOR GATE READER KEEPS THE RULE EVERY READER SHARES, on the one gate
// this log has: a writer's eviction window, half-open at both ends, on every
// kind the applier drops it on. A tombstone is a guard rather than a gate, so
// there is no deletion family to run.
func TestTheExecutorGateReaderKeepsTheEvictionWindow(t *testing.T) {
	t.Parallel()
	statelogtest.RunEvictionGates(t, func(t *testing.T) statelogtest.EvictionGateCandidate {
		return statelogtest.EvictionGateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  executordomain.Domain{},
				Applier: executordomain.NewApplier(),
				Encode:  encodeSuiteRecord,
				Kinds:   kindNames(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return executordomain.NewGates(db) },
			Kind:         string(executordomain.KindConversation),
			SubjectKinds: kindNames(),
			Create: func(id, writer, opID string) ([]byte, error) {
				return captureRecord(id, writer, opID, 1)
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return captureRecord(id, writer, opID, 2)
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(executordomain.EvictionSubject(node), writer, opID,
					executordomain.OpEviction, executordomain.ScopeSet{Subject: true},
					evictionOf(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(executordomain.EvictionSubject(node), writer, opID,
					executordomain.OpEviction, executordomain.ScopeSet{Subject: true},
					evictionOf(node, true))
			},
			// THE EVICTION IS THE ONLY GATE: said with an empty map, which
			// is an answer, where nil would be one left unsaid.
			ApplyGates: map[string]func(id, writer, opID string) ([]byte, error){},
		}
	})
}

// kindNames is every kind the domain writes, from the enum.
func kindNames() []string {
	out := make([]string, 0, len(executordomain.ObjectKinds))
	for _, k := range executordomain.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// suiteStamped is the instant every suite record is stamped at.
var suiteStamped = time.Unix(1_700_000_000, 0).UTC()

// suiteConversation is the conversation every suite capture is about.
const suiteConversation = "chat:suite"

// suiteTurn is the turn a suite record about id is about: derived, so the
// same id names the same turn in every record the suite builds.
func suiteTurn(id string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("turn/"+id)).String()
}

// suiteObject is a valid object key, minted at the suite's instant.
var suiteObject = objstore.KeyAt(suiteStamped).String()

// captureOf is a capture of id's turn at capture ordinal seq.
func captureOf(id string, seq int64) executordomain.Capture {
	return executordomain.Capture{
		V: 1, Handle: id, Turn: suiteTurn(id), ConversationKey: suiteConversation,
		Harness: "claude-code", SessionID: "session-" + id, Object: suiteObject,
		ObjectHash: string(objstore.HashOf([]byte(id))), ObjectSize: 1024,
		ContentKey: "k1", ArchiveFormat: 1, LaunchSeq: 1, CaptureSeq: seq,
		SeatBudget: executordomain.SealedMaxBytes,
	}
}

// captureRecord is a capture of id's turn, written by writer.
func captureRecord(id, writer, opID string, seq int64) ([]byte, error) {
	turn := suiteTurn(id)
	return rawRecord(executordomain.ConversationSubject(id, suiteConversation, turn), writer,
		opID, executordomain.OpCapture, seatScope(id), captureOf(id, seq))
}

// seatScope is the scope a capture declares: its seat.
func seatScope(handle string) executordomain.ScopeSet {
	return executordomain.ScopeSet{Terms: []executordomain.ScopeTerm{
		{Kind: executordomain.TermSeat, Seat: handle}}}
}

// encodeSuiteRecord builds one valid record of each kind about id at any
// version.
//
// THE FIRST THREE KINDS ARE A REAL SEQUENCE: a turn's capture writes a row, its
// seat's removal deletes the row and tombstones the live turn, and a node's
// eviction writes the gate's row.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	subject, op, scope, payload := suitePayload(executordomain.ObjectKind(kind), id)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return executordomain.Encode(executordomain.MutationRecord{
		RecordEnvelope: executordomain.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: "suite-node", Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorSystem,
	})
}

// suitePayload is one valid (subject, op, scope, payload) per kind about id.
func suitePayload(kind executordomain.ObjectKind, id string) (
	executordomain.Subject, executordomain.OpKind, executordomain.ScopeSet, any) {

	sentinel := executordomain.ScopeSet{Subject: true}
	switch kind {
	case executordomain.KindConversation:
		return executordomain.ConversationSubject(id, suiteConversation, suiteTurn(id)),
			executordomain.OpCapture, seatScope(id), captureOf(id, 1)
	case executordomain.KindSeat:
		return executordomain.SeatSubject(id), executordomain.OpSeatDelete, sentinel,
			executordomain.SeatDelete{V: 1, Handle: id}
	case executordomain.KindEviction:
		return executordomain.EvictionSubject(id), executordomain.OpEviction, sentinel,
			evictionOf(id, false)
	case executordomain.KindGeneration:
		return executordomain.GenerationSubject(2), executordomain.OpGeneration, sentinel,
			executordomain.Generation{V: 1, Generation: 2, By: "suite", StreamAt: suiteStamped}
	case executordomain.KindBarrier:
		return executordomain.Subject{Kind: kind, ID: id}, executordomain.OpBarrier, sentinel, nil
	}
	return executordomain.Subject{Kind: kind, ID: id}, executordomain.OpSeatDelete, sentinel,
		executordomain.SeatDelete{V: 1, Handle: id}
}

// evictionOf is node's eviction, or its readmission.
func evictionOf(node string, readmit bool) executordomain.Eviction {
	return executordomain.Eviction{
		V: executordomain.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: suiteStamped, Readmitted: readmit,
	}
}

// rawRecord is one record the gate family applies, written by writer.
func rawRecord(subject executordomain.Subject, writer, opID string, op executordomain.OpKind,
	scope executordomain.ScopeSet, payload any) ([]byte, error) {

	var body json.RawMessage
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = encoded
	}
	return executordomain.Encode(executordomain.MutationRecord{
		RecordEnvelope: executordomain.RecordEnvelope{
			V: executordomain.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: writer, Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorSystem,
	})
}

// encodeSuiteGate is the eviction record a peer publishes onto this log.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return rawRecord(executordomain.EvictionSubject(node), "suite-peer", opID,
		executordomain.OpEviction, executordomain.ScopeSet{Subject: true},
		evictionOf(node, readmit))
}

// suiteWrite is one write through the domain's own production writer, which is
// where the framework's stamp is kept or lost: a turn's end, which needs no
// object uploaded first.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	w, err := executordomain.NewWriter(executordomain.WriterDeps{Publisher: pub, DB: db})
	if err != nil {
		return err
	}
	_, err = w.End(ctx, executordomain.Call{
		By:   iam.Actor{Name: "suite", Kind: iam.ActorSystem},
		OpID: statelog.NewOpID(time.Now(), "suite"),
	}, executordomain.NewEnd{
		Handle: "suite-seat", Turn: uuid.NewString(), ConversationKey: suiteConversation,
		Ask: "Suite", Ending: executordomain.Ending{Outcome: executordomain.OutcomeCompleted},
	})
	return err
}
