package iamapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// THE MINT SURFACE: what `POST /iam/credentials` hands the domain, and what it
// answers. What a token may CARRY is internal/iamdomain's decision, taken in
// the snapshot it reads the owner in, and certified there against a real
// broker; these cases hold the surface to handing it the right owner and to
// rendering its answer honestly.

// A TOKEN IS MINTED FOR THE PERSON THE ROUTE NAMES, and never one a body does.
//
// The route's authority is decided on `?person=` — or on the caller when there
// is none — and the handler used to mint for a `person` field in the BODY
// instead. So anybody could mint a token on anybody's account by asking about
// themselves: the table saw a person minting their own, and the handler handed
// somebody else's to the writer. Mutation: read the owner off the body again
// and the writer is asked for alice's.
func TestATokenIsMintedForThePersonTheRouteNames(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	caller := ordinary() // bob, holding state:read and nothing to administer
	got := r.as(caller, http.MethodPost, "/iam/credentials",
		map[string]any{"person": alice.String(), "label": "mine"})
	if got.status != http.StatusCreated {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if r.writer.minted.PersonID != caller.ID.String() {
		t.Errorf("the token was minted for %q, want the caller %s — a body "+
			"naming somebody else is not who the route decided on",
			r.writer.minted.PersonID, caller.ID)
	}
	// AND THE DOMAIN IS TOLD WHO MINTED IT, off the resolved principal:
	// that is what a person's own token is decided on.
	if r.writer.minted.Minter != caller.ID.String() {
		t.Errorf("the domain was told %q minted it, want the caller %s",
			r.writer.minted.Minter, caller.ID)
	}
	// AND NAMING SOMEBODY ELSE WHERE IT COUNTS is decided by the table:
	// an ordinary caller may not mint on the administrator's account.
	if got := r.as(caller, http.MethodPost, "/iam/credentials?person="+
		alice.String(), map[string]any{}); got.status != http.StatusForbidden {
		t.Errorf("an ordinary caller minting on alice's account answered %d, "+
			"want 403", got.status)
	}
}

// A MINTED TOKEN'S VALUE IS IN THE ANSWER, AND IT IS THE TOKEN THE ESTATE WILL
// VERIFY: the verifier the writer was handed, at the position the mint landed.
func TestAMintedTokenIsShownOnceAndVerifiesAgainstWhatWasStored(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPost,
		"/iam/credentials?person="+alice.String(), map[string]any{"label": "release"})
	if got.status != http.StatusCreated {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	value, _ := got.body["token"].(string)
	token, ok := credential.ParseToken(value)
	if !ok {
		t.Fatalf("the answer's token %q is not one this engine can parse", value)
	}
	minted := r.writer.minted
	switch {
	case token.ID != minted.ID:
		t.Errorf("the token names credential %s and the writer stored %s",
			token.ID, minted.ID)
	case !credential.VerifyToken(minted.Verifier, token):
		t.Error("the token does not verify against the verifier the writer " +
			"was handed, so nothing this surface mints would ever work")
	case strings.Contains(minted.Verifier, token.Secret):
		t.Error("the stored verifier carries the secret itself")
	}
	// THE POSITION IS WHERE THE MINT LANDED, which only the answer knew.
	if token.Position != 7 {
		t.Errorf("the token carries position %d, want the landed 7",
			token.Position)
	}
	if minted.Label != "release" ||
		minted.ExpiresAt != at.Add(credential.DefaultTokenLifetime) {
		t.Errorf("the mint was asked for label %q expiring %s", minted.Label,
			minted.ExpiresAt)
	}
	// A FRESH OPERATION, never the caller's Idempotency-Key: a replayed
	// mint would hand back the first attempt's record with this
	// attempt's value — a token that verifies against nothing.
	if !strings.HasPrefix(minted.OpID, "credentials:mint:"+minted.ID) {
		t.Errorf("the mint published as operation %q", minted.OpID)
	}
}

// AN IDEMPOTENCY KEY DOES NOT MAKE A MINT REPLAY. Mutation: honour the key
// and this answers the key as the operation.
func TestAMintIgnoresTheCallersIdempotencyKey(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost,
		"/iam/credentials?person="+alice.String(), strings.NewReader(string(body)))
	req.Header.Set("Idempotency-Key", "the-callers-key")
	req = req.WithContext(iam.WithPrincipal(req.Context(), administrator()))
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if r.writer.minted.OpID == "the-callers-key" {
		t.Error("the mint published under the caller's key, so a retry after " +
			"an unknown answer hands back a token nobody holds the secret of")
	}
}

