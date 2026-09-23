package engine

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
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

// enrolMachine enrols one active service account through the node's own
// writer, so a row under its login exists for a retained record to cover.
func enrolMachine(t *testing.T, e *Engine, id, login string) {
	t.Helper()
	if _, err := e.native.iamWriter.Enrol(t.Context(), iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: login, OpID: "enrol-" + login, Reason: "a pipeline",
	}); err != nil {
		t.Fatalf("enrol %s: %v", login, err)
	}
}

// A RETAINED RECORD COVERS EXACTLY ITS PERSON'S BUCKET — EVERY RETAINED RECORD.
//
// A Tier A token's binding, the record a login keeps its holder's work under
// and the session table each refuse to vouch for somebody while this node
// holds a record about them it could not apply: a 401 there would sign a
// person out of a session that record opened, on a node that simply has not
// applied it. The answer was the runner's EARLIEST deferral, which the runner
// read without its scope — so no retained record was ever about anybody, and
// that refusal never fired. And even with its scope, the earliest record is
// about one bucket: a later record about somebody else was invisible to it.
//
// Here three principals in three buckets are enrolled, and the node retains a
// record about A and then one about C. A and C are covered, the later C
// included; B, in a bucket neither touches, is not; and a login nobody holds
// is covered by any record there is, since nothing can say whose enrolment a
// record this node could not decode is. The CONTROL is the same four reads
// before anything was retained.
func TestARetainedRecordCoversExactlyItsPersonsBucket(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, b := retentionNode(t)
	people := peopleInBuckets(3)
	logins := []string{"ci:anna", "ci:bystander", "ci:cora"}
	for i, id := range people {
		enrolMachine(t, e, id, logins[i])
	}
	a, c := people[0], people[2]

	reader := e.native.iamReader
	cases := []struct {
		name  string
		login string
		want  bool
	}{
		{"the person the earliest retained record is about", "ci:anna", true},
		{"the person a LATER retained record is about", "ci:cora", true},
		{"a person in a bucket no retained record touches", "ci:bystander", false},
		{"a login nobody holds", "ci:nobody", true},
	}
	vouched := func(login string) iamdomain.Vouch {
		t.Helper()
		_, vouch, err := reader.PersonByLoginVouched(ctx, login)
		if err != nil {
			t.Fatalf("PersonByLoginVouched(%s): %v", login, err)
		}
		return vouch
	}
	for _, tc := range cases {
		if vouched(tc.login).Deferred {
			t.Fatalf("%s: deferred on a node that retained nothing", tc.name)
		}
	}

	retain(t, e, b, retainedFromNewerBuild, claimRecord(t, a, "anna.first"))
	retain(t, e, b, retainedUnderUnknownKey, claimRecord(t, c, "cora.later"))
	for _, tc := range cases {
		if got := vouched(tc.login).Deferred; got != tc.want {
			t.Errorf("%s: deferred %v, want %v", tc.name, got, tc.want)
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

// A MACHINE TOKEN IS VOUCHED FOR ONLY WHERE NOTHING RETAINED COVERS ITS OWNER,
// read in the SAME SNAPSHOT as the token's row.
//
// A token is checked on every request a pipeline makes, and the rows that say
// whether it still stands — the credential, the owner's epoch — are exactly
// what a retained record about the owner may have changed: a newer peer's
// revocation, or one signed under a keyring key this node was not restarted
// with. The deferral was the runner's, read beside the transaction — after the
// rows first, which paired rows from before a reprocessed revocation with
// "nothing deferred", then before them — and it carried no scope, so it covered
// nobody in either order. It is now the deferral index inside the row's own
// snapshot, which is a verdict that held at one instant.
//
// Two service accounts in different buckets each hold a token and the node
// retains a record about the first. The first token is not vouched for, the
// second is, and a token this node holds no row for is covered by any record
// retained at all, since nothing can say whose it is. The CONTROL is the same
// three reads before anything was retained.
func TestAMachineTokenIsVouchedForOnlyWhereNothingRetainedCoversItsOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, b := retentionNode(t)
	machines := peopleInBuckets(2)
	tokens := make([]string, len(machines))
	for i, id := range machines {
		login := fmt.Sprintf("ci:bucket-%d", i)
		enrolMachine(t, e, id, login)
		secret, err := credential.NewTokenSecret()
		if err != nil {
			t.Fatal(err)
		}
		tokens[i] = uuid.Must(uuid.NewV7()).String()
		if _, err := e.native.iamWriter.MintToken(ctx, iamdomain.TokenMint{
			PersonID: id, ID: tokens[i],
			Verifier:  credential.TokenVerifier(tokens[i], secret),
			ExpiresAt: time.Now().Add(credential.DefaultTokenLifetime),
			OpID:      "mint-" + login, Reason: "a pipeline's token",
		}); err != nil {
			t.Fatalf("mint %s's token: %v", login, err)
		}
	}
	unknown := uuid.Must(uuid.NewV7()).String()

	reader := e.native.iamReader
	deferredOf := func(id string) bool {
		t.Helper()
		row, err := reader.MachineToken(ctx, id)
		if err != nil {
			t.Fatalf("MachineToken(%s): %v", id, err)
		}
		return row.Deferred
	}
	for _, id := range append(slices.Clone(tokens), unknown) {
		if deferredOf(id) {
			t.Fatalf("token %s is not vouched for on a node that retained nothing", id)
		}
	}

	retain(t, e, b, retainedFromNewerBuild,
		claimRecord(t, machines[0], "ci:successor"))
	for _, tc := range []struct {
		name string
		id   string
		want bool
	}{
		{"the token whose owner a retained record is about", tokens[0], true},
		{"a token whose owner no retained record touches", tokens[1], false},
		{"a token this node holds no row for", unknown, true},
	} {
		if got := deferredOf(tc.id); got != tc.want {
			t.Errorf("%s: deferred %v, want %v", tc.name, got, tc.want)
		}
	}
}
