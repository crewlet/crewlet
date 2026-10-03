package iamdomain_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE IDENTITY ESTATE IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, and it is the
// FIFTH domain to run it — the fourth STRICT one.
//
// What the four before it could not exercise is the case this one is: a domain
// whose ROWS ARE CIPHERTEXT. The suite's determinism half compares table
// CONTENT rather than counting rows, which it has to here — two nodes writing
// different ciphertext for one record would pass a count and fail the claim
// that their tables are byte-identical. That the comparison holds at all is
// what says the sealing happens at the WRITER rather than on each node, which
// is what keeps a name, an address and a seed ciphertext on the log and in
// every node's rows alike.
func TestTheIdentityDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain: iamdomain.Domain{},
			// NO DIRECTORY SIGNAL: nothing here rebuilds a registry.
			Applier: iamdomain.NewApplier("suite-node", nil),
			// Migrate is nil: these tables ship in the replicated
			// estate's own migrations, so a fresh store already has
			// them. A domain that created its tables from test code
			// would be a schema the suite proved and the migration did
			// not.
			Encode: encodeSuiteRecord,
			// THE VERSIONED-FIELD TABLE, and a record carrying each of
			// its fields — which is what certifies the path the table
			// names is where the encoder actually writes the field.
			Fields:   iamdomain.VersionedFields(),
			Carrying: carryingSuiteField,
			Kinds:    suiteKinds(),
			Rows:     iamdomain.NewRows,
			Write:    suiteWrite,
			// THE GATE RECORD, which this domain had an applier, a fence
			// and a table for and no writer — so the trim never learned
			// an evicted node had left this log.
			EncodeGate: encodeSuiteGate,
		}
	})
}

// carryingSuiteField builds a valid record carrying exactly one versioned
// field, with its version left for the encoder to stamp.
//
// EVERY FIELD THE TABLE NAMES HAS A CASE, and a field without one fails here
// rather than passing unexamined: the suite reads the version this returns,
// and a record that did not carry the field would be stamped at 1 and reported
// as the path being wrong, which is a fixture fault dressed as a domain one.
func carryingSuiteField(field statelog.VersionedField) ([]byte, error) {
	at := time.Unix(1_700_000_000, 0).UTC()
	person := iamdomain.PeopleScope(suitePerson)
	rec := iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			OpID: "suite-carrying", CreatedAt: at, Gen: 1, Writer: "suite-node",
		},
		Actor: "suite", ActorKind: iam.KindMachine,
	}
	var payload any
	switch field.Name {
	case "MutationRecord.CollectsSpent":
		bucket := iamdomain.BucketOf(suitePerson)
		rec.Subject, rec.Op, rec.Scope = iamdomain.SweepSubject(bucket),
			iamdomain.OpSweep, iamdomain.BucketScope(bucket)
		payload = iamdomain.Sweep{V: iamdomain.DocumentVersion, Bucket: bucket, Expired: at}
		rec.CollectsSpent = true
	case "MutationRecord.OperatorID":
		// A STAGE CHANGE THROUGH A TOKEN, the gesture the field was
		// added for: its trail row names the credential.
		rec.Subject, rec.Op, rec.Scope = iamdomain.PersonSubject(suitePerson),
			iamdomain.OpStatus, person
		rec.Person = suitePerson
		rec.OperatorID = "pat:018f3a9c-0000-7000-8000-00000000000a"
	case "Session.EnrolmentOnly":
		rec.Subject, rec.Op, rec.Scope = iamdomain.SessionSubject("suite-lineage"),
			iamdomain.OpOpen, person
		rec.Person = suitePerson
		payload = iamdomain.Session{V: iamdomain.DocumentVersion, Person: suitePerson,
			Epoch: 1, AbsoluteExpiresAt: at.Add(time.Hour), EnrolmentOnly: true}
	case "Invitation.Verifier", "Invitation.Seat":
		blind := "suite-blind"
		rec.Subject, rec.Op = iamdomain.EmailSubject(blind), iamdomain.OpInvite
		rec.Scope = iamdomain.BucketScope(iamdomain.BucketOf(blind))
		invitation := iamdomain.Invitation{V: iamdomain.DocumentVersion, ID: "suite-invite",
			Sealed: "sealed:address", ExpiresAt: at.Add(time.Hour)}
		if field.Name == "Invitation.Verifier" {
			invitation.Verifier = iamdomain.InvitationVerifier("suite-secret")
		} else {
			invitation.Seat = "018f3a9c-0000-7000-8000-0000000000cc"
		}
		payload = invitation
	default:
		return nil, fmt.Errorf("no suite record carries %s — add a case that sets "+
			"it and nothing else versioned", field.Name)
	}
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rec.Mutation = body
	}
	return iamdomain.Encode(rec)
}

