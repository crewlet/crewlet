package authapi_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/secrets"
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

// surface builds the sign-in surface over fakes.
//
// EVERY DEPENDENCY IS A FAKE and none is nil, which is what the constructor
// requires: a surface built around a missing half is a wiring mistake it
// refuses by name rather than a posture.
func surface(t *testing.T) *authapi.Service {
	t.Helper()
	return build(t, bootstrapFor(t))
}

// build is the shared construction over a Tier A a case chose.
func build(t *testing.T, b config.Bootstrap) *authapi.Service {
	t.Helper()
	return buildWith(t, b, nil)
}

// buildWith is [build] with one or more of the fakes replaced, for a case whose
// subject is what a seam ANSWERS rather than which routes exist.
func buildWith(t *testing.T, b config.Bootstrap,
	replace func(*authapi.Options)) *authapi.Service {

	t.Helper()
	signer := fixtureSigner(t)
	throttle := credential.NewThrottle(credential.ThrottleDeps{
		Now: func() time.Time { return clock },
		// NO PAD IN A TEST, because the pad is a wall-clock sleep: the
		// timing defence has its own cases in internal/iam/credential,
		// and paying it here would make every refusal case take its
		// deadline.
		Sleep: func(context.Context, time.Duration) {},
	})
	opts := authapi.Options{
		Bootstrap: &b,
		Directory: stubDirectory{},
		Writer:    stubWriter{},
		Signer:    signer,
		Hasher:    credential.NewHasher(credential.Default(), 1),
		Throttle:  throttle,
		Blinder:   fixtureBlinder(t),
		Sealer:    stubSealer{},
		Sessions:  stubSessions{},
		Clients:   auth.NewClients(&b),
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
	// THE WORK A SURFACE RUNS AFTER ITS ANSWERS ENDS WITH THE CASE, as it
	// ends with the listener on a node: cut at once, since a case that
	// needs a rewrite to land waits for it itself.
	t.Cleanup(func() {
		cut, cancel := context.WithCancel(context.Background())
		cancel()
		svc.Stop(cut)
	})
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

func (stubDirectory) InvitationByID(context.Context, string) (iamdomain.InvitationRow, error) {
	return iamdomain.InvitationRow{}, nil
}

func (stubDirectory) SessionStanding(context.Context, string, time.Time) (string, bool, error) {
	return "", false, nil
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

// stubSealer opens every invitation's address as one fixed value, and seals a
// credential secret for real — under one fixture key, with the DOMAIN's own
// associated data ([iamdomain.AADForCredential]) — so a seed sealed for one
// person's credential opens as that credential and as nothing else, exactly as
// a running node's per-person sealer answers.
type stubSealer struct{ address string }

func (s stubSealer) Open(context.Context, string, iamdomain.Field, string) (string, error) {
	return s.address, nil
}

func (stubSealer) SealCredential(_ context.Context, person, credentialID string,
	field iamdomain.Field, plaintext string) (string, error) {

	return fixtureCipher.Encrypt(plaintext,
		iamdomain.AADForCredential(person, credentialID, field))
}

func (stubSealer) OpenCredential(_ context.Context, person, credentialID string,
	field iamdomain.Field, sealed string) (string, error) {

	plain, err := fixtureCipher.Decrypt(sealed,
		iamdomain.AADForCredential(person, credentialID, field))
	if err != nil {
		return "", fmt.Errorf("open %s's %s %s: %w", person, field, credentialID, err)
	}
	return plain, nil
}

// fixtureCipher is the one key [stubSealer] seals under.
var fixtureCipher = func() secrets.Cipher {
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": []byte(strings.Repeat("s", 32))},
	})
	if err != nil {
		panic(err)
	}
	return cipher
}()

// sealedSeed is a TOTP seed sealed as a person's credential, as enrolment
// stores it.
func sealedSeed(t *testing.T, person, credentialID, seed string) string {
	t.Helper()
	sealed, err := stubSealer{}.SealCredential(t.Context(), person, credentialID,
		iamdomain.FieldTOTP, seed)
	if err != nil {
		t.Fatalf("seal a seed: %v", err)
	}
	return sealed
}

type stubSessions struct{}

func (stubSessions) Resolve(context.Context, string, string) (session.Identity, error) {
	return session.Identity{}, nil
}
