package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
			Session: session.SessionRow{Found: true, Epoch: 3,
				ProvedAt: at.Add(-10 * time.Minute)},
			Person: session.PersonRow{
				Found: true, Epoch: 3, Stage: iam.StageActive,
				Login: "sarah.chen", Seat: sessionSeat, SeatAt: 900,
				Colleague: iam.ColleagueWrite,
				Grants:    []iam.Grant{iam.GrantStateRead, iam.GrantConfigRead},
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
		Signer: s.signer, Directory: s.dir, Applier: s.dir, Chart: s.chart,
		External: b.API.ExternalBase(),
		OnReuse:  s.ended.record,
		Audit:    newAuditTrail(s.t),
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

	// caughtUp is what this node's rows become once its applier reaches
	// the position a wait asked for, and nil for a node that never gets
	// there. waitedFor is every position a wait asked for.
	caughtUp  *session.Identity
	waitedFor []uint64
}

// AwaitApplied arrives when the rows a case caught this node up to cover the
// position, and never otherwise: a node that does not catch up answers as a
// wait that ran out.
func (d *fakeDirectory) AwaitApplied(_ context.Context, position uint64) error {
	d.waitedFor = append(d.waitedFor, position)
	if d.caughtUp == nil || d.caughtUp.Applied < position {
		return context.DeadlineExceeded
	}
	d.identity = *d.caughtUp
	return nil
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
	epochs  []uint64
}