// suiteWrite is one write through the identity estate's own [iamdomain.Writer]
// — the builder every write path in the domain shares, [iamdomain.Writer.request],
// which is where the framework's stamp is kept or lost.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db *store.DB) error {
	w, err := iamdomain.NewWriter(iamdomain.WriterDeps{
		Publisher: pub, DB: db, Actor: "suite", ActorKind: iam.KindMachine,
		Grants: []iam.Grant{iam.GrantFleetOperate},
	})
	if err != nil {
		return err
	}
	_, err = w.EvictNode(ctx, statelog.NewOpID(time.Now(), "suite-evict"), "suite-peer")
	return err
}

// encodeSuiteGate is the eviction record a peer's writer publishes onto this
// log — or, with readmit, the inverse commit that takes the node back.
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
// certified by the same family as the tracker's, the knowledge base's and the
// org chart's.
//
// This reader answered the eviction before the removal, in two reads, until it
// took the shared shape — and nothing noticed, because it was tested only
// against its own idea of the rule. Its removal gate is keyed on the PERSON,
// so the kind it is asked about is the person's own subject: a record whose
// person is in its payload is one this reader is never handed
// ([iamdomain.Gates]).
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
			Reader: func(db *store.DB) statelog.Gates { return iamdomain.NewGates(db) },
			// A PERSON, which is what a removal takes out of the estate for
			// ever: its tombstone is this log's deletion marker.
			Kind: string(iamdomain.KindPerson),
			Create: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(iamdomain.PersonSubject(id), iamdomain.OpEnrol,
					id, writer, opID, iamdomain.PeopleScope(id), gateSuitePerson(nil))
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(iamdomain.PersonSubject(id), iamdomain.OpUpdate,
					id, writer, opID, iamdomain.PeopleScope(id),
					gateSuitePerson([]iam.Grant{iam.GrantStateRead}))
			},
			// A REMOVAL IS THE PURGE: the one record here with no inverse,
			// and the one that writes the tombstone every later record
			// about the person is dropped by.
			Purge: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(iamdomain.PersonSubject(id), iamdomain.OpRemove,
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
			// A LOGIN CLAIM: it arbitrates on the login, and the person it
			// binds is in its payload — the record whose removal gate only
			// the body can name.
			About: func(id, writer, opID string) (statelog.Subject, []byte, error) {
				subject := iamdomain.LoginSubject("claim." + id)
				body, err := gateSuiteRecord(subject, iamdomain.OpClaim, id, writer,
					opID, iamdomain.PeopleScope(id),
					iamdomain.Claim{V: iamdomain.DocumentVersion, Person: id})
				return statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
					body, err
			},
		}
	})
}

// gateSuiteRecord is one record the gate family applies, written by writer.
//
// A GATE RECORD CARRIES ITS PINNED VERSION, as the writer's own do: a removal
// and an eviction are the records a node must never be able to defer.
func gateSuiteRecord(subject iamdomain.Subject, op iamdomain.OpKind, person,
	writer, opID string, scope iamdomain.ScopeSet, payload any) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	version := iamdomain.BaseRecordVersion
	if (iamdomain.RecordEnvelope{Op: op}).InstallsGate() {
		version = iamdomain.GateRecordVersion
	}
	return iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
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

// suitePerson is the one person every suite record is about, so the first
// three kinds are a real sequence rather than three unrelated records.
const suitePerson = "018f3a9c-0000-7000-8000-00000000f00d"

// suitePayload is one valid (op, person, payload, scope) per kind.
//
// # Why the person, the address and the login are the first three kinds
//
// The suite publishes the FIRST THREE kinds a candidate declares, twice each,
// in order. So the declaration order decides what gets certified — and this
// domain's three are chosen to be a real sequence: a person is minted, their
// address is claimed for them, and their login is claimed for them. A claim
// naming a person nobody minted is a record whose apply has nothing to attach
// to, so any other order would certify a failure rather than a domain.
func suitePayload(kind iamdomain.ObjectKind, id string) (
	iamdomain.OpKind, string, any, iamdomain.ScopeSet) {

	person := iamdomain.PeopleScope(suitePerson)
	switch kind {
	case iamdomain.KindPerson:
		return iamdomain.OpEnrol, id, iamdomain.Person{
			V: iamdomain.DocumentVersion, Kind: iam.KindPerson,
			Stage:      iam.StageActive,
			NameSealed: "sealed:name", EmailSealed: "sealed:email",
			Grants: []iam.Grant{iam.GrantPeopleManage},
		}, iamdomain.PeopleScope(id)
	case iamdomain.KindEmail:
		return iamdomain.OpClaim, suitePerson, iamdomain.Claim{
			V: iamdomain.DocumentVersion, Person: suitePerson,
			Sealed: "sealed:address",
		}, person
	case iamdomain.KindLogin, iamdomain.KindSeat:
		return iamdomain.OpClaim, suitePerson, iamdomain.Claim{
			V: iamdomain.DocumentVersion, Person: suitePerson,
		}, person
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
			V: iamdomain.DocumentVersion, Generation: 1, By: "suite",
		}, iamdomain.RootScope()
	case iamdomain.KindBarrier:
		return iamdomain.OpBarrier, "", nil, iamdomain.RootScope()
	}
	return iamdomain.OpUpdate, suitePerson, nil, person
}
