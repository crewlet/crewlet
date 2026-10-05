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
	check func(iamdomain.Person) error) error {

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
// Somebody sets a password when somebody else may have the old one, so the
// record that replaces it moves the person's revocation epoch too — a machine
// token minted before it is refused afterwards, and so is every session opened
// before it — and revokes every reset link the person still holds, the one
// spent included, so a link sets one password. Exactly one record lands, and
// the person holds exactly one password: the new one.
//
// The CONTROL is a token minted after the set, which verifies. Mutation: state
// the epoch the person is at instead of the next one and the old token still
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

	opens := func(p iamdomain.Person) error {
		if !iamdomain.ResetOf(p, link).Opens(secret, brokerAt) {
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
	row, err := rig.reader(t).ResetByID(t.Context(), link)
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

// A GRANT GAINED REVOKES AN OUTSTANDING RESET LINK.
//
// A link is judged at its issue against the grants of whoever issued it, and
// the issuer is shown it and can spend it — so a grant the person gains while
// it is outstanding, judged against nobody holding the link, would reach
// whoever spends it. The edit that adds a grant revokes the link in its own
// record. The CONTROL is an edit that only takes a grant away, which leaves the link opening. Mutation:
// drop the revocation from UpdatePerson and the link still opens once the
// person holds config:write.
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
		row, err := rig.reader(t).ResetByID(t.Context(), link)
		if err != nil {
			t.Fatal(err)
		}
		return row.Opens(secret, brokerAt)
	}

	edit(func(held []iam.Grant) []iam.Grant {
		return slices.DeleteFunc(slices.Clone(held),
			func(g iam.Grant) bool { return g == iam.GrantSecretRead })
	})
	if !opens() {
		t.Fatal("an edit that only took a grant away revoked the link — " +
			"the control")
	}

	edit(func(held []iam.Grant) []iam.Grant {
		return append(slices.Clone(held), iam.GrantConfigWrite)
	})
	if opens() {
		t.Error("the link still opens after its person gained config:write, " +
			"which nobody holding it was judged against")
	}
}

// A RESET LINK IS NO BEARER.
//
// It is a credential row like a machine token's, under an id in the clear and
// a secret hashed beside it — so it must never verify where a token is
// presented: presented as one, it is refused, and a link opens only as a link.
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
	row, err := rig.reader(t).ResetByID(t.Context(), link)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Opens(secret, brokerAt) || row.Login != "sam.doe" {
		t.Errorf("the link does not open as a link: %+v", row)
	}
	if row.Opens(secret, brokerAt.Add(credential.ResetLinkLifetime+time.Second)) {
		t.Error("a link opened past its day")
	}
	if tokenRow, _ := rig.reader(t).ResetByID(t.Context(), token.ID); tokenRow.ID != "" {
		t.Errorf("a machine token resolved as a reset link: %+v", tokenRow)
	}
}
