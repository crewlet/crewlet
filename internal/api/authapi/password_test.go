package authapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// newPassword is a password every floor here accepts.
const newPassword = "a-different-long-passphrase"

// passwordRig is the sign-in rig over one person who holds a password alone,
// behind the real guard, with its estate as the sessions every cookie is
// validated against.
func passwordRig(t *testing.T) (*signInRig, http.Handler) {
	t.Helper()
	r := newSignInRigWith(t, func(o *authapi.Options) {
		o.Sessions = o.Writer.(*estate)
	})
	passwordOnly(r.estate)
	return r, guarded(t, r)
}

// holderRig is [passwordRig] over a person who holds an authenticator app and
// a set of recovery codes beside the password, on a surface clock the case
// moves — so a session proved before the move and one proved after it are told
// apart.
func holderRig(t *testing.T) (*signInRig, http.Handler, *movingClock) {
	t.Helper()
	moving := &movingClock{at: clock}
	r := newSignInRigWith(t, func(o *authapi.Options) {
		o.Sessions = o.Writer.(*estate)
		o.Now = moving.now
	})
	return r, guarded(t, r), moving
}

// signedIn signs the rig's person in through h and answers the cookie.
func signedIn(t *testing.T, h http.Handler) string {
	t.Helper()
	return signedInWith(t, h, "")
}

// signedInWith is [signedIn] presenting code, or none where it is empty.
func signedInWith(t *testing.T, h http.Handler, code string) string {
	t.Helper()
	fields := map[string]string{"login": "jane.doe", "password": password}
	if code != "" {
		fields["code"] = code
	}
	login, _ := json.Marshal(fields)
	rec, cookie := send(t, h, http.MethodPost, "/auth/login", string(login), "")
	if rec.Code != http.StatusOK || cookie == "" {
		t.Fatalf("the sign-in answered %d with cookie %q: %s", rec.Code, cookie,
			rec.Body)
	}
	return cookie
}

// changeBody is a password change's body.
func changeBody(current, next string) string {
	return codedChange(current, next, "")
}

// codedChange is a password change's body presenting a second factor's code,
// or none where it is empty.
func codedChange(current, next, code string) string {
	fields := map[string]string{"current_password": current, "new_password": next}
	if code != "" {
		fields["code"] = code
	}
	body, _ := json.Marshal(fields)
	return string(body)
}

// heldExtra is one carried field of the rig's person's live credential of
// method, decoded into out — or false where they hold none.
func heldExtra(t *testing.T, e *estate, method iamdomain.CredentialMethod,
	field string, out any) bool {

	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.person.Credentials {
		if c.Method != method || !c.RevokedAt.IsZero() {
			continue
		}
		if err := json.Unmarshal(c.Extra[field], out); err != nil {
			t.Fatalf("the %s credential's %s: %v", method, field, err)
		}
		return true
	}
	return false
}

// reaches reports whether a cookie still reaches a guarded route.
func reaches(t *testing.T, h http.Handler, cookie string) bool {
	t.Helper()
	rec, _ := send(t, h, http.MethodGet, "/iam/people", "", cookie)
	return rec.Code == http.StatusOK
}

// emitted is every event of type T the rig's trail was handed.
func emitted[T events.Payload](a *recordingAudit) []T {
	payloads, _ := a.snapshot()
	var out []T
	for _, p := range payloads {
		if typed, ok := p.(T); ok {
			out = append(out, typed)
		}
	}
	return out
}

