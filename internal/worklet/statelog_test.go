package worklet_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE TRACKER IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, not by a second one
// written beside it.
//
// The properties every data node's copy rests on — the declaration is
// consistent, an unknown version still yields an envelope, the same records
// produce the same rows, and applying a record twice is applying it once —
// belong to the framework and are the same for every domain it carries. A
// domain-specific copy of them would be a second opinion about a contract the
// framework enforces, and the first time the two disagreed the domain's own
// would be the one that ran.
func TestTheTrackerIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  worklet.Domain{},
			Applier: worklet.NewApplier("suite-node", nil),
			// Migrate is nil: the tracker's tables ship in the
			// replicated estate's own migration, so a fresh store
			// already has them. A domain that created its tables from
			// test code would be a schema the suite proved and the
			// migration did not.
			Encode: encodeSuiteRecord,
			// EVERY ARBITRATED KIND, with the three the apply cases
			// use first. The declaration case checks that a domain
			// publishes every kind it arbitrates — an anchor row for a
			// kind nothing writes is a row nothing ever reads — so a
			// partial list here would report the FIXTURE as the fault.
			Kinds: suiteKinds(),
			// THE PRODUCTION TABLE. It is empty — every field the
			// tracker writes is in the base format — so there is no
			// record to carry; the field this build's successor adds
			// brings a Carrying case built through the writer's own
			// encoder, which the suite refuses to run without.
			Fields:     worklet.VersionedFields(),
			Rows:       worklet.NewRows,
			Write:      suiteWrite,
			EncodeGate: encodeSuiteGate,
		}
	})
}

// encodeSuiteGate is the eviction record a peer's writer publishes onto this
// log — or, with readmit, the inverse commit that takes the node back.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	at := time.Unix(1_700_000_000, 0).UTC()
	body, err := json.Marshal(worklet.Eviction{
		V: worklet.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: at, Readmitted: readmit,
	})
	if err != nil {
		return nil, err
	}
	op := "suite-evict-" + node
	if readmit {
		op = "suite-readmit-" + node
	}
	return worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			// PINNED, as the production writer pins it: a gate is read
			// by every build there will ever be ([worklet.GateRecordVersion]).
			V: worklet.GateRecordVersion, OpID: op,
			Subject: worklet.EvictionSubject(node), Op: worklet.OpEviction,
			CreatedAt: at, Writer: "suite-peer",
			Scope: worklet.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "suite", ActorKind: worklet.AuthorSystem,
	}.Encode()
}

// suiteWrite is one write through Worklet's own [worklet.Writer], the
// builder every write path in the domain shares, which is where the
// framework's stamp is kept or lost.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	w, err := worklet.NewWriter(worklet.WriterDeps{
		Publisher: pub, DB: db, NodeID: statelogtest.SuiteWriter,
		Actor: "suite", ActorKind: worklet.AuthorSystem,
	})
	if err != nil {
		return err
	}
	_, err = w.EvictNode(ctx, "suite-evict", "node-away")
	return err
}

// encodeSuiteRecord builds one valid record at an arbitrary version.
//
// IT ENCODES AT ANY VERSION, including one above this build's, because that is
// exactly what a newer peer publishes and what this build has to retain — a
// writer that refused would leave the deferral contract untestable.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	at := time.Unix(1_700_000_000, 0).UTC()
	subject := worklet.Subject{Kind: worklet.ObjectKind(kind), ID: id}

	var payload any
	op := worklet.OpCreate
	switch worklet.ObjectKind(kind) {
	case worklet.KindProject:
		payload = worklet.Project{
			V: worklet.DocumentVersion, Key: id, Name: "Suite " + id,
			CreatedAt: at, UpdatedAt: at,
		}
	case worklet.KindTask:
		payload = worklet.Task{
			V: worklet.DocumentVersion, ID: id, Key: "SUITE-1",
			Project: "SUITE", Type: "task", Title: "suite " + id,
			Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted,
			Priority: worklet.PriorityNone, Rank: worklet.RankOrigin,
			CreatedAt: at, UpdatedAt: at,
		}
	case worklet.KindView:
		payload = worklet.View{
			V: worklet.DocumentVersion, ID: id, Name: "suite " + id,
			Type:      worklet.ViewList,
			Container: worklet.Container{Kind: "workspace", ID: ""},
			CreatedAt: at, UpdatedAt: at,
		}
	default:
		payload = map[string]any{"id": id}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	record := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Gen: 0, Writer: "suite-node",
			Scope: worklet.ScopeSet{Subject: true},
		},
		Mutation:  body,
		Actor:     "suite",
		ActorKind: worklet.AuthorSystem,
	}
	// VERSION ZERO IS THE WRITER'S PATH: the domain's own Encode stamps it,
	// which is what the suite's stamping case reads back. Any other version
	// is ENCODED DIRECTLY, because the suite needs exactly the record a peer
	// at that version would publish — including one above this build's —
	// and not the version this build would have chosen for it.
	if version == 0 {
		return record.Encode()
	}
	return json.Marshal(record)
}