// WHAT THE DOMAIN REFUSES IS ANSWERED FOR WHAT IT IS: an authority refusal is
// 403 and a malformed request is 400, never the 500 an unclassified error is.
func TestAMintTheDomainRefusesIsAnsweredForWhatItIs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"a grant the owner does not hold",
			fmt.Errorf("%w: config:write is not among [state:read]",
				iamdomain.ErrRefused), http.StatusForbidden},
		{"a reach that is no level", fmt.Errorf("%w: \"admin\" is not a "+
			"colleague level", iamdomain.ErrInvalidToken), http.StatusBadRequest},
	} {
		r := newRig(t)
		r.writer.err = tc.err
		got := r.as(administrator(), http.MethodPost,
			"/iam/credentials?person="+alice.String(), map[string]any{})
		if got.status != tc.want {
			t.Errorf("%s answered %d, want %d (body %v)", tc.name, got.status,
				tc.want, got.body)
		}
		if got.body["token"] != nil {
			t.Errorf("%s answered a token value", tc.name)
		}
	}
}

// "FOREVER" IS UNEXPRESSIBLE, and a caller asking for it is told so rather
// than silently clamped: five years is an intention the answer contradicts.
func TestATokenCannotOutliveTheCeiling(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	days := int(credential.MaxTokenLifetime/(24*time.Hour)) + 1
	got := r.as(administrator(), http.MethodPost,
		"/iam/credentials?person="+alice.String(),
		map[string]any{"expires_in_days": days})
	if got.status != http.StatusBadRequest {
		t.Errorf("a token past a year answered %d, want 400 (body %v)",
			got.status, got.body)
	}
	if len(r.writer.calls) != 0 {
		t.Errorf("the writer was asked for %v", r.writer.calls)
	}
}

// A TOKEN DOES NOT MINT A TOKEN, whoever it acts as.
//
// One minted from a token is one whoever holds a pipeline's environment can
// renew for ever, a year at a time, with nobody present. The request goes
// through the real guard so the credential SHAPE is the guard's own answer.
// Mutation: drop the check and the writer is asked to mint.
func TestATokenCannotMintAToken(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	presented, row := aliceToken(t)
	rec := throughTheGuard(t, r, row, http.MethodPost, "/iam/credentials",
		presented)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a token minting a token answered %d, want 403: %s",
			rec.Code, rec.Body.String())
	}
	if len(r.writer.calls) != 0 {
		t.Errorf("the writer was asked for %v", r.writer.calls)
	}
}

// A TIER A TOKEN NAMES WHO THE TOKEN IS FOR. It is the deployment's credential
// and owns nothing, so a mint that names nobody is told to name somebody
// rather than answered a 404 about an id no row holds — which is what
// `crewlet iam token` without -person reaches. Mutation: drop the check and
// this answers 404.
func TestATierATokenNamesWhoATokenIsFor(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// A SERVICE ACCOUNT is what a Tier A token mints for — a person's
	// own token is theirs alone, which the domain decides.
	service := uuid.MustParse("018f3a9c-0000-7000-8000-0000000005e1")
	r.directory.people[service.String()] = iamdomain.PersonRow{
		ID: service.String(), Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "svc:release", Grants: []iam.Grant{iam.GrantStateRead},
	}
	const value = "a-tier-a-token-long-enough-to-pass"
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: value,
		Grants: []iam.Grant{iam.GrantPeopleManage, iam.GrantStateRead}}}
	guarded := auth.New(&b).Middleware(r.mux)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/iam/credentials", http.StatusBadRequest},
		{"/iam/credentials?person=" + service.String(), http.StatusCreated},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+value)
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("POST %s answered %d, want %d: %s", tc.path, rec.Code,
				tc.want, rec.Body.String())
		}
	}
}

