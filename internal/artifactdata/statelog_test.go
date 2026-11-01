package artifactdata_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE DATASTORE DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, from its
// declaration through its envelope, determinism, idempotency, versioned
// fields, stamp, evictions and node gates, exactly as every other domain is.
// The stamp case runs a write through the production writer, which is a
// SEQUENCED write: the suite certifies the pattern's stamp there.
func TestTheDatastoreDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  artifactdata.Domain{},
			Applier: artifactdata.NewApplier("suite-node", nil),
			// Migrate is nil: these tables ship in the replicated estate's
			// own migration (0044).
			Encode:     encodeSuiteRecord,
			Fields:     artifactdata.VersionedFields(),
			Kinds:      kindNames(),
			Rows:       artifactdata.NewRows,
			Write:      suiteWrite,
			EncodeGate: encodeSuiteGate,
			// THE PRODUCTION ENCODER the engine hands the trim (ADR-0073).
			EncodeTrimmed: artifactdata.EncodeTrimmed,
			// AND OF ITS READ INDEX'S BARRIER, which the engine registers
			// for this log: certified to write no row.
			EncodeBarrier: artifactdata.EncodeBarrier,
		}
	})
}

// THE DATASTORE GATE READER KEEPS THE RULE EVERY READER SHARES: the deletion
// marker first, the purge's own record exempt, then the writer's eviction
// window.
func TestTheDatastoreGateReaderKeepsTheSharedRule(t *testing.T) {
	t.Parallel()
	statelogtest.RunGates(t, func(t *testing.T) statelogtest.GateCandidate {
		return statelogtest.GateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  artifactdata.Domain{},
				Applier: artifactdata.NewApplier("suite-node", nil),
				Encode:  encodeRawRecord,
				Kinds:   kindNames(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return artifactdata.NewGates(db) },
			Kind:         string(artifactdata.KindStore),
			SubjectKinds: kindNames(),
			Create: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(artifactdata.KindStore, id, writer, opID, artifactdata.OpWrite,
					writePayload("first"))
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(artifactdata.KindStore, id, writer, opID, artifactdata.OpWrite,
					writePayload("again by "+opID))
			},
			Purge: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(artifactdata.KindStore, id, writer, opID, artifactdata.OpPurge,
					artifactdata.PurgePayload{V: 1})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(artifactdata.KindEviction, node, writer, opID,
					artifactdata.OpEviction, evictionOf(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(artifactdata.KindEviction, node, writer, opID,
					artifactdata.OpEviction, evictionOf(node, true))
			},
		}
	})
}

// kindNames is every kind the domain writes, from the enum.
func kindNames() []string {
	out := make([]string, 0, len(artifactdata.ObjectKinds))
	for _, k := range artifactdata.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// suiteNamespace maps the suite's plain ids onto artifact ids.
var suiteNamespace = uuid.MustParse("4f0c2a7e-1b6d-5c38-9e21-8a7d3b6c5e40")

// suiteStamped is the instant every suite record is stamped at.
var suiteStamped = time.Unix(1_700_000_000, 0).UTC()

// suiteActor is who every suite record names.
var suiteActor = iam.Actor{Name: "suite", Kind: iam.ActorOperator, OperatorID: "suite"}

// suiteAuthority is the stamp every suite write carries.
func suiteAuthority() artifacts.Authority {
	return artifacts.Authority{Actor: suiteActor, Role: artifacts.RoleOwner, Limits: limits(),
		Origin: artifacts.OriginAPI}
}

// writePayload is a one-op write into a schemaless collection.
func writePayload(title string) artifactdata.WritePayload {
	value, _ := json.Marshal(title)
	return artifactdata.WritePayload{V: 1, Authority: suiteAuthority(), Write: artifacts.DataWrite{
		Ops: []artifacts.DataOp{{Op: artifacts.DataSet, Collection: "tasks", Doc: "row-1",
			Fields: map[string]json.RawMessage{"title": value}}},
	}}
}

// encodeSuiteRecord builds one valid record of each kind, the id mapped onto
// an artifact id.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	store := id
	if artifactdata.ObjectKind(kind) == artifactdata.KindStore {
		store = uuid.NewSHA1(suiteNamespace, []byte(id)).String()
	}
	return encodeRecord(kind, store, opID, version)
}

