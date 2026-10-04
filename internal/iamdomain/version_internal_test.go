package iamdomain

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE STAMP IS THE LOWEST VERSION THAT READS WHAT A RECORD CARRIES, AND A
// VERSION SET BELOW IT, OR ON A RECORD EVERY BUILD MUST READ, IS REFUSED.
//
// A record above a peer's decode ceiling is DEFERRED on that peer, and a
// deferred record blocks every later record in its scope there. So the ceiling
// is what this build reads and never what it writes: [Encode] stamps an
// unstamped record from the fields it carries, which is what makes a field a
// later build adds hold back only the records that carry it. And the two ways a
// caller can put a version on a record by hand are each refused for the
// failure they would cause: one below what its fields need is applied lossily
// by exactly the builds it exists to hold back, and a gate, a barrier or a
// generation carrying a field is one an older node defers or retains — a
// deferred removal is a person off-boarded still signing in there. The table
// is empty today, so the case states a field a later build would add. Mutation:
// stamp at the table's ceiling, or drop either refusal, and a case fails.
func TestTheStampIsTheLowestVersionThatReadsTheRecord(t *testing.T) {
	t.Parallel()
	person := "0192f00d-0000-7000-8000-0000000000aa"
	status := func(v int, reason string) MutationRecord {
		return MutationRecord{
			RecordEnvelope: RecordEnvelope{V: v, OpID: "op-1",
				Subject: PersonSubject(person), Op: OpStatus, Scope: PeopleScope(person)},
			Person: person, Actor: "ana.admin", ActorKind: iam.KindPerson,
			OperatorID: "pat:x", Reason: reason,
		}
	}
	stamped := func(rec MutationRecord, fields statelog.RecordFields) (int, error) {
		body, err := rec.encodeWith(fields)
		if err != nil {
			return 0, err
		}
		env, err := DecodeEnvelope(body)
		return env.V, err
	}
	// A FIELD A LATER BUILD ADDS, on every op.
	later := append(VersionedFields(), statelog.VersionedField{
		Name: "MutationRecord.Reason", Since: RecordVersion + 1,
		Path: []string{"reason"}})

	if got, err := stamped(status(0, ""), versionedFields); err != nil || got != BaseRecordVersion {
		t.Errorf("a record carrying nothing versioned is stamped (%d, %v), want %d",
			got, err, BaseRecordVersion)
	}
	if got, err := stamped(status(0, "left the building"), later); err != nil ||
		got != RecordVersion+1 {
		t.Errorf("a record carrying a field a later build added is stamped (%d, %v), "+
			"want %d", got, err, RecordVersion+1)
	}
	if got, err := stamped(status(0, ""), later); err != nil || got != BaseRecordVersion {
		t.Errorf("a record NOT carrying that field is stamped (%d, %v), want %d — "+
			"every older node would retain it for nothing", got, err, BaseRecordVersion)
	}

	// A VERSION SET BY HAND IS KEPT AT OR ABOVE WHAT THE RECORD NEEDS, and
	// refused below it.
	if got, err := stamped(status(RecordVersion+1, "left"), later); err != nil ||
		got != RecordVersion+1 {
		t.Errorf("a relayed record at version %d came back (%d, %v)",
			RecordVersion+1, got, err)
	}
	if _, err := stamped(status(BaseRecordVersion, "left"), later); err == nil {
		t.Error("a record carrying a later field, set by hand to the base " +
			"version, was encoded — every build that predates the field would " +
			"apply it")
	}

	// A RECORD EVERY BUILD MUST READ carries no versioned field, set or not.
	gate := MutationRecord{
		RecordEnvelope: RecordEnvelope{OpID: "op-remove",
			Subject: DirectorySubject(), Op: OpRemove, Scope: PeopleScope(person)},
		Person: person, Reason: "left the building",
	}
	if _, err := gate.encodeWith(later); err == nil {
		t.Error("a removal carrying a later field was encoded — a gate an " +
			"older node cannot read is one it defers")
	}
	gate.V = RecordVersion + 1
	if _, err := gate.encodeWith(later); err == nil {
		t.Error("a removal carrying a later field at the version that reads it " +
			"was encoded — a gate stays at the base for ever")
	}
}

// A CREDENTIAL RIDES EVERY RECORD ITS PARTY WRITES, the gates included.
//
// A token acts as its owner, so a record's actor is the owner either way; the
// credential is what tells the two gestures apart, and a record that leaves it
// off is one more trail row reading a token's gesture as the owner's own — a
// removal and a company-wide invalidation above all, the gestures somebody
// comes back to an audit trail for. A party that acted through nothing — the
// node's own writer, which writes every sign-in and every duty — carries none.
// Mutation: leave it off the gates, or off every record, and a row goes red.
func TestACredentialRidesEveryRecordItsPartyWrites(t *testing.T) {
	t.Parallel()
	const via = "pat:0192f00d-0000-7000-8000-00000000000a"
	person := "0192f00d-0000-7000-8000-0000000000aa"
	bucket := BucketOf(person)
	now := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	for _, tc := range []struct {
		op      OpKind
		subject Subject
		scope   ScopeSet
	}{
		{OpUpdate, PersonSubject(person), PeopleScope(person)},
		{OpStatus, PersonSubject(person), PeopleScope(person)},
		{OpClose, SessionSubject("0192f00d-0000-7000-8000-0000000000bb"),
			PeopleScope(person)},
		{OpRemove, DirectorySubject(), PeopleScope(person)},
		{OpInvalidate, InvalidationSubject(), RootScope()},
		{OpEviction, EvictionSubject("node-z"), RootScope()},
		{OpSweep, SweepSubject(bucket), BucketScope(bucket)},
	} {
		through := &Writer{Actor: "ana.admin", ActorKind: iam.KindPerson,
			OperatorID: via, Now: now}
		rec, err := through.record(tc.subject, tc.op, person, tc.scope, nil, "")
		if err != nil {
			t.Fatalf("%s: build the record: %v", tc.op, err)
		}
		if rec.OperatorID != via {
			t.Errorf("%s through a token carries operator %q, want %q",
				tc.op, rec.OperatorID, via)
		}
		if _, err := Encode(rec); err != nil {
			t.Errorf("%s through a token does not encode: %v", tc.op, err)
		}

		own := &Writer{Actor: "node-a", ActorKind: iam.KindEngine, Now: now}
		rec, err = own.record(tc.subject, tc.op, person, tc.scope, nil, "")
		if err != nil {
			t.Fatalf("%s: build the node's record: %v", tc.op, err)
		}
		if rec.OperatorID != "" {
			t.Errorf("the node's own %s carries operator %q, want none",
				tc.op, rec.OperatorID)
		}
	}
}
