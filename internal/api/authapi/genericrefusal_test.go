package authapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// armsDirectory answers each login a sign-in arm is about: somebody who does
// not exist, somebody who cannot act, somebody who holds no password, a
// reservation, a directory this node cannot read — and a person whose password
// is rightPassword, for the arm that presents another and the control that
// presents it.
type armsDirectory struct {
	stubDirectory
	verifier string
}

// rightPassword is the one password the arms' people hold.
const rightPassword = "the-one-right-passphrase"

func (d armsDirectory) PersonByLogin(_ context.Context, login string) (
	iamdomain.Sighting, error) {

	password := []iamdomain.Credential{{V: iamdomain.DocumentVersion,
		ID: "cred-" + login, Method: iamdomain.MethodPassword, Verifier: d.verifier}}
	switch login {
	case "broken.store":
		return iamdomain.Sighting{}, errors.New("the replicated estate is not open")
	case "sam.suspended":
		return iamdomain.Sighting{ID: "p-sam", Kind: iam.KindPerson,
			Stage: iam.StageSuspended, Login: login, Credentials: password}, nil
	case "half.enrolled":
		return iamdomain.Sighting{ID: "p-half", Login: login, Reserved: true}, nil
	case "provider.only":
		return iamdomain.Sighting{ID: "p-oidc", Kind: iam.KindPerson,
			Stage: iam.StageActive, Login: login,
			Credentials: []iamdomain.Credential{{V: iamdomain.DocumentVersion,
				ID: "link-1", Method: iamdomain.MethodOIDC}}}, nil
	case "dana.sre":
		return iamdomain.Sighting{ID: "p-dana", Kind: iam.KindPerson,
			Stage: iam.StageActive, Login: login, Credentials: password}, nil
	}
	return iamdomain.Sighting{}, nil
}

// PersonByEmailBlind finds nobody: the address arm is somebody who does not
// exist, looked up the way an address is.
func (armsDirectory) PersonByEmailBlind(context.Context, string) (iamdomain.Sighting, error) {
	return iamdomain.Sighting{}, nil
}

// padRecorder is a throttle's sleep, recorded rather than slept.
type padRecorder struct {
	mu     sync.Mutex
	sleeps []time.Duration
}

func (p *padRecorder) sleep(_ context.Context, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sleeps = append(p.sleeps, d)
}

func (p *padRecorder) take() []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.sleeps
	p.sleeps = nil
	return out
}

// ONE GENERIC REFUSAL FOR EVERY LOGIN ARM.
//
// A sign-in surface must not be a roster: whatever went wrong — nobody by that
// login, nobody at that address, a person suspended, a half-finished
// enrolment, somebody who signs in only through a provider, a wrong password,
// or a directory this node could not read — the caller gets ONE status, ONE
// body, byte for byte, padded to ONE deadline measured from arrival, and one
// failed attempt on the trail. Told apart by any of the three, the refusal
// says which logins exist and which are worth guessing at. The credential
// package certifies the pad's arithmetic and the decoy's cost; this is the
// route-level half, where one arm answering differently used to be a matter of
// nobody having written the line.
//
// The control is the right password, which is NOT refused — or every arm
// would pass on a surface that refused everything.
//
// Mutation: answer any one arm with another code, or skip the pad on any one
// arm, and its row goes red.
func TestOneGenericRefusalForEveryLoginArm(t *testing.T) {
	t.Parallel()
	hasher := credential.NewHasher(cheap, 1)
	verifier, err := hasher.Hash(rightPassword)
	if err != nil {
		t.Fatal(err)
	}
	pads := &padRecorder{}
	audit := &recordingAudit{}
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendLocal
	mux := http.NewServeMux()
	buildWith(t, b, nil, func(o *authapi.Options) {
		throttle, err := credential.NewThrottle(credential.ThrottleDeps{
			Now: func() time.Time { return clock }, Sleep: pads.sleep,
		})
		if err != nil {
			t.Fatalf("credential.NewThrottle: %v", err)
		}
		o.Throttle = throttle
		o.Hasher = hasher
		o.Directory = armsDirectory{verifier: verifier}
		o.Audit = audit
	}).Routes(mux)

	signIn := func(source int, login, password string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/auth/login",
			strings.NewReader(`{"login":"`+login+`","password":"`+password+`"}`))
		// A SOURCE PER ARM, so no arm is the one the per-source ceiling
		// turned away: that refusal is deliberately specific, and it is
		// not what this case is about.
		r.RemoteAddr = "198.51.100." + strconv.Itoa(source) + ":5100"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}

	arms := []struct{ name, login, password string }{
		{"nobody by that login", "nobody.here", rightPassword},
		{"nobody at that address", "gone@example.com", rightPassword},
		{"a directory this node cannot read", "broken.store", rightPassword},
		{"a person suspended, with the right password", "sam.suspended", rightPassword},
		{"an enrolment nobody finished", "half.enrolled", rightPassword},
		{"a person who signs in only through a provider", "provider.only", rightPassword},
		{"a wrong password", "dana.sre", "not-the-passphrase-at-all"},
	}
	var first *httptest.ResponseRecorder
	for i, arm := range arms {
		rec := signIn(i+1, arm.login, arm.password)
		if first == nil {
			first = rec
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s answered %d (%s), want 401", arm.name, rec.Code, rec.Body)
			}
		}
		if rec.Code != first.Code || rec.Body.String() != first.Body.String() {
			t.Errorf("%s answered %d %s, and %s answered %d %s — two refusals "+
				"a caller can tell apart", arm.name, rec.Code, rec.Body,
				arms[0].name, first.Code, first.Body)
		}
		if got := pads.take(); len(got) != 1 || got[0] != credential.PadDeadline {
			t.Errorf("%s was padded %v, want once to the %s deadline measured "+
				"from arrival", arm.name, got, credential.PadDeadline)
		}
	}
	_, failures := audit.snapshot()
	counted := 0
	for _, f := range failures {
		if f.Method == types.FailPassword && !f.Throttled {
			counted++
		}
	}
	if counted != len(arms) {
		t.Errorf("the trail tallied %d failed sign-ins, want one per arm (%d)",
			counted, len(arms))
	}

	// THE CONTROL: the right password is not the refusal.
	if rec := signIn(len(arms)+1, "dana.sre", rightPassword); rec.Code != http.StatusOK {
		t.Errorf("the right password answered %d (%s), want 200", rec.Code, rec.Body)
	}
}
