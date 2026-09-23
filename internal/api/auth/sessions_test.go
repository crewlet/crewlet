package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
)

// --- the rig ------------------------------------------------------------ //

const (
	sessionPerson = "018f3a9c-0000-7000-8000-0000000000aa"
	sessionStart  = 4096
	sessionSeat   = "platform-lead"
)

// signedIn is one person with a live cookie, and the two seams a guard
// resolves them through.
type signedIn struct {
	t      *testing.T
	at     time.Time
	signer *session.Signer
	cookie string
	dir    *fakeDirectory
	chart  *fakeChart
	ended  *endings
}

// sessionKeyring is the fleet keyring every signer in this file is built
// from, so a cookie one mints is one another can verify.
func sessionKeyring() runtoken.Material {
	return runtoken.Material{
		ActiveID: "k1",
		Keys:     []runtoken.KeyMaterial{{ID: "k1", Material: "the-active-key-material"}},
	}
}

func newSignedIn(t *testing.T) *signedIn {
	t.Helper()
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	signer, err := session.New(session.Options{
		Material: sessionKeyring(), RotateAfter: time.Hour,
		Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("build a signer: %v", err)
	}
	lineage := lineageAt(t, at)
	cookie, err := signer.Mint(session.Mint{
		Lineage: lineage, Person: sessionPerson, Epoch: 3, Generation: 1,
		StartPosition: sessionStart, AbsoluteExpiresAt: at.Add(8 * time.Hour),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return &signedIn{
		t: t, at: at, signer: signer, cookie: cookie,
		dir: &fakeDirectory{identity: session.Identity{
			Applied:    sessionStart,
			Generation: 1,
			Session:    session.SessionRow{Found: true, Epoch: 3},
			Person: session.PersonRow{
				Found: true, Epoch: 3, Stage: iam.StageActive,
				Login: "sarah.chen", Seat: sessionSeat, SeatAt: 900,
				Colleague: iam.ColleagueWrite,
				Grants:    []iam.Grant{iam.GrantStateRead, iam.GrantConfigRead},
				ReauthAt:  at.Add(10 * time.Minute),
			},
		}},
		chart: &fakeChart{position: 1000, seats: map[string]session.Seat{
			sessionSeat: {Handle: sessionSeat, Kind: "human", Unit: "platform"},
		}},
		ended: &endings{},
	}
}

// guardWithSession is a guard whose Tier A holds one token and whose session
// arm reads this rig.
func (s *signedIn) guard(ceiling ...iam.Grant) *auth.Guard {
	s.t.Helper()
	if len(ceiling) == 0 {
		ceiling = iam.AllGrants
	}
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{{ID: "ci", Token: "a-tier-a-token"}}
	b.API.Auth.MaxGrants = ceiling
	b.API.ExternalURL = "http://127.0.0.1:8080"
	arm, err := auth.NewSessions(auth.SessionsDeps{
		Signer: s.signer, Directory: s.dir, Chart: s.chart,
		External: b.API.ExternalBase(),
		OnReuse:  s.ended.record,
		Now:      func() time.Time { return s.at },
	})
	if err != nil {
		s.t.Fatalf("build the session arm: %v", err)
	}
	return auth.New(&b).WithSessions(arm)
}

// call runs one request through a guard, presenting whatever the mutator sets.
type answered struct {
	status    int
	body      map[string]any
	principal iam.Principal
	how       iam.Resolution
	cookies   []*http.Cookie
}

func (s *signedIn) call(g *auth.Guard, method, path string,
	prepare func(*http.Request)) answered {

	s.t.Helper()
	var out answered
	out.how = iam.Unknown
	handler := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out.principal, out.how = iam.From(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	if prepare != nil {
		prepare(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	result := rec.Result()
	defer result.Body.Close()
	out.status = result.StatusCode
	out.cookies = result.Cookies()
	_ = json.NewDecoder(result.Body).Decode(&out.body)
	return out
}

// withCookie presents this rig's bearer.
func (s *signedIn) withCookie(r *http.Request) {
	r.AddCookie(&http.Cookie{Name: session.CookieBaseName, Value: s.cookie})
}

// lineageAt mints a uuid7 whose embedded instant is at, by hand: the library
// reads the clock, and a rig that needs a session to have started at a named
// moment cannot wait for it.
func lineageAt(t *testing.T, at time.Time) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint a lineage: %v", err)
	}
	millis := at.UnixMilli()
	for i := range 6 {
		id[i] = byte(millis >> (8 * (5 - i)))
	}
	return id
}

type fakeDirectory struct {
	identity session.Identity
	err      error
}

func (d *fakeDirectory) Resolve(context.Context, string, string) (
	session.Identity, error) {

	if d.err != nil {
		return session.Identity{}, d.err
	}
	return d.identity, nil
}

type fakeChart struct {
	position uint64
	lag      time.Duration
	seats    map[string]session.Seat
	err      error
}

func (c *fakeChart) Seat(_ context.Context, ref string) (session.Seat, bool, error) {
	if c.err != nil {
		return session.Seat{}, false, c.err
	}
	seat, found := c.seats[ref]
	return seat, found, nil
}

func (c *fakeChart) Position(context.Context) (uint64, time.Duration, error) {
	if c.err != nil {
		return 0, 0, c.err
	}
	return c.position, c.lag, nil
}

type endings struct {
	mu      sync.Mutex
	persons []string
}

func (e *endings) record(_ context.Context, person string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.persons = append(e.persons, person)
}

func (e *endings) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.persons...)
}

// --- what a cookie buys ------------------------------------------------- //

// A COOKIE IS A CREDENTIAL, which until this arm existed it was not: a person
// could sign in, receive a bearer, and be refused by every guarded route on
// the deployment that minted it.
func TestASignedInPersonIsResolvedFromTheirCookie(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	got := rig.call(rig.guard(), http.MethodGet, "/agents", rig.withCookie)
	if got.status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %v)", got.status, got.body)
	}
	if got.how != iam.Resolved {
		t.Fatalf("resolution %v, want resolved", got.how)
	}
	if got.principal.Login != "sarah.chen" {
		t.Errorf("login %q, want sarah.chen", got.principal.Login)
	}
	if got.principal.Kind != iam.KindPerson {
		t.Errorf("kind %q, want person", got.principal.Kind)
	}
	// THE SEAT COMES FROM THE CHART, which is what lets a lead gate
	// resolve at all: a principal with no handle leads nobody.
	if got.principal.Seat != sessionSeat {
		t.Errorf("seat %q, want %q", got.principal.Seat, sessionSeat)
	}
	if got.principal.Position != "platform" {
		t.Errorf("unit %q, want platform", got.principal.Position)
	}
	if !got.principal.Can(iam.GrantConfigRead) {
		t.Errorf("grants %v do not carry the row's own", got.principal.Grants)
	}
}

