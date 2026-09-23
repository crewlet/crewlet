package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// --- a bound Tier A token ------------------------------------------------ //

const (
	opsToken = "a-tier-a-token-long-enough"
	opsLogin = "token:ops"
)

// fakeBinding is the identity directory's answer for one login.
type fakeBinding struct {
	row session.PersonRow
	err error
}

func (f fakeBinding) BoundSeat(_ context.Context, login string) (session.PersonRow, error) {
	if f.err != nil {
		return session.PersonRow{}, f.err
	}
	if login != opsLogin {
		return session.PersonRow{}, nil
	}
	return f.row, nil
}

// boundOps is the `ops` token bound to platform-lead at chart position 900,
// on a chart at 1000 that holds that seat.
func boundOps() (fakeBinding, *fakeChart) {
	return fakeBinding{row: session.PersonRow{
			Found: true, Stage: iam.StageActive, Login: opsLogin,
			Seat: sessionSeat, SeatAt: 900,
		}}, &fakeChart{position: 1000, seats: map[string]session.Seat{
			sessionSeat: {Handle: sessionSeat, Kind: "human", Unit: "platform"},
		}}
}

// tokenGuard is a guard over one Tier A token, `ops`, whose binding reads dir
// and chart.
func tokenGuard(t *testing.T, bindings auth.SeatBindings) *auth.Guard {
	t.Helper()
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: opsToken,
		Grants: []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite}}}
	b.API.Auth.MaxGrants = iam.AllGrants
	return auth.New(&b).BindSeats(bindings)
}

// asOps runs one request presenting the `ops` token.
func asOps(t *testing.T, g *auth.Guard, method, path string) answered {
	t.Helper()
	out := answered{how: iam.Unknown}
	handler := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out.principal, out.how = iam.From(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+opsToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	result := rec.Result()
	defer result.Body.Close()
	out.status = result.StatusCode
	_ = json.NewDecoder(result.Body).Decode(&out.body)
	return out
}

// A BOUND TOKEN ACTS AS ITS SEAT, with the chart's own answer about it.
//
// The control for every case below: bound, and the seat is there, human and
// current, so the token acts as that seat's holder — a person, under the
// seat's handle, carrying the unit and the binding's position — with the
// grants TIER A declared, never the directory's.
func TestABoundTokenActsAsTheSeatTheChartHolds(t *testing.T) {
	t.Parallel()
	dir, chart := boundOps()
	got := asOps(t, tokenGuard(t, auth.SeatBindings{Directory: dir, Chart: chart}),
		http.MethodGet, "/agents")
	if got.status != http.StatusOK || got.how != iam.Resolved {
		t.Fatalf("status %d resolution %v, want 200 resolved (body %v)",
			got.status, got.how, got.body)
	}
	p := got.principal
	if p.Kind != iam.KindPerson || p.Seat != sessionSeat {
		t.Errorf("acts as %s %q, want a person at %q", p.Kind, p.Seat, sessionSeat)
	}
	if p.Position != "platform" || p.SeatAt != 900 {
		t.Errorf("position %q seat-at %d, want platform at 900", p.Position, p.SeatAt)
	}
	if p.Login != opsLogin {
		t.Errorf("login %q, want the token's own %q", p.Login, opsLogin)
	}
	if len(p.Grants) != 2 || p.Can(iam.GrantPeopleManage) {
		t.Errorf("grants %v, want exactly what Tier A declared", p.Grants)
	}
}

// A TOKEN FOLLOWS ITS SEAT THROUGH A RENAME.
//
// The row holds the handle as it was written; the chart resolves a former
// handle to the seat it names now. Taking the row's handle raw — what the
// lookup this replaced did — wrote every audit row under a handle nothing
// answers to, and once the chart gave that old handle to a NEW seat, carried
// the new seat's lead relations.
func TestABoundTokenFollowsItsSeatThroughARename(t *testing.T) {
	t.Parallel()
	dir, chart := boundOps()
	chart.seats = map[string]session.Seat{
		sessionSeat: {Handle: "platform-director", Kind: "human", Unit: "platform"},
	}
	got := asOps(t, tokenGuard(t, auth.SeatBindings{Directory: dir, Chart: chart}),
		http.MethodGet, "/agents")
	if got.status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %v)", got.status, got.body)
	}
	if got.principal.Seat != "platform-director" {
		t.Errorf("acts as %q, want the seat's current handle platform-director",
			got.principal.Seat)
	}
}

