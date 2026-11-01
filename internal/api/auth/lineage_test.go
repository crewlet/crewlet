package auth_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// WHO A CREDENTIAL'S NAME STANDS FOR is what a request through the credential
// would be, decided from the rows alone.
//
// A view ticket carries the name of the credential it was minted through, and
// the artifact origin asks this on every document request. These cases hold
// the resolver to two things: the principal a live name resolves to is the
// one the guard composes for the credential itself, field for field; and
// every way a credential ends, or a node cannot tell, reaches the caller as
// anonymous or unknown, never confused.

// standings is a fake [auth.SessionStandings]. A case states its fields
// before it asks; the lock is for the requests an assistant case sends at once.
type standings struct {
	mu    sync.Mutex
	owner string
	live  bool
	err   error
	asked []string
}

func (s *standings) SessionStanding(_ context.Context, lineage string, _ time.Time) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, lineage)
	return s.owner, s.live, s.err
}

func lineages(t *testing.T, g *auth.Guard, s auth.SessionStandings) *auth.Lineages {
	t.Helper()
	l, err := auth.NewLineages(g, s)
	if err != nil {
		t.Fatalf("NewLineages: %v", err)
	}
	return l
}

// samePrincipal fails unless the two principals are the one composition.
func samePrincipal(t *testing.T, got, want iam.Principal) {
	t.Helper()
	if got.ID != want.ID || got.Login != want.Login || got.Kind != want.Kind ||
		got.Seat != want.Seat || got.Position != want.Position || got.Stage != want.Stage ||
		got.Via != want.Via || !got.ReauthAt.Equal(want.ReauthAt) ||
		!slices.Equal(got.Grants, want.Grants) {
		t.Fatalf("the name resolved to %+v, and the credential itself to %+v", got, want)
	}
}

// TestASessionsNameStandsForWhatItsCookieDoes: the control, and every way a
// session's rows end it. Mutation: compose from the bearer-less row without
// the seat binding and the principal acts as nobody's seat; skip the
// standing's liveness and a session the rows no longer hold live keeps the
// frame.
func TestASessionsNameStandsForWhatItsCookieDoes(t *testing.T) {
	t.Parallel()
	s := newSignedIn(t)
	g := s.guard()
	byCookie := s.call(g, http.MethodGet, "/agents", s.withCookie)
	if byCookie.how != iam.Resolved {
		t.Fatalf("the cookie resolved %v", byCookie.how)
	}
	name := iam.SessionName(s.lineage.String())
	st := &standings{owner: sessionPerson, live: true}
	got, how := lineages(t, g, st).Principal(t.Context(), name)
	if how != iam.Resolved {
		t.Fatalf("a live session's name resolved %v", how)
	}
	samePrincipal(t, got, byCookie.principal)
	if len(st.asked) != 1 || st.asked[0] != s.lineage.String() {
		t.Errorf("the standing was asked about %v, want the lineage alone", st.asked)
	}

	for _, tc := range []struct {
		name  string
		world func(*signedIn, *standings)
		want  iam.Resolution
	}{
		{"the rows say the session is no longer live", func(_ *signedIn, st *standings) {
			st.live = false
		}, iam.Anonymous},
		{"the standing could not be read", func(_ *signedIn, st *standings) {
			st.err = errors.New("the identity log's end could not be read")
		}, iam.Unknown},
		{"the person signed out everywhere since", func(s *signedIn, _ *standings) {
			s.dir.identity.Person.Epoch = 4
		}, iam.Anonymous},
		{"the person is suspended", func(s *signedIn, _ *standings) {
			s.dir.identity.Person.Stage = iam.StageSuspended
		}, iam.Anonymous},
		{"the directory could not be read", func(s *signedIn, _ *standings) {
			s.dir.err = errors.New("the estate is not open")
		}, iam.Unknown},
		{"the session may only enrol a second factor", func(s *signedIn, _ *standings) {
			s.dir.identity.Session.EnrolmentOnly = true
		}, iam.Anonymous},
		{"the person's seat is gone from the chart", func(s *signedIn, _ *standings) {
			s.chart.seats = nil
		}, iam.Anonymous},
		{"the chart could not be read", func(s *signedIn, _ *standings) {
			s.chart.err = errors.New("no company yet")
		}, iam.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSignedIn(t)
			st := &standings{owner: sessionPerson, live: true}
			tc.world(s, st)
			if _, how := lineages(t, s.guard(), st).Principal(t.Context(), name); how != tc.want {
				t.Errorf("resolved %v, want %v", how, tc.want)
			}
		})
	}
}

