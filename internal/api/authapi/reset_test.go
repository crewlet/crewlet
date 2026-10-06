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

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// withResetLink puts one reset link on the rig's person, as an administrator's
// issue does, and answers its id and secret.
func withResetLink(t *testing.T, e *estate, change func(*iamdomain.Credential)) (
	string, string) {

	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	secret, err := credential.NewResetSecret()
	if err != nil {
		t.Fatal(err)
	}
	link := iamdomain.Credential{
		ID: id, Method: iamdomain.MethodReset,
		Verifier:  credential.ResetVerifier(id, secret),
		ExpiresAt: clock.Add(credential.ResetLinkLifetime),
	}
	if change != nil {
		change(&link)
	}
	e.mu.Lock()
	e.person.Credentials = append(e.person.Credentials, link)
	e.mu.Unlock()
	return id, secret
}

// viewReset is the GET a reset screen makes, the secret in its header.
func viewReset(t *testing.T, h http.Handler, id, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/reset/"+id, nil)
	req.Header.Set("X-Crewlet-Reset-Secret", secret)
	req.RemoteAddr = "203.0.113.9:4711"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// spendReset is the POST that sets the password, the secret in its body.
func spendReset(t *testing.T, h http.Handler, id, secret, next string) (
	*httptest.ResponseRecorder, string) {

	t.Helper()
	body, _ := json.Marshal(map[string]string{"secret": secret, "password": next})
	return send(t, h, http.MethodPost, "/auth/reset/"+id, string(body), "")
}

// A RESET LINK SETS A PASSWORD ONCE, ENDS EVERY SESSION, AND SIGNS NOBODY IN.
//
// The view says whose password it sets and spends nothing; the spend sets the
// new password in one record that ends every session the person held — a
// session open before it reaches nothing after — and answers the login with
// no cookie, because the person signs in next, where a second factor they hold
// still applies. The link is spent: the same link again is the 410 every dead
// link answers, and not a failed attempt, since it proved itself.
//
// The CONTROL is the session reaching a guarded route before the spend.
// Mutation: hand out a session from the spend and a cookie is set; leave the
// link unspent and the second spend sets a second password.
func TestAResetLinkSetsAPasswordOnceAndSignsNobodyIn(t *testing.T) {
	t.Parallel()
	r, h := passwordRig(t)
	open := signedIn(t, h)
	if !reaches(t, h, open) {
		t.Fatal("the session reached nothing before the spend; this case tests nothing")
	}
	id, secret := withResetLink(t, r.estate, nil)

	viewed := viewReset(t, h, id, secret)
	var view map[string]any
	_ = json.Unmarshal(viewed.Body.Bytes(), &view)
	if viewed.Code != http.StatusOK || view["login"] != "jane.doe" {
		t.Fatalf("the view answered %d %v, want whose password it sets",
			viewed.Code, view)
	}
	if len(r.estate.passwordSets) != 0 {
		t.Fatal("viewing the link spent it")
	}

	spent, cookie := spendReset(t, h, id, secret, newPassword)
	var answer map[string]any
	_ = json.Unmarshal(spent.Body.Bytes(), &answer)
	if spent.Code != http.StatusOK || answer["login"] != "jane.doe" || cookie != "" {
		t.Fatalf("the spend answered %d %v with cookie %q, want the login and "+
			"no session", spent.Code, answer, cookie)
	}
	if reaches(t, h, open) {
		t.Error("a session open before the reset still reaches a guarded route")
	}
	if got := emitted[types.IAMPasswordReset](r.audit); len(got) != 1 ||
		got[0].Credential != id || got[0].Login != "jane.doe" {
		t.Errorf("announced %+v, want one reset from this link", got)
	}

	again, _ := spendReset(t, h, id, secret, "yet-another-long-passphrase")
	if again.Code != http.StatusGone || codeOf(t, again) != string(httpjson.CodeResetSpent) {
		t.Errorf("the spent link answered %d: %s", again.Code, again.Body)
	}
	if len(r.estate.passwordSets) != 1 {
		t.Errorf("the link set %d passwords, want one", len(r.estate.passwordSets))
	}
	if _, failures := r.audit.snapshot(); len(failures) != 0 {
		t.Errorf("a spent link that proved itself was counted: %v", failures)
	}
}

// EVERY DEAD RESET LINK IS ONE REFUSAL, IN THE SAME BYTES.
//
// An id that is no link, a secret that is not the id's, a link past its day,
// one an administrator revoked, and one whose person was suspended since — one
// 410, because told apart they would say which ids exist. Only the two that
// did not prove themselves are failed attempts. The CONTROL is a live link,
// which the view opens. Mutation: answer an expired link differently and its
// bytes are not the others'; count a proved link and the expired row counts.
func TestEveryDeadResetLinkIsOneRefusal(t *testing.T) {
	t.Parallel()
	var first []byte
	for _, tc := range []struct {
		name    string
		change  func(*iamdomain.Credential)
		stage   iam.Stage
		id      func(string) string
		secret  func(string) string
		counted bool
		status  int
	}{
		{name: "a live link (the control)", status: http.StatusOK},
		{name: "an id that is no link", counted: true, status: http.StatusGone,
			id: func(string) string { return uuid.Must(uuid.NewV7()).String() }},
		{name: "a secret that is not the link's", counted: true,
			status: http.StatusGone,
			secret: func(s string) string {
				// FLIPPED, never set: a secret already ending in the
				// character set is the link's own, and was one run in
				// sixty-four.
				last := "0"
				if strings.HasSuffix(s, last) {
					last = "1"
				}
				return s[:len(s)-1] + last
			}},
		{name: "a link past its day", status: http.StatusGone,
			change: func(c *iamdomain.Credential) { c.ExpiresAt = clock.Add(-time.Minute) }},
		{name: "a link revoked", status: http.StatusGone,
			change: func(c *iamdomain.Credential) { c.RevokedAt = clock.Add(-time.Minute) }},
		{name: "a person suspended since", status: http.StatusGone,
			stage: iam.StageSuspended},
	} {
		r, h := passwordRig(t)
		if tc.stage != "" {
			r.estate.person.Stage = tc.stage
		}
		id, secret := withResetLink(t, r.estate, tc.change)
		if tc.id != nil {
			id = tc.id(id)
		}
		if tc.secret != nil {
			secret = tc.secret(secret)
		}
		rec := viewReset(t, h, id, secret)
		if rec.Code != tc.status {
			t.Errorf("%s: the view answered %d, want %d: %s", tc.name, rec.Code,
				tc.status, rec.Body)
			continue
		}
		_, failures := r.audit.snapshot()
		if counted := len(failures) == 1 && failures[0].Method == types.FailReset; counted != tc.counted {
			t.Errorf("%s: counted %v, want counted %v", tc.name, failures, tc.counted)
		}
		if tc.status != http.StatusGone {
			continue
		}
		if first == nil {
			first = rec.Body.Bytes()
		} else if rec.Body.String() != string(first) {
			t.Errorf("%s answered %s, and another dead link %s — told apart "+
				"they say which ids exist", tc.name, rec.Body, first)
		}
	}
}

// A WEAK PASSWORD FROM A LIVE LINK IS 422, and spends nothing. Mutation: drop
// the floor from the spend and the weak password is set.
func TestAWeakPasswordFromAResetLinkIsRefused(t *testing.T) {
	t.Parallel()
	r, h := passwordRig(t)
	id, secret := withResetLink(t, r.estate, nil)
	rec, _ := spendReset(t, h, id, secret, "short")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("answered %d, want 422: %s", rec.Code, rec.Body)
	}
	if len(r.estate.passwordSets) != 0 {
		t.Error("a weak password reached the writer")
	}
	if ok := viewReset(t, h, id, secret).Code; ok != http.StatusOK {
		t.Errorf("the link no longer opens after a refused spend: %d", ok)
	}
}

