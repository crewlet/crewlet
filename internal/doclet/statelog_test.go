package doclet_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE PAGES DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, and it is the
// THIRD domain to run it — the second STRICT one.
//
// What the pair before it could not exercise is the case a single strict
// domain makes invisible: with one strict domain and one compacted, a
// checkpoint, a readiness input and an identity claim are each one value, and
// "per domain" and "per node" are indistinguishable. Two strict domains on one
// node is what tells them apart.
func TestThePagesDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain: doclet.Domain{},
			// A NIL SKILL DETECTOR, which is the case the Divergent
			// class exists for: this build has no parser wired, so it
			// writes skill = 0 where a build with one writes 1, and
			// the identity claim must still hold. A suite run with a
			// detector would prove the easy half.
			Applier: doclet.NewApplier(nil, nil),
			// Migrate is nil: these tables ship in the replicated
			// estate's own migration, so a fresh store already has
			// them. A domain that created its tables from test code
			// would be a schema the suite proved and the migration
			// did not.
			Encode: encodeSuiteRecord,
			// THE VERSIONED-FIELD TABLE. It is empty — every field the
			// knowledge base writes is in the base format — so there is
			// no record to carry; the field this build's successor adds
			// brings a Carrying case, which the suite refuses to run
			// without.
			Fields: doclet.VersionedFields(),
			Kinds:  suiteKinds(),
			Rows:   doclet.NewRows,
			Write:  suiteWrite,
			// THE PRODUCTION ENCODER the engine hands the trim.
			EncodeTrimmed: doclet.EncodeTrimmed,
			// AND OF ITS READ INDEX'S BARRIER, which the engine registers
			// for this log: certified to write no row.
			EncodeBarrier: doclet.EncodeBarrier,
			// THE GATE RECORD, which this domain had an applier, a fence
			// and a table for and no writer — so the trim never learned an
			// evicted node had left this log.
			EncodeGate: encodeSuiteGate,
		}
	})
}

// encodeSuiteGate is the eviction record a peer's store publishes onto this
// log — or, with readmit, the inverse commit that takes the node back.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	op, payload, scope := suitePayload(doclet.KindEviction, node)
	eviction := payload.(doclet.Eviction)
	eviction.Readmitted = readmit
	body, err := marshal(eviction)
	if err != nil {
		return nil, err
	}
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return doclet.Encode(doclet.MutationRecord{
		RecordEnvelope: doclet.RecordEnvelope{
			V: doclet.RecordVersion, OpID: opID,
			Subject: doclet.EvictionSubject(node), Op: op, Scope: scope,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Gen: 1,
			Writer: "suite-peer",
		},
		Mutation: body, Actor: "suite", ActorKind: doclet.AuthorOperator,
	})
}

// suiteWrite is one write through Doclet's own [doclet.Store],
// the builder every write path in the domain shares, which is where the
// framework's stamp is kept or lost.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	s, err := doclet.NewStore(doclet.Options{Publisher: pub, DB: db})
	if err != nil {
		return err
	}
	_, _, err = s.EnsureContainer(ctx, activation(0), suiteContainer, "The suite's space", "")
	return err
}

// suiteKinds is every kind, derived from the enum rather than typed again.
func suiteKinds() []string {
	out := make([]string, 0, len(doclet.ObjectKinds))
	for _, k := range doclet.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// encodeSuiteRecord builds one valid record of each kind at an arbitrary
// version.
//
// IT ENCODES AT ANY VERSION, including one above this build's, because that is
// exactly what a newer peer publishes and what this build has to retain.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	// A TITLE'S ID IS COMPOSED — "<CONTAINER>.<normalised title>" — so the
	// plain token the suite hands out is placed into the title position
	// rather than used raw. The suite never asserts the id round-trips; it
	// asserts the domain can encode, envelope and apply what it built.
	subject := doclet.Subject{Kind: doclet.ObjectKind(kind), ID: id}
	if doclet.ObjectKind(kind) == doclet.KindTitle {
		subject = doclet.TitleSubject(suiteContainer, "Page "+id)
	}
	rec := doclet.MutationRecord{
		RecordEnvelope: doclet.RecordEnvelope{
			V:         version,
			OpID:      opID,
			Subject:   subject,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Gen:       1,
			Writer:    "suite-node",
		},
		Actor:     "suite",
		ActorKind: doclet.AuthorOperator,
	}
	op, payload, scope := suitePayload(doclet.ObjectKind(kind), id)
	rec.Op = op
	rec.Scope = scope
	if payload != nil {
		body, err := marshal(payload)
		if err != nil {
			return nil, err
		}
		rec.Mutation = body
	}
	return doclet.Encode(rec)
}

