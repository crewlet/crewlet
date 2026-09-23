package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// peopleInBuckets mints n people who fall in n different identity buckets, so a
// record retained about one of them is provably about none of the others.
func peopleInBuckets(n int) []string {
	seen := map[iamdomain.Bucket]bool{}
	var out []string
	for len(out) < n {
		id := uuid.Must(uuid.NewV7()).String()
		if b := iamdomain.BucketOf(id); !seen[b] {
			seen[b] = true
			out = append(out, id)
		}
	}
	return out
}

// sessionRecord is a peer's login opening a session for somebody: the record
// whose rows a node that retained it never writes.
func sessionRecord(lineage, person string) iamdomain.MutationRecord {
	return iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.BaseRecordVersion, OpID: uuid.Must(uuid.NewV7()).String(),
			Subject: iamdomain.SessionSubject(lineage), Op: iamdomain.OpOpen,
			CreatedAt: time.Now().UTC(), Writer: "node-peer",
			Scope: iamdomain.PeopleScope(person),
		},
		Person: person, Actor: "node-peer", ActorKind: "machine",
	}
}

// A RETAINED RECORD COVERS EXACTLY ITS PERSON'S BUCKET — EVERY RETAINED RECORD.
//
// The session table and a Tier A token's binding refuse to vouch for somebody
// while this node holds a record about them it could not apply: a 401 there
// would sign a person out of a session that record opened, on a node that
// simply has not applied it. The answer was the runner's EARLIEST deferral,
// which the runner read without its scope — so no retained record was ever
// about anybody, and that refusal never fired. And even with its scope, the
// earliest record is about one bucket: a later record about somebody else was
// invisible to it.
//
// Here the node retains a record about A and then one about C. A and C are
// covered, the later C included; B, in a bucket neither touches, is not; and
// nobody-in-particular is covered by any record there is.
func TestARetainedRecordCoversExactlyItsPersonsBucket(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, b := retentionNode(t)
	people := peopleInBuckets(3)
	a, bystander, c := people[0], people[1], people[2]
	retain(t, e, b, retainedFromNewerBuild, claimRecord(t, a, "anna.first"))
	retain(t, e, b, retainedUnderUnknownKey, claimRecord(t, c, "cora.later"))

	reader := e.native.iamReader
	for _, tc := range []struct {
		name   string
		person string
		want   bool
	}{
		{"the person the earliest retained record is about", a, true},
		{"the person a LATER retained record is about", c, true},
		{"a person in a bucket no retained record touches", bystander, false},
		{"nobody in particular", "", true},
	} {
		_, deferred, err := reader.Staleness(ctx, tc.person)
		if err != nil {
			t.Fatalf("%s: Staleness: %v", tc.name, err)
		}
		if deferred != tc.want {
			t.Errorf("%s: deferred %v, want %v", tc.name, deferred, tc.want)
		}
	}
	// AND THE SESSION TABLE READS THE SAME FACT, in the rows' own snapshot:
	// a bearer for C is stalled rather than told its session is gone.
	identity, err := reader.Resolve(ctx, uuid.Must(uuid.NewV7()).String(), c)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !identity.Deferred {
		t.Error("Resolve reads C's rows as complete while this node holds a " +
			"record about C it could not apply — an absent session row there " +
			"answers 401 and signs C out of a session the record opened")
	}
}

// A REFRESH GRANT WHOSE SESSION THIS NODE RETAINED IS KEPT.
//
// The key duty collects the refresh token of every session that is over, and a
// session was over when its row was missing on a node whose checkpoint was past
// the session's start. The checkpoint moves past a record the node retains, so
// a node that retained the session's own opening record read the session as
// over and destroyed its token — the one thing the deactivation probe asks the
// provider with, so a person disabled there kept the session until its
// absolute deadline.
//
// The CONTROL is on the same node and the same pass: a grant whose session row
// is missing where nothing about its person is retained IS collected.
func TestARefreshGrantWhoseSessionThisNodeRetainedIsKept(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, b := retentionNode(t)
	people := peopleInBuckets(2)
	retainedPerson, gonePerson := people[0], people[1]

	custody := e.RefreshCustody()
	if custody == nil {
		t.Fatal("the node keeps no refresh custody")
	}
	lineage := uuid.Must(uuid.NewV7()).String()
	seq := retain(t, e, b, retainedUnderUnknownKey, sessionRecord(lineage, retainedPerson))
	started := statelog.Position{Stream: iamdomain.Domain{}.Stream().Name,
		Generation: e.native.log.Domain(iamdomain.Domain{}.Name()).runner.Committed().Generation,
		Seq:        seq}
	if err := custody.Hold(ctx, iamdomain.RefreshGrant{
		Lineage: lineage, Person: retainedPerson, Issuer: "https://idp.example.com",
		Token: "refresh-retained", Start: uint64(started.Packed()),
	}, time.Now()); err != nil {
		t.Fatalf("hold the retained session's grant: %v", err)
	}
	gone := uuid.Must(uuid.NewV7()).String()
	if err := custody.Hold(ctx, iamdomain.RefreshGrant{
		Lineage: gone, Person: gonePerson, Issuer: "https://idp.example.com",
		Token: "refresh-gone", Start: 1,
	}, time.Now()); err != nil {
		t.Fatalf("hold the gone session's grant: %v", err)
	}

	collected, skipped, err := custody.CollectEnded(ctx, e.native.iamReader, time.Now())
	if err != nil || len(skipped) != 0 {
		t.Fatalf("CollectEnded: %v %v", err, skipped)
	}
	if !slices.Equal(collected, []string{gone}) {
		t.Fatalf("the collection took %v, want exactly the session nothing "+
			"retained is about (%s) — a grant whose session this node retained "+
			"is a live session's", collected, gone)
	}
	grants, _, err := custody.Held(ctx)
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	if len(grants) != 1 || grants[0].Lineage != lineage {
		t.Errorf("custody holds %+v, want the retained session's grant alone", grants)
	}
}
