package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// rows is a session directory answering one identity for every bearer.
type rows struct{ identity session.Identity }

func (d rows) Resolve(context.Context, string, string) (session.Identity, error) {
	return d.identity, nil
}

// signInCookie posts one sign-in and answers the recorder, cookie and all.
func (r *signInRig) signInCookie(t *testing.T, login, pass, code string) *httptest.ResponseRecorder {
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

// sessionCookie is the bearer a response set, or "".
func sessionCookie(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == session.HostCookieName || c.Name == session.CookieBaseName {
			return c.Value
		}
	}
	return ""
}

// A SIGN-IN IS A PROOF, AND A STEP-UP SURFACE ACCEPTS A FRESH ONE.
//
// Every session opened by a sign-in records when its holder proved who they
// are, and the step-up surfaces — enrolling a second factor, regenerating the
// recovery codes — serve a principal whose proof is still inside the window
// and refuse one whose proof has lapsed or never happened. Nothing recorded
// the proof, so every session was stale from its first request and no person
// could enrol a second factor at all.
func TestASignInIsAProofAStepUpSurfaceAccepts(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	if rec := r.signInCookie(t, "jane.doe", password, appCode(t, clock)); rec.Code != http.StatusOK {
		t.Fatalf("sign-in answered %d", rec.Code)
	}
	if len(r.estate.starts) != 1 || !r.estate.starts[0].ProvedAt.Equal(clock) {
		t.Errorf("the session opened as %+v, want it proved at the sign-in's %s",
			r.estate.starts, clock)
	}

	mux := http.NewServeMux()
	r.svc.Routes(mux)
	enrol := func(reauth time.Time) int {
		req := httptest.NewRequest(http.MethodPost, "/auth/totp", nil)
		req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
			ID:    uuid.MustParse(r.estate.person.ID),
			Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
			ReauthAt: reauth,
		}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := enrol(clock.Add(time.Minute)); got != http.StatusOK {
		t.Errorf("a proof a minute from lapsing answered %d, want 200", got)
	}
	for name, reauth := range map[string]time.Time{
		"a lapsed proof": clock.Add(-time.Minute),
		"no proof":       {},
	} {
		if got := enrol(reauth); got != http.StatusForbidden {
			t.Errorf("%s answered %d, want 403 step_up_required", name, got)
		}
	}
}
