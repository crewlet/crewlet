package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// assistSeat is the human seat the assistant's person holds.
const assistSeat = "reviewer"

// assistChart holds the one human seat.
type assistChart struct{}

func (assistChart) Seat(_ context.Context, handle string) (session.Seat, bool, error) {
	if handle != assistSeat {
		return session.Seat{}, false, nil
	}
	return session.Seat{Handle: assistSeat, Kind: "human", Unit: "platform"}, true, nil
}

// assistStanding says the one session is live and whose it is.
type assistStanding struct{ owner string }

func (s assistStanding) SessionStanding(context.Context, string, time.Time) (string, bool, error) {
	return s.owner, true, nil
}

// assistApp is an App serving one signed-in person holding grants on a human
// seat, with the assistant's delegations and dispatcher wired, and probe
// mounted as a guarded surface when one is given.
type assistApp struct {
	app    *api.App
	b      config.Bootstrap
	arm    *auth.Sessions
	cookie *http.Cookie
	person string
	keys   runtoken.Material
	d      *auth.Delegations
	disp   *auth.Dispatcher
	// rows are the directory's rows the session arm resolves, for a case
	// that signs somebody else in beside the person.
	rows personRows
}

func newAssistApp(t *testing.T, grants []iam.Grant, probe interface {
	Routes(authz.Mux) error
}) *assistApp {
	t.Helper()
	return newAssistAppWith(t, grants, func(opts *api.Options) {
		if probe != nil {
			opts.Config = probe
		}
	})
}

