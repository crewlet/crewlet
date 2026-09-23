package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE GUARD'S THIRD ARM: a machine token becomes its OWNER.
//
// What the rows mean is internal/iam/credential's table and is certified
// there; what the directory stores is internal/iamdomain's and is certified
// against a real broker there. These cases hold the GUARD to composing what
// the table said — whose principal, which grants, which seat — and to the
// three answers reaching the caller as 200, 401 and 503.

const tokenOwner = "018f3a9c-0000-7000-8000-00000000a11c"

// machineRig is one minted token, its row, and a chart that holds its owner's
// seat.
type machineRig struct {
	t         *testing.T
	presented credential.Token
	row       credential.TokenRow
	dirErr    error
	chart     *fakeChart
	ceiling   []iam.Grant
	at        time.Time
}

func newMachineRig(t *testing.T) *machineRig {
	t.Helper()
	secret, err := credential.NewTokenSecret()
	if err != nil {
		t.Fatalf("mint a secret: %v", err)
	}
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	presented := credential.Token{
		ID: "018f3a9c-0000-7000-8000-0000000000f1", Position: 40, Secret: secret,
	}
	return &machineRig{
		t: t, presented: presented, at: at,
		ceiling: iam.AllGrants,
		row: credential.TokenRow{
			Applied: 50, Found: true, IsToken: true,
			Verifier:  credential.TokenVerifier(presented.ID, secret),
			ExpiresAt: at.Add(24 * time.Hour),
			Grants:    []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite},
			Colleague: iam.ColleagueRead,
			Epoch:     2,
			Owner: credential.TokenOwner{
				Found: true, ID: tokenOwner, Kind: iam.KindPerson,
				Stage: iam.StageActive, Login: "sarah.chen",
				Grants: []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite,
					iam.GrantConfigRead},
				Colleague: iam.ColleagueRead,
				Epoch:     2,
			},
		},
		chart: &fakeChart{position: 1000, seats: map[string]session.Seat{
			sessionSeat: {Handle: sessionSeat, Kind: "human", Unit: "platform"},
		}},
	}
}

func (m *machineRig) MachineToken(_ context.Context, id string) (credential.TokenRow, error) {
	if m.dirErr != nil {
		return credential.TokenRow{}, m.dirErr
	}
	if id != m.presented.ID {
		return credential.TokenRow{Applied: m.row.Applied}, nil
	}
	return m.row, nil
}

// guard builds a guard with the machine arm over this rig, reporting to tr.
func (m *machineRig) guard(tr *trail) *auth.Guard {
	m.t.Helper()
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = m.ceiling
	arm, err := auth.NewTokens(auth.TokensDeps{
		Directory: m, Chart: m.chart,
		Now: func() time.Time { return m.at },
	})
	if err != nil {
		m.t.Fatalf("build the token arm: %v", err)
	}
	g := auth.New(&b).WithTokens(arm)
	if tr != nil {
		g = g.WithAudit(tr)
	}
	return g
}

// present runs one request presenting value as a bearer.
func present(t *testing.T, g *auth.Guard, method, path, value string) (
	answered, string) {

	t.Helper()
	out := answered{how: iam.Unknown}
	var token string
	handler := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out.principal, out.how = iam.From(r.Context())
		token, _ = auth.PresentedToken(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+value)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	result := rec.Result()
	defer result.Body.Close()
	out.status = result.StatusCode
	_ = json.NewDecoder(result.Body).Decode(&out.body)
	return out, token
}

// A TOKEN ACTS AS ITS OWNER, carrying what the mint conferred that the owner
// still holds, cut to this node's ceiling.
//
// The control for every case below. Three narrowings compose and each is
// load-bearing: the mint's own grants (the owner's config:read was never put
// on the token), the owner's CURRENT grants (work:write withdrawn since), and
// the ceiling (state:read is all this node lets anybody carry). Mutation: take
// row.Grants instead of the effective set and work:write survives; drop the
// ceiling and so does anything the node forbids.
func TestATokenActsAsItsOwnerWithTheNarrowestOfThreeGrantSets(t *testing.T) {
	t.Parallel()
	m := newMachineRig(t)
	got, token := present(t, m.guard(nil), http.MethodGet, "/agents",
		m.presented.Value())
	if got.status != http.StatusOK || got.how != iam.Resolved {
		t.Fatalf("status %d resolution %v, want 200 resolved (body %v)",
			got.status, got.how, got.body)
	}
	p := got.principal
	if p.ID.String() != tokenOwner || p.Login != "sarah.chen" ||
		p.Kind != iam.KindPerson {
		t.Errorf("acts as %s %s %q, want the owner", p.Kind, p.ID, p.Login)
	}
	if !slices.Equal(p.Grants, []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite}) {
		t.Errorf("grants %v, want exactly what the mint conferred", p.Grants)
	}
	if p.Colleague != iam.ColleagueRead {
		t.Errorf("reach %q, want the mint's read", p.Colleague)
	}
	if token != m.presented.ID {
		t.Errorf("the request carries token %q, want %q — the mint route "+
			"cannot refuse a token it cannot see", token, m.presented.ID)
	}

	// THE OWNER LOST work:write, and the token with them.
	withdrawn := newMachineRig(t)
	withdrawn.row.Owner.Grants = []iam.Grant{iam.GrantStateRead}
	got, _ = present(t, withdrawn.guard(nil), http.MethodGet, "/agents",
		withdrawn.presented.Value())
	if slices.Contains(got.principal.Grants, iam.GrantWorkWrite) {
		t.Errorf("grants %v: a grant the owner no longer holds survived on "+
			"their token", got.principal.Grants)
	}

	// THE NODE'S CEILING clamps a token exactly as it clamps everything.
	clamped := newMachineRig(t)
	clamped.ceiling = []iam.Grant{iam.GrantStateRead}
	got, _ = present(t, clamped.guard(nil), http.MethodGet, "/agents",
		clamped.presented.Value())
	if !slices.Equal(got.principal.Grants, []iam.Grant{iam.GrantStateRead}) {
		t.Errorf("grants %v past a ceiling of state:read", got.principal.Grants)
	}
}

