package iamdomain

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY RECORD IS WRITTEN AT THE LOWEST VERSION THAT CARRIES ITS MEANING.
//
// A record above a peer's decode ceiling is DEFERRED on that peer, and a
// deferred record blocks every later record in its scope there. So the ceiling
// is what this build reads and never what it writes: a build that wrote every
// record at the ceiling would have a rolling upgrade defer everything on every
// older node — and a BARRIER an older node deferred is a linearizable read on
// that node that waits for ever. The sweep is the one op whose meaning moved,
// naming a credential is the one field that did, and a gate is pinned for ever
// whatever it would otherwise carry.
func TestEveryRecordIsWrittenAtTheLowestVersionThatCarriesIt(t *testing.T) {
	t.Parallel()
	for _, written := range []int{SweepRecordVersion, OperatorRecordVersion} {
		if RecordVersion < written {
			t.Fatalf("this build writes version %d and reads only up to %d",
				written, RecordVersion)
		}
	}
	for _, op := range OpKinds {
		for _, operated := range []bool{false, true} {
			want := BaseRecordVersion
			switch {
			case op == OpRemove || op == OpEviction || op == OpInvalidate:
				want = GateRecordVersion
			case operated:
				want = OperatorRecordVersion
			case op == OpSweep:
				want = SweepRecordVersion
			}
			if got := writeVersion(op, operated); got != want {
				t.Errorf("%s naming a credential %v is written at version %d, "+
					"want %d", op, operated, got, want)
			}
		}
	}

	payload, err := EncodeBarrier(statelog.Envelope{
		Kind: statelog.BarrierKind,
	})
	if err != nil {
		t.Fatalf("EncodeBarrier: %v", err)
	}
	env, err := DecodeEnvelope(payload)
	if err != nil {
		t.Fatalf("decode the barrier: %v", err)
	}
	if env.V != BaseRecordVersion {
		t.Errorf("a barrier is written at version %d, want %d — an older node "+
			"defers it, and its linearizable read waits for ever",
			env.V, BaseRecordVersion)
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
	}{
		{OpUpdate, PersonSubject(person), PeopleScope(person), true},
		{OpStatus, PersonSubject(person), PeopleScope(person), true},
		{OpClose, SessionSubject("0192f00d-0000-7000-8000-0000000000bb"),
			PeopleScope(person), true},
		{OpBootstrap, BootstrapSubject(), BucketScope(BootstrapBucket()), true},
		// THE GATES, pinned at version 1 for ever.
		{OpRemove, PersonSubject(person), PeopleScope(person), false},
		{OpInvalidate, InvalidationSubject(), RootScope(), false},
		// NO TRAIL ROW, so nothing reads it.
		{OpSweep, SweepSubject(bucket), BucketScope(bucket), false},
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
		if got := writeVersion(tc.op, want != ""); rec.V != got {
			t.Errorf("%s through a token is written at version %d, want %d",
				tc.op, rec.V, got)
		}

		// AND A PARTY THAT ACTED THROUGH NOTHING — the node's own
		// writer — writes every one of them where it always did.
		own := &Writer{Actor: "node-a", ActorKind: iam.KindEngine, Now: now}
		rec, err = own.record(tc.subject, tc.op, person, tc.scope, nil, "")
		if err != nil {
			t.Fatalf("%s: build the node's record: %v", tc.op, err)
		}
		if rec.OperatorID != "" || rec.V != writeVersion(tc.op, false) {
			t.Errorf("the node's own %s carries operator %q at version %d, "+
				"want none at %d", tc.op, rec.OperatorID, rec.V,
				writeVersion(tc.op, false))
		}
	}
}

// A CREDENTIAL ON A RECORD BELOW ITS VERSION IS CARRIED, NOT READ.
//
// No writer produces one; a record that had one would be applied by every
// node of the rolling upgrade the version exists to protect as an unknown key
// those builds carry — no column, and a document with the key in it. So this
// build does the same: reading it would write a trail row those nodes do not
// hold, for a record none of them will ever apply again. Mutation: drop the
// carry and the credential is read.
func TestACredentialBelowItsVersionIsCarriedNotRead(t *testing.T) {
	t.Parallel()
	person := "0192f00d-0000-7000-8000-0000000000aa"
	payload, err := Encode(MutationRecord{
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
		t.Fatalf("re-encode: %v", err)
	}
	if !strings.Contains(string(again), `"operator_id":"pat:0192f00d`) {
		t.Errorf("the carried credential did not round-trip: %s", again)
	}
}
