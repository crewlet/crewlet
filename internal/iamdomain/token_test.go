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
	"github.com/crewlet/crewlet/internal/statelog"
)

// MACHINE TOKENS, through the real writer, applier and reader.
//
// What a token may carry is a fact about its owner NOW, so the mint reads the
// owner inside the snapshot it forms the credential in, and verification reads
// the token and the owner in one snapshot again. Each half is a claim about a
// broker and a store, which is why these cases run against both.

// tokenOwner enrols one person holding a spread of grants, and returns them.
func tokenOwner(t *testing.T, rig *writeRig, login string) string {
	t.Helper()
	return tokenOwnerAt(t, rig, login, iam.ColleagueWrite)
}

// tokenOwnerAt is [tokenOwner] reaching the company's work at a given level.
func tokenOwnerAt(t *testing.T, rig *writeRig, login string,
	colleague iam.Colleague) string {

	t.Helper()
	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Jane Doe", Email: login + "@example.com", Login: login,
		Grants: []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite,
			iam.GrantPeopleManage, iam.GrantSecretRead},
		Colleague: colleague,
		OpID:      "op-enrol-" + person, Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	return person
}

// mintFor mints one token through the whole path and answers what landed and
// the token as a pipeline would present it.
func mintFor(t *testing.T, rig *writeRig, w *iamdomain.Writer,
	in iamdomain.TokenMint) (iamdomain.TokenMinted, credential.Token, error) {

	t.Helper()
	secret, err := credential.NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	if in.ID == "" {
		in.ID = uuid.Must(uuid.NewV7()).String()
	}
	if in.OpID == "" {
		in.OpID = "op-mint-" + in.ID
	}
	if in.ExpiresAt.IsZero() {
		in.ExpiresAt = brokerAt.Add(credential.DefaultTokenLifetime)
	}
	in.Verifier = credential.TokenVerifier(in.ID, secret)
	var minted iamdomain.TokenMinted
	err = rig.draining(func() error {
		var err error
		minted, err = w.MintToken(rig.t.Context(), in)
		return err
	})
	rig.drain()
	return minted, credential.Token{ID: in.ID,
		Position: uint64(minted.Position.Packed()), Secret: secret}, err
}

// checked reads a presented token back and decides it as the guard does.
func checked(t *testing.T, rig *writeRig, token credential.Token) credential.TokenCheck {
	t.Helper()
	row, err := rig.reader(t).MachineToken(t.Context(), token.ID)
	if err != nil {
		t.Fatalf("read the token: %v", err)
	}
	return credential.CheckToken(token, row, brokerAt, statelog.StallGrace)
}