// A TOKEN REVOKES TOKENS, ITSELF INCLUDED, AND NEVER ITS OWNER'S PROOF.
//
// A token acts as its owner and the table admits it here as it admits them,
// so without a rule of its own a leaked token could withdraw the owner's
// second factor — the first half of taking the account. Mutation: drop the
// method check and the password is revoked by a request carrying the token.
func TestATokenRevokesTokensAndNotItsOwnersProof(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		target  string
		want    int
		revoked bool
	}{
		{"its owner's password", "018f3a9c-0000-7000-8000-00000000000a",
			http.StatusForbidden, false},
		{"itself", tokenID, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.held = []iamdomain.Credential{
				{ID: "018f3a9c-0000-7000-8000-00000000000a",
					Method: iamdomain.MethodPassword},
				{ID: tokenID, Method: iamdomain.MethodToken},
			}
			presented, row := aliceToken(t)
			rec := throughTheGuard(t, r, row, http.MethodDelete,
				"/iam/credentials/"+tc.target, presented)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want,
					rec.Body.String())
			}
			for _, c := range r.writer.held {
				if c.ID == tc.target && c.RevokedAt.IsZero() == tc.revoked {
					t.Errorf("%s revoked=%v after the request, want %v", tc.name,
						!c.RevokedAt.IsZero(), tc.revoked)
				}
				if c.ID != tc.target && !c.RevokedAt.IsZero() {
					t.Errorf("credential %s was revoked, and nobody named it", c.ID)
				}
			}
		})
	}
}

// A TOKEN'S GESTURE HERE SAYS IT WAS THE TOKEN.
//
// A token acts as its owner, so the event a revocation announces names the
// owner as who did it — and without the credential beside it, a leaked token
// withdrawing its owner's other tokens reads on the audit feed exactly as the
// owner doing so. The writer's party carries it too, for the events the domain
// announces itself. Mutation: stop stamping the operator on the revocation and
// the event carries the owner's login.
func TestATokensRevocationSaysItWasTheToken(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.held = []iamdomain.Credential{{ID: tokenID, Method: iamdomain.MethodToken}}
	presented, row := aliceToken(t)
	if rec := throughTheGuard(t, r, row, http.MethodDelete,
		"/iam/credentials/"+tokenID, presented); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	via := iam.MachineTokenName(tokenID)
	revoked := only[types.IAMCredentialRevoked](t, r.audit)
	if revoked.By != "alice.admin" || revoked.OperatorID != via {
		t.Errorf("the revocation was announced as by %q through %q, want the "+
			"owner through %q", revoked.By, revoked.OperatorID, via)
	}
	if r.writer.actor != "alice.admin" || r.writer.operator != via {
		t.Errorf("the writer acts as %q through %q, want the owner through %q",
			r.writer.actor, r.writer.operator, via)
	}
}