// A PASSWORD CHANGE ENDS EVERY OTHER SESSION, AND KEEPS THIS BROWSER.
//
// Somebody changes a password when they think somebody else has it, so the
// change moves the person's revocation epoch with it — the session somebody
// else holds stops reaching anything — while the browser the change was made
// from is handed a new session, its old cookie ended like the rest. The new
// password is what the record carries, and the change is announced.
//
// The CONTROL is the other session reaching a guarded route before the
// change. Mutation: leave the epoch where it is and the other session still
// reaches it; open no replacement and this browser is signed out too.
func TestAPasswordChangeEndsEveryOtherSessionAndKeepsThisBrowser(t *testing.T) {
	t.Parallel()
	r, h := passwordRig(t)
	here, elsewhere := signedIn(t, h), signedIn(t, h)
	if !reaches(t, h, elsewhere) {
		t.Fatal("the other session reached nothing before the change; this " +
			"case tests nothing")
	}
	rec, replacement := send(t, h, http.MethodPost, "/auth/password",
		changeBody(password, newPassword), here)
	if rec.Code != http.StatusOK || replacement == "" {
		t.Fatalf("the change answered %d with cookie %q: %s", rec.Code,
			replacement, rec.Body)
	}
	switch {
	case reaches(t, h, elsewhere):
		t.Error("the other session still reaches a guarded route after the " +
			"password changed")
	case reaches(t, h, here):
		t.Error("the cookie the change was made with still works: it was " +
			"replaced, not kept")
	case !reaches(t, h, replacement):
		t.Error("the replacement session this browser was handed reaches nothing")
	}
	sets := r.estate.passwordSets
	if len(sets) != 1 {
		t.Fatalf("the estate was asked %d password sets, want one", len(sets))
	}
	if ok, _, _ := credential.NewHasher(cheap, 1).Verify(t.Context(),
		sets[0].Verifier, newPassword); !ok {
		t.Error("the record carries a verifier the new password does not match")
	}
	changed := emitted[types.IAMPasswordChanged](r.audit)
	if len(changed) != 1 || changed[0].Login != "jane.doe" {
		t.Errorf("announced %+v, want one password change by jane.doe", changed)
	}
	// THE CLOSE OF THIS BROWSER'S SESSION NAMES THE CHANGE'S OWN REASON: the
	// change's epoch move ended that session first, and a row keeps the first
	// reason, so a close worded otherwise put two reasons on one session — the
	// person's session list read one and the trail's close row the other.
	// Mutation: word the close apart from the change and this goes red.
	r.estate.mu.Lock()
	closes := slices.Clone(r.estate.closes)
	r.estate.mu.Unlock()
	if len(closes) != 1 || closes[0].reason != sets[0].Reason || sets[0].Reason == "" {
		t.Errorf("the change wrote %q and closed %+v, want one close with "+
			"the change's own reason", sets[0].Reason, closes)
	}
}

// A WRONG CURRENT PASSWORD CHANGES NOTHING, AND IS A COUNTED FAILURE.
//
// It is the one refusal every sign-in arm answers, on the step-up's curve, and
// the attempt is counted against its source — the current password is the
// proof the change needs, so guessing it here is guessing a password. The
// CONTROL is the right one, which changes it. Mutation: skip the
// verification and the wrong password lands a change.
func TestAWrongCurrentPasswordChangesNothingAndIsCounted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		current string
		status  int
	}{
		{"the right password (the control)", password, http.StatusOK},
		{"a wrong one", "not-the-password-at-all", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, h := passwordRig(t)
			cookie := signedIn(t, h)
			rec, _ := send(t, h, http.MethodPost, "/auth/password",
				changeBody(tc.current, newPassword), cookie)
			if rec.Code != tc.status {
				t.Fatalf("answered %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			_, failures := r.audit.snapshot()
			if tc.status == http.StatusOK {
				if len(failures) != 0 {
					t.Errorf("a right password counted %v", failures)
				}
				return
			}
			if code := codeOf(t, rec); code != string(httpjson.CodeSignInRefused) {
				t.Errorf("a wrong password answered %q, want the one sign-in refusal", code)
			}
			// AND IT NAMES THE FIELD: the caller is signed in as this person,
			// so it discloses nothing, and the sign-in's own sentence sent
			// them re-checking the wrong field. Mutation: answer the bare
			// sign-in refusal.
			if !strings.Contains(rec.Body.String(), `"field":"current_password"`) ||
				!strings.Contains(rec.Body.String(), "current password") {
				t.Errorf("a wrong current password does not name the field: %s", rec.Body)
			}
			if len(failures) != 1 || failures[0].Method != types.FailPassword {
				t.Errorf("counted %+v, want one password failure", failures)
			}
			if len(r.estate.passwordSets) != 0 {
				t.Error("a wrong current password reached the writer")
			}
		})
	}
}

// A WEAK NEW PASSWORD IS 422 WITH THE RULE'S SENTENCE, and nothing is written
// or counted: the caller is the person choosing it. Mutation: drop the floor
// and the weak password lands.
func TestAWeakNewPasswordIsRefusedWithTheRule(t *testing.T) {
	t.Parallel()
	r, h := passwordRig(t)
	cookie := signedIn(t, h)
	rec, _ := send(t, h, http.MethodPost, "/auth/password",
		changeBody(password, "short"), cookie)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("answered %d, want 422: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "characters") {
		t.Errorf("the refusal does not state the rule: %s", rec.Body)
	}
	if _, failures := r.audit.snapshot(); len(failures) != 0 ||
		len(r.estate.passwordSets) != 0 {
		t.Errorf("a weak password counted %v and wrote %d sets", failures,
			len(r.estate.passwordSets))
	}
}

