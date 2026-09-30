package chart_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CHART IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, and it is the FOURTH
// domain to run it — the third STRICT one.
//
// What the three before it could not exercise is the case this one is: a domain
// whose arbitration is NOT UNIFORM. The tracker, the vectors and the knowledge
// base each contend per object, so "the subject" and "the object" are one thing
// throughout, and every rule the framework states about the pair is trivially
// satisfied. Here one subject carries the whole STRUCTURE while each object's
// CONTENT carries its own — so a record's subject, the objects its apply
// writes and the scope it declares are three genuinely different sets, and the
// suite's determinism and deferral cases are the first to run against a domain
// where they can come apart.
//
// IT COULD NOT RUN UNTIL THE APPLIER EXISTED. The suite's four halves include
// the apply cases, and the engine's boot check refuses a register entry with a
// nil applier — so the declaration landed first and this is where it becomes
// certified rather than merely stated.
func TestTheChartDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain: chart.Domain{},
			// NO REBUILD LISTENER, which is the honest shape here and
			// not a saving: what the applier's change set feeds is a
			// derived company view, and a view rebuilt from the suite's
			// own fixtures would prove nothing about either. The set is
			// drained and dropped, exactly as it is on a node that
			// holds no view.
			Applier: chart.NewApplier("suite-node", nil),
			// Migrate is nil: these tables ship in the replicated
			// estate's own migrations, so a fresh store already has
			// them. A domain that created its tables from test code
			// would be a schema the suite proved and the migration did
			// not.
			Encode: encodeSuiteRecord,
			Kinds:  suiteKinds(),
			Rows:   chart.NewRows,
			Write:  suiteWrite,
			// THE GATE RECORD, which this domain had an applier, a fence
			// and a table for and no writer — so the trim never learned
			// an evicted node had left this log.
			EncodeGate: encodeSuiteGate,
		}
	})
}

// suiteWrite is one write through the chart's own [chart.Writer] — the
// builder every write path in the domain shares, which is where the
// framework's stamp is kept or lost.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db *store.DB) error {
	w, err := chart.NewWriter(chart.WriterDeps{
		Publisher: pub, DB: db, Runtime: org.RuntimeShape{},
		Actor: "suite", ActorKind: chart.AuthorOperator,
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
	return gateSuiteRecord(chart.EvictionSubject(node), chart.OpEviction, "suite-peer",
		opID, chart.ScopeSet{Subject: true}, gateSuiteEviction(node, readmit))
}

// THE CHART'S GATE READER KEEPS THE RULE EVERY READER SHARES, certified by the
// same family as the tracker's and the knowledge base's.
//
// This reader answered the eviction before the removal, in two reads, until it
// took the shared shape — and nothing noticed, because it was tested only
// against its own idea of the rule.
func TestTheChartGateReaderKeepsTheSharedRule(t *testing.T) {
	t.Parallel()
	statelogtest.RunGates(t, func(t *testing.T) statelogtest.GateCandidate {
		return statelogtest.GateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  chart.Domain{},
				Applier: chart.NewApplier("suite-node", nil),
				Encode:  encodeSuiteRecord,
				Kinds:   suiteKinds(),
			},
			Reader: func(db *store.DB) statelog.Gates { return chart.NewGates(db) },
			// A SEAT, which is what a removal takes out of the chart for
			// ever: its tombstone is this log's deletion marker.
			Kind: string(chart.KindSeat),
			// Each object in a unit of its own, so three creates are three
			// placements that each create what they place.
			Create: func(id, writer, opID string) ([]byte, error) {
				unit := "unit-" + id
				return gateSuiteRecord(chart.TreeSubject(), chart.OpPlace, writer, opID,
					chart.BatchScope([]chart.ScopeTerm{
						{Kind: chart.TermUnit, ID: unit},
						{Kind: chart.TermSeat, Unit: unit, ID: id},
					}),
					chart.PlacementPayload{V: chart.DocumentVersion, Edges: []chart.Edge{
						{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: unit},
							Op: chart.OpCreateUnit},
						{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: id},
							Parent: unit, Op: chart.OpCreateSeat},
					}})
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(chart.Subject{Kind: chart.KindSeat, ID: id},
					chart.OpUpsert, writer, opID,
					chart.ScopeSet{Subject: true, Unit: "unit-" + id},
					chart.SeatPayload{
						V: chart.DocumentVersion, Handle: id, Kind: chart.SeatAgent,
						Name: "Gate Seat", Goal: opID,
					})
			},
			Purge: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(chart.TreeSubject(), chart.OpRemove, writer, opID,
					chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermSeat, ID: id}}),
					chart.RemovePayload{
						V:       chart.GateRecordVersion,
						Objects: []chart.ObjectRef{{Kind: chart.KindSeat, ID: id}},
						Reason:  "the gate suite",
					})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(chart.EvictionSubject(node), chart.OpEviction,
					writer, opID, chart.ScopeSet{Subject: true},
					gateSuiteEviction(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(chart.EvictionSubject(node), chart.OpEviction,
					writer, opID, chart.ScopeSet{Subject: true},
					gateSuiteEviction(node, true))
			},
		}
	})
}

// gateSuiteRecord is one record the gate family applies, written by writer.
//
// A GATE RECORD CARRIES ITS PINNED VERSION, as the writer's own do: a removal
// and an eviction are the records a node must never be able to defer.
func gateSuiteRecord(subject chart.Subject, op chart.OpKind, writer, opID string,
	scope chart.ScopeSet, payload any) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	version := chart.RecordVersion
	if op == chart.OpRemove || op == chart.OpEviction {
		version = chart.GateRecordVersion
	}
	return chart.Encode(chart.MutationRecord{
		RecordEnvelope: chart.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Gen: 1, Writer: writer,
			Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: chart.AuthorOperator,
	})
}

// gateSuiteEviction is an eviction of node, or its readmission.
func gateSuiteEviction(node string, readmit bool) chart.Eviction {
	return chart.Eviction{V: chart.GateRecordVersion, NodeID: node, Readmit: readmit, By: "suite"}
}
