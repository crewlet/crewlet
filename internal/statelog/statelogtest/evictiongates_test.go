package statelogtest_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE EVICTION-ONLY FAMILY COMES BACK CLEAN on a control whose one apply
// gate is the eviction window, and catches every bend of it.
//
// The family certifies a reader against its own domain's applier, so it has
// no control domain of the kind [statelogtest.Run] ends with — the candidate
// is its own control. That leaves it with nothing to run against until a
// domain of its shape registers, and a family no domain runs is a claim:
// every bend it requires reported, and the coverage probe that has to reach
// every kind, would pass for a family that asked nothing. This is the domain
// it runs against meanwhile, and stays the one that says it can pass at all.
//
// Mutation: drop windowCoverage from the agreement and the applier that drops
// its object's own kind alone passes; close the window here at the
// readmission and the family reports the reader for it; drop the clause that
// a gate says it installs one and the domain that says nothing does passes;
// read a nil ApplyGates as none and the candidate that names none passes.
func TestTheEvictionGateFamilyPassesAWindowGatedControl(t *testing.T) {
	t.Parallel()
	statelogtest.RunEvictionGates(t, func(t *testing.T) statelogtest.EvictionGateCandidate {
		return windowControl()
	})
}

// windowControl is the identity-claiming control with its eviction window
// gated on both sides: an applier that drops an evicted writer's record and a
// reader that reports it, each reading the evictions the control's own
// applier wrote.
func windowControl() statelogtest.EvictionGateCandidate {
	c := control()
	c.Applier = windowApplier{}
	return statelogtest.EvictionGateCandidate{
		Candidate: c,
		Reader:    func(db store.ReplicatedReader) statelog.Gates { return windowGates{db} },
		Kind:      "widget",
		// THE EVICTION RECORD TOO: the window is a fact about the writer,
		// so it gates a record of every kind the control publishes.
		SubjectKinds: []string{"widget", controlGateKind},
		Create:       windowWidget,
		Write:        windowWidget,
		Evict: func(node, writer, opID string) ([]byte, error) {
			return windowGate(node, writer, opID, false)
		},
		Readmit: func(node, writer, opID string) ([]byte, error) {
			return windowGate(node, writer, opID, true)
		},
		// THE EVICTION IS ITS ONLY GATE, said with an empty map.
		ApplyGates: map[string]func(id, writer, opID string) ([]byte, error){},
	}
}

// windowWidget is a widget record written by writer under opID: a create and
// an ordinary write are one record to the control, whose apply is an upsert.
func windowWidget(id, writer, opID string) ([]byte, error) {
	env := controlEnvelope("widget", id, opID, 1)
	env.Writer = writer
	return encodeControl(controlRecord{Envelope: env}, stampByContent)
}

// windowGate is the control's eviction of node — or, with readmit, its
// readmission — published by writer under opID.
func windowGate(node, writer, opID string, readmit bool) ([]byte, error) {
	return json.Marshal(controlGate{
		Envelope: statelog.Envelope{
			V: 1, Kind: controlGateKind,
			Subject: statelog.Subject{Kind: controlGateKind, ID: node},
			OpID:    opID, Gen: 1, Writer: writer,
			Scope: statelog.ScopeSet{Paths: []string{controlGateKind}},
		},
		Node: node, Readmit: readmit,
	})
}

// windowApplier is the control's applier with the eviction window gated: a
// record whose writer is evicted at its position applies nowhere.
type windowApplier struct{ controlApplier }

func (windowApplier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error) {
	held, err := windowHolds(ctx, tx, rec.Writer, rec.Position)
	if err != nil || !held {
		return "", false, err
	}
	return statelog.ReasonEvicted, true, nil
}

// windowGates is the publisher's side of the same window, over the rows the
// applier wrote. The control keeps no deletion marker, so it never answers
// `deleted` and never needs the body.
type windowGates struct{ db store.ReplicatedReader }

func (g windowGates) GatedAt(ctx context.Context, _ statelog.Subject, writer, _ string,
	p statelog.Position, _ []byte) (statelog.Reason, bool, error) {

	var held bool
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		held, err = windowHolds(ctx, tx, writer, p)
		return err
	})
	if err != nil || !held {
		return "", false, err
	}
	return statelog.ReasonEvicted, true, nil
}

// windowHolds reports whether writer's eviction holds a record at p: the
// eviction below it and no readmission at or below it — half-open at both
// ends, the window [statelog.Gates] states. Both sides read it here, so the
// family's bends are the only thing that can make them disagree.
func windowHolds(ctx context.Context, tx *sql.Tx, writer string, p statelog.Position) (bool, error) {
	at := p.Packed()
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM control_evictions
		WHERE node_id = ? AND from_position < ?
		  AND (readmitted_position IS NULL OR readmitted_position > ?)`,
		writer, at, at).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("control: read %q's eviction at %s: %w", writer, p, err)
	}
	return true, nil
}
