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
	// THE GUARD'S COMPOSITION: a proof taken at `proved` stops counting an
	// hour later for an ordinary gesture and a quarter-hour later for a
	// sensitive one, and the zero instant proved nothing.
	enrol := func(proved time.Time) int {
		p := iam.Principal{
			ID:    uuid.MustParse(r.estate.person.ID),
			Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
		}
		if !proved.IsZero() {
			p.ReauthAt = proved.Add(time.Hour)
			p.SensitiveReauthAt = proved.Add(15 * time.Minute)
		}
		req := httptest.NewRequest(http.MethodPost, "/auth/totp", nil)
		req = req.WithContext(iam.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := enrol(clock.Add(-time.Minute)); got != http.StatusOK {
		t.Errorf("a proof a minute old answered %d, want 200", got)
	}
	for name, proved := range map[string]time.Time{
		"a lapsed proof": clock.Add(-2 * time.Hour),
		"no proof":       {},
	} {
		if got := enrol(proved); got != http.StatusForbidden {
			t.Errorf("%s answered %d, want 403 step_up_required", name, got)
		}
	}
}

// CHANGING HOW YOU PROVE WHO YOU ARE ASKS THE SENSITIVE WINDOW, AND THE
// REFUSAL NAMES IT.
//
// Enrolling or replacing a second factor and regenerating the recovery codes
// are the second-factor reset's gesture by another door, and `/iam` asks the
// reset and a factor's revocation inside `step_up_sensitive`: a proof forty
// minutes old — inside the hour, outside the quarter-hour — could swap the
// factor for a stranger's and read back fresh codes here while the same
// cookie was refused taking one away there. The refusal carries the reason and
// the window in the envelope every step-up refusal on this API carries, which
// is what a client needs to ask the person to confirm and replay; it used to
// be a bare code. The control is the same person a minute after proving.
//
// Mutation: ask the ordinary window and the forty-minute proof is served;
// answer the refusal without its detail and the window goes missing.
func TestChangingHowYouProveWhoYouAreAsksTheSensitiveWindow(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	mux := http.NewServeMux()
	r.svc.Routes(mux)
	ask := func(path string, proved time.Time) *httptest.ResponseRecorder {
		p := iam.Principal{
			ID:    uuid.MustParse(r.estate.person.ID),
			Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
			ReauthAt: proved.Add(time.Hour), SensitiveReauthAt: proved.Add(15 * time.Minute),
		}
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req = req.WithContext(iam.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/auth/totp", "/auth/totp/recovery"} {
		rec := ask(path, clock.Add(-40*time.Minute))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusForbidden || body["error"] != "step_up_required" ||
			body["reason"] != "step_up" || body["window"] != string(iam.RecencySensitive) {
			t.Errorf("%s on a proof forty minutes old answered %d %v, want 403 "+
				"step_up_required with reason step_up naming %s", path, rec.Code,
				body, iam.RecencySensitive)
		}
		if rec := ask(path, clock.Add(-time.Minute)); rec.Code != http.StatusOK {
			t.Errorf("%s a minute after proving answered %d %s, so the refusal "+
				"above says nothing", path, rec.Code, rec.Body)
		}
	}
}