// newAssistAppWith is [newAssistApp] with the App's options finished by more,
// for a case that mounts a surface of its own (the act route, an audit sink).
func newAssistAppWith(t *testing.T, grants []iam.Grant, more func(*api.Options)) *assistApp {
	t.Helper()
	out := &assistApp{b: config.DefaultBootstrap(), keys: runtoken.Material{ActiveID: "k1",
		Keys: []runtoken.KeyMaterial{{ID: "k1", Material: "the-assistant-test-key"}}}}
	authorize(&out.b, config.APIToken{ID: "fixture", Token: fixtureToken})
	out.b.API.ExternalURL = "http://127.0.0.1:8080"
	now := func() time.Time { return clock }
	signer, err := session.New(session.Options{Material: out.keys, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	rows := personRows{}
	out.rows = rows
	out.arm, err = auth.NewSessions(auth.SessionsDeps{Signer: signer, Directory: rows,
		Applier: rows, Chart: assistChart{}, External: out.b.API.ExternalBase(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	lineage := uuid.Must(uuid.NewV7())
	for i := range 6 {
		lineage[i] = byte(clock.UnixMilli() >> (8 * (5 - i)))
	}
	out.person = uuid.Must(uuid.NewV7()).String()
	bearer, err := signer.Mint(session.Mint{Lineage: lineage, Person: out.person, Epoch: 1,
		Generation: 1, StartPosition: 1, AbsoluteExpiresAt: clock.Add(8 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// PROVED ON THE WALL CLOCK, which is the clock a route's step-up is
	// judged on (authz.ContextGuard decides at time.Now()).
	rows[out.person] = session.Identity{Applied: cookieApplied, Generation: 1,
		Session: session.LineageRow{Found: true, Epoch: 1, ProvedAt: time.Now()},
		Person: session.PersonRow{Found: true, Epoch: 1, Stage: iam.StageActive,
			Login: "jane.doe", Seat: assistSeat, Grants: grants}}
	out.cookie = &http.Cookie{Name: session.CookieName(out.b.API.ExternalBase()), Value: bearer}

	out.d, err = auth.NewDelegations(out.keys, assistStanding{owner: out.person}, now)
	if err != nil {
		t.Fatal(err)
	}
	out.disp = auth.NewDispatcher(out.d)
	opts := api.Options{Bootstrap: &out.b, Sessions: out.arm, Delegations: out.d,
		Dispatcher: out.disp}
	more(&opts)
	out.app = newApp(t, opts)
	return out
}

// derive derives a grant through the App's own guard from the person's cookie,
// as the assistant's turn route does.
func (a *assistApp) derive(t *testing.T, req auth.AssistRequest) exec.Grant {
	t.Helper()
	var g exec.Grant
	var err error
	h := a.app.Guard().Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		g, err = a.d.Derive(r.Context(), req)
	}))
	r := httptest.NewRequest(http.MethodPost, "/assistant/conversations", nil)
	r.AddCookie(a.cookie)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if err != nil || g == nil {
		t.Fatalf("Derive through the App's guard: %v", err)
	}
	return g
}

// readTurn is a turn's read grant request.
func readTurn() auth.AssistRequest {
	return auth.AssistRequest{Conversation: uuid.Must(uuid.NewV7()),
		Turn: uuid.Must(uuid.NewV7()), Scope: iam.AssistScopeRead}
}

// approved is an exec grant request bound to call.
func approved(call exec.Request) auth.AssistRequest {
	req := readTurn()
	req.Scope, req.Proposal, req.Action = iam.AssistScopeExec, uuid.Must(uuid.NewV7()),
		authz.ActionWorkCreate
	req.Request = &call
	return req
}

// TestTheDispatcherServesTheAPIsOwnHandler: api.New binds the dispatcher to the
// very handler api.port serves, so a call through a grant is the person's own
// request through the whole chain (the guard's assistant arm included) and the
// same bytes come back as the same request made with their cookie; and the
// grant's bytes presented to that handler off the wire open nothing. Mutation:
// bind the dispatcher to the bare mux and the grant is never resolved; bind it
// to anything but this App and the cookie and the grant answer differently.
func TestTheDispatcherServesTheAPIsOwnHandler(t *testing.T) {
	t.Parallel()
	fx := newAssistApp(t, []iam.Grant{iam.GrantWorklet}, nil)
	a, b, arm, cookie, person, keys, d, disp := fx.app, fx.b, fx.arm, fx.cookie, fx.person,
		fx.keys, fx.d, fx.disp
	now := func() time.Time { return clock }
	g := fx.derive(t, readTurn())

	for _, path := range []string{"/agents", "/org"} {
		byCookie := httptest.NewRequest(http.MethodGet, path, nil)
		byCookie.AddCookie(cookie)
		want := httptest.NewRecorder()
		a.ServeHTTP(want, byCookie)
		got, err := disp.Do(t.Context(), g, exec.Request{Method: http.MethodGet, Path: path})
		if err != nil {
			t.Fatalf("Do %s: %v", path, err)
		}
		if want.Code != http.StatusOK || got.Status != want.Code ||
			string(got.Body) != want.Body.String() {
			t.Errorf("GET %s answered the cookie %d %q and the grant %d %q", path, want.Code,
				want.Body.String(), got.Status, got.Body)
		}
	}

	// The grant's own bytes, read off a dispatched request, presented to the
	// App over the wire.
	var presented string
	probe := auth.NewDispatcher(d)
	if err := probe.Bind(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		presented = r.Header.Get("Authorization")
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Do(t.Context(), g, exec.Request{Method: http.MethodGet, Path: "/agents"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(presented, "Bearer "+iam.AssistBearerPrefix) {
		t.Fatalf("the dispatched request carried %q", presented)
	}
	wire := httptest.NewRequest(http.MethodGet, "/agents", nil)
	wire.Header.Set("Authorization", presented)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, wire)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("the grant over the wire answered %d", rec.Code)
	}

	// A dispatcher over other delegations is a wiring mistake api.New refuses.
	other, err := auth.NewDelegations(keys, assistStanding{owner: person}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.New(withRequired(api.Options{Bootstrap: &b, Sessions: arm, Delegations: d,
		Dispatcher: auth.NewDispatcher(other)})); err == nil || !strings.Contains(err.Error(), "did not mint") {
		t.Errorf("api.New accepted a dispatcher over delegations it was not given: %v", err)
	}
	// And delegations already bound to one App's guard are bound to no second.
	if _, err := api.New(withRequired(api.Options{Bootstrap: &b, Sessions: arm,
		Delegations: d})); err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Errorf("api.New bound one App's delegations to a second guard: %v", err)
	}
}

// verbProbe is a guarded surface mounting one route per verb a case names,
// through authz.Router over the request's own principal, exactly as every
// real surface mounts its routes: the decision is authz.Decide's.
type verbProbe map[string]authz.Action

func (v verbProbe) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, authz.ContextGuard(authz.NoChart{}))
	for pattern, action := range v {
		if err := router.Handle(pattern, authz.Policy{Action: action},
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})); err != nil {
			return err
		}
	}
	return nil
}

// TestTheGuardAndTheAuthorityTableAgreeOnTheAssistant: through the real
// App.handler, the guard's assistant arm and authz.Decide's assistant column
// answer one rule between them. A read grant takes an AssistRead verb's GET,
// and is refused assist_scope on a write method (the guard), on an
// AssistApprove verb and on a presence row (the table); assist_refused on a
// verb of the human-only list (the table). An exec grant takes its one bound
// request, on an approve or a read verb and on a presence row (its approval
// is the person present), and nothing on the human-only list. Every route is
// first served to the same person's cookie, so each refusal is the
// assistant's alone. Mutation: drop either layer's check and its rows are
// served; refuse a presence row to an exec grant and the approved removal is.
func TestTheGuardAndTheAuthorityTableAgreeOnTheAssistant(t *testing.T) {
	t.Parallel()
	probe := verbProbe{
		"GET /probe/seats":     authz.ActionSeatsRead,    // AssistRead
		"POST /probe/seats":    authz.ActionSeatsRead,    // AssistRead, a write method
		"GET /probe/create":    authz.ActionWorkCreate,   // AssistApprove
		"POST /probe/create":   authz.ActionWorkCreate,   // AssistApprove
		"GET /probe/secrets":   authz.ActionSecretWrite,  // on the human-only list
		"GET /probe/reveal":    authz.ActionSecretReveal, // listed, beside a presence
		"DELETE /probe/reveal": authz.ActionSecretReveal,
		"GET /probe/people":    authz.ActionDirectoryWrite, // a presence row, approvable
		"DELETE /probe/people": authz.ActionDirectoryWrite,
	}
	fx := newAssistApp(t, []iam.Grant{iam.GrantAdmin}, probe)

	// THE CONTROL: the person's own cookie is served every route.
	for pattern := range probe {
		method, path, _ := strings.Cut(pattern, " ")
		r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		r.AddCookie(fx.cookie)
		r.Header.Set("Origin", "http://127.0.0.1:8080")
		rec := httptest.NewRecorder()
		fx.app.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("the person's own %s answered %d %s", pattern, rec.Code, rec.Body)
		}
	}

	answer := func(t *testing.T, g exec.Grant, call exec.Request) (int, string) {
		t.Helper()
		resp, err := fx.disp.Do(t.Context(), g, call)
		if err != nil {
			t.Fatalf("Do %s %s: %v", call.Method, call.Path, err)
		}
		var body map[string]any
		_ = json.Unmarshal(resp.Body, &body)
		reason, _ := body[authz.DetailReason].(string)
		return resp.Status, reason
	}
	scope, refused := string(authz.ReasonAssistScope), string(authz.ReasonAssistRefused)

	t.Run("a read grant", func(t *testing.T) {
		t.Parallel()
		g := fx.derive(t, readTurn())
		for _, tc := range []struct {
			call   exec.Request
			status int
			reason string
		}{
			{exec.Request{Method: http.MethodGet, Path: "/probe/seats"}, http.StatusOK, ""},
			{exec.Request{Method: http.MethodPost, Path: "/probe/seats", Body: []byte(`{}`)},
				http.StatusForbidden, scope},
			{exec.Request{Method: http.MethodGet, Path: "/probe/create"}, http.StatusForbidden, scope},
			{exec.Request{Method: http.MethodGet, Path: "/probe/secrets"}, http.StatusForbidden, refused},
			{exec.Request{Method: http.MethodGet, Path: "/probe/reveal"}, http.StatusForbidden, refused},
			{exec.Request{Method: http.MethodGet, Path: "/probe/people"}, http.StatusForbidden, scope},
		} {
			status, reason := answer(t, g, tc.call)
			if status != tc.status || reason != tc.reason {
				t.Errorf("%s %s answered %d %q, want %d %q", tc.call.Method, tc.call.Path,
					status, reason, tc.status, tc.reason)
			}
		}
	})

	t.Run("an exec grant", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			bound  exec.Request
			status int
			reason string
		}{
			{exec.Request{Method: http.MethodPost, Path: "/probe/create",
				Body: []byte(`{"title":"Ship it"}`)}, http.StatusOK, ""},
			{exec.Request{Method: http.MethodPost, Path: "/probe/seats",
				Body: []byte(`{}`)}, http.StatusOK, ""},
			{exec.Request{Method: http.MethodGet, Path: "/probe/secrets"}, http.StatusForbidden, refused},
			{exec.Request{Method: http.MethodDelete, Path: "/probe/reveal"}, http.StatusForbidden, refused},
			{exec.Request{Method: http.MethodDelete, Path: "/probe/people"}, http.StatusOK, ""},
		} {
			g := fx.derive(t, approved(tc.bound))
			other := tc.bound
			other.Body = []byte(`{"title":"Ship something else"}`)
			if status, reason := answer(t, g, other); status != http.StatusForbidden || reason != scope {
				t.Errorf("an exec grant bound to %s %s carried another body: %d %q",
					tc.bound.Method, tc.bound.Path, status, reason)
			}
			if status, reason := answer(t, g, tc.bound); status != tc.status || reason != tc.reason {
				t.Errorf("an exec grant's own %s %s answered %d %q, want %d %q",
					tc.bound.Method, tc.bound.Path, status, reason, tc.status, tc.reason)
			}
			if tc.status == http.StatusOK {
				if status, _ := answer(t, g, tc.bound); status != http.StatusUnauthorized {
					t.Errorf("an exec grant's %s %s was carried twice: %d",
						tc.bound.Method, tc.bound.Path, status)
				}
			}
		}
	})
}
