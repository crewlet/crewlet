package iamdomain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// issueReset puts one reset link on a person, as `/iam`'s issue does, and
// answers its id and secret.
func issueReset(t *testing.T, rig *writeRig, person string) (string, string) {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	secret, err := credential.NewResetSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.draining(func() error {
		_, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
			PersonID: person, OpID: operationKey(), Reason: "a reset link",
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				p.Credentials = append(p.Credentials, iamdomain.Credential{
					V: iamdomain.DocumentVersion, ID: id,
					Method:    iamdomain.MethodReset,
					Verifier:  credential.ResetVerifier(id, secret),
					ExpiresAt: brokerAt.Add(credential.ResetLinkLifetime),
				})
				return p, nil
			},
		})
		return err
	}); err != nil {
		t.Fatalf("issue a reset link: %v", err)
	}
	rig.drain()
	return id, secret
}

// setPassword sets a person's password through the node's own writer — the
// party both callers act through — judged by check.
func setPassword(t *testing.T, rig *writeRig, person, verifier string,
	check func(iamdomain.Person, iamdomain.Counters) error) error {

	t.Helper()
	return rig.draining(func() error {
		_, err := nodeWriter(rig).SetPassword(t.Context(), iamdomain.PasswordSet{
			PersonID: person, Verifier: verifier, Check: check,
			OpID: operationKey(), Reason: "set a new password from a reset link",
		})
		return err
	})
}

// A PASSWORD SET IS ONE RECORD THAT ENDS EVERY TOKEN AND EVERY LINK.
//
// Somebody else may hold the old password, so the record that replaces it moves
// the person's revocation epoch (refusing every token and session from before
// it) and revokes every reset link they hold, the spent one included, so a link
// sets one password. One record lands; the person holds one password, the new
// one. The CONTROL is a token minted after the set, which verifies. Mutation:
// state the current epoch instead of the next and the old token still
// verifies; leave the links alone and the spent link opens again.
func TestAPasswordSetIsOneRecordThatEndsEveryTokenAndLink(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	_, before, err := mintFor(t, rig, asOwner(rig, owner),
		iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	link, secret := issueReset(t, rig, owner)
	start, err := rig.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	opens := func(p iamdomain.Person, counters iamdomain.Counters) error {
		if !iamdomain.ResetOf(p, link, counters).Opens(secret, brokerAt) {
			return errors.New("the link no longer opens")
		}
		return nil
	}
	if err := setPassword(t, rig, owner, "argon-new", opens); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	rig.drain()
	if end, _ := rig.log.End(t.Context()); end != start+1 {
		t.Errorf("the set appended %d records, want one", end-start)
	}
	if got := checked(t, rig, before); got.Answer != credential.TokenRefused {
		t.Errorf("a token minted before the password was set answered %q (%s)",
			got.Answer, got.Detail)
	}
	if epoch := rig.column(`SELECT epoch FROM iam_revocation_epochs
		WHERE person_id = ?`, owner); len(epoch) != 1 || epoch[0] != "1" {
		t.Errorf("the person's epoch is %v, want 1", epoch)
	}
	passwords := rig.column(`SELECT CAST(verifier AS TEXT) FROM iam_credentials
		WHERE person_id = ? AND method = 'password'`, owner)
	if len(passwords) != 1 || passwords[0] != "argon-new" {
		t.Errorf("the person's passwords are %v, want the new one alone", passwords)
	}
	row, err := rig.resetRow(t, link)
	if err != nil {
		t.Fatal(err)
	}
	if row.RevokedAt.IsZero() || row.Opens(secret, brokerAt) {
		t.Errorf("the spent link still opens: %+v", row)
	}

	// SPENT, the link refuses a second set in that set's own snapshot, and
	// nothing lands.
	again, _ := rig.log.End(t.Context())
	if err := setPassword(t, rig, owner, "argon-third", opens); err == nil {
		t.Error("a spent link set a second password")
	}
	if end, _ := rig.log.End(t.Context()); end != again {
		t.Errorf("the refused set moved the log from %d to %d", again, end)
	}

	_, after, err := mintFor(t, rig, asOwner(rig, owner),
		iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if got := checked(t, rig, after); got.Answer != credential.TokenValid {
		t.Errorf("a token minted after the set answered %q (%s) — the control",
			got.Answer, got.Detail)
	}
}

// A PERMISSION GAINED REVOKES AN OUTSTANDING RESET LINK.
//
// A link is judged at its issue against its issuer's permissions, and the
// issuer can spend it, so a permission gained while it is outstanding would
// reach whoever spends it unjudged: the edit that adds one revokes the link in
// its own record. The CONTROL is an edit that only takes a permission away,
// which keeps the link opening. Mutation: drop the revocation from
// UpdatePerson and the link still opens once the person holds admin.
func TestAGrantGainedRevokesAnOutstandingResetLink(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := tokenOwner(t, rig, "jane.doe")
	link, secret := issueReset(t, rig, person)
	edit := func(change func([]iam.Grant) []iam.Grant) {
		t.Helper()
		if err := rig.draining(func() error {
			_, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
				PersonID: person, OpID: operationKey(), Reason: "an edit",
				Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
					p.Grants = change(p.Grants)
					return p, nil
				},
			})
			return err
		}); err != nil {
			t.Fatalf("UpdatePerson: %v", err)
		}
		rig.drain()
	}
	opens := func() bool {
		t.Helper()
		row, err := rig.resetRow(t, link)
		if err != nil {
			t.Fatal(err)
		}
		return row.Opens(secret, brokerAt)
	}

	edit(func(held []iam.Grant) []iam.Grant {
		return slices.DeleteFunc(slices.Clone(held),
			func(g iam.Grant) bool { return g == iam.GrantDoclet })
	})
	if !opens() {
		t.Fatal("an edit that only took a permission away revoked the link — " +
			"the control")
	}

	edit(func(held []iam.Grant) []iam.Grant {
		return append(slices.Clone(held), iam.GrantAdmin)
	})
	if opens() {
		t.Error("the link still opens after its person gained admin, " +
			"which nobody holding it was judged against")
	}
}

