package atlasdomain_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ATLAS DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, from its
// declaration through its envelope, determinism, idempotency, stamp, evictions
// and node gates, as every other domain is.
func TestTheAtlasDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  atlasdomain.Domain{},
			Applier: atlasdomain.NewApplier(),
			// Migrate is nil: the tables ship in the replicated estate's own
			// migration (0163).
			Encode:        encodeSuiteRecord,
			Fields:        atlasdomain.VersionedFields(),
			Carrying:      carryingSuiteRecord,
			Kinds:         kindNames(),
			Rows:          atlasdomain.NewRows,
			Write:         suiteWrite,
			EncodeGate:    encodeSuiteGate,
			EncodeBarrier: atlasdomain.EncodeBarrier,
			EncodeTrimmed: atlasdomain.EncodeTrimmed,
		}
	})
}

// THE PURGE GATE KEEPS THE RULE EVERY READER SHARES: a purged entry's marker
// drops every later record about it, the purge's own excepted, on its own
// subject and on another entry's that names it, and outranks a writer's
// eviction window, which is half-open at both ends.
func TestTheAtlasGateReaderKeepsTheSharedRule(t *testing.T) {
	t.Parallel()
	statelogtest.RunGates(t, func(t *testing.T) statelogtest.GateCandidate {
		return statelogtest.GateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  atlasdomain.Domain{},
				Applier: atlasdomain.NewApplier(),
				Encode:  encodeSuiteRecord,
				Kinds:   kindNames(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return atlasdomain.NewGates(db) },
			Kind:         string(atlasdomain.KindEntry),
			SubjectKinds: kindNames(),
			Create: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(entryOn(id), writer, opID, atlasdomain.OpClaim, sentinel(),
					existsClaim(id, opID))
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(entryOn(id), writer, opID, atlasdomain.OpClaim, sentinel(),
					purposeClaim(id, opID, "edited under "+opID))
			},
			Purge: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(entryOn(id), writer, opID, atlasdomain.OpPurge, domain(),
					atlasdomain.Purge{V: atlasdomain.GateRecordVersion})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(atlasdomain.EvictionSubject(node), writer, opID,
					atlasdomain.OpEviction, sentinel(), evictionOf(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(atlasdomain.EvictionSubject(node), writer, opID,
					atlasdomain.OpEviction, sentinel(), evictionOf(node, true))
			},
			// A RELATION FROM ANOTHER ENTRY names its object in its payload:
			// a service that depends on the purged one.
			About: func(id, writer, opID string) (statelog.Subject, []byte, error) {
				on := "about-" + id
				body, err := rawRecord(entryOn(on), writer, opID, atlasdomain.OpClaim, sentinel(),
					dependsClaim(on, id, opID))
				return statelog.Subject{Kind: string(atlasdomain.KindEntry), ID: on}, body, err
			},
		}
	})
}

// kindNames is every kind the domain writes, from the enum.
func kindNames() []string {
	out := make([]string, 0, len(atlasdomain.ObjectKinds))
	for _, k := range atlasdomain.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// suiteStamped is the instant every suite record is stamped at.
var suiteStamped = time.Unix(1_700_000_000, 0).UTC()

// sentinel is the scope of a record that writes its own subject alone, and
// domain the scope of one that reaches the whole log.
func sentinel() atlasdomain.ScopeSet { return atlasdomain.ScopeSet{Subject: true} }
func domain() atlasdomain.ScopeSet {
	return atlasdomain.ScopeSet{Terms: []atlasdomain.ScopeTerm{{Kind: atlasdomain.TermDomain}}}
}

// entryOn is the subject of the suite's entry id: the suite names its objects,
// and a record's subject is the id the writer derived, held to no grammar here.
func entryOn(id string) atlasdomain.Subject {
	return atlasdomain.Subject{Kind: atlasdomain.KindEntry, ID: id}
}

// suitePerson is the source of every suite claim.
var suitePerson = atlasdomain.Source{Kind: atlas.SourcePerson, Name: "suite"}

// existsClaim is a person's claim that the service id exists.
func existsClaim(id, opID string) atlasdomain.Claim {
	return atlasdomain.Claim{V: 1, ID: atlasdomain.ClaimID(opID),
		Entry: atlasdomain.Ref{Kind: atlas.KindService, Key: id}, Slot: string(atlas.FacetExists),
		Facet: atlas.FacetExists, Source: suitePerson}
}

// purposeClaim is a person's purpose for the service id.
func purposeClaim(id, opID, purpose string) atlasdomain.Claim {
	return atlasdomain.Claim{V: 1, ID: atlasdomain.ClaimID(opID),
		Entry: atlasdomain.Ref{Kind: atlas.KindService, Key: id}, Slot: "f\x00purpose",
		Facet: atlas.FacetFact, Fact: atlas.FactPurpose,
		Value: &atlasdomain.Value{Type: atlas.TypeLabel, Text: purpose}, Source: suitePerson}
}

// dependsClaim is a person's claim that the service from depends on the
// service the suite names to.
func dependsClaim(from, to, opID string) atlasdomain.Claim {
	return atlasdomain.Claim{V: 1, ID: atlasdomain.ClaimID(opID),
		Entry: atlasdomain.Ref{Kind: atlas.KindService, Key: from},
		Slot:  "r\x00depends_on\x00service:" + to, Facet: atlas.FacetRelation,
		Relation: atlas.RelationDependsOn,
		Target:   &atlasdomain.Target{ID: to, Kind: atlas.KindService, Key: to}, Source: suitePerson}
}

// encodeSuiteRecord builds one valid record of each kind about id at any
// version. The first three kinds are a real sequence: a claim writes an entry, a
// purge of the value that is its key erases it, and a node's eviction writes the
// gate.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	subject, op, scope, payload := suitePayload(atlasdomain.ObjectKind(kind), id, opID)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		body = nil
	}
	return atlasdomain.Encode(atlasdomain.MutationRecord{
		RecordEnvelope: atlasdomain.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: "suite-node", Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorHuman,
	})
}