// TestATokensExchangedSessionStandsForTheToken: a session a Tier A token was
// exchanged for names the token's login as its holder, and stands for the
// token's entry, through the session.
func TestATokensExchangedSessionStandsForTheToken(t *testing.T) {
	t.Parallel()
	s := newSignedIn(t)
	g := s.guard()
	name := iam.SessionName(s.lineage.String())
	got, how := lineages(t, g, &standings{owner: iam.TokenLogin("ci"), live: true}).
		Principal(t.Context(), name)
	if how != iam.Resolved || got.Login != iam.TokenLogin("ci") || got.Kind != iam.KindMachine ||
		got.Via != name {
		t.Fatalf("an exchanged session resolved %v to %+v, want the ci token through %s", how, got, name)
	}
	if _, how := lineages(t, g, &standings{owner: iam.TokenLogin("gone"), live: true}).
		Principal(t.Context(), name); how != iam.Anonymous {
		t.Errorf("a session exchanged from an entry this node no longer holds resolved %v", how)
	}
}

// TestAMachineTokensNameStandsForWhatTheTokenDoes: the token's own principal,
// and the rows that end it.
func TestAMachineTokensNameStandsForWhatTheTokenDoes(t *testing.T) {
	t.Parallel()
	m := newMachineRig(t)
	g := m.guard(nil)
	byBearer, _ := present(t, g, http.MethodGet, "/agents", m.presented.Value())
	if byBearer.how != iam.Resolved {
		t.Fatalf("the token resolved %v", byBearer.how)
	}
	name := iam.MachineTokenName(m.presented.ID)
	got, how := lineages(t, g, &standings{}).Principal(t.Context(), name)
	if how != iam.Resolved {
		t.Fatalf("a live token's name resolved %v", how)
	}
	samePrincipal(t, got, byBearer.principal)
	// A NAME CARRIES NO SECRET, so the directory is handed the id alone —
	// and the rows decide it by the table that reads no verifier
	// (credential.CheckHeld), which is why it still resolved.
	if handed := m.lastHanded(); handed.ID != m.presented.ID || handed.Secret != "" ||
		handed.Position != 0 {
		t.Errorf("a token's name handed the directory id %q at %d with a secret "+
			"(%v), want the id alone", handed.ID, handed.Position, handed.Secret != "")
	}

	for _, tc := range []struct {
		name  string
		world func(*machineRig)
		want  iam.Resolution
	}{
		{"the token was revoked", func(m *machineRig) { m.row.RevokedAt = m.at }, iam.Anonymous},
		{"the owner is suspended", func(m *machineRig) { m.row.Owner.Stage = iam.StageSuspended }, iam.Anonymous},
		{"the directory could not be read", func(m *machineRig) { m.dirErr = errors.New("down") }, iam.Unknown},
		{"this node holds no row for it", func(m *machineRig) { m.row.Found = false }, iam.Unknown},
		{"the owner's seat cannot be resolved", func(m *machineRig) {
			m.row.Owner.Seat = sessionSeat
			m.chart.err = errors.New("no company yet")
		}, iam.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMachineRig(t)
			tc.world(m)
			if _, how := lineages(t, m.guard(nil), &standings{}).Principal(t.Context(), name); how != tc.want {
				t.Errorf("resolved %v, want %v", how, tc.want)
			}
		})
	}
}

// TestATierATokensLoginStandsForItsEntry, and a name this build cannot resolve
// stands for nobody.
func TestATierATokensLoginStandsForItsEntry(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{{ID: "ci", Token: "a-tier-a-token",
		Grants: []iam.Grant{iam.GrantWorklet}}}
	b.API.Auth.Admin = iam.AdminServe
	g := auth.New(&b)
	l := lineages(t, g, &standings{})
	got, how := l.Principal(t.Context(), iam.TokenLogin("ci"))
	if how != iam.Resolved || got.Login != iam.TokenLogin("ci") ||
		!slices.Equal(got.Grants, []iam.Grant{iam.GrantWorklet}) || got.Via != "" {
		t.Fatalf("the ci token's login resolved %v to %+v", how, got)
	}
	for _, name := range []string{
		iam.TokenLogin("gone"), "session:not-a-uuid", "pat:not-a-uuid", "dev:laptop",
		"cookie:abc", "",
	} {
		if _, how := l.Principal(t.Context(), name); how != iam.Anonymous {
			t.Errorf("%q resolved %v, want anonymous", name, how)
		}
	}
	if _, err := auth.NewLineages(nil, &standings{}); err == nil {
		t.Error("a resolver was built with no guard")
	}
	if _, err := auth.NewLineages(g, nil); err == nil {
		t.Error("a resolver was built with no way to read whose a session is")
	}
}