// ONLY A PERSON CHANGES A PASSWORD.
//
// A session exchanged from a Tier A token acts as a credential of the
// deployment's own, which has no password: it is refused 403 naming why,
// before anything is read. (A machine token acting as a person is refused by
// the authority table — see TestAMachineTokenManagesNoProof.) The CONTROL is
// the person themselves, past this check, refused only for the password they
// did not send. Mutation: drop the kind check and the token's session is read
// as a person and answered as a wrong password.
func TestOnlyAPersonChangesAPassword(t *testing.T) {
	t.Parallel()
	svc := surface(t)
	mux := http.NewServeMux()
	svc.Routes(mux)
	for _, tc := range []struct {
		name   string
		who    iam.Principal
		status int
	}{
		{"a Tier A token's session", iam.Principal{ID: uuid.Must(uuid.NewV7()),
			Login: "token:ops", Kind: iam.KindMachine, Stage: iam.StageActive,
			Grants: iam.AllGrants}, http.StatusForbidden},
		{"a person (the control)", iam.Principal{ID: uuid.Must(uuid.NewV7()),
			Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive},
			http.StatusUnprocessableEntity},
	} {
		req := httptest.NewRequest(http.MethodPost, "/auth/password",
			strings.NewReader(changeBody("", "")))
		req = req.WithContext(iam.WithPrincipal(req.Context(), tc.who))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("%s answered %d, want %d: %s", tc.name, rec.Code, tc.status,
				rec.Body)
		}
		if tc.status == http.StatusForbidden && codeOf(t, rec) != string(httpjson.CodeForbidden) {
			t.Errorf("%s was refused with %s, want the engine's forbidden "+
				"envelope naming why", tc.name, rec.Body)
		}
	}
}

// A CHANGE THIS NODE HAS NOT APPLIED OPENS NO SESSION HERE.
//
// The epoch a new session is opened at is read from this node's rows, which do
// not hold the move yet — a session opened now would be ended by the change
// the moment it applied. So it says the password changed, clears the cookie,
// and the person signs in again. The CONTROL is the applied change of
// TestAPasswordChangeEndsEveryOtherSessionAndKeepsThisBrowser, which opens
// one. Mutation: open the replacement whatever the outcome and a session is
// started here.
func TestAPendingPasswordChangeOpensNoSessionHere(t *testing.T) {
	t.Parallel()
	r, h := passwordRig(t)
	cookie := signedIn(t, h)
	r.estate.pending = map[string]bool{"SetPassword": true}
	opened := len(r.estate.starts)
	rec, replacement := send(t, h, http.MethodPost, "/auth/password",
		changeBody(password, newPassword), cookie)
	if rec.Code != http.StatusAccepted || replacement != "" {
		t.Fatalf("answered %d with cookie %q, want 202 and none: %s", rec.Code,
			replacement, rec.Body)
	}
	if len(r.estate.starts) != opened {
		t.Errorf("a session was opened on a change this node has not applied")
	}
	if !slices.ContainsFunc(rec.Result().Cookies(), func(c *http.Cookie) bool {
		return c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(time.Now()))
	}) {
		t.Error("the cookie the change was made with was not cleared")
	}
}