// A TOKEN THIS NODE KNOWS IS NO GOOD IS 401, COUNTED AS A FAILED BEARER.
//
// Every refusal answers the same way — the log says which fact decided, the
// caller learns only that this credential will not do. Mutation: answer a
// refusal as unknown and a pipeline retries a revoked token for ever.
func TestATokenThisNodeKnowsIsNoGoodIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*machineRig)
		value func(*machineRig) string
	}{
		{"a wrong secret", nil, func(m *machineRig) string {
			wrong := m.presented
			wrong.Secret = "00" + wrong.Secret[2:]
			if wrong.Secret == m.presented.Secret {
				wrong.Secret = "ff" + wrong.Secret[2:]
			}
			return wrong.Value()
		}},
		{"revoked", func(m *machineRig) { m.row.RevokedAt = m.at.Add(-time.Minute) }, nil},
		{"expired", func(m *machineRig) { m.row.ExpiresAt = m.at.Add(-time.Second) }, nil},
		{"an owner signed out everywhere since", func(m *machineRig) {
			m.row.Owner.Epoch = 3
		}, nil},
		{"an owner suspended", func(m *machineRig) {
			m.row.Owner.Stage = iam.StageSuspended
		}, nil},
		{"no such token on a node that covers its mint", func(m *machineRig) {
			m.row = credential.TokenRow{Applied: 50}
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMachineRig(t)
			if tc.setup != nil {
				tc.setup(m)
			}
			value := m.presented.Value()
			if tc.value != nil {
				value = tc.value(m)
			}
			tr := newAuditTrail(t)
			got, _ := present(t, m.guard(tr), http.MethodGet, "/agents", value)
			if got.status != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401 (body %v)", got.status, got.body)
			}
			if got.how == iam.Resolved {
				t.Errorf("resolved as %v", got.principal)
			}
			if n := tr.failed(types.FailBearer); n != 1 {
				t.Errorf("counted %d bearer failures, want 1", n)
			}
		})
	}
}

// A TOKEN THIS NODE CANNOT CHECK IS 503, NEVER 401.
//
// A node that has not yet applied the mint the token names, one past the
// stall grace, or one that cannot read its directory at all: each is "this
// node cannot tell", and a 401 there would teach a pipeline that a credential
// is broken when the node is. Mutation: fold unknown into refused and every
// case answers 401.
func TestATokenThisNodeCannotCheckIsUnavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*machineRig)
	}{
		{"a node below the mint", func(m *machineRig) {
			m.row = credential.TokenRow{Applied: 39}
		}},
		{"a node past the stall grace", func(m *machineRig) {
			m.row.Lag = statelog.StallGrace + time.Second
		}},
		{"a directory that cannot be read", func(m *machineRig) {
			m.dirErr = errors.New("the replicated estate is not open")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMachineRig(t)
			tc.setup(m)
			tr := newAuditTrail(t)
			got, _ := present(t, m.guard(tr), http.MethodGet, "/agents",
				m.presented.Value())
			if got.status != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503 (body %v)", got.status, got.body)
			}
			if got.body["error"] != string(httpjson.CodeIdentityUnavailable) {
				t.Errorf("code %v, want %q", got.body["error"],
					httpjson.CodeIdentityUnavailable)
			}
			if n := tr.failed(types.FailBearer); n != 0 {
				t.Errorf("counted %d failures for a token nobody refused", n)
			}
		})
	}
}