// A TOKEN BOUND TO A SEAT THE CHART WILL NOT LET IT ACT AS IS REFUSED, NAMING
// IT — exactly as a signed-in person bound there is.
//
// Removed, tombstoned, or an agent's seat: the lookup this replaced returned
// the row's handle regardless, so the token went on acting as a seat that no
// longer existed, or as an agent. It is 403 `seat_unavailable` naming the
// seat, and on `/auth/` — the one surface that never reads a seat — the
// credential is still resolved as itself, with the binding's position beside
// an empty seat.
func TestABoundTokenIsRefusedASeatTheChartWillNotLetItActAs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		chart func(*fakeChart)
	}{
		{"removed, on a chart that covers the binding", func(c *fakeChart) {
			delete(c.seats, sessionSeat)
		}},
		{"tombstoned", func(c *fakeChart) {
			c.seats[sessionSeat] = session.Seat{Handle: sessionSeat, Tombstoned: true}
		}},
		{"an agent's seat", func(c *fakeChart) {
			c.seats[sessionSeat] = session.Seat{Handle: sessionSeat, Kind: "agent"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, chart := boundOps()
			tc.chart(chart)
			g := tokenGuard(t, auth.SeatBindings{Directory: dir, Chart: chart})
			got := asOps(t, g, http.MethodGet, "/agents")
			if got.status != http.StatusForbidden {
				t.Fatalf("status %d, want 403 (body %v)", got.status, got.body)
			}
			if got.body["error"] != string(httpjson.CodeSeatUnavailable) {
				t.Errorf("code %v, want %q", got.body["error"],
					httpjson.CodeSeatUnavailable)
			}
			if detail, _ := got.body["detail"].(string); !contains(detail, sessionSeat) {
				t.Errorf("detail %q does not name the seat", detail)
			}
			own := asOps(t, g, http.MethodPost, "/auth/session")
			if own.how != iam.Resolved {
				t.Fatalf("resolution %v on /auth/session, want resolved", own.how)
			}
			if own.principal.Seat != "" || own.principal.Kind != iam.KindMachine {
				t.Errorf("acts as %s %q, want the bare machine", own.principal.Kind,
					own.principal.Seat)
			}
			if own.principal.SeatAt != 900 {
				t.Errorf("SeatAt %d beside an empty seat, want 900: zero reads "+
					"as a credential nobody ever bound", own.principal.SeatAt)
			}
		})
	}
}

// A TOKEN WHOSE SEAT THIS NODE CANNOT RESOLVE IS 503, NEVER THE BARE
// CREDENTIAL.
//
// Behind the binding on the chart, past the stall grace, or unable to read
// the directory at all: each is "this node cannot say", and answered as the
// bare credential it is one actor writing under a seat on one node and under
// the token's own name on the next. 503 identity_unavailable, which a client
// retries.
func TestABoundTokenThisNodeCannotResolveIsUnavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*fakeBinding, *fakeChart)
	}{
		{"a chart this node has not applied as far as the binding",
			func(_ *fakeBinding, c *fakeChart) {
				delete(c.seats, sessionSeat)
				c.position = 899
			}},
		{"a chart past the stall grace", func(_ *fakeBinding, c *fakeChart) {
			c.lag = statelog.StallGrace + time.Second
		}},
		{"a chart view that cannot be read", func(_ *fakeBinding, c *fakeChart) {
			c.err = errors.New("the view is not built")
		}},
		{"a directory that cannot be read", func(d *fakeBinding, _ *fakeChart) {
			d.err = errors.New("the replicated estate is not open")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, chart := boundOps()
			tc.setup(&dir, chart)
			got := asOps(t, tokenGuard(t, auth.SeatBindings{Directory: dir,
				Chart: chart}), http.MethodGet, "/agents")
			if got.status != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503 (body %v)", got.status, got.body)
			}
			if got.body["error"] != string(httpjson.CodeIdentityUnavailable) {
				t.Errorf("code %v, want %q", got.body["error"],
					httpjson.CodeIdentityUnavailable)
			}
		})
	}
	// AND A DIRECTORY WITH NO CHART is a binding that cannot be resolved,
	// not a credential nobody bound.
	dir, _ := boundOps()
	got := asOps(t, tokenGuard(t, auth.SeatBindings{Directory: dir}),
		http.MethodGet, "/agents")
	if got.status != http.StatusServiceUnavailable {
		t.Errorf("a binding with no chart to resolve it in answered %d, want 503",
			got.status)
	}
}

// AN UNBOUND TOKEN IS THE BARE CREDENTIAL, which is ordinary.
//
// The other controls: nobody in the directory binds the token, or nothing
// wired a directory at all — and in both it acts as the machine it is, under
// its own login, never refused and never 503.
func TestAnUnboundTokenActsAsItself(t *testing.T) {
	t.Parallel()
	_, chart := boundOps()
	for name, bindings := range map[string]auth.SeatBindings{
		"a directory holding no binding": {Directory: fakeBinding{}, Chart: chart},
		"no binding source wired":        {},
	} {
		got := asOps(t, tokenGuard(t, bindings), http.MethodGet, "/agents")
		if got.status != http.StatusOK || got.how != iam.Resolved {
			t.Errorf("%s: status %d resolution %v, want 200 resolved", name,
				got.status, got.how)
			continue
		}
		if got.principal.Kind != iam.KindMachine || got.principal.Seat != "" ||
			got.principal.Login != opsLogin {
			t.Errorf("%s: acts as %s %q under %q, want the machine %q", name,
				got.principal.Kind, got.principal.Seat, got.principal.Login,
				opsLogin)
		}
	}
}
