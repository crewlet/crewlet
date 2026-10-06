package authapi_test

import (
	"encoding/json"
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

// signedIn signs the rig's person in through h and answers the cookie.
func signedIn(t *testing.T, h http.Handler) string {
	t.Helper()
	login, _ := json.Marshal(map[string]string{"login": "jane.doe", "password": password})
	rec, cookie := send(t, h, http.MethodPost, "/auth/login", string(login), "")
	if rec.Code != http.StatusOK || cookie == "" {
		t.Fatalf("the sign-in answered %d with cookie %q: %s", rec.Code, cookie,
			rec.Body)
	}
	return cookie
}

// changeBody is a password change's body.
func changeBody(current, next string) string {
	body, _ := json.Marshal(map[string]string{
		"current_password": current, "new_password": next,
	})
	return string(body)
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
