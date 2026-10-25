package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/assistant/exec"
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

// TestTheDispatcherServesTheAPIsOwnHandler: api.New binds the dispatcher to the
// very handler api.port serves, so a call through a grant is the person's own
// request through the whole chain (the guard's assistant arm included) and the
// same bytes come back as the same request made with their cookie; and the
// grant's bytes presented to that handler off the wire open nothing. Mutation:
// bind the dispatcher to the bare mux and the grant is never resolved; bind it
// to anything but this App and the cookie and the grant answer differently.
func TestTheDispatcherServesTheAPIsOwnHandler(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	authorize(&b, config.APIToken{ID: "fixture", Token: fixtureToken})
	b.API.ExternalURL = "http://127.0.0.1:8080"
	keys := runtoken.Material{ActiveID: "k1",
		Keys: []runtoken.KeyMaterial{{ID: "k1", Material: "the-assistant-test-key"}}}
	now := func() time.Time { return clock }
	signer, err := session.New(session.Options{Material: keys, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	rows := personRows{}
	arm, err := auth.NewSessions(auth.SessionsDeps{Signer: signer, Directory: rows,
		Applier: rows, Chart: assistChart{}, External: b.API.ExternalBase(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	lineage := uuid.Must(uuid.NewV7())
	for i := range 6 {
		lineage[i] = byte(clock.UnixMilli() >> (8 * (5 - i)))
	}
	person := uuid.Must(uuid.NewV7()).String()
	bearer, err := signer.Mint(session.Mint{Lineage: lineage, Person: person, Epoch: 1,
		Generation: 1, StartPosition: 1, AbsoluteExpiresAt: clock.Add(8 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	rows[person] = session.Identity{Applied: cookieApplied, Generation: 1,
		Session: session.LineageRow{Found: true, Epoch: 1, ProvedAt: clock},
		Person: session.PersonRow{Found: true, Epoch: 1, Stage: iam.StageActive,
			Login: "jane.doe", Seat: assistSeat, Grants: []iam.Grant{iam.GrantTracker}}}
	cookie := &http.Cookie{Name: session.CookieName(b.API.ExternalBase()), Value: bearer}

	d, err := auth.NewDelegations(keys, assistStanding{owner: person}, now)
	if err != nil {
		t.Fatal(err)
	}
	disp := auth.NewDispatcher(d)
	a := newApp(t, api.Options{Bootstrap: &b, Sessions: arm, Delegations: d, Dispatcher: disp})

	var g exec.Grant
	derive := a.Guard().Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		g, err = d.Derive(r.Context(), auth.AssistRequest{Conversation: uuid.Must(uuid.NewV7()),
			Turn: uuid.Must(uuid.NewV7()), Scope: iam.AssistScopeRead})
	}))
	r := httptest.NewRequest(http.MethodPost, "/assistant/conversations", nil)
	r.AddCookie(cookie)
	derive.ServeHTTP(httptest.NewRecorder(), r)
	if err != nil || g == nil {
		t.Fatalf("Derive through the App's guard: %v", err)
	}

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
	if !strings.HasPrefix(presented, "Bearer "+auth.AssistBearerPrefix) {
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
