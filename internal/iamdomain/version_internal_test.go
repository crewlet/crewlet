package iamdomain

import (
	"encoding/json"
	"strings"
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
// deferred removal is a person off-boarded still signing in there. Mutation:
// stamp at [RecordVersion], or drop either refusal, and a case fails.
func TestTheStampIsTheLowestVersionThatReadsTheRecord(t *testing.T) {
	t.Parallel()
	person := "0192f00d-0000-7000-8000-0000000000aa"
	status := func(v int, operator string) MutationRecord {
		return MutationRecord{
			RecordEnvelope: RecordEnvelope{V: v, OpID: "op-1",
				Subject: PersonSubject(person), Op: OpStatus, Scope: PeopleScope(person)},
			Person: person, Actor: "ana.admin", ActorKind: iam.KindPerson,
			OperatorID: operator,
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

	if got, err := stamped(status(0, ""), versionedFields); err != nil || got != BaseRecordVersion {
		t.Errorf("a record carrying nothing versioned is stamped (%d, %v), want %d",
			got, err, BaseRecordVersion)
	}
	if got, err := stamped(status(0, "pat:x"), versionedFields); err != nil ||
		got != OperatorRecordVersion {
		t.Errorf("a record naming a credential is stamped (%d, %v), want %d",
			got, err, OperatorRecordVersion)
	}
	// A FIELD A LATER BUILD ADDS holds back only the records carrying it.
	later := append(VersionedFields(), statelog.VersionedField{
		Name: "MutationRecord.Reason", Since: RecordVersion + 1, Op: string(OpStatus),
		Path: []string{"reason"}})
	withReason := status(0, "")
	withReason.Reason = "left the building"
	if got, err := stamped(withReason, later); err != nil || got != RecordVersion+1 {
		t.Errorf("a record carrying a field a later build added is stamped (%d, %v), "+
			"want %d", got, err, RecordVersion+1)
	}
	if got, err := stamped(status(0, ""), later); err != nil || got != BaseRecordVersion {
		t.Errorf("a record NOT carrying that field is stamped (%d, %v), want %d — "+
			"every older node would retain it for nothing", got, err, BaseRecordVersion)
	}

	// A VERSION SET BY HAND IS KEPT AT OR ABOVE WHAT THE RECORD NEEDS, and
	// refused below it.
	if got, err := stamped(status(RecordVersion, ""), versionedFields); err != nil ||
		got != RecordVersion {
		t.Errorf("a relayed record at version %d came back (%d, %v)", RecordVersion, got, err)
	}
	if _, err := stamped(status(BaseRecordVersion, "pat:x"), versionedFields); err == nil {
		t.Error("a record naming a credential, set by hand to the base version, " +
			"was encoded — every build that predates the field would apply it")
	}

	// A RECORD EVERY BUILD MUST READ carries no versioned field, set or not.
	gate := MutationRecord{
		RecordEnvelope: RecordEnvelope{V: GateRecordVersion, OpID: "op-remove",
			Subject: PersonSubject(person), Op: OpRemove, Scope: PeopleScope(person)},
		Person: person, OperatorID: "pat:x",
	}
	if _, err := gate.encodeWith(versionedFields); err == nil {
		t.Error("a removal carrying a credential was encoded — a gate an older " +
			"node cannot read is one it defers")
	}
	gate.V = OperatorRecordVersion
	if _, err := gate.encodeWith(versionedFields); err == nil {
		t.Error("a removal carrying a credential at the version that reads it was " +
			"encoded — a gate is pinned at version 1 for ever")
	}
}

// A CREDENTIAL RIDES EXACTLY THE RECORDS WHOSE TRAIL ROW READS IT, and only at
// the version that defines it.
//
// A token acts as its owner, so the trail's actor is the owner either way; the
// credential is what tells the two gestures apart, and a record that writes a
// trail row without it is one more row reading a token's gesture as the
// owner's own. Carried anywhere else it is read by nothing and still defers the
// record on every older node — and on a GATE it would be a field on a payload
// pinned for ever, which is a gate an older node cannot read, which is a halt.
// Mutation: carry it on every record, or on none, or write it at the base
// version, and a row below goes red.
func TestACredentialRidesExactlyTheRecordsWhoseTrailRowReadsIt(t *testing.T) {
	t.Parallel()
	const via = "pat:0192f00d-0000-7000-8000-00000000000a"
	person := "0192f00d-0000-7000-8000-0000000000aa"
	bucket := BucketOf(person)
	now := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	for _, tc := range []struct {
		op      OpKind
		subject Subject
		scope   ScopeSet
		names   bool
		// through and own are the versions the record is written at by
		// the party acting through a token and by the node's own writer.
		through, own int
	}{
		{OpUpdate, PersonSubject(person), PeopleScope(person), true,
			OperatorRecordVersion, BaseRecordVersion},
		{OpStatus, PersonSubject(person), PeopleScope(person), true,
			OperatorRecordVersion, BaseRecordVersion},
		{OpClose, SessionSubject("0192f00d-0000-7000-8000-0000000000bb"),
			PeopleScope(person), true, OperatorRecordVersion, BaseRecordVersion},
		// THE GATES, pinned at version 1 for ever.
		{OpRemove, PersonSubject(person), PeopleScope(person), false,
			GateRecordVersion, GateRecordVersion},
		{OpInvalidate, InvalidationSubject(), RootScope(), false,
			GateRecordVersion, GateRecordVersion},
		// NO TRAIL ROW, so nothing reads it.
		{OpSweep, SweepSubject(bucket), BucketScope(bucket), false,
			SweepRecordVersion, SweepRecordVersion},
	} {
		through := &Writer{Actor: "ana.admin", ActorKind: iam.KindPerson,
			OperatorID: via, Now: now}
		rec, err := through.record(tc.subject, tc.op, person, tc.scope, nil, "")
		if err != nil {
			t.Fatalf("%s: build the record: %v", tc.op, err)
		}
		want := ""
		if tc.names {
			want = via
		}
		if rec.OperatorID != want {
			t.Errorf("%s through a token carries operator %q, want %q",
				tc.op, rec.OperatorID, want)
		}
		if got := encodedVersion(t, rec); got != tc.through {
			t.Errorf("%s through a token is written at version %d, want %d",
				tc.op, got, tc.through)
		}

		// AND A PARTY THAT ACTED THROUGH NOTHING — the node's own
		// writer — writes every one of them where it always did.
		own := &Writer{Actor: "node-a", ActorKind: iam.KindEngine, Now: now}
		rec, err = own.record(tc.subject, tc.op, person, tc.scope, nil, "")
		if err != nil {
			t.Fatalf("%s: build the node's record: %v", tc.op, err)
		}
		if got := encodedVersion(t, rec); rec.OperatorID != "" || got != tc.own {
			t.Errorf("the node's own %s carries operator %q at version %d, "+
				"want none at %d", tc.op, rec.OperatorID, got, tc.own)
		}
	}
}

// encodedVersion is the version rec is published at.
func encodedVersion(t *testing.T, rec MutationRecord) int {
	t.Helper()
	body, err := Encode(rec)
	if err != nil {
		t.Fatalf("encode the %s record: %v", rec.Op, err)
	}
	env, err := DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode the %s record: %v", rec.Op, err)
	}
	return env.V
}

// A CREDENTIAL ON A RECORD BELOW ITS VERSION IS CARRIED, NOT READ.
//
// No writer produces one — [Encode] refuses it — but a peer that broke the rule
// would have it applied by every node of the rolling upgrade the version exists
// to protect as an unknown key those builds carry: no column, and a document
// with the key in it. So this build does the same: reading it would write a
// trail row those nodes do not hold, for a record none of them will ever apply
// again. And relaying it keeps the version its writer gave it, since what this
// build carries is not what its own table stamps. Mutation: drop the carry and
// the credential is read; stamp over the carried bytes and the relay is
// refused.
func TestACredentialBelowItsVersionIsCarriedNotRead(t *testing.T) {
	t.Parallel()
	person := "0192f00d-0000-7000-8000-0000000000aa"
	// AS THE PEER PUBLISHED IT, past the encoder that would have refused it.
	payload, err := json.Marshal(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: SweepRecordVersion, OpID: "op-early",
			Subject: PersonSubject(person), Op: OpStatus,
			Scope: PeopleScope(person),
		},
		Person: person, Actor: "ana.admin", ActorKind: iam.KindPerson,
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	rec, err := Decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.OperatorID != "" {
		t.Errorf("a version-%d record's credential was read as %q", rec.V,
			rec.OperatorID)
	}
	again, err := Encode(rec)
	if err != nil {
		t.Fatalf("re-encode the relayed record: %v", err)
	}
	if !strings.Contains(string(again), `"operator_id":"pat:0192f00d`) {
		t.Errorf("the carried credential did not round-trip: %s", again)
	}
	if env, err := DecodeEnvelope(again); err != nil || env.V != SweepRecordVersion {
		t.Errorf("the relayed record went out at (%d, %v), want the version its "+
			"writer gave it, %d", env.V, err, SweepRecordVersion)
	}
}
