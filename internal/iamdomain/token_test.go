package iamdomain_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// MACHINE TOKENS, through the real writer, applier and reader. What a token
// may carry is a fact about its owner NOW: the mint reads the owner in the
// snapshot it forms the credential in, and verification reads token and owner
// in one snapshot again. Each half claims something of a broker and a store,
// so these run against real ones.

// tokenOwner enrols one person holding a spread of grants, and returns them.
func tokenOwner(t *testing.T, rig *writeRig, login string) string {
	t.Helper()
	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Creation{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Jane Doe", Email: login + "@example.com", Login: login,
		Seat:          rig.vacantSeat(strings.ReplaceAll(login, ".", "-")),
		LinkExpiresAt: firstLinkExpiry,
		Grants:        []iam.Grant{iam.GrantWorklet, iam.GrantDoclet},
		OpID:          "op-enrol-" + person, Reason: "a hire",
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
		Position: uint64(minted.Result.Position.Packed()), Secret: secret}, err
}

// asOwner is a person's own party, as when minting their own token, holding
// grants, or every grant when none are given. Every case but those about who
// may mint uses it: a person's token is theirs alone to mint.
func asOwner(rig *writeRig, owner string, grants ...iam.Grant) *iamdomain.Writer {
	if len(grants) == 0 {
		grants = iam.AllGrants
	}
	return rig.writer.As(iam.Principal{
		ID: uuid.MustParse(owner), Kind: iam.KindPerson, Login: "token.owner",
		Stage: iam.StageActive, Grants: grants,
	})
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

// A TOKEN IS MINTED FROM ITS OWNER'S CURRENT PERMISSIONS, AND VERIFIES.
//
// Asked for nothing in particular, it carries every permission the owner
// holds, in the vocabulary's order. Mutation: default an omitted list to
// nothing and the token carries no app.
func TestATokenIsMintedFromItsOwnersCurrentGrantsAndVerifies(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	minted, token, err := mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{
		PersonID: owner, Label: "Claude on my laptop", Reason: "a PAT",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !slices.Equal(minted.Grants, []iam.Grant{iam.GrantWorklet,
		iam.GrantDoclet}) {
		t.Errorf("the token carries %v, want every permission the owner holds",
			minted.Grants)
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
		{"a permission the owner does not hold", asOwner(rig, owner),
			iamdomain.TokenMint{Grants: []iam.Grant{iam.GrantAdmin}},
			iamdomain.ErrRefused},
		{"a name this build does not know", asOwner(rig, owner),
			iamdomain.TokenMint{Grants: []iam.Grant{"swarm"}}, iamdomain.ErrRefused},
		// The owner signed in holding less than their row declares (a node
		// that refuses admin, say): whoever mints sees the value, so a token
		// carries nothing the minting party does not hold.
		{"a permission the MINTING party does not hold",
			asOwner(rig, owner, iam.GrantWorklet),
			iamdomain.TokenMint{Grants: []iam.Grant{iam.GrantDoclet}},
			iamdomain.ErrRefused},
		{"an expiry already past", asOwner(rig, owner), iamdomain.TokenMint{
			ExpiresAt: brokerAt.Add(-time.Minute)}, iamdomain.ErrInvalidToken},
		{"an expiry past a year", asOwner(rig, owner), iamdomain.TokenMint{
			ExpiresAt: brokerAt.Add(credential.MaxTokenLifetime + time.Hour)},
			iamdomain.ErrInvalidToken},
	} {
		tc.in.PersonID = owner
		if _, _, err := mintFor(t, rig, tc.w, tc.in); !errors.Is(err, tc.wants) {
			t.Errorf("%s: the mint answered %v, want %v", tc.name, err, tc.wants)
		}
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
	if _, _, err := mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{
		PersonID: owner}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a token was minted for a suspended owner (%v)", err)
	}
	binding := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Creation{
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

// A PERSON'S TOKEN IS THEIRS ALONE TO MINT, AND A SERVICE ACCOUNT'S IS AN
// ADMINISTRATOR'S — DECIDED ON THE WRITER'S OWN PARTY.
//
// Whoever mints a token sees its value, and the token acts as its owner, so an
// administrator minting for a person would hold a credential acting as them; a
// service account has no login page, so admin mints for it. Who mints is the
// writer's principal, never a mint field a caller could fill in as the owner.
// Mutations, each red: decide the person arm on anything but the party's id
// (Tier A mints), drop it (the administrator mints), drop the machine-token
// refusal (jane's token mints another), read the token's name, not its class
// (a malformed one mints), drop the machine arm (a non-admin mints for a
// pipeline).
func TestAPersonsTokenIsMintedByThatPersonAlone(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")

	// THE DEPLOYMENT, AND A TIER A TOKEN HOLDING ADMIN, each minting on
	// jane's account.
	for name, w := range map[string]*iamdomain.Writer{
		"the node's own writer": rig.writer,
		"a Tier A token holding admin": rig.writer.As(
			principalNamed(iam.TokenLogin("ops"), iam.KindMachine, iam.AllGrants)),
		"an administrator": rig.writer.As(
			principalNamed("ana.admin", iam.KindPerson, iam.AllGrants)),
	} {
		if _, _, err := mintFor(t, rig, w, iamdomain.TokenMint{
			PersonID: owner}); !errors.Is(err, iamdomain.ErrRefused) {
			t.Fatalf("%s minting a person's token answered %v, want a "+
				"refusal", name, err)
		}
	}
	held, err := rig.reader(t).Credentials(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	// HER FIRST PASSWORD LINK IS ON IT FROM HER CREATE, and nothing a
	// refused mint left.
	tokens := slices.DeleteFunc(held, func(c iamdomain.CredentialRow) bool {
		return c.Method != iamdomain.MethodToken
	})
	if len(tokens) != 0 {
		t.Errorf("the refused mints left %d tokens on jane's account", len(tokens))
	}
	// JANE HERSELF.
	if _, token, err := mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{
		PersonID: owner}); err != nil {
		t.Fatalf("jane minting her own token: %v", err)
	} else if got := checked(t, rig, token); got.Answer != credential.TokenValid {
		t.Errorf("jane's own token answered %q (%s)", got.Answer, got.Detail)
	}
	// AND NOT THROUGH ONE OF HER OWN TOKENS: a party is composed as the
	// token's owner, so without its own refusal a token mints the next, a
	// year at a time, with nobody present.
	throughToken := iam.Principal{
		ID: uuid.MustParse(owner), Kind: iam.KindPerson, Login: "jane.doe",
		Stage: iam.StageActive, Grants: iam.AllGrants,
		Via: iam.MachineTokenName(uuid.Must(uuid.NewV7()).String()),
	}
	if _, _, err := mintFor(t, rig, rig.writer.As(throughToken),
		iamdomain.TokenMint{PersonID: owner}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("jane's own machine token minting another answered %v, want "+
			"a refusal", err)
	}
	// AND NOT THROUGH AN ID OF THE TOKEN'S CLASS this build cannot parse:
	// the class decides, never the spelling, or the person arm mints as jane.
	malformed := *rig.writer.As(throughToken)
	malformed.OperatorID = iam.MachineTokenPrefix + "x"
	if _, _, err := mintFor(t, rig, &malformed,
		iamdomain.TokenMint{PersonID: owner}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("an operator id of the token's class minting another answered "+
			"%v, want a refusal", err)
	}

	// A SERVICE ACCOUNT: the administrator mints for it, and a party
	// without admin may not.
	service := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Creation{
		PersonID: service, Kind: iam.KindMachine, Stage: iam.StageActive,
		Name: "Release pipeline", Login: "svc:release",
		Grants: []iam.Grant{iam.GrantWorklet, iam.GrantDoclet},
		OpID:   "op-enrol-svc", Reason: "a pipeline",
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	administrator := rig.writer.As(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantAdmin}))
	if _, _, err := mintFor(t, rig, administrator, iamdomain.TokenMint{
		PersonID: service}); err != nil {
		t.Errorf("an administrator minting a service account's token: %v", err)
	}
	colleague := rig.writer.As(principalNamed("dana.sre", iam.KindPerson,
		[]iam.Grant{iam.GrantWorklet, iam.GrantDoclet}))
	if _, _, err := mintFor(t, rig, colleague, iamdomain.TokenMint{
		PersonID: service,
		Grants:   []iam.Grant{iam.GrantWorklet}}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a party without %s minting for a service account answered "+
			"%v, want a refusal", iam.GrantAdmin, err)
	}
}

// A TOKEN IS NEVER MINTED THROUGH THE ASSISTANT.
//
// The Dashboard Assistant is composed as its person, so without its own lock a
// turn could mint a credential acting as them that a model is one step from
// holding. A mint whose operator is an assistant grant (composed through
// [iamdomain.Writer.As] as the request path does), or merely names the
// assistant's class, is refused [iamdomain.ErrAssistRefused] wrapping
// [iamdomain.ErrRefused] and mints nothing. The CONTROL is jane at her own
// keyboard. Mutation: drop the assistant's lock from mayMintFor and the person
// arm mints for it.
func TestATokenIsNeverMintedThroughTheAssistant(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	keyboard := iam.Principal{
		ID: uuid.MustParse(owner), Kind: iam.KindPerson, Login: "jane.doe",
		Stage: iam.StageActive, Grants: iam.AllGrants,
	}
	composed := keyboard
	composed.Via = iam.AssistName(uuid.Must(uuid.NewV7()))
	composed.AssistScope = iam.AssistScopeExec
	malformed := *rig.writer.As(keyboard)
	malformed.OperatorID = iam.AssistPrefix + "x"
	for through, w := range map[string]*iamdomain.Writer{
		"an assistant grant":             rig.writer.As(composed),
		"an id of the assistant's class": &malformed,
	} {
		if _, _, err := mintFor(t, rig, w, iamdomain.TokenMint{
			PersonID: owner}); !errors.Is(err, iamdomain.ErrAssistRefused) ||
			!errors.Is(err, iamdomain.ErrRefused) {
			t.Errorf("a mint through %s answered %v, want %v wrapping %v", through,
				err, iamdomain.ErrAssistRefused, iamdomain.ErrRefused)
		}
	}
	held, err := rig.reader(t).Credentials(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if tokens := slices.DeleteFunc(held, func(c iamdomain.CredentialRow) bool {
		return c.Method != iamdomain.MethodToken
	}); len(tokens) != 0 {
		t.Errorf("the refused mints left %d tokens on jane's account", len(tokens))
	}
	if _, _, err := mintFor(t, rig, rig.writer.As(keyboard), iamdomain.TokenMint{
		PersonID: owner}); err != nil {
		t.Errorf("jane at her own keyboard could not mint her token: %v", err)
	}
}

// SIGNING THE OWNER OUT EVERYWHERE ENDS EVERY TOKEN THEY MINTED, and a token
// minted after it works — what makes offboarding complete.
func TestSigningTheOwnerOutEverywhereEndsTheirTokens(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	_, before, err := mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{PersonID: owner})
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
	_, after, err := mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{PersonID: owner})
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
// The restore runbook's last step: a backup restores tokens revoked after it,
// unrevoked, and nothing says which, so the session generation's bump reaches
// a token as it reaches a cookie. A token minted after the bump works.
// Mutation: drop the generation from the mint and the later token is refused
// too; drop the check and the earlier one survives.
func TestInvalidatingEverythingEndsTokensMintedBeforeIt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	_, before, err := mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{PersonID: owner})
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
	_, after, err := mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{PersonID: owner})
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
	_, token, err := mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	revokeToken(t, rig, owner, token.ID)
	if got := checked(t, rig, token); got.Answer != credential.TokenRefused {
		t.Errorf("a revoked token answered %q (%s)", got.Answer, got.Detail)
	}
}