// A RESET LINK SPENT TWICE SETS ONE PASSWORD AND ANSWERS ONE SUCCESS.
//
// Two spends of one link with different passwords reach a node that has not
// applied the first — two tabs, or a retry after a lost answer with the
// password typed again — so both open the link there. The first sets its
// password; the second is the 410 every spent link answers, never a
// `password_set` for a password nothing set, which would send the person to
// sign in with one that does not work. The CONTROL is the first spend's 200.
// Mutation: publish every spend of a link under one operation derived from
// it, and the ledger answers the second spend 200.
func TestASecondSpendOfAResetLinkIsNeverAnsweredAsSettingItsPassword(t *testing.T) {
	t.Parallel()
	r, h := passwordRig(t)
	id, secret := withResetLink(t, r.estate, nil)
	r.estate.mu.Lock()
	r.estate.behindResets = slices.Clone(r.estate.person.Credentials)
	r.estate.mu.Unlock()

	first, _ := spendReset(t, h, id, secret, newPassword)
	if first.Code != http.StatusOK {
		t.Fatalf("the first spend answered %d: %s", first.Code, first.Body)
	}
	second, _ := spendReset(t, h, id, secret, "yet-another-long-passphrase")
	if second.Code != http.StatusGone ||
		codeOf(t, second) != string(httpjson.CodeResetSpent) {
		t.Errorf("the second spend answered %d: %s — its password was never set",
			second.Code, second.Body)
	}
	if got := emitted[types.IAMPasswordReset](r.audit); len(got) != 1 {
		t.Errorf("announced %d resets, want the one that set a password", len(got))
	}
}

// A SPEND THE FRAMEWORK COLLAPSED IS STILL THIS REQUEST'S, AND IS ANNOUNCED.
//
// Each spend is its own operation, so a copy the ledger names under it — an
// append whose acknowledgement was lost, resolved from the ledger — is the
// spend this request made: 200, and one `iam_password_reset`. The CONTROL is
// the answer itself. Mutation: skip the announcement for a collapsed result
// and the spend that set the password leaves no row on the trail.
func TestACollapsedSpendOfAResetLinkIsAnnounced(t *testing.T) {
	t.Parallel()
	r, h := passwordRig(t)
	id, secret := withResetLink(t, r.estate, nil)
	r.estate.collapsed = map[string]bool{"SetPassword": true}

	spent, _ := spendReset(t, h, id, secret, newPassword)
	if spent.Code != http.StatusOK {
		t.Fatalf("the spend answered %d: %s", spent.Code, spent.Body)
	}
	if got := emitted[types.IAMPasswordReset](r.audit); len(got) != 1 ||
		got[0].Credential != id {
		t.Errorf("announced %+v, want the one reset this request made", got)
	}
}