// A PASSWORD CHANGE ASKS THE SECOND FACTOR ITS PERSON HOLDS, AS A STEP-UP DOES.
//
// The change ends every other session and token the person holds, and every
// reset link. On the current password alone, somebody holding a person's
// session cookie and their password — but not their authenticator — changed
// it, signed the owner out everywhere and locked them out until an
// administrator issued them a reset link. So it is proved as a step-up from
// them is proved: without the code it is ASKED FOR, changes nothing and is
// counted as nothing; with a wrong one it is the one refusal, a counted
// second-factor failure, and changes nothing; with a right one — the app's or
// a recovery code — it lands, the code is SPENT, and the session this browser
// is handed is proved NOW, not when the session it replaced was. The CONTROL
// is a person holding no second factor, whose change takes no code and lands
// exactly as it always did.
//
// Mutations: drop the second-factor check and the code-less change lands;
// check the code without spending it and the app's step stays where it was
// and the recovery code stays usable; keep the replaced session's proof for a
// holder and the replacement is not proved now.
func TestAPasswordChangeAsksTheSecondFactorItsPersonHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		holder bool
		// code is what the change presents, at the surface's moved clock.
		code    func(t *testing.T, r *signInRig, at time.Time) string
		refused httpjson.Code
		counted bool
		factor  types.SecondFactor
	}{
		{name: "a holder presenting no code", holder: true,
			code:    func(*testing.T, *signInRig, time.Time) string { return "" },
			refused: httpjson.CodeSecondFactorRequired},
		{name: "a holder presenting a wrong code", holder: true,
			code: func(t *testing.T, _ *signInRig, at time.Time) string {
				return wrongAppCodeAt(t, at)
			},
			refused: httpjson.CodeSignInRefused, counted: true},
		{name: "a holder presenting the app's code", holder: true,
			code: func(t *testing.T, _ *signInRig, at time.Time) string {
				return appCode(t, at)
			},
			factor: types.FactorTOTP},
		{name: "a holder presenting a recovery code", holder: true,
			code: func(_ *testing.T, r *signInRig, _ time.Time) string {
				return r.recovery[2]
			},
			factor: types.FactorRecovery},
		{name: "somebody holding no second factor (the control)",
			code: func(*testing.T, *signInRig, time.Time) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, h, moving := holderRig(t)
			signInCode := func(i int) string { return r.recovery[i] }
			if !tc.holder {
				passwordOnly(r.estate)
				signInCode = func(int) string { return "" }
			}
			// SIGNED IN TWICE BY RECOVERY CODE, so the app's step is
			// untouched until the change spends it.
			here, elsewhere := signedInWith(t, h, signInCode(0)),
				signedInWith(t, h, signInCode(1))
			if !reaches(t, h, elsewhere) {
				t.Fatal("the other session reached nothing before the change; " +
					"this case tests nothing")
			}
			var stepBefore int64
			heldExtra(t, r.estate, iamdomain.MethodTOTP, "last_step", &stepBefore)
			var codesBefore []string
			heldExtra(t, r.estate, iamdomain.MethodRecovery, "verifiers", &codesBefore)
			r.estate.mu.Lock()
			opened := len(r.estate.starts)
			r.estate.mu.Unlock()

			// THE SESSIONS ABOVE WERE PROVED AT clock; the change is
			// made ten minutes later.
			moving.advance(10 * time.Minute)
			at := moving.now()
			rec, replacement := send(t, h, http.MethodPost, "/auth/password",
				codedChange(password, newPassword, tc.code(t, r, at)), here)
			_, failures := r.audit.snapshot()

			if tc.refused != "" {
				if rec.Code != http.StatusUnauthorized || codeOf(t, rec) != string(tc.refused) {
					t.Fatalf("answered %d %s, want 401 %s", rec.Code, rec.Body, tc.refused)
				}
				if replacement != "" {
					t.Error("a refused change handed this browser a session")
				}
				switch {
				case len(r.estate.passwordSets) != 0:
					t.Error("a change without its second factor reached the writer")
				case !reaches(t, h, elsewhere):
					t.Error("a change without its second factor ended the other session")
				case !reaches(t, h, here):
					t.Error("a change without its second factor ended this browser's session")
				}
				r.estate.mu.Lock()
				started := len(r.estate.starts) - opened
				r.estate.mu.Unlock()
				if started != 0 {
					t.Errorf("a refused change opened %d sessions", started)
				}
				var stepAfter int64
				heldExtra(t, r.estate, iamdomain.MethodTOTP, "last_step", &stepAfter)
				if stepAfter != stepBefore {
					t.Errorf("a refused change spent the app's step %d", stepAfter)
				}
				if got := len(failures); (got == 1) != tc.counted || got > 1 {
					t.Errorf("counted %+v, want counted %v", failures, tc.counted)
				}
				if tc.counted && failures[0].Method != types.FailSecondFactor {
					t.Errorf("counted %+v, want a second-factor failure", failures[0])
				}
				// THE OLD PASSWORD STILL SIGNS THE PERSON IN.
				signedInWith(t, h, signInCode(3))
				return
			}

			if rec.Code != http.StatusOK || replacement == "" {
				t.Fatalf("answered %d with cookie %q: %s", rec.Code, replacement, rec.Body)
			}
			if len(failures) != 0 {
				t.Errorf("a change that proved itself counted %v", failures)
			}
			switch {
			case len(r.estate.passwordSets) != 1:
				t.Errorf("the estate was asked %d password sets, want one",
					len(r.estate.passwordSets))
			case reaches(t, h, elsewhere):
				t.Error("the other session still reaches a guarded route after " +
					"the password changed")
			case !reaches(t, h, replacement):
				t.Error("the replacement session this browser was handed reaches nothing")
			}
			// PROVED NOW, whoever changed it.
			r.estate.mu.Lock()
			last := r.estate.starts[len(r.estate.starts)-1]
			r.estate.mu.Unlock()
			if !last.ProvedAt.Equal(at) {
				t.Errorf("the replacement was proved at %s, want now (%s), not "+
					"when the session it replaced was (%s)", last.ProvedAt, at, clock)
			}
			// AND THE CODE IS SPENT, as a step-up spends it.
			var stepAfter int64
			heldExtra(t, r.estate, iamdomain.MethodTOTP, "last_step", &stepAfter)
			var codesAfter []string
			heldExtra(t, r.estate, iamdomain.MethodRecovery, "verifiers", &codesAfter)
			switch tc.factor {
			case types.FactorTOTP:
				if stepAfter != credential.TOTPStep(at) {
					t.Errorf("the app's last step is %d after the change, want the "+
						"step its code was for, %d", stepAfter, credential.TOTPStep(at))
				}
			case types.FactorRecovery:
				if len(codesAfter) != len(codesBefore)-1 {
					t.Errorf("%d recovery codes are left after the change spent "+
						"one, want %d", len(codesAfter), len(codesBefore)-1)
				}
			}
			ups := emitted[types.IAMStepUpCompleted](r.audit)
			if len(ups) != 1 || ups[0].SecondFactor != tc.factor {
				t.Errorf("announced %+v, want one confirmation proved by %q", ups,
					tc.factor)
			}
		})
	}
}

