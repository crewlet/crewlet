package tracker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// novelFields is a field table with one row, standing in for a field a later
// build adds: the stamping rule is pinned over a table the test controls,
// independent of whichever rows the production table holds today.
var novelFields = statelog.RecordFields{
	{Name: "Comment.Novel", Since: 2, Op: string(OpPatch), Path: []string{"mutation", "comment", "novel"}},
}

func stampRecord(t *testing.T, op OpKind, v int, mutation any) MutationRecord {
	t.Helper()
	body, err := json.Marshal(mutation)
	if err != nil {
		t.Fatalf("marshal the mutation: %v", err)
	}
	return MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: v, OpID: "op-stamp", Subject: Subject{Kind: KindTask, ID: "stamp"},
			Op: op, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope: ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "ana", ActorKind: AuthorAgent,
	}
}

func stampedVersion(t *testing.T, body []byte) int {
	t.Helper()
	env, err := DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode the envelope: %v", err)
	}
	return env.V
}

// A RECORD WITHOUT A NEW FIELD STILL ENCODES AT VERSION ONE, whatever the
// build reads.
//
// Stamping the build's own version was the bug: the day [RecordVersion] moved,
// every write a new node made — the ones carrying nothing new included — would
// have been retained by every old node, with everything its scope met, for the
// length of the upgrade.
func TestARecordWithoutANewFieldStillEncodesAtVersionOne(t *testing.T) {
	t.Parallel()
	for name, mutation := range map[string]any{
		"a patch that sets no new field": map[string]any{"comment": map[string]any{"body": "hi"}},
		"a new field's key set to null":  map[string]any{"comment": map[string]any{"novel": nil}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body, err := stampRecord(t, OpPatch, 0, mutation).encodeWith(novelFields)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got := stampedVersion(t, body); got != 1 {
				t.Fatalf("stamped version %d, want 1 — an older build retains it "+
					"for nothing", got)
			}
		})
	}
	// And the production encoder, over the production table.
	body, err := stampRecord(t, OpPatch, 0, map[string]any{"title": "x"}).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := stampedVersion(t, body); got != 1 {
		t.Fatalf("the production encoder stamped %d, want 1", got)
	}
}

// A RECORD CARRYING A NEW FIELD IS STAMPED AT THAT FIELD'S VERSION, so a build
// that predates the field retains it rather than applying it with the field
// dropped — and a record carrying the same key under ANOTHER op is not, since
// the rule is scoped to the payload it was written for.
func TestARecordCarryingANewFieldIsStampedAtItsVersion(t *testing.T) {
	t.Parallel()
	carrying := map[string]any{"comment": map[string]any{"novel": false}}
	body, err := stampRecord(t, OpPatch, 0, carrying).encodeWith(novelFields)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := stampedVersion(t, body); got != 2 {
		t.Fatalf("a record carrying the new field was stamped %d, want 2 — a "+
			"build reading 1 decodes it and drops the field", got)
	}
	env, err := Domain{}.Envelope(body)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.ReadableBy(1) {
		t.Fatal("a build reading version 1 would apply the record carrying a " +
			"version-2 field")
	}

	body, err = stampRecord(t, OpCreate, 0, carrying).encodeWith(novelFields)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := stampedVersion(t, body); got != 1 {
		t.Fatalf("a create carrying the patch field's key was stamped %d, want 1 "+
			"— the row is scoped to the op it names", got)
	}
}

// A VERSION THE CALLER SET BELOW WHAT THE RECORD CARRIES IS REFUSED, and a
// GATE RECORD MAY CARRY NO VERSIONED FIELD AT ALL — its version is pinned at
// [GateRecordVersion] so every build there will ever be can read it.
func TestAStampBelowWhatTheRecordCarriesIsRefused(t *testing.T) {
	t.Parallel()
	carrying := map[string]any{"comment": map[string]any{"novel": "yes"}}
	_, err := stampRecord(t, OpPatch, 1, carrying).encodeWith(novelFields)
	if err == nil || !strings.Contains(err.Error(), "Comment.Novel (version 2)") {
		t.Fatalf("a patch stamped 1 carrying a version-2 field encoded: %v", err)
	}

	gate := stampRecord(t, OpPurge, GateRecordVersion, carrying)
	fields := statelog.RecordFields{{Name: "Purge.Novel", Since: 2,
		Path: []string{"mutation", "comment", "novel"}}}
	_, err = gate.encodeWith(fields)
	if err == nil || !strings.Contains(err.Error(), "installs an apply gate") {
		t.Fatalf("a purge carrying a versioned field encoded: %v", err)
	}

	// A RELAY KEEPS ITS WRITER'S VERSION, the one case a set version above
	// the minimum is right.
	body, err := stampRecord(t, OpPatch, 7, carrying).encodeWith(novelFields)
	if err != nil {
		t.Fatalf("a relayed version-7 record: %v", err)
	}
	if got := stampedVersion(t, body); got != 7 {
		t.Fatalf("a relayed record was restamped %d, want its writer's 7", got)
	}
}