// suitePayload is one valid (op, payload, scope) per kind.
//
// # Why the container, the title and the page are the first three kinds
//
// The suite publishes the FIRST THREE kinds a candidate declares, twice each,
// in order. So the declaration order decides what gets certified — and this
// domain's three are chosen to be a real sequence rather than three unrelated
// records: a container, a create on a title whose payload names the page, and
// then a patch on that page. A page patch on a page no create wrote is a
// malformed record under a strict replay, so any other order would certify a
// failure.
func suitePayload(kind doclet.ObjectKind, id string) (doclet.OpKind, any, doclet.ScopeSet) {
	switch kind {
	case doclet.KindContainer:
		return doclet.OpPatch, doclet.ContainerPayload{
			V: doclet.DocumentVersion, Key: id, Name: "The " + id + " space",
		}, doclet.ScopeSet{Subject: true}
	case doclet.KindTitle:
		return doclet.OpCreate, doclet.CreatePayload{
			V: doclet.DocumentVersion, PageID: id, Container: suiteContainer,
			Title: "Page " + id, Body: "# " + id + "\n\nbody\n",
			Status: doclet.StatusPublished, Author: "suite",
		}, doclet.ScopeSet{Subject: true, Container: suiteContainer}
	case doclet.KindPage:
		body := "# " + id + "\n\nedited\n"
		return doclet.OpPatch, doclet.PagePatch{
			V: doclet.DocumentVersion, Body: &body,
			Labels: []string{"runbook", "suite"},
		}, doclet.ScopeSet{Subject: true, Container: suiteContainer}
	case doclet.KindEviction:
		return doclet.OpEviction, doclet.Eviction{
			V: doclet.GateRecordVersion, NodeID: id, EvictedBy: "suite",
			EvictedAt: time.Unix(1_700_000_000, 0).UTC(),
		}, doclet.ScopeSet{Subject: true}
	case doclet.KindGeneration:
		return doclet.OpGeneration, doclet.Generation{
			V: doclet.GateRecordVersion, Generation: 2, By: "suite",
			StreamCreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		}, doclet.ScopeSet{Subject: true}
	case doclet.KindBarrier:
		return doclet.OpBarrier, nil, doclet.ScopeSet{Subject: true}
	case doclet.KindTrimmed:
		return doclet.OpTrimmed, nil, doclet.ScopeSet{Subject: true}
	}
	return doclet.OpPatch, nil, doclet.ScopeSet{Subject: true}
}

// suiteContainer is the space every suite page lives in.
const suiteContainer = "SUITE"

// marshal is json.Marshal, named so the encode path reads as one step.
func marshal(v any) ([]byte, error) { return json.Marshal(v) }

// THE KNOWLEDGE BASE'S GATE READER KEEPS THE RULE EVERY READER SHARES,
// certified by the same family as the tracker's.
//
// This reader answered the eviction before the deletion until it took the
// tracker's shape, and nothing noticed, because each reader was tested — where
// it was tested at all — against its own idea of the rule.
func TestThePagesGateReaderKeepsTheSharedRule(t *testing.T) {
	t.Parallel()
	statelogtest.RunGates(t, func(t *testing.T) statelogtest.GateCandidate {
		return statelogtest.GateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  doclet.Domain{},
				Applier: doclet.NewApplier(nil, nil),
				Encode:  encodeSuiteRecord,
				Kinds:   suiteKinds(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return doclet.NewGates(db) },
			Kind:         string(doclet.KindPage),
			SubjectKinds: kindNames(doclet.ObjectKinds),
			Create: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(doclet.TitleSubject(suiteContainer, "Page "+id),
					doclet.OpCreate, writer, opID, doclet.CreatePayload{
						V: doclet.DocumentVersion, PageID: id, Container: suiteContainer,
						Title: "Page " + id, Body: "# " + id + "\n",
						Status: doclet.StatusPublished, Author: "suite",
					})
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(doclet.PageSubject(id), doclet.OpPatch, writer, opID,
					doclet.PagePatch{V: doclet.DocumentVersion, Labels: []string{opID}})
			},
			Purge: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(doclet.PageSubject(id), doclet.OpPurge, writer, opID,
					doclet.StatusPayload{V: doclet.DocumentVersion, Reason: "the gate suite"})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(doclet.EvictionSubject(node), doclet.OpEviction,
					writer, opID, gateSuiteEviction(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(doclet.EvictionSubject(node), doclet.OpEviction,
					writer, opID, gateSuiteEviction(node, true))
			},
		}
	})
}

// kindNames is every kind the knowledge base writes, as the strings its
// envelope carries — the build's own list rather than one kept beside it, so a
// kind added to the domain is a kind the gate family asks about.
func kindNames(kinds []doclet.ObjectKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

// gateSuiteRecord is one record the gate family applies, written by writer.
func gateSuiteRecord(subject doclet.Subject, op doclet.OpKind, writer, opID string,
	payload any) ([]byte, error) {

	body, err := marshal(payload)
	if err != nil {
		return nil, err
	}
	scope := doclet.ScopeSet{Subject: true, Container: suiteContainer}
	if subject.Kind == doclet.KindEviction {
		scope = doclet.ScopeSet{Subject: true}
	}
	return doclet.Encode(doclet.MutationRecord{
		RecordEnvelope: doclet.RecordEnvelope{
			V: doclet.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Gen: 1, Writer: writer,
			Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: doclet.AuthorOperator,
	})
}

// gateSuiteEviction is an eviction of node, or its readmission.
func gateSuiteEviction(node string, readmitted bool) doclet.Eviction {
	return doclet.Eviction{
		V: doclet.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: time.Unix(1_700_000_000, 0).UTC(), Readmitted: readmitted,
	}
}
