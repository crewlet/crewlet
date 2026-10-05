package iamdomain

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY RECORD THE WRITER BUILDS IS STAMPED AT THE BASE VERSION.
//
// A peer decides by the version alone whether to apply a record or retain it,
// so the version a record of each kind travels at is a contract between the
// builds of one rolling upgrade: one higher than its fields need and every
// older node retains what it could have applied — every sign-in, if it is a
// session start. The field table is empty, so every record of every op is
// stamped at the base, whoever writes it: a party that acted through a
// credential and one that did not — the node's own, which writes every sign-in
// and every duty — and the two records nothing decides are encoded as the
// framework asks for them. Mutation: stamp every record at a version above the
// base, and a row here fails.
func TestEveryRecordTheWriterBuildsIsStampedAtTheBase(t *testing.T) {
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
		return Invitation{V: DocumentVersion, ID: "inv-1", EmailBlind: blind,
			Sealed: "sealed:address", Verifier: InvitationVerifier("secret"),
			Seat: seat, ExpiresAt: now().Add(time.Hour)}
	}
	document := Person{V: DocumentVersion, Kind: iam.KindPerson,
		Stage: iam.StageActive, NameSealed: "sealed:name"}
	type build struct {
		name    string
		op      OpKind
		subject Subject
		scope   ScopeSet
		person  string
		payload any
	}
	builds := []build{
		{"an invitation", OpInvite, DirectorySubject(), BucketScope(BucketOf(blind)),
			"", invitation("")},
		{"an invitation that binds a seat", OpInvite, DirectorySubject(),
			BucketScope(BucketOf(blind)), "", invitation("0192f00d-0000-7000-8000-0000000000cc")},
		{"an invitation's cancellation", OpCancel, DirectorySubject(),
			BucketScope(BucketOf(blind)), "",
			Cancellation{V: DocumentVersion, Invitation: "inv-1", EmailBlind: blind}},
		{"a password change", OpPassword, PersonSubject(person), PeopleScope(person),
			person, PasswordChange{V: DocumentVersion, Person: document, Epoch: 2}},
		{"an enrolment", OpEnrol, DirectorySubject(), PeopleScope(person), person,
			Enrolled{V: DocumentVersion, Person: document,
				Holds: Identifiers{EmailBlind: blind, Login: "sarah.chen"}}},
		{"a redemption", OpEnrol, DirectorySubject(),
			BucketScope(bucket, BucketOf(blind)), person,
			Enrolled{V: DocumentVersion, Person: document,
				Holds:      Identifiers{EmailBlind: blind, Login: "sarah.chen"},
				Invitation: "inv-1"}},
		{"an identity change", OpIdentity, DirectorySubject(), PeopleScope(person),
			person, IdentityChange{V: DocumentVersion, Login: "sarah.c"}},
		{"a person's update", OpUpdate, PersonSubject(person), PeopleScope(person), person,
			document},
		{"a stage change", OpStatus, PersonSubject(person), PeopleScope(person), person,
			nil},
		{"a revocation", OpRevoke, PersonSubject(person), PeopleScope(person), person,
			nil},
		{"a removal", OpRemove, DirectorySubject(), PeopleScope(person), person,
			nil},
		{"a session start", OpOpen, SessionSubject("lineage-1"), PeopleScope(person), person,
			session(false)},
		{"a session start that may only enrol", OpOpen, SessionSubject("lineage-2"),
			PeopleScope(person), person, session(true)},
		{"a session end", OpClose, SessionSubject("lineage-1"), PeopleScope(person), person,
			nil},
		{"an invalidation", OpInvalidate, InvalidationSubject(), RootScope(), "",
			nil},
		{"a sweep", OpSweep, SweepSubject(bucket), BucketScope(bucket), "",
			Sweep{V: DocumentVersion, Bucket: bucket, Expired: now()}},
		{"an eviction", OpEviction, EvictionSubject("node-z"), RootScope(), "",
			Eviction{V: DocumentVersion, By: "suite"}},
	}
	covered := map[OpKind]bool{OpBarrier: true, OpGeneration: true}
	for _, b := range builds {
		covered[b.op] = true
		for _, party := range []struct {
			name   string
			writer *Writer
		}{
			{"the node's own writer", &Writer{Actor: "node-a",
				ActorKind: iam.KindMachine, Now: now}},
			{"a party acting through a token", &Writer{Actor: "ana.admin",
				ActorKind: iam.KindPerson, OperatorID: via, Now: now}},
		} {
			got, err := stampedAsWritten(party.writer, b.subject, b.op, b.person,
				b.scope, b.payload)
			if err != nil {
				t.Errorf("%s by %s: %v", b.name, party.name, err)
				continue
			}
			if got != BaseRecordVersion {
				t.Errorf("%s by %s is stamped version %d, want the base %d",
					b.name, party.name, got, BaseRecordVersion)
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
			t.Errorf("%s is stamped version %d, want the base %d", name,
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
