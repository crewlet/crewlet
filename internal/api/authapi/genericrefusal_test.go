package authapi_test

import (
	"context"
	"encoding/json"
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
// reservation, a directory this node cannot read — a person whose password is
// rightPassword, for the arm that presents another and the control that
// presents it — and a person who holds rightPassword AND a second factor, an
// authenticator app and a recovery set, for the arms that get past the first
// factor and fail at the second.
type armsDirectory struct {
	stubDirectory
	verifier string

	// factors are the second factors "tess.factor" holds beside her
	// password.
	factors []iamdomain.Credential
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
	case "tess.factor":
		return iamdomain.Sighting{ID: factorPerson, Kind: iam.KindPerson,
			Stage: iam.StageActive, Login: login,
			Credentials: append(password, d.factors...)}, nil
	}
	return iamdomain.Sighting{}, nil
}

// factorPerson is the id of the arms' person who holds a second factor — the
// id her seed is sealed to.
const factorPerson = "p-tess"

// wrongAppCode is six digits that are not the app code for totpSeed at clock,
// nor at either step the drift tolerance also accepts.
func wrongAppCode(t *testing.T) string {
	t.Helper()
	step := credential.TOTPStep(clock)
	accepted := map[string]bool{}
	for _, at := range []int64{step - 1, step, step + 1} {
		code, err := credential.TOTPCode(totpSeed, at)
		if err != nil {
			t.Fatal(err)
		}
		accepted[code] = true
	}
	for _, candidate := range []string{"000000", "111111", "222222"} {
		if !accepted[candidate] {
			return candidate
		}
	}
	t.Fatal("every candidate is a code the app accepts at this instant")
	return ""
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
// a directory this node could not read, or the right password with a wrong app
// code or a recovery code already spent — the caller gets ONE status, ONE
// body, byte for byte, padded to ONE deadline measured from admission, and one
// failed attempt on the trail. The last two leave by a path of their own, the
// second-factor check, so they are held here with the rest rather than
// assumed to answer alike. Told apart by any of the three, the refusal
// says which logins exist and which are worth guessing at. The credential
// package certifies the pad's arithmetic and the decoy's cost; this is the
// route-level half, where one arm answering differently used to be a matter of
// nobody having written the line.
//
// The control is the right password, which is NOT refused — or every arm
// would pass on a surface that refused everything.
//
// Mutation: answer any one arm with another code, or skip the pad on any one
// arm — the second-factor refusal given a code of its own included — and its
// row goes red.
func TestOneGenericRefusalForEveryLoginArm(t *testing.T) {
	t.Parallel()
	hasher := credential.NewHasher(cheap, 1)
	verifier, err := hasher.Hash(rightPassword)
	if err != nil {
		t.Fatal(err)
	}
	// TESS'S SECOND FACTORS: an app, and a recovery set from which one code
	// is already spent — gone from the set, as a sign-in that used it
	// leaves it.
	codes, verifiers, err := credential.NewRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	spentRecovery := codes[0]
	remaining, _ := json.Marshal(verifiers[1:])
	factors := []iamdomain.Credential{
		{V: iamdomain.DocumentVersion, ID: "tess-app", Method: iamdomain.MethodTOTP,
			Verifier: sealedSeed(t, factorPerson, "tess-app", totpSeed)},
		{V: iamdomain.DocumentVersion, ID: "tess-codes", Method: iamdomain.MethodRecovery,
			Extra: map[string]json.RawMessage{"verifiers": remaining}},
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
		o.Directory = armsDirectory{verifier: verifier, factors: factors}
		o.Audit = audit
	}).Routes(mux)

	signIn := func(source int, login, password, code string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{
			"login": login, "password": password, "code": code,
		})
		r := httptest.NewRequest(http.MethodPost, "/auth/login",
			strings.NewReader(string(body)))
		// A SOURCE PER ARM, so no two arms share a pair on the curve —
		// the two second-factor arms type one login — and none is the
		// one the curve turned away: that refusal is deliberately
		// specific, and it is not what this case is about.
		r.RemoteAddr = "198.51.100." + strconv.Itoa(source) + ":5100"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}

	arms := []struct{ name, login, password, code string }{
		{"nobody by that login", "nobody.here", rightPassword, ""},
		{"nobody at that address", "gone@example.com", rightPassword, ""},
		{"a directory this node cannot read", "broken.store", rightPassword, ""},
		{"a person suspended, with the right password", "sam.suspended", rightPassword, ""},
		{"an enrolment nobody finished", "half.enrolled", rightPassword, ""},
		{"a person who signs in only through a provider", "provider.only", rightPassword, ""},
		{"a wrong password", "dana.sre", "not-the-passphrase-at-all", ""},
		// PAST THE FIRST FACTOR, and refused at the second — a path of its
		// own through the second-factor check, which must answer in the
		// same bytes at the same deadline as every arm above.
		{"the right password and a wrong app code", "tess.factor", rightPassword,
			wrongAppCode(t)},
		{"the right password and a spent recovery code", "tess.factor", rightPassword,
			spentRecovery},
	}
	var first *httptest.ResponseRecorder
	for i, arm := range arms {
		rec := signIn(i+1, arm.login, arm.password, arm.code)
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
				"from admission", arm.name, got, credential.PadDeadline)
		}
	}
	_, failures := audit.snapshot()
	counted := map[types.FailureMethod]int{}
	for _, f := range failures {
		if !f.Throttled {
			counted[f.Method]++
		}
	}
	if counted[types.FailPassword] != len(arms)-2 ||
		counted[types.FailSecondFactor] != 2 {
		t.Errorf("the trail tallied %v, want one failed sign-in per arm: %d at "+
			"the password and 2 at the second factor", counted, len(arms)-2)
	}

	// THE CONTROLS: the right password is not the refusal, and neither is
	// the right password with the right code.
	if rec := signIn(len(arms)+1, "dana.sre", rightPassword, ""); rec.Code != http.StatusOK {
		t.Errorf("the right password answered %d (%s), want 200", rec.Code, rec.Body)
	}
	if rec := signIn(len(arms)+2, "tess.factor", rightPassword, appCode(t, clock)); rec.Code != http.StatusOK {
		t.Errorf("the right password and code answered %d (%s), want 200",
			rec.Code, rec.Body)
	}
}
