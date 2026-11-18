package iamdomain_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE IDENTITY ESTATE IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, as a domain
// whose ROWS ARE CIPHERTEXT: the determinism half compares table CONTENT, so
// it holds only because sealing happens at the WRITER, not on each node,
// which keeps names, addresses and seeds ciphertext on the log too.
func TestTheIdentityDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain: iamdomain.Domain{},
			// NO DIRECTORY SIGNAL: nothing here rebuilds a registry.
			Applier: iamdomain.NewApplier("suite-node", nil),
			// Migrate is nil: the tables ship in the replicated estate's
			// migrations, so the suite proves the schema they create.
			Encode: encodeSuiteRecord,
			// THE PRODUCTION TABLE, which is empty. A row added to it
			// needs a Carrying here, built through the writer's own
			// encoder; the suite refuses a table without one.
			Fields: iamdomain.VersionedFields(),
			Kinds:  suiteKinds(),
			Rows:   iamdomain.NewRows,
			Write:  suiteWrite,
			// THE PRODUCTION ENCODER the engine hands the trim (ADR-0080).
			EncodeTrimmed: iamdomain.EncodeTrimmed,
			// AND OF ITS READ INDEX'S BARRIER: certified to write no row.
			EncodeBarrier: iamdomain.EncodeBarrier,
			// THE GATE RECORD, through a peer's writer, so the trim learns
			// an evicted node has left this log.
			EncodeGate: encodeSuiteGate,
		}
	})
}

// suiteWrite is one write through [iamdomain.Writer]'s shared builder,
// [iamdomain.Writer.request], where the framework's stamp is kept or lost.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	w, err := iamdomain.NewWriter(iamdomain.WriterDeps{
		Publisher: pub, DB: db, Actor: "suite", ActorKind: iam.KindMachine,
		Grants: []iam.Grant{iam.GrantAdmin},
	})
	if err != nil {
		return err
	}
	_, err = w.EvictNode(ctx, statelog.NewOpID(time.Now(), "suite-evict"), "suite-peer")
	return err
}

// encodeSuiteGate is the eviction record a peer's writer publishes onto this
// log, or with readmit the inverse commit that takes the node back.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return gateSuiteRecord(iamdomain.EvictionSubject(node), iamdomain.OpEviction,
		"", "suite-peer", opID, iamdomain.RootScope(),
		iamdomain.Eviction{V: iamdomain.GateRecordVersion, Readmit: readmit, By: "suite"})
}

// THE IDENTITY ESTATE'S GATE READER KEEPS THE RULE EVERY READER SHARES,
// certified by the same family as every other domain's. Its removal gate is
// keyed on the PERSON, so it is asked about the person's own subject; a record
// whose person is in its payload never reaches it ([iamdomain.Gates]).
func TestTheIdentityGateReaderKeepsTheSharedRule(t *testing.T) {
	t.Parallel()
	statelogtest.RunGates(t, func(t *testing.T) statelogtest.GateCandidate {
		return statelogtest.GateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  iamdomain.Domain{},
				Applier: iamdomain.NewApplier("suite-node", nil),
				Encode:  encodeSuiteRecord,
				Kinds:   suiteKinds(),
			},
			Reader: func(db store.ReplicatedReader) statelog.Gates { return iamdomain.NewGates(db) },
			// A PERSON: a removal's tombstone is this log's deletion marker.
			Kind: string(iamdomain.KindPerson),
			// EVERY KIND, so the applier is asked which the tombstone
			// covers: a directory record names its person in the payload
			// alone, and a session about somebody else is left alone.
			SubjectKinds: suiteKinds(),
			// AN ENROLMENT, on the directory, names the person in its
			// payload alone.
			Create: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(iamdomain.DirectorySubject(), iamdomain.OpEnrol,
					id, writer, opID, iamdomain.PeopleScope(id), iamdomain.Enrolled{
						V: iamdomain.DocumentVersion, Person: gateSuitePerson(nil),
						Holds: iamdomain.Identifiers{Login: id + ".suite"},
					})
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(iamdomain.PersonSubject(id), iamdomain.OpUpdate,
					id, writer, opID, iamdomain.PeopleScope(id),
					gateSuitePerson([]iam.Grant{iam.GrantWorklet}))
			},
			// A REMOVAL IS THE PURGE: no inverse, and its tombstone drops
			// every later record about the person.
			Purge: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(iamdomain.DirectorySubject(), iamdomain.OpRemove,
					id, writer, opID, iamdomain.PeopleScope(id),
					iamdomain.Removal{V: iamdomain.GateRecordVersion})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(iamdomain.EvictionSubject(node),
					iamdomain.OpEviction, "", writer, opID, iamdomain.RootScope(),
					iamdomain.Eviction{V: iamdomain.GateRecordVersion, By: "suite"})
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(iamdomain.EvictionSubject(node),
					iamdomain.OpEviction, "", writer, opID, iamdomain.RootScope(),
					iamdomain.Eviction{V: iamdomain.GateRecordVersion, Readmit: true,
						By: "suite"})
			},
			// AN IDENTITY CHANGE arbitrates on the directory with its
			// person in the payload: only the body names its removal gate.
			About: func(id, writer, opID string) (statelog.Subject, []byte, error) {
				subject := iamdomain.DirectorySubject()
				body, err := gateSuiteRecord(subject, iamdomain.OpIdentity, id, writer,
					opID, iamdomain.PeopleScope(id),
					iamdomain.IdentityChange{V: iamdomain.DocumentVersion,
						Login: "moved." + id})
				return statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
					body, err
			},
		}
	})
}