// suitePayload is one valid (subject, op, scope, payload) per kind about id.
func suitePayload(kind atlasdomain.ObjectKind, id, opID string) (
	atlasdomain.Subject, atlasdomain.OpKind, atlasdomain.ScopeSet, any) {

	switch kind {
	case atlasdomain.KindEntry:
		return entryOn(id), atlasdomain.OpClaim, sentinel(), existsClaim(id, opID)
	case atlasdomain.KindValue:
		return atlasdomain.ValueSubject(id), atlasdomain.OpPurge, domain(),
			atlasdomain.Purge{V: atlasdomain.GateRecordVersion}
	case atlasdomain.KindEviction:
		return atlasdomain.EvictionSubject(id), atlasdomain.OpEviction, sentinel(), evictionOf(id, false)
	case atlasdomain.KindExpiry:
		return atlasdomain.ExpirySubject(suiteStamped), atlasdomain.OpExpire, domain(),
			atlasdomain.Expire{V: 1, Through: suiteStamped}
	case atlasdomain.KindGeneration:
		return atlasdomain.GenerationSubject(2), atlasdomain.OpGeneration, sentinel(),
			atlasdomain.Generation{V: 1, Generation: 2, By: "suite", StreamAt: suiteStamped}
	case atlasdomain.KindTrimmed:
		floor := statelog.Position{Stream: atlasdomain.Domain{}.Stream().Name, Generation: 1, Seq: 5}
		return atlasdomain.TrimmedSubject(floor), atlasdomain.OpTrimmed, sentinel(), nil
	}
	return atlasdomain.Subject{Kind: kind, ID: id}, atlasdomain.OpBarrier, sentinel(), nil
}

// carryingSuiteRecord builds one valid record carrying exactly the named
// versioned field: an integration's read of an entry for the `read` op.
func carryingSuiteRecord(field statelog.VersionedField) ([]byte, error) {
	const id = "carrying"
	switch field.Name {
	case "Op read":
		return rawRecord(entryOn(id), "suite-node", "suite-carrying-read", atlasdomain.OpRead, sentinel(),
			readOf(id, "suite-carrying-read"))
	case "Claim.Source agent":
		claim := existsClaim(id, "suite-carrying-agent")
		claim.Source = atlasdomain.Source{Kind: atlas.SourceAgent, Name: "sre", Turn: "run-1",
			Evidence: atlas.EvidenceCall}
		return rawRecord(entryOn(id), "suite-node", "suite-carrying-agent", atlasdomain.OpClaim, sentinel(), claim)
	}
	return nil, fmt.Errorf("no suite record carries %s: add one here with its row", field.Name)
}

// readOf is an integration's read of the hostname id: it exists, and the
// voice's earlier claim on one slot is withdrawn.
func readOf(id, opID string) atlasdomain.Read {
	src := atlasdomain.Source{Kind: atlas.SourceIntegration, Name: "cloudflare"}
	slot := string(atlas.FacetExists)
	return atlasdomain.Read{V: 1, Source: src, ObservedAt: suiteStamped,
		Claims: []atlasdomain.Claim{{V: 1, ID: atlasdomain.ReadClaimID(opID, slot),
			Entry: atlasdomain.Ref{Kind: atlas.KindHostname, Key: id}, Slot: slot,
			Facet: atlas.FacetExists, Source: src, ObservedAt: suiteStamped}},
		Withdrawn: []string{"f\x00url\x00https://old.example"}}
}

// encodeSuiteGate is a node's eviction or readmission as a peer publishes it.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return rawRecord(atlasdomain.EvictionSubject(node), "suite-peer", opID,
		atlasdomain.OpEviction, sentinel(), evictionOf(node, readmit))
}

// evictionOf is node's eviction, or its readmission.
func evictionOf(node string, readmit bool) atlasdomain.Eviction {
	return atlasdomain.Eviction{V: atlasdomain.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: suiteStamped, Readmitted: readmit}
}

// rawRecord is one record as a writer stamps it.
func rawRecord(subject atlasdomain.Subject, writer, opID string, op atlasdomain.OpKind,
	scope atlasdomain.ScopeSet, payload any) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return atlasdomain.Encode(atlasdomain.MutationRecord{
		RecordEnvelope: atlasdomain.RecordEnvelope{
			OpID: opID, Subject: subject, Op: op, CreatedAt: suiteStamped, Gen: 1,
			Writer: writer, Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorHuman,
	})
}

// suiteWrite is one write through the domain's own production writer: a
// person's claim that a service exists, which needs nothing before it.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	w, err := atlasdomain.NewWriter(atlasdomain.WriterDeps{Publisher: pub, DB: db})
	if err != nil {
		return err
	}
	claim, err := atlas.Exists(atlas.MustRef(atlas.KindService, "checkout-api"),
		atlas.PersonSource("suite"), time.Now())
	if err != nil {
		return err
	}
	_, err = w.Record(ctx, atlasdomain.Call{
		By:   iam.Actor{Name: "suite", Kind: iam.ActorHuman},
		OpID: statelog.NewOpID(time.Now(), "suite"),
	}, claim, atlasdomain.NoIfMatch)
	return err
}
