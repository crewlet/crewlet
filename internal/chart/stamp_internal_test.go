package chart

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE STAMP IS THE LOWEST VERSION THAT READS WHAT A RECORD CARRIES, AND A
// VERSION SET BELOW IT, OR ON A RECORD EVERY BUILD MUST READ, IS REFUSED.
//
// The applier reads a record's rules off its version, so a record stamped
// below what it carries is applied by this very build under rules it was not
// decided under — and by an older peer, lossily. A gate, a barrier and a
// generation are pinned for ever, so a field on one is a record an older node
// defers or retains. And a field a later build adds holds back only the records
// that carry it. Mutation: stamp at [RecordVersion], or drop either refusal,
// and a case fails.
func TestTheStampIsTheLowestVersionThatReadsTheRecord(t *testing.T) {
	t.Parallel()
	placing := func(v int, structural bool, edges ...Edge) MutationRecord {
		body, err := json.Marshal(PlacementPayload{V: DocumentVersion, Edges: edges})
		if err != nil {
			t.Fatal(err)
		}
		return MutationRecord{
			RecordEnvelope: RecordEnvelope{V: v, OpID: "op-1", Subject: TreeSubject(),
				Op: OpPlace, Scope: BatchScope([]ScopeTerm{{Kind: TermUnit, ID: "eng"}})},
			Mutation: body, Actor: "ana", ActorKind: AuthorHuman,
			ManagesStructural: structural,
		}
	}
	unit := ObjectRef{Kind: KindUnit, ID: "eng"}
	stamped := func(rec MutationRecord, fields statelog.RecordFields) (int, error) {
		body, err := rec.encodeWith(fields)
		if err != nil {
			return 0, err
		}
		env, err := DecodeEnvelope(body)
		return env.V, err
	}
	for name, tc := range map[string]struct {
		rec  MutationRecord
		want int
	}{
		"an edge in the base format":     {placing(0, false, Edge{Object: unit}), 1},
		"an edge stating its verb":       {placing(0, false, Edge{Object: unit, Op: OpCreateUnit}), exactVersion},
		"a record under version 3 rules": {placing(0, true, Edge{Object: unit, Op: OpCreateUnit}), managesVersion},
		"a relay at the version it had":  {placing(managesVersion, true, Edge{Object: unit}), managesVersion},
	} {
		if got, err := stamped(tc.rec, versionedFields); err != nil || got != tc.want {
			t.Errorf("%s is stamped (%d, %v), want %d", name, got, err, tc.want)
		}
	}

	// A FIELD A LATER BUILD ADDS holds back only the records carrying it.
	later := append(VersionedFields(), statelog.VersionedField{
		Name: "MutationRecord.Revision", Since: RecordVersion + 1, Path: []string{"revision"}})
	withRevision := placing(0, true, Edge{Object: unit})
	withRevision.Revision = "rev-1"
	if got, err := stamped(withRevision, later); err != nil || got != RecordVersion+1 {
		t.Errorf("a record carrying a later build's field is stamped (%d, %v), want %d",
			got, err, RecordVersion+1)
	}

	// SET BY HAND BELOW WHAT IT CARRIES: refused.
	if _, err := stamped(placing(1, true, Edge{Object: unit}), versionedFields); err == nil {
		t.Error("a record decided under version 3's rules, set by hand to version " +
			"1, was encoded — this build would apply it under version 1's")
	}
	// A GATE CARRIES NO VERSIONED FIELD, set or not.
	gate := MutationRecord{
		RecordEnvelope: RecordEnvelope{V: GateRecordVersion, OpID: "op-remove",
			Subject: TreeSubject(), Op: OpRemove,
			Scope: BatchScope([]ScopeTerm{{Kind: TermUnit, ID: "eng"}})},
		ManagesStructural: true,
	}
	if _, err := gate.encodeWith(versionedFields); err == nil {
		t.Error("a removal carrying a versioned field was encoded — a gate an " +
			"older node cannot read is one it defers")
	}
	gate.V = managesVersion
	if _, err := gate.encodeWith(versionedFields); err == nil {
		t.Error("a removal at the version its field needs was encoded — a gate " +
			"is pinned at version 1 for ever")
	}
}
