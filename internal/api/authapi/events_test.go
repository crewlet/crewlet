package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// recordingAudit keeps what the surface announced and what it only counted.
type recordingAudit struct {
	mu       sync.Mutex
	emitted  []events.Payload
	failures []authevents.Failure
}

func (a *recordingAudit) Emit(_ context.Context, payload events.Payload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.emitted = append(a.emitted, payload)
}

// EmitOnce publishes every time: nothing here is about the coalescing, which
// internal/iam/authevents certifies.
func (a *recordingAudit) EmitOnce(ctx context.Context, _ string, _ time.Duration,
	payload events.Payload) bool {

	a.Emit(ctx, payload)
	return true
}

func (a *recordingAudit) Failed(_ context.Context, f authevents.Failure) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures = append(a.failures, f)
}

func (a *recordingAudit) snapshot() ([]events.Payload, []authevents.Failure) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.emitted), slices.Clone(a.failures)
}

// cheap is an argon2id cost a test can afford: the shipped one is asserted in
// internal/iam/credential, and nothing here is about it.
var cheap = credential.Params{Memory: 64, Time: 1, Threads: 1, KeyLen: 32}

// estate is one person's identity rows, read by the surface as its directory
// and written by it as its writer — so a credential the surface spends is gone
// the next time the surface looks, the way a real node's own rows are.
type estate struct {
	stubDirectory
	stubWriter

	mu     sync.Mutex
	person iamdomain.Sighting

	// before runs against the credential set a SetCredentials decide
	// reads, ahead of the surface's own Apply — the other writer that
	// landed first in a race.
	before func([]iamdomain.Credential) []iamdomain.Credential

	// counters are the revocation epoch and session generation a session
	// is opened at, which the writer reads in its own snapshot.
	counters iamdomain.SessionOpened

	// starts are the sessions this estate was asked to open, in order.
	starts []iamdomain.SessionStart
}

func (e *estate) PersonByLogin(_ context.Context, login string) (iamdomain.Sighting, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if login != e.person.Login {
		return iamdomain.Sighting{}, nil
	}
	held := e.person
	held.Credentials = slices.Clone(e.person.Credentials)
	return held, nil
}

func (e *estate) AnyPerson(context.Context) (bool, error) { return true, nil }

func (e *estate) OpenSession(_ context.Context, in iamdomain.SessionStart) (iamdomain.SessionOpened, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.starts = append(e.starts, in)
	opened := e.counters
	opened.Position = statelog.Position{Stream: "CREWLET_IAM_LOG", Seq: 9}
	return opened, nil
}

func (e *estate) SetCredentials(_ context.Context, in iamdomain.CredentialSet) (
	statelog.Position, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	held := slices.Clone(e.person.Credentials)
	if e.before != nil {
		held = e.before(held)
	}
	e.person.Credentials = in.Apply(held)
	return statelog.Position{Stream: "CREWLET_IAM_LOG", Seq: 10}, nil
}

const (
	password = "a-long-enough-passphrase"
	totpSeed = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
)

// signInRig is the surface over one person who holds a password, an
// authenticator app and a set of recovery codes.
type signInRig struct {
	svc      *authapi.Service
	estate   *estate
	audit    *recordingAudit
	recovery []string
}

func newSignInRig(t *testing.T) *signInRig {
	t.Helper()
	hasher := credential.NewHasher(cheap, 1)
	verifier, err := hasher.Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	codes, verifiers, err := credential.NewRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	stepRaw, _ := json.Marshal(int64(0))
	verifiersRaw, _ := json.Marshal(verifiers)
	e := &estate{person: iamdomain.Sighting{
		ID: "0192f00d-0000-7000-8000-00000000000a", Kind: iam.KindPerson,
		Stage: iam.StageActive, Login: "jane.doe",
		Credentials: []iamdomain.Credential{
			{ID: "pw", Method: iamdomain.MethodPassword, Verifier: verifier},
			{ID: "app", Method: iamdomain.MethodTOTP, Verifier: totpSeed,
				Extra: map[string]json.RawMessage{"last_step": stepRaw}},
			{ID: "codes", Method: iamdomain.MethodRecovery,
				Extra: map[string]json.RawMessage{"verifiers": verifiersRaw}},
		},
	}}
	audit := &recordingAudit{}
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendLocal
	svc := buildWith(t, b, nil, func(o *authapi.Options) {
		o.Directory, o.Writer, o.Hasher, o.Audit = e, e, hasher, audit
	})
	return &signInRig{svc: svc, estate: e, audit: audit, recovery: codes}
}