// suiteKinds is every kind the domain arbitrates, ordered so the three the
// apply cases exercise come first: a whole-document object, the task, and a
// second whole-document object with a different table shape.
func suiteKinds() []string {
	first := []worklet.ObjectKind{
		worklet.KindProject, worklet.KindTask, worklet.KindView,
	}
	out := make([]string, 0, len(worklet.ObjectKinds))
	for _, k := range first {
		out = append(out, string(k))
	}
	for _, k := range worklet.ObjectKinds {
		if !k.Arbitrated() || slices.Contains(first, k) {
			continue
		}
		out = append(out, string(k))
	}
	return out
}

// THE TRACKER'S GATE READER KEEPS THE RULE EVERY READER SHARES, certified by
// the same family as the knowledge base's.
//
// The two readers agreed on which gate they report, on the purge's own id
// falling through to the eviction, and on the eviction window only because
// their code looked alike — and the knowledge base's had already drifted to
// the opposite precedence once. A family both run is what makes agreeing a
// test rather than a resemblance.
func TestTheTrackersGateReaderKeepsTheSharedRule(t *testing.T) {
	t.Parallel()
	statelogtest.RunGates(t, func(t *testing.T) statelogtest.GateCandidate {
		return statelogtest.GateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  worklet.Domain{},
				Applier: worklet.NewApplier("suite-node", nil),
				Encode:  encodeSuiteRecord,
				Kinds:   suiteKinds(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return worklet.NewGates(db) },
			Kind:         string(worklet.KindTask),
			SubjectKinds: kindNames(worklet.ObjectKinds),
			Create: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(worklet.TaskSubject(id), worklet.OpCreate,
					writer, opID, newTask(id))
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(worklet.TaskSubject(id), worklet.OpPatch,
					writer, opID, worklet.TaskPatch{Title: ptr("edited " + opID)})
			},
			Purge: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(worklet.TaskSubject(id), worklet.OpPurge,
					writer, opID, map[string]any{
						"v": worklet.GateRecordVersion, "reason": "the gate suite",
					})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(worklet.EvictionSubject(node), worklet.OpEviction,
					writer, opID, gateSuiteEviction(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(worklet.EvictionSubject(node), worklet.OpEviction,
					writer, opID, gateSuiteEviction(node, true))
			},
		}
	})
}

// kindNames is every kind the tracker writes, as the strings its envelope
// carries — the build's own list rather than one kept beside it, so a kind
// added to the domain is a kind the gate family asks about.
func kindNames(kinds []worklet.ObjectKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

// gateSuiteRecord is one record the gate family applies, written by writer.
func gateSuiteRecord(subject worklet.Subject, op worklet.OpKind, writer, opID string,
	payload any) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	scope := worklet.ScopeSet{Subject: true, Container: "ENG"}
	if subject.Kind == worklet.KindEviction {
		scope = worklet.ScopeSet{Subject: true}
	}
	return json.Marshal(worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: writer,
			Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: worklet.AuthorSystem,
	})
}

// gateSuiteEviction is an eviction of node, or its readmission.
func gateSuiteEviction(node string, readmitted bool) worklet.Eviction {
	return worklet.Eviction{
		V: worklet.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: time.Unix(1_700_000_000, 0).UTC(), Readmitted: readmitted,
	}
}
