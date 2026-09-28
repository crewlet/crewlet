package authapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
)

// codeOf is the machine-readable code an answer carries.
func codeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("an answer that is not the envelope: %d %s", rec.Code, rec.Body)
	}
	return body.Error
}

// A KNOWN PASSWORD NEVER CLEARS THE CURVE ON THE CODE IT GUARDS.
//
// Somebody holding a person's password but not their authenticator guesses at
// the six digits. The password alone proves itself — and the answer to it is
// `second_factor_required`, a sign-in not yet complete. Counted as a success
// it would clear the pair, and presenting the password without a code between
// every guess would wipe each wrong code off the curve: a million codes at
// line rate. It releases the attempt instead, so every wrong code climbs the
// curve whatever is sent between them, and the fourth is 429 with the wait.
//
// The rig's clock stands still and its sleep returns at once, so the waits
// below are the ones the curve DECIDED — 1s and 3s served inside the request,
// then 7s refused.
//
// Mutation: succeed the ticket on the code-less prompt and every wrong code
// starts from nothing, so none is ever refused.
func TestAKnownPasswordNeverClearsTheCurveOnTheCode(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	wrong := wrongAppCode(t)
	for guess := 1; guess <= 3; guess++ {
		prompt := r.signIn(t, "jane.doe", password, "")
		if prompt.Code != http.StatusUnauthorized ||
			codeOf(t, prompt) != string(httpjson.CodeSecondFactorRequired) {
			t.Fatalf("before guess %d the password alone answered %d %s, want "+
				"the second-factor prompt", guess, prompt.Code, prompt.Body)
		}
		refused := r.signIn(t, "jane.doe", password, wrong)
		if refused.Code != http.StatusUnauthorized ||
			codeOf(t, refused) != string(httpjson.CodeSignInRefused) {
			t.Fatalf("wrong code %d answered %d %s, want the ordinary refusal "+
				"after a wait served in the request", guess, refused.Code, refused.Body)
		}
	}
	fourth := r.signIn(t, "jane.doe", password, wrong)
	if fourth.Code != http.StatusTooManyRequests {
		t.Fatalf("the fourth wrong code answered %d %s, want 429 — the prompts "+
			"between the guesses cleared the curve", fourth.Code, fourth.Body)
	}
	if got := fourth.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After %q, want the 7 seconds three failures earn", got)
	}
}

// A STEP-UP'S PASSWORD MEETS THE SAME CURVE AS THE SIGN-IN'S.
//
// The step-up is keyed on who the session signed in as and where the request
// came from — the pair a password sign-in by that login climbs — because a
// stolen cookie must not be a way round the curve on the password it guards.
// Three wrong passwords, and the fourth is 429 with the wait; THE CONTROL is
// the same session from another address, which is another pair and owes
// nothing.
//
// Mutation: admit the step-up on no subject, and the fourth wrong password is
// answered like the first.
func TestAStepUpsPasswordMeetsTheSameCurve(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	stepUp := func(source, pass string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"password": pass})
		req := httptest.NewRequest(http.MethodPost, "/auth/step-up",
			strings.NewReader(string(body)))
		req.RemoteAddr = source + ":4711"
		req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
			ID:    uuid.MustParse(r.estate.person.ID),
			Login: r.estate.person.Login, Kind: iam.KindPerson,
			Stage: iam.StageActive, ReauthAt: clock.Add(time.Hour),
			SensitiveReauthAt: clock.Add(15 * time.Minute),
		}))
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		r.svc.Routes(mux)
		mux.ServeHTTP(rec, req)
		return rec
	}
	const here, elsewhere = "203.0.113.9", "198.51.100.40"
	for guess := 1; guess <= 3; guess++ {
		if rec := stepUp(here, "not-the-passphrase-at-all"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d answered %d %s, want the ordinary refusal",
				guess, rec.Code, rec.Body)
		}
	}
	fourth := stepUp(here, "not-the-passphrase-at-all")
	if fourth.Code != http.StatusTooManyRequests {
		t.Fatalf("the fourth wrong step-up password answered %d %s, want 429",
			fourth.Code, fourth.Body)
	}
	if got := fourth.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After %q, want the 7 seconds three failures earn", got)
	}
	if rec := stepUp(elsewhere, "not-the-passphrase-at-all"); rec.Code != http.StatusUnauthorized {
		t.Errorf("the same session from another address answered %d %s, want "+
			"the ordinary refusal — another pair owes nothing", rec.Code, rec.Body)
	}
}

// signInFrom posts one sign-in from source and answers the whole response.
func signInFrom(t *testing.T, mux *http.ServeMux, source, login, pass,
	code string) *httptest.ResponseRecorder {

	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"login": login, "password": pass, "code": code,
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/login",
		strings.NewReader(string(body)))
	req.RemoteAddr = net.JoinHostPort(source, "4711")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// movingClock is a clock a case moves by hand, shared by a throttle and the