// A PERSON MINTS THEIR OWN TOKEN FROM THEIR SESSION, and only that way.
//
// The self-service path: a signed-in person, through the REAL guard's session
// arm, posting with no `?person=` — so the owner is the caller and the domain
// is told the caller minted it, which is the one mint a person's account
// admits. The other two parties are the halves around it: an administrator
// naming somebody else's account is told by the domain that a person's token
// is theirs alone (the writer's refusal renders as 403), and a request
// presenting a token is refused before the body ([TestATokenCannotMintAToken]).
// Mutation: drop the minter from the handler and the domain is told nobody.
func TestAPersonMintsTheirOwnTokenFromTheirSession(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	signer, err := session.New(session.Options{
		Material: runtoken.Material{ActiveID: "k1",
			Keys: []runtoken.KeyMaterial{{ID: "k1", Material: "the-active-key-material"}}},
		RotateAfter: time.Hour, Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	lineage := uuid.Must(uuid.NewV7())
	for i := range 6 {
		lineage[i] = byte(at.UnixMilli() >> (8 * (5 - i)))
	}
	cookie, err := signer.Mint(session.Mint{Lineage: lineage,
		Person: bob.String(), Epoch: 1, StartPosition: 5,
		AbsoluteExpiresAt: at.Add(8 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.ExternalURL = "http://127.0.0.1:8080"
	arm, err := auth.NewSessions(auth.SessionsDeps{
		Signer: signer, Chart: noSeats{}, External: b.API.ExternalBase(),
		Audit: quietAudit{}, Now: func() time.Time { return at },
		Directory: sessionRows{session.Identity{Applied: 10,
			Session: session.SessionRow{Found: true, Epoch: 1, ProvedAt: at},
			Person: session.PersonRow{Found: true, Epoch: 1,
				Stage: iam.StageActive, Login: "bob.sre",
				Grants: []iam.Grant{iam.GrantStateRead}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/iam/credentials",
		strings.NewReader(`{"label":"my laptop"}`))
	req.AddCookie(&http.Cookie{Name: session.CookieBaseName, Value: cookie})
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rec := httptest.NewRecorder()
	auth.New(&b).WithSessions(arm).Middleware(r.mux).ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a person minting their own token answered %d: %s", rec.Code,
			rec.Body.String())
	}
	if got := r.writer.minted; got.PersonID != bob.String() || got.Minter != bob.String() {
		t.Errorf("the domain was asked for a token owned by %q minted by %q, "+
			"want bob minting his own", got.PersonID, got.Minter)
	}
}

// sessionRows is a session directory answering one identity for every bearer.
type sessionRows struct{ identity session.Identity }

func (d sessionRows) Resolve(context.Context, string, string) (session.Identity, error) {
	return d.identity, nil
}

// quietAudit is a guard trail that keeps nothing.
type quietAudit struct{}

func (quietAudit) Emit(context.Context, events.Payload) {}

func (quietAudit) EmitOnce(context.Context, string, time.Duration, events.Payload) bool {
	return true
}

func (quietAudit) Failed(context.Context, authevents.Failure) {}

const tokenID = "018f3a9c-0000-7000-8000-0000000000f1"

// aliceToken is a token alice holds and the row that verifies it.
func aliceToken(t *testing.T) (credential.Token, credential.TokenRow) {
	t.Helper()
	secret, err := credential.NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	presented := credential.Token{ID: tokenID, Position: 3, Secret: secret}
	return presented, credential.TokenRow{
		Applied: 10, Found: true, IsToken: true,
		Verifier:  credential.TokenVerifier(presented.ID, secret),
		ExpiresAt: time.Now().Add(time.Hour),
		Grants:    []iam.Grant{iam.GrantStateRead},
		Owner: credential.TokenOwner{
			Found: true, ID: alice.String(), Kind: iam.KindPerson,
			Stage: iam.StageActive, Login: "alice.admin",
			Grants: []iam.Grant{iam.GrantStateRead},
		},
	}
}

// throughTheGuard runs one request presenting a machine token through the
// real guard in front of this rig's routes.
func throughTheGuard(t *testing.T, r *rig, row credential.TokenRow, method,
	path string, presented credential.Token) *httptest.ResponseRecorder {

	t.Helper()
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	arm, err := auth.NewTokens(auth.TokensDeps{
		Directory: oneToken{row}, Chart: noSeats{},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+presented.Value())
	rec := httptest.NewRecorder()
	auth.New(&b).WithTokens(arm).Middleware(r.mux).ServeHTTP(rec, req)
	return rec
}

// oneToken is a directory holding one token row.
type oneToken struct{ row credential.TokenRow }

func (d oneToken) MachineToken(context.Context, string) (credential.TokenRow, error) {
	return d.row, nil
}

// noSeats is a chart view holding no seats.
type noSeats struct{}

func (noSeats) Seat(context.Context, string) (session.Seat, bool, error) {
	return session.Seat{}, false, nil
}

func (noSeats) Position(context.Context) (uint64, time.Duration, error) {
	return 1, 0, nil
}