// A TOKEN IS MINTED FROM ITS OWNER'S CURRENT GRANTS, AND VERIFIES.
//
// Asked for nothing in particular, it carries every grant the owner holds that
// a token may carry — never secrets:read or people:manage, which need a person
// present — and the owner's own reach. Mutation: drop the refused grants'
// filter and the token carries the secret store.
func TestATokenIsMintedFromItsOwnersCurrentGrantsAndVerifies(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	minted, token, err := mintFor(t, rig, rig.writer, iamdomain.TokenMint{
		PersonID: owner, Label: "Claude on my laptop", Reason: "a PAT",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !slices.Equal(minted.Grants, []iam.Grant{iam.GrantStateRead,
		iam.GrantWorkWrite}) || minted.Colleague != iam.ColleagueWrite {
		t.Errorf("the token carries %v at %q, want the owner's grants a token "+
			"may carry, at their reach", minted.Grants, minted.Colleague)
	}
	if got := checked(t, rig, token); got.Answer != credential.TokenValid {
		t.Fatalf("a freshly minted token answered %q (%s)", got.Answer, got.Detail)
	}
	row, err := rig.reader(t).MachineToken(t.Context(), token.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Owner.Login != "jane.doe" || row.Owner.ID != owner ||
		!slices.Equal(row.Grants, minted.Grants) {
		t.Errorf("the row reads owner %q (%s) carrying %v", row.Owner.Login,
			row.Owner.ID, row.Grants)
	}
}

// A MINT REFUSES EVERYTHING A TOKEN MAY NOT BE.
func TestAMintRefusesWhatATokenMayNotCarry(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	for _, tc := range []struct {
		name  string
		w     *iamdomain.Writer
		in    iamdomain.TokenMint
		wants error
	}{
		{"a grant the owner does not hold", rig.writer, iamdomain.TokenMint{
			Grants: []iam.Grant{iam.GrantConfigWrite}}, iamdomain.ErrRefused},
		{"secrets:read, which the owner holds", rig.writer, iamdomain.TokenMint{
			Grants: []iam.Grant{iam.GrantSecretRead}}, iamdomain.ErrRefused},
		{"people:manage, which the owner holds", rig.writer, iamdomain.TokenMint{
			Grants: []iam.Grant{iam.GrantPeopleManage}}, iamdomain.ErrRefused},
		{"a reach that is no level at all", rig.writer, iamdomain.TokenMint{
			Grants: []iam.Grant{}, Colleague: "admin"}, iamdomain.ErrInvalidToken},
		{"a grant the MINTING party does not hold", narrowAdmin(rig),
			iamdomain.TokenMint{Grants: []iam.Grant{iam.GrantWorkWrite}},
			iamdomain.ErrRefused},
		{"an expiry already past", rig.writer, iamdomain.TokenMint{
			ExpiresAt: brokerAt.Add(-time.Minute)}, iamdomain.ErrInvalidToken},
		{"an expiry past a year", rig.writer, iamdomain.TokenMint{
			ExpiresAt: brokerAt.Add(credential.MaxTokenLifetime + time.Hour)},
			iamdomain.ErrInvalidToken},
	} {
		tc.in.PersonID = owner
		if _, _, err := mintFor(t, rig, tc.w, tc.in); !errors.Is(err, tc.wants) {
			t.Errorf("%s: the mint answered %v, want %v", tc.name, err, tc.wants)
		}
	}

	// MORE REACH THAN THE OWNER HAS: a token narrows and never widens.
	reader := tokenOwnerAt(t, rig, "ravi.reader", iam.ColleagueRead)
	if _, _, err := mintFor(t, rig, rig.writer, iamdomain.TokenMint{
		PersonID: reader, Colleague: iam.ColleagueWrite}); !errors.Is(err,
		iamdomain.ErrRefused) {
		t.Errorf("a token reaching the work at write was minted for an owner "+
			"who reads it (%v)", err)
	}

	// A SUSPENDED OWNER, and the Tier A token's own binding row, are
	// nobody a token may act as.
	if err := rig.draining(func() error {
		_, err := rig.writer.SetStage(t.Context(), owner, iam.StageSuspended,
			"op-suspend", "on leave")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if _, _, err := mintFor(t, rig, rig.writer, iamdomain.TokenMint{
		PersonID: owner}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a token was minted for a suspended owner (%v)", err)
	}
	binding := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: binding, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: iam.TokenLogin("ops"), OpID: "op-binding", Reason: "binds ops",
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if _, _, err := mintFor(t, rig, rig.writer, iamdomain.TokenMint{
		PersonID: binding}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a token was minted on the row that binds a Tier A token "+
			"(%v)", err)
	}
}

// SIGNING THE OWNER OUT EVERYWHERE ENDS EVERY TOKEN THEY MINTED, and a token
// minted after it works — what makes offboarding complete.
func TestSigningTheOwnerOutEverywhereEndsTheirTokens(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	_, before, err := mintFor(t, rig, rig.writer, iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.draining(func() error {
		_, err := rig.writer.Revoke(t.Context(), owner, "op-revoke", "offboarded")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if got := checked(t, rig, before); got.Answer != credential.TokenRefused {
		t.Errorf("a token minted before the owner was signed out everywhere "+
			"answered %q (%s)", got.Answer, got.Detail)
	}
	_, after, err := mintFor(t, rig, rig.writer, iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if got := checked(t, rig, after); got.Answer != credential.TokenValid {
		t.Errorf("a token minted after the owner was signed out everywhere "+
			"answered %q (%s) — it was minted below the epoch", got.Answer,
			got.Detail)
	}
}

// INVALIDATING EVERY CREDENTIAL ENDS EVERY TOKEN MINTED BEFORE IT.
//
// The restore runbook's last step: a backup taken before a token was revoked
// restores it unrevoked, and nothing can say which ones were, so the session
// generation's bump has to reach a token as it reaches a cookie. A token minted
// after the bump works. Mutation: drop the generation from the mint and the
// token after the bump is refused too; drop the check and the one before it
// survives.
func TestInvalidatingEverythingEndsTokensMintedBeforeIt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	_, before, err := mintFor(t, rig, rig.writer, iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.draining(func() error {
		_, err := rig.writer.InvalidateAll(t.Context(), "op-invalidate",
			"restored from a backup")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if got := checked(t, rig, before); got.Answer != credential.TokenRefused {
		t.Errorf("a token minted before every credential was invalidated "+
			"answered %q (%s)", got.Answer, got.Detail)
	}
	_, after, err := mintFor(t, rig, rig.writer, iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if got := checked(t, rig, after); got.Answer != credential.TokenValid {
		t.Errorf("a token minted after the invalidation answered %q (%s)",
			got.Answer, got.Detail)
	}
}

// A REVOKED TOKEN IS REFUSED, and its row stays to say so.
func TestARevokedTokenIsRefusedAndItsRowStays(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	_, token, err := mintFor(t, rig, rig.writer, iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.draining(func() error {
		_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
			PersonID: owner,
			Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
				for i := range held {
					if held[i].ID == token.ID {
						held[i].RevokedAt = brokerAt.Add(-time.Second)
					}
				}
				return held
			},
			OpID: "op-revoke-token", Reason: "leaked",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if got := checked(t, rig, token); got.Answer != credential.TokenRefused {
		t.Errorf("a revoked token answered %q (%s)", got.Answer, got.Detail)
	}
}

// A TOKEN IS ITS OWNER NOW, read back in the snapshot verification takes.
//
// The mint stated what it conferred; what the token CARRIES on a request is
// that set cut to what the owner still holds, and whether it works at all
// follows the owner's stage — both read off the rows as they are after the
// mint, never frozen into the credential. Mutation: read the owner's grants
// off the mint's record, or their stage off the document the suspension left
// stale, and the demoted or suspended owner's token goes on working as before.
func TestATokenFollowsItsOwnersStandingAfterTheMint(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	_, token, err := mintFor(t, rig, rig.writer, iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.draining(func() error {
		_, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
			PersonID: owner,
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				p.Grants = []iam.Grant{iam.GrantStateRead}
				return p, nil
			},
			OpID: "op-demote", Reason: "moved teams",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	row, err := rig.reader(t).MachineToken(t.Context(), token.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := row.EffectiveGrants(); !slices.Equal(got, []iam.Grant{iam.GrantStateRead}) {
		t.Errorf("a demoted owner's token carries %v, want state:read alone", got)
	}
	if got := checked(t, rig, token); got.Answer != credential.TokenValid {
		t.Fatalf("a demoted owner's token answered %q (%s); demotion narrows "+
			"it and does not end it", got.Answer, got.Detail)
	}

	if err := rig.draining(func() error {
		_, err := rig.writer.SetStage(t.Context(), owner, iam.StageSuspended,
			"op-suspend", "on leave")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if got := checked(t, rig, token); got.Answer != credential.TokenRefused {
		t.Errorf("a suspended owner's token answered %q (%s)", got.Answer,
			got.Detail)
	}
}