// case driving it.
type movingClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *movingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *movingClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// WRONG CODES FROM MANY ADDRESSES CLIMB ONE CURVE: THE PERSON'S.
//
// Somebody holding a person's password guesses at their second factor from a
// block of addresses. Each address and each spelling was a fresh pair on the
// curve, so every guess was the first on its own and only the verify cap
// bounded them — six digits fall to a /48 of IPv6 in about an hour. A code is
// decided on the person's own curve too: three wrong codes from three
// addresses, and the fourth, from a fourth, is 429 with the seven seconds three
// failures earn, counted as a turned-away second-factor attempt. On to the
// ceiling, and the surface says so — somebody holding this person's password
// is guessing at their code — naming the person and the address, once it is
// reached and not before.
//
// Mutations: decide a code on its pair alone and the fourth address's guess is
// refused like the first, with no wait; announce on any failure and the first
// wrong code is announced.
func TestWrongCodesFromManyAddressesClimbThePersonsCurve(t *testing.T) {
	t.Parallel()
	moving := &movingClock{at: clock}
	r := newSignInRigWith(t, func(o *authapi.Options) {
		o.Throttle = credential.NewThrottle(credential.ThrottleDeps{
			Now:   moving.now,
			Sleep: func(context.Context, time.Duration) {},
		})
	})
	mux := http.NewServeMux()
	r.svc.Routes(mux)
	wrong := wrongAppCode(t)
	announced := func() []types.IAMSecondFactorThrottled {
		emitted, _ := r.audit.snapshot()
		var out []types.IAMSecondFactorThrottled
		for _, e := range emitted {
			if a, ok := e.(types.IAMSecondFactorThrottled); ok {
				out = append(out, a)
			}
		}
		return out
	}

	for i := 1; i <= 3; i++ {
		rec := signInFrom(t, mux, fmt.Sprintf("2001:db8:%x::1", i), "jane.doe",
			password, wrong)
		if rec.Code != http.StatusUnauthorized ||
			codeOf(t, rec) != string(httpjson.CodeSignInRefused) {
			t.Fatalf("wrong code %d answered %d %s, want the ordinary refusal",
				i, rec.Code, rec.Body)
		}
	}
	fourth := signInFrom(t, mux, "2001:db8:4::1", "jane.doe", password, wrong)
	if fourth.Code != http.StatusTooManyRequests {
		t.Fatalf("the fourth wrong code, from a fourth address, answered %d %s, "+
			"want 429 — each address was a curve of its own", fourth.Code, fourth.Body)
	}
	if got := fourth.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After %q, want the 7 seconds three wrong codes earn", got)
	}
	_, failures := r.audit.snapshot()
	last := failures[len(failures)-1]
	if !last.Throttled || last.Method != types.FailSecondFactor ||
		last.Person != r.estate.person.ID {
		t.Errorf("the turned-away code counted as %+v, want a throttled "+
			"second-factor attempt naming the person", last)
	}
	if got := announced(); len(got) != 0 {
		t.Fatalf("the curve was announced at its fourth failure: %+v", got)
	}

	// ON TO THE CEILING, each wrong code after the wait it was told.
	moving.advance(7 * time.Second)
	for i := 5; ; i++ {
		rec := signInFrom(t, mux, fmt.Sprintf("2001:db8:%x::1", i), "jane.doe",
			password, wrong)
		if rec.Code == http.StatusTooManyRequests {
			wait, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
			moving.advance(time.Duration(wait) * time.Second)
			continue
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong code from address %d answered %d %s", i, rec.Code, rec.Body)
		}
		if len(announced()) > 0 || i > 40 {
			break
		}
	}
	_, failures = r.audit.snapshot()
	wrongCodes := 0
	for _, f := range failures {
		if f.Method == types.FailSecondFactor && !f.Throttled {
			wrongCodes++
		}
	}
	if wrongCodes != credential.CurveSteps {
		t.Errorf("the ceiling was announced after %d wrong codes, want the %d "+
			"that reach it", wrongCodes, credential.CurveSteps)
	}
	got := announced()
	if len(got) != 1 || got[0].Person != r.estate.person.ID ||
		got[0].Login != "jane.doe" || !strings.HasPrefix(got[0].Remote, "2001:db8:") ||
		strings.Contains(got[0].Remote, "4711") {
		t.Fatalf("the ceiling was announced as %+v, want once, naming the "+
			"person, their login and the address", got)
	}

	// THE RIGHT CODE, after the wait, signs in and lifts the curve: the
	// wrong codes after it start again from the first step — the second of
	// them owes a second, served in the request, not the ceiling.
	moving.advance(credential.DelayCeiling)
	if rec := signInFrom(t, mux, "2001:db8:ff::1", "jane.doe", password,
		appCode(t, clock)); rec.Code != http.StatusOK {
		t.Fatalf("the right code after the wait answered %d %s", rec.Code, rec.Body)
	}
	for i, source := range []string{"2001:db8:fe::1", "2001:db8:fd::1"} {
		if rec := signInFrom(t, mux, source, "jane.doe", password,
			wrong); rec.Code != http.StatusUnauthorized {
			t.Errorf("wrong code %d after the right one answered %d %s, want the "+
				"ordinary refusal — the sign-in did not lift the person's curve",
				i+1, rec.Code, rec.Body)
		}
	}
}
