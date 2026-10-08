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
	"github.com/crewlet/crewlet/internal/iamdomain"
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
	// hour later, and the zero instant proved nothing.
	enrol := func(proved time.Time) int {
		p := iam.Principal{
			ID:    uuid.MustParse(r.estate.person.ID),
			Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
		}
		if !proved.IsZero() {
			p.ReauthAt = proved.Add(time.Hour)
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

// CHANGING HOW YOU PROVE WHO YOU ARE ASKS THE STEP-UP WINDOW, AND THE
// REFUSAL NAMES IT.
//
// Enrolling or replacing a second factor and regenerating the recovery codes
// are the second-factor reset's gesture by another door, decided by the verb
// `/iam` asks of the reset and of a factor's revocation, so a proof two hours
// old is refused here as it is there. The refusal carries the reason and the
// window in the envelope every step-up refusal on this API carries, which is
// what a client needs to ask the person to confirm and replay; it used to be a
// bare code. The control is the same person a minute after proving.
//
// Mutation: decide a verb that asks no proof and the two-hour proof is served;
// answer the refusal without its detail and the window goes missing.
func TestChangingHowYouProveWhoYouAreAsksTheStepUpWindow(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	mux := http.NewServeMux()
	r.svc.Routes(mux)
	ask := func(path string, proved time.Time) *httptest.ResponseRecorder {
		p := iam.Principal{
			ID:    uuid.MustParse(r.estate.person.ID),
			Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
			ReauthAt: proved.Add(time.Hour),
		}
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req = req.WithContext(iam.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/auth/totp", "/auth/totp/recovery"} {
		rec := ask(path, clock.Add(-2*time.Hour))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusForbidden || body["error"] != "step_up_required" ||
			body["reason"] != "step_up" || body["window"] != string(iam.RecencyStepUp) {
			t.Errorf("%s on a proof two hours old answered %d %v, want 403 "+
				"step_up_required with reason step_up naming %s", path, rec.Code,
				body, iam.RecencyStepUp)
		}
		if rec := ask(path, clock.Add(-time.Minute)); rec.Code != http.StatusOK {
			t.Errorf("%s a minute after proving answered %d %s, so the refusal "+
				"above says nothing", path, rec.Code, rec.Body)
		}
	}
}

// THE SESSION ROUTE NAMES THE SESSION THE REQUEST CARRIED.
//
// A person's sessions are listed by lineage, and the one a browser is reading
// that list from is the one it must not be offered a named sign-out of — so
// `GET /auth/session` says which lineage is this browser's, and it is the
// cookie's own, never another session of the same person. The CONTROL is the
// second browser, which reads its own. Mutation: drop the lineage and both
// answers are empty; read it from anything but the presented bearer and the
// two browsers name one session.
func TestTheSessionRouteNamesTheSessionTheRequestCarried(t *testing.T) {
	t.Parallel()
	r, h := passwordRig(t)
	here, elsewhere := signedIn(t, h), signedIn(t, h)
	if len(r.estate.starts) != 2 {
		t.Fatalf("two sign-ins opened %d sessions", len(r.estate.starts))
	}
	lineageOf := func(cookie string) string {
		t.Helper()
		rec, _ := send(t, h, http.MethodGet, "/auth/session", "", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /auth/session answered %d: %s", rec.Code, rec.Body)
		}
		var got struct {
			Lineage string `json:"lineage"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got.Lineage
	}
	for i, cookie := range []string{here, elsewhere} {
		if got, want := lineageOf(cookie), r.estate.starts[i].Lineage; got != want {
			t.Errorf("browser %d read lineage %q, want its own session's %q", i, got, want)
		}
	}
}

// THE SESSION ROUTE CARRIES THE PERSON'S OWN NAME.
//
// The chart names a SEAT, never the person in it — and a person recorded
// before every person held a seat, as the rig's is, has no seat to name at
// all — so the dashboard drew their login in the sidebar and greeted them with
// nothing: the name they typed when they redeemed their invitation is on
// their row, sealed, and nothing answered it. The CONTROL is a row this node cannot open, which
// answers no name and still says who the caller is. Mutation: drop the name
// and the first case is empty; fail the answer on a value that does not open
// and the control is refused.
func TestTheSessionRouteCarriesThePersonsOwnName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sealed, want string
	}{
		{"a name their row holds", "", "Jane Doe"},
		{"a name this node cannot open (the control)", "not-a-sealed-value", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, h := passwordRig(t)
			sealed := tc.sealed
			if sealed == "" {
				var err error
				if sealed, err = fixtureSealer.Seal(r.estate.person.ID,
					iamdomain.FieldName, "Jane Doe"); err != nil {
					t.Fatal(err)
				}
			}
			r.estate.mu.Lock()
			r.estate.nameSealed = sealed
			r.estate.mu.Unlock()
			rec, _ := send(t, h, http.MethodGet, "/auth/session", "", signedIn(t, h))
			var got struct {
				Name  string `json:"name"`
				Login string `json:"login"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if rec.Code != http.StatusOK || got.Login != "jane.doe" || got.Name != tc.want {
				t.Errorf("GET /auth/session answered %d naming %q (%q), want %q",
					rec.Code, got.Name, got.Login, tc.want)
			}
		})
	}
}