// THE CEILING APPLIES TO A PERSON EXACTLY AS IT DOES TO A TOKEN.
//
// `api.auth.max_grants` is a decision-time bound, so lowering it on one node
// takes effect on that node's next request. Without this a person's row would
// be the only credential shape the ceiling did not reach — and the row is the
// one an administrator edits, which is precisely what the ceiling is there to
// bound.
func TestANodesCeilingClampsASignedInPersonsGrants(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	got := rig.call(rig.guard(iam.GrantStateRead), http.MethodGet, "/agents",
		rig.withCookie)
	if got.how != iam.Resolved {
		t.Fatalf("resolution %v, want resolved", got.how)
	}
	if got.principal.Can(iam.GrantConfigRead) {
		t.Errorf("grants %v survived a ceiling that withholds config:read",
			got.principal.Grants)
	}
	if !got.principal.Can(iam.GrantStateRead) {
		t.Errorf("grants %v lost what the ceiling permits", got.principal.Grants)
	}
}

// AN EXPLICIT CREDENTIAL WINS OVER AN AMBIENT ONE.
//
// A browser sends its cookie on every request whether or not the caller meant
// to; a header is only there because somebody put it there. The control is the
// second arm: a WRONG header stays anonymous rather than being quietly
// upgraded by the cookie in the jar, or `curl -H 'Authorization: Bearer
// typo'` from a signed-in browser would act as the person.
func TestAnExplicitBearerWinsOverAnAmbientCookie(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	g := rig.guard()
	got := rig.call(g, http.MethodGet, "/agents", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer a-tier-a-token")
		rig.withCookie(r)
	})
	if got.how != iam.Resolved || got.principal.Login != iam.TokenLogin("ci") {
		t.Errorf("resolved as %q/%v, want the Tier A token",
			got.principal.Login, got.how)
	}
	wrong := rig.call(g, http.MethodGet, "/agents", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer not-a-token")
		rig.withCookie(r)
	})
	if wrong.status != http.StatusUnauthorized {
		t.Errorf("a wrong header beside a good cookie answered %d, want 401",
			wrong.status)
	}
}