// WRONG CODES ON A PASSWORD CHANGE CLIMB THE PERSON'S OWN CURVE.
//
// The code is decided on the step-up's path, so it meets the curve keyed on
// the PERSON as well as the one keyed on the pair: somebody holding the cookie
// and the password would otherwise divide the pair's curve by every address
// they hold, and six digits fall to a /48 in about an hour. Three wrong codes
// from three addresses, and the fourth, from a fourth, is 429 with the seven
// seconds three failures earn — counted as a turned-away second-factor attempt
// — and nothing is changed.
//
// Mutation: check the code without the person's curve and the fourth
// address's code is refused like the first, with no wait.
func TestWrongCodesOnAPasswordChangeClimbThePersonsCurve(t *testing.T) {
	t.Parallel()
	r, h, _ := holderRig(t)
	cookie := signedInWith(t, h, r.recovery[0])
	wrong := wrongAppCode(t)
	change := func(i int) *httptest.ResponseRecorder {
		rec, _ := sendFrom(t, h, fmt.Sprintf("2001:db8:%x::1", i), http.MethodPost,
			"/auth/password", codedChange(password, newPassword, wrong), cookie)
		return rec
	}
	for i := 1; i <= 3; i++ {
		if rec := change(i); rec.Code != http.StatusUnauthorized ||
			codeOf(t, rec) != string(httpjson.CodeSignInRefused) {
			t.Fatalf("wrong code %d answered %d %s, want the one refusal", i,
				rec.Code, rec.Body)
		}
	}
	fourth := change(4)
	if fourth.Code != http.StatusTooManyRequests {
		t.Fatalf("the fourth wrong code, from a fourth address, answered %d %s, "+
			"want 429 — each address was a curve of its own", fourth.Code, fourth.Body)
	}
	if got := fourth.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After %q, want the 7 seconds three wrong codes earn", got)
	}
	_, failures := r.audit.snapshot()
	if last := failures[len(failures)-1]; !last.Throttled ||
		last.Method != types.FailSecondFactor {
		t.Errorf("the turned-away code counted as %+v, want a throttled "+
			"second-factor attempt", last)
	}
	if len(r.estate.passwordSets) != 0 {
		t.Error("a change on a wrong code reached the writer")
	}
}
