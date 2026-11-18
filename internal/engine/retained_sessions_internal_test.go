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

// enrolMachine creates one active service account through the node's own
// writer, so a row under its login exists for a retained record to cover. It
// is bound to no seat (optional, ADR-0032), so no company need declare a human
// seat; the id is the caller's because the cases turn on its bucket. The op id
// is MINTED with its instant ([statelog.NewOpID]): one without is read as older
// than every ledger loss and answered `unknown` unpublished once a sweep has
// run.
func enrolMachine(t *testing.T, e *Engine, id, login string) {
	t.Helper()
	if _, err := e.core.Load().iamWriter.Create(t.Context(), iamdomain.Creation{
		PersonID: id, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: login, OpID: statelog.NewOpID(time.Now(), "create"),
		Reason: "a pipeline",
	}); err != nil {
		t.Fatalf("create %s: %v", login, err)
	}
}

// A RETAINED RECORD COVERS EXACTLY ITS PERSON'S BUCKET — EVERY RETAINED RECORD.
//
// A Tier A token's binding, a login's work record and the session table each
// refuse to vouch for somebody while this node holds a record about them it
// could not apply: a 401 there would sign them out of a session that record
// opened, on a node that merely has not applied it. So coverage is per
// bucket, over every retained record, not the first.
//
// Records about A then C are retained: A and C (the later included) are
// covered, B in an untouched bucket is not, and a login nobody holds is covered
// by any record, since nothing says whose enrolment an undecodable record is.
// The CONTROL is the same four reads before anything was retained.
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

	reader := e.core.Load().iamReader
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

	retain(t, e, b, retainedFromNewerBuild, enrolRecord(t, a, "anna.first", ""))
	retain(t, e, b, retainedUnderUnknownKey, enrolRecord(t, c, "cora.later", ""))
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

// A MACHINE TOKEN IS VOUCHED FOR ONLY WHERE NOTHING RETAINED COVERS ITS OWNER,
// read in the SAME SNAPSHOT as the token's row.
//
// A retained record about the owner (a newer peer's revocation, or one under a
// keyring key this node lacks) may have changed the token's rows, credential
// and epoch. A deferral read beside their snapshot can pair pre-revocation
// rows with "nothing deferred", hence the owner-scoped index inside it.
//
// Of two tokens whose owners sit in different buckets, a record about the
// first's owner leaves only the second vouched for; a token with no row here is
// covered by any record, since nothing says whose it is. The CONTROL is the
// same three reads, retaining none.
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
		if _, err := e.core.Load().iamWriter.MintToken(ctx, iamdomain.TokenMint{
			PersonID: id, ID: tokens[i],
			Verifier:  credential.TokenVerifier(tokens[i], secret),
			ExpiresAt: time.Now().Add(credential.DefaultTokenLifetime),
			OpID:      statelog.NewOpID(time.Now(), "mint"),
			Reason:    "a pipeline's token",
		}); err != nil {
			t.Fatalf("mint %s's token: %v", login, err)
		}
	}
	unknown := uuid.Must(uuid.NewV7()).String()

	reader := e.core.Load().iamReader
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
		enrolRecord(t, machines[0], "ci:successor", ""))
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
