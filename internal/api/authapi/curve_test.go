package authapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
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