// gateSuiteRecord is one record the gate family applies, written by writer.
func gateSuiteRecord(subject iamdomain.Subject, op iamdomain.OpKind, person,
	writer, opID string, scope iamdomain.ScopeSet, payload any) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.BaseRecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Gen: 1, Writer: writer,
			Scope: scope,
		},
		Mutation: body, Person: person, Actor: "suite", ActorKind: iam.KindMachine,
	})
}

// gateSuitePerson is an enrolled person's document, holding grants.
func gateSuitePerson(grants []iam.Grant) iamdomain.Person {
	return iamdomain.Person{
		V: iamdomain.DocumentVersion, Kind: iam.KindPerson, Stage: iam.StageActive,
		NameSealed: "sealed:name", EmailSealed: "sealed:email", Grants: grants,
	}
}

// suiteKinds is every kind this domain declares, as the suite takes them.
func suiteKinds() []string {
	out := make([]string, 0, len(iamdomain.ObjectKinds))
	for _, kind := range iamdomain.ObjectKinds {
		out = append(out, string(kind))
	}
	return out
}

func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	rec := iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V:         version,
			OpID:      opID,
			Subject:   iamdomain.Subject{Kind: iamdomain.ObjectKind(kind), ID: id},
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Gen:       1,
			Writer:    "suite-node",
		},
		Actor:     "suite",
		ActorKind: iam.KindMachine,
	}
	// A SINGLETON KIND NAMES NO OBJECT, whatever id the suite hands it.
	if !rec.Subject.Kind.Identified() {
		rec.Subject.ID = ""
	}
	op, person, payload, scope := suitePayload(iamdomain.ObjectKind(kind), id)
	rec.Op = op
	rec.Person = person
	rec.Scope = scope
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rec.Mutation = body
	}
	return iamdomain.Encode(rec)
}

// suitePerson is the person a session, a status change and a sweep are about.
const suitePerson = "018f3a9c-0000-7000-8000-00000000f00d"

// suitePayload is one valid (op, person, payload, scope) per kind: the
// directory enrols a person under the id and the person kind then changes
// their document, the sequence [iamdomain.ObjectKinds]' order certifies.
func suitePayload(kind iamdomain.ObjectKind, id string) (
	iamdomain.OpKind, string, any, iamdomain.ScopeSet) {

	person := iamdomain.PeopleScope(suitePerson)
	switch kind {
	case iamdomain.KindDirectory:
		return iamdomain.OpEnrol, id, iamdomain.Enrolled{
			V: iamdomain.DocumentVersion,
			Person: iamdomain.Person{
				V: iamdomain.DocumentVersion, Kind: iam.KindPerson,
				Stage:      iam.StageActive,
				NameSealed: "sealed:name", EmailSealed: "sealed:email",
				Grants: []iam.Grant{iam.GrantAdmin},
			},
			Holds: iamdomain.Identifiers{EmailBlind: "blind-" + id,
				Login: id + ".suite"},
		}, iamdomain.PeopleScope(id)
	case iamdomain.KindPerson:
		return iamdomain.OpUpdate, id, iamdomain.Person{
			V: iamdomain.DocumentVersion, Kind: iam.KindPerson,
			Stage:      iam.StageActive,
			NameSealed: "sealed:renamed", EmailSealed: "sealed:email",
			Grants: []iam.Grant{iam.GrantWorklet},
		}, iamdomain.PeopleScope(id)
	case iamdomain.KindSession:
		return iamdomain.OpOpen, suitePerson, iamdomain.Session{
			V: iamdomain.DocumentVersion, Person: suitePerson, Epoch: 1,
			AbsoluteExpiresAt: time.Unix(1_800_000_000, 0).UTC(),
		}, person
	case iamdomain.KindSweep:
		return iamdomain.OpSweep, "", iamdomain.Sweep{
			V: iamdomain.DocumentVersion, Bucket: 0,
		}, iamdomain.BucketScope(0)
	case iamdomain.KindEviction:
		return iamdomain.OpEviction, "", iamdomain.Eviction{
			V: iamdomain.GateRecordVersion, By: "suite",
		}, iamdomain.RootScope()
	case iamdomain.KindGeneration:
		return iamdomain.OpGeneration, "", iamdomain.Generation{
			V: iamdomain.GateRecordVersion, Generation: 1, By: "suite",
		}, iamdomain.RootScope()
	case iamdomain.KindBarrier:
		return iamdomain.OpBarrier, "", nil, iamdomain.RootScope()
	case iamdomain.KindTrimmed:
		return iamdomain.OpTrimmed, "", nil, iamdomain.RootScope()
	}
	return iamdomain.OpUpdate, suitePerson, nil, person
}