// A TOKEN WHOSE OWNER IS BOUND TO A SEAT ACTS AS THAT SEAT, through the table
// every other credential's binding goes through.
//
// A person's own token is them, and a person bound to a seat writes AS the
// seat — so the token does too, with the kind following the seat and the
// chart's current handle. Mutation: skip the binding and it acts as a bare
// person nobody seated; take the row's handle raw and a rename is lost.
func TestABoundOwnersTokenActsAsTheirSeat(t *testing.T) {
	t.Parallel()
	m := newMachineRig(t)
	m.row.Owner.Seat, m.row.Owner.SeatAt = sessionSeat, 900
	m.chart.seats[sessionSeat] = session.Seat{
		Handle: "platform-director", Kind: "human", Unit: "platform",
	}
	got, _ := present(t, m.guard(nil), http.MethodGet, "/agents",
		m.presented.Value())
	if got.status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %v)", got.status, got.body)
	}
	p := got.principal
	if p.Kind != iam.KindPerson || p.Seat != "platform-director" {
		t.Errorf("acts as %s %q, want a person at the seat's current handle",
			p.Kind, p.Seat)
	}
	if p.Position != "platform" || p.SeatAt != 900 {
		t.Errorf("position %q seat-at %d, want platform at 900", p.Position, p.SeatAt)
	}

	// A SERVICE ACCOUNT bound to a seat is a machine acting as it, and the
	// kind follows the seat for Tier A's reason.
	svc := newMachineRig(t)
	svc.row.Owner.Kind, svc.row.Owner.Login = iam.KindMachine, "svc:deploy"
	svc.row.Owner.Seat, svc.row.Owner.SeatAt = sessionSeat, 900
	got, _ = present(t, svc.guard(nil), http.MethodGet, "/agents",
		svc.presented.Value())
	if got.principal.Kind != iam.KindPerson || got.principal.Seat != sessionSeat {
		t.Errorf("a bound service account acts as %s %q, want a person at %q",
			got.principal.Kind, got.principal.Seat, sessionSeat)
	}

	// AN UNBOUND SERVICE ACCOUNT is the machine it is.
	bare := newMachineRig(t)
	bare.row.Owner.Kind, bare.row.Owner.Login = iam.KindMachine, "svc:deploy"
	got, _ = present(t, bare.guard(nil), http.MethodGet, "/agents",
		bare.presented.Value())
	if got.principal.Kind != iam.KindMachine || got.principal.Seat != "" {
		t.Errorf("an unbound service account acts as %s %q, want the bare "+
			"machine", got.principal.Kind, got.principal.Seat)
	}
}

// A TOKEN WHOSE SEAT IS GONE IS REFUSED NAMING IT, and one whose seat this
// node cannot resolve is 503 — the two answers every bound credential gets.
func TestABoundOwnersTokenIsHeldToTheirSeatsStanding(t *testing.T) {
	t.Parallel()
	gone := newMachineRig(t)
	gone.row.Owner.Seat, gone.row.Owner.SeatAt = sessionSeat, 900
	delete(gone.chart.seats, sessionSeat)
	got, _ := present(t, gone.guard(nil), http.MethodGet, "/agents",
		gone.presented.Value())
	if got.status != http.StatusForbidden ||
		got.body["error"] != string(httpjson.CodeSeatUnavailable) {
		t.Errorf("a removed seat answered %d %v, want 403 %s", got.status,
			got.body["error"], httpjson.CodeSeatUnavailable)
	}

	behind := newMachineRig(t)
	behind.row.Owner.Seat, behind.row.Owner.SeatAt = sessionSeat, 900
	delete(behind.chart.seats, sessionSeat)
	behind.chart.position = 899
	got, _ = present(t, behind.guard(nil), http.MethodGet, "/agents",
		behind.presented.Value())
	if got.status != http.StatusServiceUnavailable {
		t.Errorf("a chart below the binding answered %d, want 503", got.status)
	}
}

// A NODE WITH NO TOKEN ARM REFUSES A TOKEN-SHAPED BEARER like any other it
// does not hold: the shape chooses the arm and never admits anything.
func TestATokenIsOnlyAsGoodAsTheArmThatChecksIt(t *testing.T) {
	t.Parallel()
	m := newMachineRig(t)
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	got, _ := present(t, auth.New(&b), http.MethodGet, "/agents",
		m.presented.Value())
	if got.status != http.StatusUnauthorized {
		t.Errorf("a node with no token arm answered %d, want 401", got.status)
	}
}

// THE ARM REFUSES TO BE BUILT HALF-WIRED, naming which half.
func TestTheTokenArmNeedsItsDirectoryAndItsChart(t *testing.T) {
	t.Parallel()
	m := newMachineRig(t)
	if _, err := auth.NewTokens(auth.TokensDeps{Chart: m.chart}); err == nil {
		t.Error("built with no directory")
	}
	if _, err := auth.NewTokens(auth.TokensDeps{Directory: m}); err == nil {
		t.Error("built with no chart")
	}
}
