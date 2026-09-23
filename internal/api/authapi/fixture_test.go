package authapi_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
)

// clock is the instant every case here reads, so a deadline a case asserts is
// one it chose rather than one the wall clock happened to give.
var clock = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

// bootstrapFor is a Tier A that would serve this surface: an external URL, a
// ceiling and a keyring.
func bootstrapFor(t *testing.T) config.Bootstrap {
	t.Helper()
	b := config.DefaultBootstrap()
	b.API.Host = "127.0.0.1"
	b.API.ExternalURL = "https://crewlet.example.com"
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: "a-token-long-enough-for-the-floor", Grants: iam.AllGrants},
	}
	return b
}

// withProvider is [surface] for a deployment that signs in through an
// identity provider, which mounts two more routes.
func withProvider(t *testing.T) *authapi.Service {
	t.Helper()
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{
		Issuer:   "https://idp.example.com",
		ClientID: "crewlet",
	}
	return build(t, b, oidc.NewProvider(oidc.Config{
		Issuer: b.API.Auth.OIDC.Issuer, ClientID: "crewlet",
		RedirectURI: b.API.ExternalBase() + auth.PathAuthOIDCCallback,
	}, nil, func() time.Time { return clock }))
}

// surface builds the sign-in surface over fakes.
//
// EVERY DEPENDENCY IS A FAKE and none is nil, which is what the constructor
// requires: a surface built around a missing half is a wiring mistake it
// refuses by name rather than a posture.
func surface(t *testing.T) *authapi.Service {
	t.Helper()
	return build(t, bootstrapFor(t), nil)
}

// build is the shared construction, so the two fixtures differ in exactly the
// field they are about.
func build(t *testing.T, b config.Bootstrap, provider *oidc.Provider) *authapi.Service {
	t.Helper()
	return buildWith(t, b, provider, nil)
}

// buildWith is [build] with one or more of the fakes replaced, for a case whose
// subject is what a seam ANSWERS rather than which routes exist.
func buildWith(t *testing.T, b config.Bootstrap, provider *oidc.Provider,
	replace func(*authapi.Options)) *authapi.Service {

	t.Helper()
	signer := fixtureSigner(t)
	throttle, err := credential.NewThrottle(credential.ThrottleDeps{
		Now: func() time.Time { return clock },
		// NO PAD IN A TEST, because the pad is a wall-clock sleep: the
		// timing defence has its own cases in internal/iam/credential,
		// and paying it here would make every refusal case take its
		// deadline.
		Sleep: func(context.Context, time.Duration) {},
	})
	if err != nil {
		t.Fatalf("credential.NewThrottle: %v", err)
	}
	opts := authapi.Options{
		Bootstrap: &b,
		Directory: stubDirectory{},
		Writer:    stubWriter{},
		Signer:    signer,
		Hasher:    credential.NewHasher(credential.Default(), 1),
		Throttle:  throttle,
		Blinder:   fixtureBlinder(t),
		Opener:    stubOpener{},
		Sessions:  stubSessions{},
		Clients:   auth.NewClients(&b),
		Provider:  provider,
		Custody:   stubCustody{},
		Cipher:    stubCipher{},
		Audit:     &recordingAudit{},
		Now:       func() time.Time { return clock },
	}
	if replace != nil {
		replace(&opts)
	}
	svc, err := authapi.New(opts)
	if err != nil {
		t.Fatalf("authapi.New: %v", err)
	}
	return svc
}