// A RESET LINK ENDS WITH ITS PERSON'S SESSIONS AND TOKENS.
//
// A link's copy stays wherever it was sent, and judged by its stage alone a
// reactivated person's old link would open again. So it is stamped with the
// person's epoch and the company's generation at its issue and ended by either
// moving, as a machine token is, on the link's screen and in the spend's own
// snapshot: a suspension, signing out everywhere and the restore runbook's
// invalidate-all each end it. The CONTROL is a link issued after the gesture,
// which opens. Mutation: drop the counters from EndedBy's reset arm and every
// row opens again.
func TestAResetLinkEndsWithItsPersonsSessionsAndTokens(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		gesture func(t *testing.T, rig *writeRig, person string) error
	}{
		{"suspended and reactivated", func(t *testing.T, rig *writeRig, person string) error {
			for _, stage := range []iam.Stage{iam.StageSuspended, iam.StageActive} {
				if _, err := rig.writer.SetStage(t.Context(), person, stage,
					operationKey(), "a stage"); err != nil {
					return err
				}
			}
			return nil
		}},
		{"signed out everywhere", func(t *testing.T, rig *writeRig, person string) error {
			_, err := rig.writer.Revoke(t.Context(), person, operationKey(),
				"signed out everywhere")
			return err
		}},
		{"everybody's sessions invalidated", func(t *testing.T, rig *writeRig, _ string) error {
			_, err := rig.writer.InvalidateAll(t.Context(), operationKey(),
				"a restore")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWriteRig(t)
			person := tokenOwner(t, rig, "jane.doe")
			link, secret := issueReset(t, rig, person)
			if err := rig.draining(func() error {
				return tc.gesture(t, rig, person)
			}); err != nil {
				t.Fatalf("the gesture: %v", err)
			}
			rig.drain()
			row, err := rig.resetRow(t, link)
			if err != nil {
				t.Fatal(err)
			}
			if row.Opens(secret, brokerAt) {
				t.Errorf("the link issued before the gesture still opens: %+v", row)
			}
			opens := func(p iamdomain.Person, counters iamdomain.Counters) error {
				if !iamdomain.ResetOf(p, link, counters).Opens(secret, brokerAt) {
					return errors.New("the link no longer opens")
				}
				return nil
			}
			if err := setPassword(t, rig, person, "argon-new", opens); err == nil {
				t.Error("the spend set a password from the ended link")
			}

			after, fresh := issueReset(t, rig, person)
			row, err = rig.resetRow(t, after)
			if err != nil {
				t.Fatal(err)
			}
			if !row.Opens(fresh, brokerAt) {
				t.Errorf("a link issued after the gesture does not open — the "+
					"control: %+v", row)
			}
		})
	}
}

