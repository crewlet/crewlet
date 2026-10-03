package iamdomain

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY RECORD THE WRITER BUILDS IS STAMPED THE VERSION IT HAS ALWAYS HAD.
//
// A peer decides by the version alone whether to apply a record or retain it,
// so the version a record of each kind travels at is a contract between the
// builds of one rolling upgrade, and moving it in either direction is a fault:
// one higher and every older node retains what it could have applied — every
// sign-in, if it is a session start — one lower and an older node applies, by
// rules that drop it, a field or a predicate it does not know. The table is
// that contract, stated as numbers rather than computed: a gate is 1 for ever,
// a sweep 2, a record naming the credential its actor acted through 3, a
// session start that may only enrol and every invitation 4, and everything
// else the base. Every op is built through the writer's own funnel
// ([Writer.record]) with the payload its gesture carries, by a party that acted
// through a credential and by one that did not — the node's own, which writes
// every sign-in and every duty — and the two records nothing decides are
// encoded as the framework asks for them.
//
// Mutation: drop a row from the field table, or the sweep's marker, or stamp
// every record at [RecordVersion], and a row here fails.
func TestEveryRecordTheWriterBuildsKeepsItsVersion(t *testing.T) {
	t.Parallel()
	const (
		via    = "pat:0192f00d-0000-7000-8000-00000000000a"
		person = "0192f00d-0000-7000-8000-0000000000aa"
		blind  = "b1ind00000000000000000000000000000000000000000000000000000000ab"
	)
	bucket := BucketOf(person)
	now := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	session := func(restricted bool) any {
		return Session{V: DocumentVersion, Person: person, Epoch: 1,
			AbsoluteExpiresAt: now().Add(time.Hour), ProvedAt: now(),
			EnrolmentOnly: restricted}
	}
	invitation := func(seat string) any {
		return Invitation{V: DocumentVersion, ID: "inv-1", Sealed: "sealed:address",
			Verifier: InvitationVerifier("secret"), Seat: seat,
			ExpiresAt: now().Add(time.Hour)}
	}
	type build struct {
		name    string
		op      OpKind
		subject Subject
		scope   ScopeSet
		person  string
		payload any
		// base and operated are the versions a record of this build is
		// stamped at by a party acting through no credential and by one
		// acting through a machine token.
		base, operated int
	}
	builds := []build{
		{"an invitation", OpInvite, EmailSubject(blind), BucketScope(BucketOf(blind)),
			"", invitation(""), 4, 4},
		{"an invitation that binds a seat", OpInvite, EmailSubject(blind),
			BucketScope(BucketOf(blind)), "", invitation("0192f00d-0000-7000-8000-0000000000cc"),
			4, 4},
		{"an address claim", OpClaim, EmailSubject(blind), PeopleScope(person), person,
			Claim{V: DocumentVersion, Person: person, Sealed: "sealed:address"}, 1, 3},
		{"a login claim", OpClaim, LoginSubject("sarah.chen"), PeopleScope(person), person,
			Claim{V: DocumentVersion, Person: person}, 1, 3},
		{"a redemption", OpRedeem, EmailSubject(blind),
			BucketScope(bucket, BucketOf(blind)), person,
			Claim{V: DocumentVersion, Person: person, Sealed: "sealed:address"}, 1, 3},
		{"a release", OpRelease, LoginSubject("sarah.chen"), PeopleScope(person), person,
			nil, 1, 3},
		{"an enrolment", OpEnrol, PersonSubject(person), PeopleScope(person), person,
			Person{V: DocumentVersion, Kind: iam.KindPerson, Stage: iam.StageActive,
				NameSealed: "sealed:name"}, 1, 3},
		{"a person's update", OpUpdate, PersonSubject(person), PeopleScope(person), person,
			Person{V: DocumentVersion, Kind: iam.KindPerson, Stage: iam.StageActive,
				NameSealed: "sealed:name"}, 1, 3},
		{"a stage change", OpStatus, PersonSubject(person), PeopleScope(person), person,
			nil, 1, 3},
		{"a revocation", OpRevoke, PersonSubject(person), PeopleScope(person), person,
			nil, 1, 3},
		{"a removal", OpRemove, PersonSubject(person), PeopleScope(person), person,
			nil, 1, 1},
		{"a session start", OpOpen, SessionSubject("lineage-1"), PeopleScope(person), person,
			session(false), 1, 3},
		{"a session start that may only enrol", OpOpen, SessionSubject("lineage-2"),
			PeopleScope(person), person, session(true), 4, 4},
		{"a session end", OpClose, SessionSubject("lineage-1"), PeopleScope(person), person,
			nil, 1, 3},
		{"an invalidation", OpInvalidate, InvalidationSubject(), RootScope(), "",
			nil, 1, 1},
		{"a sweep", OpSweep, SweepSubject(bucket), BucketScope(bucket), "",
			Sweep{V: DocumentVersion, Bucket: bucket, Expired: now()}, 2, 2},
		{"an eviction", OpEviction, EvictionSubject("node-z"), RootScope(), "",
			Eviction{V: GateRecordVersion, By: "suite"}, 1, 1},
	}
	covered := map[OpKind]bool{OpBarrier: true, OpGeneration: true}
	for _, b := range builds {
		covered[b.op] = true
		for _, party := range []struct {
			name   string
			writer *Writer
			want   int
		}{
			{"the node's own writer", &Writer{Actor: "node-a",
				ActorKind: iam.KindMachine, Now: now}, b.base},
			{"a party acting through a token", &Writer{Actor: "ana.admin",
				ActorKind: iam.KindPerson, OperatorID: via, Now: now}, b.operated},
		} {
			got, err := stampedAsWritten(party.writer, b.subject, b.op, b.person,
				b.scope, b.payload)
			if err != nil {
				t.Errorf("%s by %s: %v", b.name, party.name, err)
				continue
			}
			if got != party.want {
				t.Errorf("%s by %s is stamped version %d, and it has always been "+
					"%d", b.name, party.name, got, party.want)
			}
		}
	}
	for _, op := range OpKinds {
		if !covered[op] {
			t.Errorf("no build here writes a %s record — every op the writer "+
				"can emit is a row of this table", op)
		}
	}

	// THE TWO RECORDS NO WRITER DECIDES: the read index's barrier and a
	// reanchor's generation, both at the base for ever.
	barrier, err := EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind})
	if err != nil {
		t.Fatalf("EncodeBarrier: %v", err)
	}
	generation, keeps, err := GenerationRecord{}.GenerationRecord(statelog.GenerationFacts{
		Generation: 2, Case: statelog.ReanchorRecreated,
		Inputs: statelog.ReanchorInputs{Stream: Domain{}.Stream().Name},
		By:     "ops", Writer: "node-a", At: now(),
	})
	if err != nil || !keeps {
		t.Fatalf("encode a generation: (%v, %v)", keeps, err)
	}
	for name, body := range map[string][]byte{
		"a barrier": barrier, "a generation": generation.Payload,
	} {
		env, err := DecodeEnvelope(body)
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if env.V != BaseRecordVersion {
			t.Errorf("%s is stamped version %d, and it has always been %d", name,
				env.V, BaseRecordVersion)
		}
	}
}

// stampedAsWritten builds one record through the writer's funnel with the
// payload its gesture carries, as the gesture does, and reads back the version
// it is published at.
func stampedAsWritten(w *Writer, subject Subject, op OpKind, person string,
	scope ScopeSet, payload any) (int, error) {

	var mutation []byte
	if payload != nil {
		var err error
		if mutation, err = json.Marshal(payload); err != nil {
			return 0, err
		}
	}
	rec, err := w.record(subject, op, person, scope, mutation, "")
	if err != nil {
		return 0, fmt.Errorf("build the record: %w", err)
	}
	body, err := Encode(rec)
	if err != nil {
		return 0, fmt.Errorf("encode the record: %w", err)
	}
	env, err := DecodeEnvelope(body)
	if err != nil {
		return 0, fmt.Errorf("decode the envelope: %w", err)
	}
	return env.V, nil
}