// login posts one sign-in and answers the status.
func (r *signInRig) login(t *testing.T, login, pass, code string) int {
	t.Helper()
	return r.signIn(t, login, pass, code).Code
}

// signIn posts one sign-in and answers the whole response.
func (r *signInRig) signIn(t *testing.T, login, pass, code string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"login": login, "password": pass, "code": code,
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(string(body)))
	req.RemoteAddr = "203.0.113.9:4711"
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	r.svc.Routes(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func appCode(t *testing.T, at time.Time) string {
	t.Helper()
	code, err := credential.TOTPCode(totpSeed, credential.TOTPStep(at))
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// A FAILED SIGN-IN IS COUNTED WITH WHAT WAS TYPED AS ITS SUBJECT, AND NEVER
// ANNOUNCED.
//
// The subject goes to the trail, which keys it in memory and publishes only
// how many different ones a client tried; the person goes with it only where
// THIS ENGINE resolved one. Nothing is emitted: a failed attempt is authored
// by whoever can reach the listener.
//
// Mutation: announce the refusal as an event and the emitted count is not
// zero; drop the resolved person and the second case names nobody.
func TestAFailedSignInIsCountedAndNeverAnnounced(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	if got := r.login(t, "nobody.here", password, ""); got != http.StatusUnauthorized {
		t.Fatalf("an unknown login answered %d", got)
	}
	if got := r.login(t, "jane.doe", "the-wrong-passphrase", ""); got != http.StatusUnauthorized {
		t.Fatalf("a wrong password answered %d", got)
	}
	emitted, failures := r.audit.snapshot()
	if len(emitted) != 0 {
		t.Errorf("failed sign-ins announced %v", emitted)
	}
	if len(failures) != 2 {
		t.Fatalf("counted %d failures, want 2", len(failures))
	}
	unknown, wrong := failures[0], failures[1]
	if unknown.Method != types.FailPassword || unknown.Subject != "nobody.here" ||
		unknown.Person != "" || unknown.Client != "203.0.113.9" {
		t.Errorf("unknown-login failure = %+v", unknown)
	}
	if wrong.Person != r.estate.person.ID {
		t.Errorf("a wrong password for a real login named person %q, want %q",
			wrong.Person, r.estate.person.ID)
	}
}

// A SIGN-IN ANNOUNCES ITS METHOD AND ITS SECOND FACTOR, and the app code it
// used is SPENT.
//
// A code used once and not recorded works for the whole drift window —
// ninety seconds in which a code read off somebody's screen signs its reader
// in. Mutation: skip the spend and the replay signs in.
func TestAnAppCodeSignsInOnceAndTheSignInSaysSo(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	code := appCode(t, clock)
	if got := r.login(t, "jane.doe", password, code); got != http.StatusOK {
		t.Fatalf("a correct password and code answered %d", got)
	}
	emitted, _ := r.audit.snapshot()
	if len(emitted) != 1 {
		t.Fatalf("announced %d events, want the one sign-in", len(emitted))
	}
	started, ok := emitted[0].(types.IAMSessionStarted)
	if !ok || started.Method != types.SignInPassword || started.SecondFactor != types.FactorTOTP ||
		started.Person != r.estate.person.ID || started.Lineage == "" ||
		started.Remote != "203.0.113.9" {
		t.Errorf("sign-in event = %#v", emitted[0])
	}
	if got := r.login(t, "jane.doe", password, code); got != http.StatusUnauthorized {
		t.Errorf("the same app code signed in twice (answered %d)", got)
	}
}

// A RECOVERY CODE WORKS BESIDE AN APP, IS SPENT, AND SAYS HOW MANY ARE LEFT.
//
// It used to be checked against the app's six digits whenever the person held
// an app — so the codes that exist for the day the app is lost worked for
// nobody who had one — and, where it was reached, never removed. Mutation:
// check only the first factor held and the first sign-in fails; skip the spend
// and the replay signs in.
func TestARecoveryCodeSignsInOnceAndSaysHowManyAreLeft(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	if got := r.login(t, "jane.doe", password, r.recovery[3]); got != http.StatusOK {
		t.Fatalf("a correct password and recovery code answered %d", got)
	}
	emitted, _ := r.audit.snapshot()
	var used *types.IAMRecoveryCodeUsed
	var started *types.IAMSessionStarted
	for _, e := range emitted {
		switch v := e.(type) {
		case types.IAMRecoveryCodeUsed:
			used = &v
		case types.IAMSessionStarted:
			started = &v
		}
	}
	if used == nil || used.Remaining != credential.RecoveryCodeCount-1 ||
		used.Person != r.estate.person.ID {
		t.Errorf("recovery event = %+v, want %d remaining", used, credential.RecoveryCodeCount-1)
	}
	if started == nil || started.SecondFactor != types.FactorRecovery {
		t.Errorf("sign-in event = %+v, want the recovery factor named", started)
	}
	if got := r.login(t, "jane.doe", password, r.recovery[3]); got != http.StatusUnauthorized {
		t.Errorf("the same recovery code signed in twice (answered %d)", got)
	}
	if got := r.login(t, "jane.doe", password, r.recovery[4]); got != http.StatusOK {
		t.Errorf("an unspent recovery code was refused (answered %d)", got)
	}
}

// A CODE SPENT BY SOMEBODY ELSE BETWEEN THE CHECK AND THE WRITE IS REFUSED.
//
// Two sign-ins presenting one code at the same moment both pass the check
// against the rows they read; the write is where they meet, and the one whose
// decide finds the step already recorded has lost. Mutation: decide the spend
// from the credential the check read rather than the one the write's own
// snapshot holds, and both sign in.
func TestACodeSpentConcurrentlyIsRefusedToTheLoser(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	step := credential.TOTPStep(clock)
	r.estate.before = func(held []iamdomain.Credential) []iamdomain.Credential {
		out := slices.Clone(held)
		for i, c := range out {
			if c.ID == "app" {
				raw, _ := json.Marshal(step)
				extra := map[string]json.RawMessage{"last_step": raw}
				out[i].Extra = extra
			}
		}
		return out
	}
	if got := r.login(t, "jane.doe", password, appCode(t, clock)); got != http.StatusUnauthorized {
		t.Errorf("the second of two concurrent uses of one code signed in (answered %d)", got)
	}
	emitted, failures := r.audit.snapshot()
	if len(emitted) != 0 {
		t.Errorf("the losing sign-in announced %v", emitted)
	}
	if len(failures) != 1 || failures[0].Method != types.FailSecondFactor {
		t.Errorf("failures = %+v, want one second-factor failure", failures)
	}
}

// A THROTTLED ATTEMPT IS COUNTED AS ONE THE CEILING TURNED AWAY.
func TestAThrottledAttemptIsCountedAsThrottled(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	for range credential.AdmitLimit {
		r.login(t, "nobody.here", password, "")
	}
	if got := r.login(t, "nobody.here", password, ""); got != http.StatusTooManyRequests {
		t.Fatalf("past the limit answered %d, want 429", got)
	}
	_, failures := r.audit.snapshot()
	last := failures[len(failures)-1]
	if !last.Throttled || last.Method != types.FailPassword || last.Subject != "" {
		t.Errorf("throttled failure = %+v: want it marked throttled, on the "+
			"route's method, and naming nobody — it was refused before the "+
			"body was read", last)
	}
}