// TestACredentialsNameIsTheOneLineagesResolves: a session and a machine token
// by their names, a Tier A token and the development principal by their
// logins, and nothing for a principal acting through no credential.
func TestACredentialsNameIsTheOneLineagesResolves(t *testing.T) {
	t.Parallel()
	session := iam.SessionName("0192f00d-0000-7000-8000-0000000000c1")
	pat := iam.MachineTokenName("0192f00d-0000-7000-8000-0000000000c2")
	for _, tc := range []struct {
		p    iam.Principal
		want string
	}{
		{iam.Principal{Kind: iam.KindPerson, Login: "rita.reviewer", Seat: "reviewer", Via: session}, session},
		{iam.Principal{Kind: iam.KindMachine, Login: "svc:ci", Via: pat}, pat},
		{iam.Principal{Kind: iam.KindMachine, Login: iam.TokenLogin("ops")}, iam.TokenLogin("ops")},
		{iam.Principal{Kind: iam.KindPerson, Login: iam.TokenLogin("ops"), Seat: "reviewer"}, iam.TokenLogin("ops")},
		{iam.Principal{Kind: iam.KindMachine, Login: auth.DevPrincipalLoginPrefix + "laptop"},
			auth.DevPrincipalLoginPrefix + "laptop"},
	} {
		if got, err := auth.LineageOf(tc.p); err != nil || got != tc.want {
			t.Errorf("LineageOf(%+v) = %q, %v; want %q", tc.p, got, err, tc.want)
		}
	}
	for _, p := range []iam.Principal{
		{Kind: iam.KindSeat, Seat: "writer-agent"},
		{Kind: iam.KindPerson, Login: "rita.reviewer", Seat: "reviewer"},
		{Kind: iam.KindPerson, Login: "rita.reviewer", Via: "cookie:abc"},
		{Kind: iam.KindMachine, Login: "svc:ci"},
		{Kind: iam.KindEngine, Login: "node-1"},
	} {
		if _, err := auth.LineageOf(p); !errors.Is(err, auth.ErrNoLineage) {
			t.Errorf("LineageOf(%+v) = %v, want ErrNoLineage", p, err)
		}
	}
}

// A SESSION'S NAME ON A NODE BEHIND ITS ROWS NEVER SERVES A WRITE: the cookie
// on that node is served reads and answered 503 for a write (the `behind` row),
// and a grant, which presents the session's name rather than its bearer, is
// judged at its own request's need (Lineages.PrincipalFor with needOf) and,
// holding no start position to wait for, is refused at every need rather than
// served. So a write through a grant is never admitted where the person's own
// write would not be. Mutation: let a name with no row read as behind and the
// write is served.
func TestASessionsNameOnANodeBehindItNeverServesAWrite(t *testing.T) {
	t.Parallel()
	for _, need := range []session.Need{session.NeedRead, session.NeedWrite} {
		s := newSignedIn(t)
		name := iam.SessionName(s.lineage.String())
		s.dir.identity.Applied = sessionStart - 1
		s.dir.identity.Session.Found = false
		st := &standings{owner: sessionPerson, live: true}
		if _, how := lineages(t, s.guard(), st).PrincipalFor(t.Context(), name, need); how == iam.Resolved {
			t.Errorf("a session's name on a node behind its row resolved at need %v", need)
		}
	}
	s := newSignedIn(t)
	st := &standings{owner: sessionPerson, live: true}
	if _, how := lineages(t, s.guard(), st).PrincipalFor(t.Context(),
		iam.SessionName(s.lineage.String()), session.NeedWrite); how != iam.Resolved {
		t.Errorf("a live session's name resolved %v for a write, want it served", how)
	}
}