// revokeToken withdraws one token through the whole path and applies it.
func revokeToken(t *testing.T, rig *writeRig, owner, id string) {
	t.Helper()
	if err := rig.draining(func() error {
		_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
			PersonID: owner,
			Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
				for i := range held {
					if held[i].ID == id {
						held[i].RevokedAt = brokerAt.Add(-time.Second)
					}
				}
				return held, nil
			},
			OpID: "op-revoke-" + id, Reason: "leaked",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
}

// A TOKEN IS ITS OWNER NOW, read back in the snapshot verification takes.
//
// What it carries on a request is the mint's set cut to what the owner still
// holds, and whether it works follows the owner's stage, both read off the rows
// after the mint, never frozen into the credential. Mutation: read the owner's
// grants off the mint's record, or their stage off the document the suspension
// left stale, and the demoted or suspended owner's token works as before.
func TestATokenFollowsItsOwnersStandingAfterTheMint(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	_, token, err := mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.draining(func() error {
		_, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
			PersonID: owner,
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				p.Grants = []iam.Grant{iam.GrantWorklet}
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
	if got := row.EffectiveGrants(); !slices.Equal(got, []iam.Grant{iam.GrantWorklet}) {
		t.Errorf("a demoted owner's token carries %v, want Worklet alone", got)
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

// A TOKEN MINTED ADMIN CARRIES WHAT ITS OWNER KEEPS.
//
// Admin is mintable: what keeps a token off the gestures that need a person
// present is the authority table's presence and this domain's refusal of an
// administrative record written through one. So an administrator's token asked
// for nothing carries admin too, one minted [admin] carries every app through
// it, and both are cut per request to what the owner keeps: demoted to the
// knowledge base, the [admin] token carries that, still valid. Mutation: refuse
// admin at the mint and the first case goes red; read the mint's list without
// expanding it and the demoted owner's token carries nothing.
func TestATokenMintedAdminCarriesWhatItsOwnerKeeps(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Creation{
		PersonID: owner, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Ana Admin", Email: "ana@example.com", Login: "ana.admin",
		Seat: rig.vacantSeat("ana-admin"), LinkExpiresAt: firstLinkExpiry,
		Grants: []iam.Grant{iam.GrantAdmin},
		OpID:   "op-enrol-" + owner, Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	everything, _, err := mintFor(t, rig, asOwner(rig, owner, iam.GrantAdmin),
		iamdomain.TokenMint{PersonID: owner})
	if err != nil {
		t.Fatalf("mint with nothing asked: %v", err)
	}
	if !slices.Equal(everything.Grants, iam.AllGrants) {
		t.Errorf("an administrator's token asked for nothing carries %v, want %v",
			everything.Grants, iam.AllGrants)
	}
	minted, token, err := mintFor(t, rig, asOwner(rig, owner, iam.GrantAdmin),
		iamdomain.TokenMint{PersonID: owner, Grants: []iam.Grant{iam.GrantAdmin}})
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	if !slices.Equal(minted.Grants, []iam.Grant{iam.GrantAdmin}) {
		t.Errorf("a token minted admin records %v", minted.Grants)
	}
	row, err := rig.reader(t).MachineToken(t.Context(), token.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := row.EffectiveGrants(); !slices.Equal(got, iam.AllGrants) {
		t.Errorf("a token minted admin carries %v, want admin and every app", got)
	}
	if err := rig.draining(func() error {
		_, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
			PersonID: owner,
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				p.Grants = []iam.Grant{iam.GrantDoclet}
				return p, nil
			},
			OpID: operationKey(), Reason: "stepped back",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	if row, err = rig.reader(t).MachineToken(t.Context(), token.ID); err != nil {
		t.Fatal(err)
	}
	if got := row.EffectiveGrants(); !slices.Equal(got,
		[]iam.Grant{iam.GrantDoclet}) {
		t.Errorf("after the owner stepped back the token carries %v, want the "+
			"Doclet alone", got)
	}
	if got := checked(t, rig, token); got.Answer != credential.TokenValid {
		t.Errorf("the demoted owner's admin token answered %q (%s); a demotion "+
			"narrows it and does not end it", got.Answer, got.Detail)
	}
}

// A TOKEN ASKED FOR NOTHING CARRIES WHAT ITS MINTER MAY CONFER.
//
// A token carries nothing the minting party does not hold, and the default is
// held to that rather than refused by it: on a node whose `api.auth.admin` is
// `refuse`, an administrator signs in with their apps alone, so their token
// asked for nothing carries those apps (else `crewlet iam token -login`, which
// sends no list unless -grants names one, mints nothing there); where admin is
// served it carries admin too. Mutation: default to the owner's whole set and
// the refusing node's mint is refused.
func TestATokenAskedForNothingCarriesWhatItsMinterMayConfer(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Creation{
		PersonID: owner, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Ana Admin", Email: "ana@example.com", Login: "ana.admin",
		Seat: rig.vacantSeat("ana-admin"), LinkExpiresAt: firstLinkExpiry,
		Grants: []iam.Grant{iam.GrantAdmin},
		OpID:   "op-enrol-" + owner, Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	held := []iam.Grant{iam.GrantAdmin}
	for _, tc := range []struct {
		posture iam.AdminPosture
		want    []iam.Grant
	}{
		{iam.AdminRefuse, iam.Apps},
		{iam.AdminServe, iam.AllGrants},
	} {
		minter := asOwner(rig, owner, tc.posture.Effective(held)...)
		minted, _, err := mintFor(t, rig, minter, iamdomain.TokenMint{PersonID: owner})
		if err != nil {
			t.Errorf("on a node that will %s admin, the owner's own mint asked "+
				"for nothing was refused: %v", tc.posture, err)
			continue
		}
		if !slices.Equal(minted.Grants, tc.want) {
			t.Errorf("on a node that will %s admin, the token carries %v, want %v",
				tc.posture, minted.Grants, tc.want)
		}
	}
}
