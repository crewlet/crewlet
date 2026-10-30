package iamdomain_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A REFUSAL IS TOLD WITHOUT THE IDS A PERSON NEVER CHOSE, AND LOGGED WITH THEM.
//
// The identity directory's surface and the sign-in surface answer a refusal's
// sentence as the detail a person reads, and the sentences named what they
// refused by id: "invitation 0192f… has already been used", "session 4c1e…
// belongs to person 0192a…, not 0192b…", "0192c… is a person, so only they can
// create…", "request 0192d… already sent invitation 0192e…", the token a
// request came through by its credential id — and a set of permissions as Go
// prints a slice, "[worklet doclet]". The ids now ride
// [iamdomain.Refusal.About], which [iamdomain.Refusal.Error] — what a log line
// prints — carries and [iamdomain.Sentence] does not; a reused key's ids are
// the key itself and what it derives, which the answer and its log carry as
// the op id. And the class word in front of a refusal ("not allowed: ") and of
// a value's refusal ("invalid login: ") is cut.
//
// Walked through the real writer and applier, one row per refusal that named
// an id. Mutations: write any of the ids back into its sentence and that row
// goes red on the sentence; drop About from Error and every row naming ids
// for the log goes red; print a grant set with %v again and the two
// permission rows do; stop Sentence cutting the class word and every refusal
// row does.
func TestARefusalTellsAPersonNoIdAndTheLogEveryOne(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	ctx := rig.t.Context()
	owner := tokenOwner(t, rig, "jane.doe")
	other := tokenOwner(t, rig, "ana.other")
	suspended := tokenOwner(t, rig, "sam.away")
	if err := rig.draining(func() error {
		_, err := rig.writer.SetStage(ctx, suspended, iam.StageSuspended,
			operationKey(), "on leave")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()

	spentAt := "spent@example.com"
	spentSeat := rig.vacantSeat("spent-joiner")
	spent, err := inviteFor(t, rig, spentAt, spentSeat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = redeemAs(t, rig, spent, spentAt, spent.Secret, spentSeat); err != nil {
		t.Fatalf("redeem the invitation: %v", err)
	}
	rig.drain()
	openAt := "open@example.com"
	openSeat := rig.vacantSeat("open-joiner")
	open, err := inviteFor(t, rig, openAt, openSeat)
	if err != nil {
		t.Fatal(err)
	}
	lineage := rig.openSession(owner, brokerAt.Add(time.Hour))

	reused := uuid.Must(uuid.NewV7()).String()
	if err = rig.enrol(iamdomain.Creation{
		PersonID: reused, Kind: iam.KindMachine, Stage: iam.StageActive,
		Name: "Release pipeline", Login: "svc:release",
		Grants: []iam.Grant{iam.GrantWorklet}, OpID: operationKey(),
		Reason: "a pipeline",
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	secondKey := operationKey()

	// THE TWO GESTURES THAT DRAIN, made here rather than in the table, so
	// each runs with the test's own context in view.
	otherSession := rig.draining(func() error {
		_, err := rig.writer.CloseSession(ctx, lineage, other, "signed out",
			operationKey())
		return err
	})
	reusedKey := rig.draining(func() error {
		_, err := rig.writer.Create(ctx, iamdomain.Creation{
			PersonID: reused, Kind: iam.KindMachine, Stage: iam.StageActive,
			Name: "Another pipeline", Login: "svc:another",
			Grants: []iam.Grant{iam.GrantWorklet}, OpID: secondKey,
			Reason: "a pipeline",
		})
		return err
	})

	tokenAdmin := *anAdmin(rig)
	tokenAdmin.OperatorID = iam.MachineTokenName(uuid.Must(uuid.NewV7()).String())
	// THE OWNER, ACTING THROUGH ONE OF THEIR OWN TOKENS, which may never
	// mint another.
	tokenOwnerWriter := asOwner(rig, owner)
	tokenOwnerWriter.OperatorID = iam.MachineTokenName(uuid.Must(uuid.NewV7()).String())
	// AN INVITATION AT ITS DEADLINE on the rig's clock, which opens nothing.
	expiredAt, expiredSeat := "late@example.com", "late-joiner"
	expired := issueOnto(t, rig, expiredAt, expiredSeat, brokerAt)
	mint := func(w *iamdomain.Writer, in iamdomain.TokenMint) error {
		_, _, err := mintFor(t, rig, w, in)
		return err
	}

	for _, tc := range []struct {
		name    string
		gesture func() error
		says    string   // in the sentence a person reads
		logged  []string // in the error a log prints, and never in the sentence
		hidden  []string // in neither: what the answer's op id already carries
	}{
		{"a spent invitation's link", func() error {
			_, err := redeemAs(t, rig, spent, spentAt, spent.Secret, spentSeat)
			return err
		}, "this invitation has already been used", []string{spent.ID}, nil},
		{"a secret that isn't the invitation's", func() error {
			_, err := redeemAs(t, rig, open, openAt, "not-the-secret", openSeat)
			return err
		}, "that isn't this invitation's link", []string{open.ID}, nil},
		{"another person's session", func() error { return otherSession },
			"that session isn't theirs", []string{lineage, owner, other}, nil},
		{"a person's token minted by an administrator", func() error {
			return mint(anAdmin(rig), iamdomain.TokenMint{PersonID: owner})
		}, "only they can create their own access tokens", []string{owner}, nil},
		{"an owner who may not act", func() error {
			return mint(asOwner(rig, suspended), iamdomain.TokenMint{
				PersonID: suspended})
		}, "they're suspended, so a token can't act for them",
			[]string{suspended}, nil},
		{"a permission the owner does not hold", func() error {
			return mint(asOwner(rig, owner), iamdomain.TokenMint{PersonID: owner,
				Grants: []iam.Grant{iam.GrantAdmin}})
		}, "a token can only have what its owner has, and its owner doesn't have admin",
			[]string{`"worklet" and "doclet"`}, nil},
		{"a name this build does not know, on a token", func() error {
			return mint(asOwner(rig, owner), iamdomain.TokenMint{PersonID: owner,
				Grants: []iam.Grant{"swarm"}})
		}, iam.NotAGrant("swarm"), nil, nil},
		{"an expired invitation's link", func() error {
			_, err := redeemAs(t, rig, expired, expiredAt, expired.Secret, expiredSeat)
			return err
		}, "this invitation has expired", []string{expired.ID}, nil},
		{"an invitation for another address", func() error {
			_, err := redeemAs(t, rig, open, "somebody.else@example.com", open.Secret,
				openSeat)
			return err
		}, "this invitation is for a different email address", []string{open.ID}, nil},
		{"a token minting a token", func() error {
			return mint(tokenOwnerWriter, iamdomain.TokenMint{PersonID: owner})
		}, "A token can't create another, so sign in to create one",
			[]string{tokenOwnerWriter.OperatorID}, nil},
		{"a permission the caller does not hold", func() error {
			// THE CHECK AN EDIT ASKS BEFORE ANYTHING IS PUBLISHED, which
			// the directory's surface answers a PATCH conferring it with.
			return asOwner(rig, owner, iam.GrantWorklet).MayConfer(
				[]iam.Grant{iam.GrantWorklet}, []iam.Grant{iam.GrantAdmin})
		}, "you can't give admin because you don't have it", []string{"worklet"}, nil},
		{"a name this build does not know", func() error {
			_, err := anAdmin(rig).Create(ctx, iamdomain.Creation{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
				Stage: iam.StageActive, Login: "svc:future",
				Grants: []iam.Grant{"swarm"}, OpID: operationKey(),
				Reason: "a pipeline",
			})
			return err
		}, `Use "admin", "worklet"`, nil, nil},
		{"an administrative record through a machine token", func() error {
			_, err := tokenAdmin.InvalidateAll(ctx, operationKey(), "a restore")
			return err
		}, "Sign in and do it yourself", []string{tokenAdmin.OperatorID}, nil},
		{"a key that already added somebody", func() error { return reusedKey },
			"it already added somebody", nil, []string{reused, secondKey}},
	} {
		err := tc.gesture()
		if err == nil {
			t.Errorf("%s: was not refused", tc.name)
			continue
		}
		said := iamdomain.Sentence(err)
		if !strings.Contains(said, tc.says) {
			t.Errorf("%s: a person is told %q, want it to say %q", tc.name, said,
				tc.says)
		}
		for _, word := range []string{iamdomain.ErrRefused.Error() + ":",
			"[", "]", "iamdomain"} {
			if strings.Contains(said, word) {
				t.Errorf("%s: a person is told %q, which carries %q", tc.name,
					said, word)
			}
		}
		for _, id := range append(append([]string{}, tc.logged...), tc.hidden...) {
			if strings.Contains(said, id) {
				t.Errorf("%s: a person is told %q, which names %s", tc.name,
					said, id)
			}
		}
		for _, id := range tc.logged {
			if !strings.Contains(err.Error(), id) {
				t.Errorf("%s: the log is told %q, which loses %s", tc.name,
					err.Error(), id)
			}
		}
		for _, id := range tc.hidden {
			if strings.Contains(err.Error(), id) {
				t.Errorf("%s: the error says %q, which names %s the answer "+
					"already carries", tc.name, err.Error(), id)
			}
		}
		var refusal *iamdomain.Refusal
		if errors.As(err, &refusal) == errors.Is(err, iamdomain.ErrOperationReused) {
			t.Errorf("%s: answered %v, want a refusal or a reused key", tc.name, err)
		}
	}
}

// A VALUE'S REFUSAL IS TOLD WITHOUT ITS SENTINEL.
//
// A surface answers [iamdomain.ErrInvalidLogin], [iamdomain.ErrInvalid],
// [iamdomain.ErrInvalidToken] and [iamdomain.ErrNotFindable] 400 with the
// sentence after the sentinel; the sentinel's words are what errors.Is finds.
// Mutation: drop a sentinel from the cut list and its row goes red.
func TestAValuesRefusalIsToldWithoutItsSentinel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sentinel error
		rest     string
	}{
		{iamdomain.ErrInvalidLogin, `"Jane" isn't a valid person login`},
		{iamdomain.ErrInvalid, "the reason is 600 bytes, and the limit is 500"},
		{iamdomain.ErrInvalidToken, "a token needs an expiry date"},
		{iamdomain.ErrNotFindable, "a person needs an email address to sign in"},
	} {
		wrapped := fmt.Errorf("%w: %s", tc.sentinel, tc.rest)
		if got := iamdomain.Sentence(wrapped); got != tc.rest {
			t.Errorf("%v: a person is told %q, want %q", tc.sentinel, got, tc.rest)
		}
	}
}