// fixtureSigner is the signer every surface here is built with, so a case can
// read back a cookie the surface minted.
func fixtureSigner(t *testing.T) *session.Signer {
	t.Helper()
	signer, err := session.New(session.Options{
		Material: runtoken.Material{
			ActiveID: "k1",
			Keys:     []runtoken.KeyMaterial{{ID: "k1", Material: "a-fixture-signing-key"}},
		},
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	return signer
}

type stubDirectory struct{}

func (stubDirectory) PersonByLogin(context.Context, string) (iamdomain.Sighting, error) {
	return iamdomain.Sighting{}, nil
}

func (stubDirectory) PersonByEmailBlind(context.Context, string) (iamdomain.Sighting, error) {
	return iamdomain.Sighting{}, nil
}

func (stubDirectory) PersonBySubjectBlind(context.Context, string, time.Time) (
	iamdomain.Sighting, error) {
	return iamdomain.Sighting{}, nil
}

func (stubDirectory) AnyPerson(context.Context) (bool, error) { return false, nil }

func (stubDirectory) InvitationByID(context.Context, string) (iamdomain.InvitationRow, error) {
	return iamdomain.InvitationRow{}, nil
}

func (stubDirectory) SessionStanding(context.Context, string, time.Time) (string, bool, error) {
	return "", false, nil
}

func (stubDirectory) OutstandingBootstrapCodes(context.Context, time.Time) (
	[]iamdomain.BootstrapCode, error) {

	return nil, nil
}

type stubWriter struct{}

// applied is a write that landed at a position and was applied here — what a
// stub answers for a write whose outcome is not what its case is about. A
// zero Result is NOT that: its outcome is none of the three, and this surface
// builds nothing on a write it cannot call landed.
func applied(at statelog.Position) statelog.Result {
	return statelog.Result{Outcome: statelog.OutcomeApplied, Position: at,
		OpID: "op:" + at.String()}
}

func (stubWriter) OpenSession(context.Context, iamdomain.SessionStart) (iamdomain.SessionOpened, error) {
	return iamdomain.SessionOpened{Result: applied(statelog.Position{})}, nil
}

func (stubWriter) CloseSession(context.Context, string, string, string, string) (statelog.Result, error) {
	return applied(statelog.Position{}), nil
}

func (stubWriter) Revoke(context.Context, string, string, string) (statelog.Result, error) {
	return applied(statelog.Position{}), nil
}

func (stubWriter) Enrol(context.Context, iamdomain.Enrolment) (statelog.Result, error) {
	return applied(statelog.Position{}), nil
}

func (stubWriter) WithdrawBootstrap(context.Context, string, string, string) (
	statelog.Result, error) {

	return applied(statelog.Position{}), nil
}

func (stubWriter) MintBootstrap(context.Context, iamdomain.BootstrapMint) (statelog.Result, error) {
	return applied(statelog.Position{}), nil
}

func (stubWriter) SpendBootstrap(context.Context, iamdomain.BootstrapSpend) (statelog.Result, error) {
	return applied(statelog.Position{}), nil
}

func (stubWriter) SetCredentials(context.Context, iamdomain.CredentialSet) (statelog.Result, error) {
	return applied(statelog.Position{}), nil
}

func (stubWriter) SpendInvitation(context.Context, iamdomain.InvitationSpend) (statelog.Result, error) {
	return applied(statelog.Position{}), nil
}

// fixtureBlinder is a real blinder over a fixture key: the surface resolves one
// per request, and a blinder already in hand is its own source.
func fixtureBlinder(t *testing.T) *iamdomain.Blinder {
	t.Helper()
	blinder, err := iamdomain.NewBlinder([]byte(strings.Repeat("k",
		iamdomain.MinBlindKeyBytes)))
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	return blinder
}

type stubOpener struct{}

func (stubOpener) Open(context.Context, string, iamdomain.Field, string) (string, error) {
	return "", nil
}

// stubCipher seals nothing and opens nothing, which is what a case about
// ROUTES needs: the flight's own sealing has its own suite in
// internal/iam/oidc.
type stubCipher struct{}

func (stubCipher) Encrypt(plaintext, _ string) (string, error) { return plaintext, nil }
func (stubCipher) Decrypt(sealed, _ string) (string, error)    { return sealed, nil }

// stubCustody keeps nothing, which a case about ROUTES needs: what a sign-in
// hands custody has its own case in keep_internal_test.go.
type stubCustody struct{}

func (stubCustody) Hold(context.Context, iamdomain.RefreshGrant, time.Time) error {
	return nil
}

type stubSessions struct{}

func (stubSessions) Resolve(context.Context, string, string) (session.Identity, error) {
	return session.Identity{}, nil
}
