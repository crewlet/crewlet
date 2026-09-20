package tracker_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// retiredBodies is one pre-removal payload per retired kind, as the build that
// still published it wrote one.
//
// The body matters. A retired record is one whose payload this build may be
// entirely unable to decode, so the gate has to answer from the ENVELOPE — a
// gate that decoded first would fault on exactly the records it exists to read
// past. Every one of these is a shape this build has no type for.
var retiredBodies = map[tracker.ObjectKind]map[string]any{
	tracker.KindSprint: {
		"number": 3, "state": "active", "project": "ENG",
		"policy": map[string]any{"capacity": 21, "measure": "points"},
	},
	tracker.KindGoal: {
		"id": "g-1", "name": "Ship the thing", "health": "on_track",
		"owners": []any{"ana"},
		"targets": []any{map[string]any{
			"id": "t-1", "name": "the work", "type": "tasks",
			"projects": []any{"ENG"},
		}},
	},
}

// retiredRecord is a record as the PRE-REMOVAL build published one: a readable
// record version, a subject kind this build has retired, and a body from the
// table above.
func retiredRecord(kind tracker.ObjectKind, id string) tracker.MutationRecord {
	body, _ := json.Marshal(retiredBodies[kind])
	return tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: string(kind) + "-" + id,
			Subject:   tracker.Subject{Kind: kind, ID: id},
			Op:        tracker.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Writer:    "node-old",
			Scope:     tracker.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: body, Actor: "ana", ActorKind: tracker.AuthorHuman,
	}
}

// A RETIRED KIND IS READ PAST, NOT FAULTED ON.
//
// # What this protects
//
// Removing sprints deleted `KindSprint` from [tracker.ObjectKinds] and from
// the applier's dispatch, and removing goals did the same to `KindGoal`. The
// dispatch has no default case — an unmatched kind falls out of the switch to
// an unconditional error — and `statelog` turns that error into a rolled-back
// batch and a checkpoint that does not advance. The applier retries the same
// record for ever.
//
// Nothing upstream catches it. The deferral that handles a NEWER peer's record
// is keyed on the record VERSION (`rec.V > domain.RecordVersion()`), and a
// retired record's version is one this build reads perfectly — it is the KIND
// that is gone. The tracker stream's subject filter is a wildcard, so the
// record is delivered rather than filtered out. And the log holds it for
// `stream.tracker_retention` — seven days by default, unbounded where the trim
// cannot advance.
//
// So the failure is: one old peer publishing during a rolling upgrade, or one
// old record still in the log, wedges the NEWEST node in the fleet — silently,
// and at the position it stopped.
//
// THE WALK IS OVER [tracker.RetiredKinds] rather than over one kind, so the
// kind retired next is covered without anybody remembering to — and a
// retirement whose body this build happens to decode proves nothing, which is
// why [retiredBodies] has to carry one per kind.
func TestARetiredKindIsGatedRatherThanFaultedOn(t *testing.T) {
	t.Parallel()
	for _, kind := range tracker.RetiredKinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			if retiredBodies[kind] == nil {
				t.Fatalf("%q is retired and has no body in retiredBodies — "+
					"a record this build can decode is not the record this "+
					"gate exists for", kind)
			}
			h := newApplyHarness(t)

			rows, err := h.apply(retiredRecord(kind, "ENG-3"),
				time.Unix(1_700_000_100, 0).UTC())

			var gate *gateError
			if !errors.As(err, &gate) {
				t.Fatalf("a %s record answered %v, want it GATED — an ungated "+
					"retired kind matches no case in the applier's dispatch and "+
					"faults, which stops this node's checkpoint at that position "+
					"for as long as the record is in the log", kind, err)
			}
			if gate.reason != statelog.ReasonRetired {
				t.Errorf("the gate answered %q, want %q — the reason is what a "+
					"`statelog_record_gated` line and the gated metric carry, and "+
					"an eviction or a deletion would name the wrong cause",
					gate.reason, statelog.ReasonRetired)
			}
			if rows != 0 {
				t.Errorf("a gated record wrote %d rows, want none", rows)
			}
		})
	}
}

// AND AN UNKNOWN KIND STILL FAULTS.
//
// The other half, and the one that stops the fix above from becoming a licence
// to swallow anything. A kind NOBODY ever published is a writer publishing a
// kind it never declared — the applier's original comment is right about that
// one, and failing is what makes the mistake visible. Retirement is a
// statement about the log's own history, not a general tolerance.
//
// Without this case the gate could be widened to every unknown kind and the
// suite would stay green while a real writer bug went silent.
func TestAKindNobodyEverPublishedStillFaults(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)

	rec := retiredRecord(tracker.KindSprint, "ENG-3")
	rec.Subject.Kind = "nonesuch"
	rec.OpID = "nonesuch-1"
	_, err := h.apply(rec, time.Unix(1_700_000_100, 0).UTC())

	var gate *gateError
	if errors.As(err, &gate) {
		t.Fatalf("a kind nothing ever published was GATED as %q — the "+
			"retirement gate is the retired list, not every kind this "+
			"build does not know", gate.reason)
	}
	if err == nil {
		t.Fatal("a kind nothing ever published applied cleanly — a writer " +
			"publishing a kind it never declared must be visible, which is " +
			"what the applier's unconditional return is for")
	}
}

// A RETIRED KIND IS NOT PUBLISHABLE.
//
// The list would be actively harmful if a retired kind could still be minted:
// this build would publish records that every node, itself included, drops.
// So the two sets are disjoint by assertion, in both directions.
func TestARetiredKindIsNeverAlsoALiveOne(t *testing.T) {
	t.Parallel()
	if len(tracker.RetiredKinds) == 0 {
		t.Fatal("no kind is retired, so this case asserts nothing — if the " +
			"last retired kind was dropped, drop its gate and these cases too")
	}
	for _, k := range tracker.RetiredKinds {
		if k.Valid() {
			t.Errorf("%q is both retired and in ObjectKinds — this build "+
				"would publish a record every node drops", k)
		}
		if !k.Retired() {
			t.Errorf("%q is in RetiredKinds and Retired() is false", k)
		}
		if k.Arbitrated() {
			t.Errorf("%q is retired and arbitrated — nothing publishes it, "+
				"so an expectation on its subject is one nobody forms", k)
		}
	}
	for _, k := range tracker.ObjectKinds {
		if k.Retired() {
			t.Errorf("%q is a live kind and Retired() is true — the applier "+
				"would gate every record this build writes on it", k)
		}
	}
}