// A LINK THESE ROWS DO NOT HOLD IS DEAD ONLY WHERE THEY HOLD THE WHOLE LOG.
//
// On a node behind the log, or holding a record it could not apply, a link
// issued through a peer is an id these rows lack, and answering it `410` (ask
// for a new one) and counting a guesser would be wrong. So the reset and
// invitation lookups say whether the rows vouch for their answer at the log's
// end, read first: they do at the end they applied (the CONTROL) and not past
// it, for a held link and an unknown id alike. Mutation: answer Vouched true
// without the proof and the case past the end reads vouched.
func TestALinkTheseRowsDoNotHoldIsDeadOnlyWhereTheyHoldTheLog(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := tokenOwner(t, rig, "jane.doe")
	link, _ := issueReset(t, rig, person)
	end, err := rig.end(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	unknown := uuid.Must(uuid.NewV7()).String()
	for _, tc := range []struct {
		name string
		end  uint64
		want bool
	}{
		{"at the end these rows applied (the control)", end, true},
		{"at an end past it", end + 1, false},
	} {
		for _, id := range []string{link, unknown} {
			row, err := rig.reader(t).ResetByID(t.Context(), id, tc.end)
			if err != nil {
				t.Fatalf("%s: ResetByID(%s): %v", tc.name, id, err)
			}
			if row.Vouched != tc.want {
				t.Errorf("%s: the reset lookup of %s vouched %v, want %v",
					tc.name, id, row.Vouched, tc.want)
			}
			invitation, err := rig.reader(t).InvitationByID(t.Context(), id, tc.end)
			if err != nil {
				t.Fatalf("%s: InvitationByID(%s): %v", tc.name, id, err)
			}
			if invitation.Vouched != tc.want {
				t.Errorf("%s: the invitation lookup of %s vouched %v, want %v",
					tc.name, id, invitation.Vouched, tc.want)
			}
		}
	}
}

// A RESET LINK IS NO BEARER.
//
// It is a credential row shaped like a machine token's (a clear id, a hashed
// secret), so presented as a token it is refused; it opens only as a link.
// The CONTROL is a real token, which verifies. Mutation: drop the method check
// from the token read's verdict and the link's id is treated as a token's.
func TestAResetLinkIsNoBearer(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "sam.doe")
	_, token, err := mintFor(t, rig, asOwner(rig, owner),
		iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if got := checked(t, rig, token); got.Answer != credential.TokenValid {
		t.Fatalf("the control token answered %q (%s)", got.Answer, got.Detail)
	}
	link, secret := issueReset(t, rig, owner)
	end, _ := rig.log.End(t.Context())
	forged := credential.Token{ID: link, Position: end, Secret: secret}
	if got := checked(t, rig, forged); got.Answer != credential.TokenRefused {
		t.Errorf("a reset link presented as a bearer answered %q (%s)",
			got.Answer, got.Detail)
	}
	row, err := rig.resetRow(t, link)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Opens(secret, brokerAt) || row.Login != "sam.doe" {
		t.Errorf("the link does not open as a link: %+v", row)
	}
	if row.Opens(secret, brokerAt.Add(credential.ResetLinkLifetime+time.Second)) {
		t.Error("a link opened past its day")
	}
	if tokenRow, _ := rig.resetRow(t, token.ID); tokenRow.ID != "" {
		t.Errorf("a machine token resolved as a reset link: %+v", tokenRow)
	}
}

// A RESET LINK ITS PERSON USED IS LISTED AS USED, NOT AS WITHDRAWN.
//
// Setting a password revokes every link the person holds, the spent one
// included, so a listing reading the revocation alone would call the link they
// just used withdrawn. The spent link is marked; the CONTROL is a second link
// the same set revoked, which is not. Mutation: leave the spent link unmarked
// and both read as withdrawn.
func TestAResetLinkItsPersonUsedIsListedAsUsed(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	spent, _ := issueReset(t, rig, owner)
	other, _ := issueReset(t, rig, owner)
	if err := rig.draining(func() error {
		_, err := nodeWriter(rig).SetPassword(t.Context(), iamdomain.PasswordSet{
			PersonID: owner, Verifier: "argon-new", Spends: spent,
			OpID: operationKey(), Reason: "set a new password from a reset link",
		})
		return err
	}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	rig.drain()
	rows, err := rig.reader(t).Credentials(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]iamdomain.CredentialRow{}
	for _, row := range rows {
		listed[row.ID] = row
	}
	for id, want := range map[string]bool{spent: true, other: false} {
		row := listed[id]
		if row.RevokedAt.IsZero() || row.Spent != want {
			t.Errorf("the link %s is listed revoked at %v, spent %v — want "+
				"revoked, spent %v", id, row.RevokedAt, row.Spent, want)
		}
	}
}