// 503 AND NEVER 401 ON A NODE THAT CANNOT TELL.
//
// A browser reads 401 as "sign in again" and discards the cookie, so one
// stalled applier answering 401 signs everybody on that node out and
// stampedes the identity provider. The status is the whole assertion.
func TestANodeThatCannotReadTheEstateAnswers503AndKeepsTheCookie(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	rig.dir.err = context.DeadlineExceeded
	got := rig.call(rig.guard(), http.MethodGet, "/agents", rig.withCookie)
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", got.status)
	}
	if got.body["error"] != string(httpjson.CodeIdentityUnavailable) {
		t.Errorf("code %v, want %q", got.body["error"],
			httpjson.CodeIdentityUnavailable)
	}
	for _, c := range got.cookies {
		if c.MaxAge < 0 {
			t.Errorf("the cookie was cleared on an outage, so the browser has "+
				"to sign in again once the node recovers: %v", c)
		}
	}
}

// A NODE THAT IS MERELY BEHIND SERVES READS AND REFUSES WRITES.
//
// The `behind` row of the session table, reached through the guard rather
// than asserted off the map: this node has not applied the record the bearer
// names, which is a read it can honestly serve from what it has and a write
// it must not accept.
func TestANodeBehindTheSessionServesReadsAndRefusesWrites(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	rig.dir.identity.Applied = sessionStart - 1
	rig.dir.identity.Session.Found = false
	g := rig.guard()
	if got := rig.call(g, http.MethodGet, "/agents", rig.withCookie); got.status != http.StatusOK {
		t.Errorf("a read answered %d, want 200", got.status)
	}
	write := rig.call(g, http.MethodPost, "/work/items", rig.withCookie)
	if write.status != http.StatusServiceUnavailable {
		t.Errorf("a write answered %d, want 503", write.status)
	}
}

// AN ENDED SESSION IS 401 AND THE COOKIE IS CLEARED, so a browser stops
// re-presenting a value that can never work again.
func TestAnEndedSessionIsRefusedAndTheCookieCleared(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	rig.dir.identity.Session.Ended = true
	got := rig.call(rig.guard(), http.MethodGet, "/agents", rig.withCookie)
	if got.status != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", got.status)
	}
	var cleared bool
	for _, c := range got.cookies {
		if c.Name == session.CookieBaseName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("the cookie was not cleared: %v", got.cookies)
	}
}

// A PERSON WHOSE SEAT IS GONE IS 403 NAMING THE SEAT, NOT 401.
//
// They are exactly who they say they are and signing in again changes
// nothing, so 401 would loop a browser through the sign-in page for ever. The
// detail names the seat, because the person locked out and whoever removed it
// both need to know which one.
func TestAPersonWhoseSeatIsGoneIsRefusedNamingIt(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	delete(rig.chart.seats, sessionSeat)
	got := rig.call(rig.guard(), http.MethodGet, "/agents", rig.withCookie)
	if got.status != http.StatusForbidden {
		t.Fatalf("status %d, want 403 (body %v)", got.status, got.body)
	}
	if got.body["error"] != string(httpjson.CodeSeatUnavailable) {
		t.Errorf("code %v, want %q", got.body["error"], httpjson.CodeSeatUnavailable)
	}
	detail, _ := got.body["detail"].(string)
	if !contains(detail, sessionSeat) {
		t.Errorf("detail %q does not name the seat", detail)
	}
	// THE COOKIE SURVIVES, because the bearer is live and works the
	// moment somebody rebinds them — clearing it would sign out a person
	// whose only problem is a chart edit.
	for _, c := range got.cookies {
		if c.Name == session.CookieBaseName && c.MaxAge < 0 {
			t.Errorf("the cookie was cleared by a seat refusal: %v", c)
		}
	}
	// AND THEY ARE STILL THE PERSON, with an EMPTY handle beside a
	// non-zero binding position — the pair that says "a binding was
	// decided and this node will not honour it". Read on the one surface
	// the refusal does not cover, since everywhere else the handler
	// never runs.
	own := rig.call(rig.guard(), http.MethodPost, "/auth/session", rig.withCookie)
	if own.how != iam.Resolved {
		t.Fatalf("resolution %v on /auth/session, want resolved", own.how)
	}
	if own.principal.Seat != "" {
		t.Errorf("seat %q, want empty: a handle the chart could not confirm "+
			"is the silent fall-through this refusal exists to prevent",
			own.principal.Seat)
	}
	if own.principal.SeatAt == 0 {
		t.Errorf("SeatAt 0 beside an empty seat reads as a genuinely " +
			"seatless person, which this one is not")
	}
	if own.principal.Login != "sarah.chen" {
		t.Errorf("login %q, want sarah.chen", own.principal.Login)
	}
}