func (e *endings) record(_ context.Context, person string, epoch uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.persons = append(e.persons, person)
	e.epochs = append(e.epochs, epoch)
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

// A SESSION'S PROOF COUNTS FOR THIS NODE'S STEP-UP WINDOW, AND NO PROOF IS STALE.
//
// The principal's ReauthAt is the instant a step-up surface stops accepting
// this session's proof: when the session proved who its holder is, plus this
// node's own `step_up` window. It was read off a person field nothing ever
// set, so every session was stale from its first request and no person could
// reach a step-up surface at all — enrolling a second factor included. A
// session that proved nothing — a Tier A token's exchanged cookie, or a row
// this node has not applied — stays stale, because a stale-but-set instant
// would one day make it fresh.
func TestASessionsProofCountsForTheStepUpWindow(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	got := rig.call(rig.guard(), http.MethodGet, "/agents", rig.withCookie)
	if got.how != iam.Resolved {
		t.Fatalf("resolution %v, want resolved", got.how)
	}
	proved := rig.dir.identity.Session.ProvedAt
	want := proved.Add(config.DefaultSessionStepUp)
	if !got.principal.ReauthAt.Equal(want) {
		t.Errorf("reauth deadline %s, want the proof at %s plus the %s window",
			got.principal.ReauthAt, proved, config.DefaultSessionStepUp)
	}
	if !got.principal.Fresh(rig.at) {
		t.Error("a session proved ten minutes ago is stale inside an hour's window")
	}
	// AND THE SENSITIVE WINDOW IS ITS OWN, composed from the same proof and
	// this node's `step_up_sensitive`: two deadlines, because a gesture asks
	// for one window or the other and one instant cannot answer both.
	sensitive := proved.Add(config.DefaultSessionStepUpSensitive)
	if !got.principal.SensitiveReauthAt.Equal(sensitive) {
		t.Errorf("sensitive deadline %s, want the proof at %s plus the %s window",
			got.principal.SensitiveReauthAt, proved,
			config.DefaultSessionStepUpSensitive)
	}
	if !got.principal.Proved(iam.RecencySensitive, rig.at) {
		t.Error("a session proved ten minutes ago is stale inside fifteen")
	}

	// A PROOF FORTY MINUTES OLD is inside the hour and outside the quarter:
	// the ordinary gestures go on and revealing a secret asks again.
	older := newSignedIn(t)
	older.dir.identity.Session.ProvedAt = older.at.Add(-40 * time.Minute)
	got = older.call(older.guard(), http.MethodGet, "/agents", older.withCookie)
	if !got.principal.Proved(iam.RecencyStepUp, older.at) ||
		got.principal.Proved(iam.RecencySensitive, older.at) {
		t.Errorf("a proof forty minutes old reads step_up=%v sensitive=%v, "+
			"want fresh and stale",
			got.principal.Proved(iam.RecencyStepUp, older.at),
			got.principal.Proved(iam.RecencySensitive, older.at))
	}

	unproved := newSignedIn(t)
	unproved.dir.identity.Session.ProvedAt = time.Time{}
	got = unproved.call(unproved.guard(), http.MethodGet, "/agents",
		unproved.withCookie)
	// THE ZERO DEADLINE, and not a window added to nothing: the two are
	// both stale, and only the first says "never proved" to a reader of
	// `reauth_at` rather than naming the year one.
	if got.how != iam.Resolved || !got.principal.ReauthAt.IsZero() ||
		!got.principal.SensitiveReauthAt.IsZero() {
		t.Errorf("a session that proved nothing resolved %v with reauth "+
			"deadlines %s and %s — want resolved with none", got.how,
			got.principal.ReauthAt, got.principal.SensitiveReauthAt)
	}
}

// A CREDENTIAL WITH NOBODY AT A KEYBOARD IS FRESH BY CONSTRUCTION, in BOTH
// windows.
//
// A Tier A token has no second factor, no session and no person behind it:
// there is nothing else it could ever present, so presenting it is the proof.
// Fresh only for the ordinary window, the break-glass credential could not
// reveal a secret or change who holds authority on the one day it exists for —
// the day the identity provider is down.
func TestATierATokenIsFreshInBothWindows(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	got := rig.call(rig.guard(), http.MethodGet, "/agents", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer a-tier-a-token")
	})
	if got.how != iam.Resolved {
		t.Fatalf("a presented Tier A token resolved %v", got.how)
	}
	for _, r := range []iam.Recency{iam.RecencyStepUp, iam.RecencySensitive} {
		if !got.principal.Proved(r, time.Now()) {
			t.Errorf("a Tier A token is stale for %s", r)
		}
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

// A PROVIDER'S GROUP GRANTS RIDE THE SESSION THAT PRESENTED THEM.
//
// What a person holds is (their declared grants ∪ what their session carries)
// ∩ this node's ceiling, at decision time. The OIDC callback used to merge the
// mapped grants into the sign-in's sighting and then drop them, so no group
// mapping ever conferred anything. The carried half is unioned, never a
// replacement for the declared one; it is clamped like everything else; and a
// session that carries none — any other sign-in — confers only the declared
// set.
func TestAProvidersGroupGrantsRideTheSessionThatPresentedThem(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	rig.dir.identity.Session.GroupGrants = []iam.Grant{
		iam.GrantWorkWrite, iam.GrantStateRead, iam.GrantSecretWrite,
	}
	got := rig.call(rig.guard(iam.GrantStateRead, iam.GrantConfigRead,
		iam.GrantWorkWrite), http.MethodGet, "/agents", rig.withCookie)
	if got.how != iam.Resolved {
		t.Fatalf("resolution %v, want resolved", got.how)
	}
	want := []iam.Grant{iam.GrantStateRead, iam.GrantConfigRead, iam.GrantWorkWrite}
	if !slices.Equal(got.principal.Grants, want) {
		t.Errorf("grants %v, want %v: the declared set, then what the "+
			"session carries, once each and clamped to the ceiling",
			got.principal.Grants, want)
	}

	plain := newSignedIn(t)
	got = plain.call(plain.guard(), http.MethodGet, "/agents", plain.withCookie)
	if got.how != iam.Resolved || got.principal.Can(iam.GrantWorkWrite) {
		t.Errorf("a session carrying nothing resolved %v with %v — want the "+
			"declared set alone", got.how, got.principal.Grants)
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

// A NODE THAT STAYS BEHIND SERVES READS AND REFUSES WRITES.
//
// The `behind` row of the session table, reached through the guard rather
// than asserted off the map: this node has not applied the record the bearer
// names, which is a read it can honestly serve from what it has and a write
// it must not accept — once the wait for the bearer's start has run out. A
// read never waits, because it is served on the signature and the epoch.
func TestANodeBehindTheSessionServesReadsAndRefusesWrites(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	rig.dir.identity.Applied = sessionStart - 1
	rig.dir.identity.Session.Found = false
	g := rig.guard()
	if got := rig.call(g, http.MethodGet, "/agents", rig.withCookie); got.status != http.StatusOK {
		t.Errorf("a read answered %d, want 200", got.status)
	}
	if len(rig.dir.waitedFor) != 0 {
		t.Errorf("a read waited for %v — it is served on the bearer's own "+
			"proof, and parking it would cost every tab a node's lag", rig.dir.waitedFor)
	}
	write := rig.call(g, http.MethodPost, "/work/items", rig.withCookie)
	if write.status != http.StatusServiceUnavailable {
		t.Errorf("a write answered %d, want 503", write.status)
	}
}

// A WRITE STRAIGHT AFTER SIGNING IN WAITS FOR THE SESSION'S OWN START.
//
// A sign-in answers before any node applies the session it opened — the one
// write in the estate that does not wait — and the bearer carries the position
// its start landed at, so whoever reads it next owes that position a wait.
// The request guard used to answer a write on a node below it 503 at once,
// which is every node for the few hundred milliseconds an apply takes, and
// every `crewlet iam token -login` run: it signs in and mints in the same
// breath. The write waits for exactly the bearer's start position and is then
// decided on the rows. Mutation: drop the wait from the session arm and the
// write answers 503.
func TestAWriteStraightAfterSigningInWaitsForItsSession(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	applied := rig.dir.identity
	rig.dir.identity.Applied = sessionStart - 1
	rig.dir.identity.Session = session.SessionRow{}
	rig.dir.caughtUp = &applied

	write := rig.call(rig.guard(), http.MethodPost, "/work/items", rig.withCookie)
	if write.status != http.StatusOK || write.how != iam.Resolved {
		t.Fatalf("a write presenting a session this node was about to apply "+
			"answered %d (%s), want it served once the node caught up",
			write.status, write.how)
	}
	if !slices.Equal(rig.dir.waitedFor, []uint64{sessionStart}) {
		t.Errorf("the guard waited for %v, want exactly the bearer's start "+
			"position %d", rig.dir.waitedFor, sessionStart)
	}
	if write.principal.Login != "sarah.chen" {
		t.Errorf("the write resolved as %q, want the person the session is "+
			"theirs", write.principal.Login)
	}
}

// THE ARM IS REFUSED WITHOUT AN APPLIER TO WAIT ON, because without one every
// write made straight after signing in is a 503 on every node.
func TestTheSessionArmNeedsAnApplier(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	_, err := auth.NewSessions(auth.SessionsDeps{
		Signer: rig.signer, Directory: rig.dir, Chart: rig.chart,
		Audit: newAuditTrail(t),
	})
	if err == nil || !strings.Contains(err.Error(), "applier") {
		t.Fatalf("a session arm with no applier answered %v, want a refusal "+
			"naming it", err)
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