// encodeRawRecord is [encodeSuiteRecord] with the id used as given.
func encodeRawRecord(kind, id, opID string, version int) ([]byte, error) {
	return encodeRecord(kind, id, opID, version)
}

// encodeRecord is one record of kind about id, at version.
func encodeRecord(kind, id, opID string, version int) ([]byte, error) {
	var (
		subject artifactdata.Subject
		op      artifactdata.OpKind
		payload any
	)
	switch artifactdata.ObjectKind(kind) {
	case artifactdata.KindStore:
		subject, op, payload = artifactdata.Subject{Kind: artifactdata.KindStore, ID: id},
			artifactdata.OpWrite, writePayload("suite "+id)
	case artifactdata.KindEviction:
		subject, op, payload = artifactdata.EvictionSubject(id), artifactdata.OpEviction,
			evictionOf(id, false)
	case artifactdata.KindGeneration:
		subject, op, payload = artifactdata.GenerationSubject(2), artifactdata.OpGeneration,
			artifactdata.Generation{V: 1, Generation: 2, By: "suite", StreamAt: suiteStamped}
	case artifactdata.KindBarrier:
		subject, op = artifactdata.BarrierSubject(), artifactdata.OpBarrier
	case artifactdata.KindTrimmed:
		subject, op = artifactdata.TrimmedSubject(statelog.Position{Generation: 1, Seq: 2}),
			artifactdata.OpTrimmed
	default:
		subject, op = artifactdata.Subject{Kind: artifactdata.ObjectKind(kind), ID: id},
			artifactdata.OpWrite
	}
	body, err := marshalPayload(payload)
	if err != nil {
		return nil, err
	}
	return artifactdata.Encode(artifactdata.MutationRecord{
		RecordEnvelope: artifactdata.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op, CreatedAt: suiteStamped,
			Gen: 1, Writer: "suite-node", Scope: artifactdata.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: suiteActor.Name, ActorKind: suiteActor.Kind,
		OperatorID: suiteActor.OperatorID,
	})
}

// evictionOf is node's eviction, or its readmission.
func evictionOf(node string, readmit bool) artifactdata.Eviction {
	return artifactdata.Eviction{V: artifactdata.GateRecordVersion, NodeID: node,
		EvictedBy: "suite", EvictedAt: suiteStamped, Readmitted: readmit}
}

// rawRecord is one record the gate family applies, written by writer.
func rawRecord(kind artifactdata.ObjectKind, id, writer, opID string, op artifactdata.OpKind,
	payload any) ([]byte, error) {

	body, err := marshalPayload(payload)
	if err != nil {
		return nil, err
	}
	return artifactdata.Encode(artifactdata.MutationRecord{
		RecordEnvelope: artifactdata.RecordEnvelope{
			V: artifactdata.RecordVersion, OpID: opID,
			Subject: artifactdata.Subject{Kind: kind, ID: id}, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: writer,
			Scope: artifactdata.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: suiteActor.Name, ActorKind: suiteActor.Kind,
		OperatorID: suiteActor.OperatorID,
	})
}

// marshalPayload is a payload's bytes, nil for the barrier's absent one.
func marshalPayload(payload any) ([]byte, error) {
	if payload == nil {
		return nil, nil
	}
	return json.Marshal(payload)
}

// encodeSuiteGate is the eviction record a peer publishes onto this log.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return rawRecord(artifactdata.KindEviction, node, "suite-peer", opID,
		artifactdata.OpEviction, evictionOf(node, readmit))
}

// suiteWrite is one write through the domain's own production writer: a
// sequenced write, which is where the framework's stamp is kept or lost.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	w, err := artifactdata.NewWriter(artifactdata.WriterDeps{
		Publisher: pub, DB: db, SeatExists: seatExists,
	})
	if err != nil {
		return err
	}
	_, err = w.Write(ctx, artifactdata.Call{Authority: suiteAuthority()},
		artifacts.IDFor("the suite's store"), writePayload("suite").Write)
	return err
}