// A NODE BEHIND ON THE CHART IS 503, NOT 403.
//
// The control for the case above: absent from the view is the SAME
// observation whether the seat was removed or the hire has not arrived, and
// only the position tells them apart. Collapsed, a fleet mid-apply refuses
// everybody who was just hired.
func TestANodeBehindOnTheChartIsUnavailableRatherThanForbidden(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	delete(rig.chart.seats, sessionSeat)
	rig.chart.position = 899 // below the binding's 900
	got := rig.call(rig.guard(), http.MethodGet, "/agents", rig.withCookie)
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 (body %v)", got.status, got.body)
	}
}

// A CHART APPLIER PAST THE STALL GRACE IS UNAVAILABLE, which is only
// reachable because the engine now measures a DURATION lag and hands it to
// the reader: with the lag hardcoded at zero this arm could never fire, and a
// node an hour behind answered as a caught-up one.
func TestAChartPastTheStallGraceIsUnavailable(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	rig.chart.lag = statelog.StallGrace + time.Second
	got := rig.call(rig.guard(), http.MethodGet, "/agents", rig.withCookie)
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 (body %v)", got.status, got.body)
	}
}

// SIGNING OUT STILL WORKS WHEN THE SEAT IS GONE.
//
// The refusal above is written only for a GUARDED route. `/auth/logout` is
// how somebody ends the session they are holding, and refusing it would
// leave a leaver with a live bearer and no way to end it.
func TestASeatRefusalDoesNotReachTheSignOutRoute(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	delete(rig.chart.seats, sessionSeat)
	g := rig.guard()
	for _, path := range []string{"/auth/logout", "/auth/logout-all",
		"/auth/session", "/auth/step-up", "/auth/totp/enrol"} {

		got := rig.call(g, http.MethodPost, path, rig.withCookie)
		if got.status != http.StatusOK {
			t.Errorf("%s answered %d, want 200 (body %v)",
				path, got.status, got.body)
		}
	}
	// THE CONTROL. Every other guarded route still refuses, so the
	// exemption is the /auth surface rather than the refusal going away.
	if got := rig.call(g, http.MethodGet, "/agents", rig.withCookie); //
	got.status != http.StatusForbidden {
		t.Errorf("an ordinary route answered %d, want 403", got.status)
	}
}

// A REPLAYED COOKIE ENDS EVERY SESSION OF THAT PERSON.
//
// The one thing nobody can establish from a replay is which of the two
// holders is the person, so the epoch is bumped rather than the lineage
// ended: ending only the lineage would leave whoever captured it holding
// whatever they rotate to next.
func TestAReplayedCookieAsksForThePersonsEpochToBeBumped(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	// A BEARER FROM THE FUTURE. Its rotation index is derived from the
	// MINTING clock, so one signed four windows ahead of the validating
	// node's clock carries an index this fleet could not have issued — the
	// one positive evidence of theft rotate.go recognises, since a lagging
	// index is an idle session and a captured cookie in identical bytes.
	lineage := lineageAt(t, rig.at)
	future, err := session.New(session.Options{
		Material:    sessionKeyring(),
		RotateAfter: time.Hour,
		Now:         func() time.Time { return rig.at.Add(4 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("build a future-clocked signer: %v", err)
	}
	ahead, err := future.Mint(session.Mint{
		Lineage:           lineage,
		Person:            sessionPerson,
		Epoch:             3,
		Generation:        1,
		StartPosition:     sessionStart,
		AbsoluteExpiresAt: rig.at.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	rig.cookie = ahead
	got := rig.call(rig.guard(), http.MethodGet, "/agents", rig.withCookie)
	if got.status != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 (body %v)", got.status, got.body)
	}
	if ended := rig.ended.all(); len(ended) != 1 || ended[0] != sessionPerson {
		t.Errorf("ended %v, want exactly %q", ended, sessionPerson)
	}
}

// THE WIRE VALUE IS internal/iam/session's OWN.
//
// internal/api/httpjson is a leaf and cannot import that package to share the
// constant, so the two are held equal here — the same idiom
// internal/clientsource uses for a value the dashboard declares separately.
// Drift is silent: a browser would receive a code its own table has no
// sentence for.
func TestTheSeatCodeIsTheSessionPackagesOwn(t *testing.T) {
	t.Parallel()
	if string(httpjson.CodeSeatUnavailable) != session.CodeNoSeat {
		t.Errorf("httpjson says %q and session says %q",
			httpjson.CodeSeatUnavailable, session.CodeNoSeat)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
