package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TIER A TOKEN EXCHANGED FOR A SESSION IS A SESSION THAT WORKS — as the
// token, re-read from the configuration on every request.
//
// It used to be minted for the token's DERIVED principal id, which no
// directory row holds: once the start record applied, every request answered
// 401 and cleared the cookie, and before that it served a grantless nobody. A
// token the directory binds to a seat was refused the exchange outright. These
// cases run the exchange route and the guard together, over one identity
// estate that remembers the sessions the route opened, because each half was
// right on its own and the bug was the seam.

const (
	opsValue   = "a-tier-a-token-for-the-ops-team-01"
	boundValue = "a-tier-a-token-bound-to-a-seat-0002"
	boundSeat  = "platform-lead"
)

// exchangeRig is the sign-in surface and the guard over one estate.
type exchangeRig struct {
	t       *testing.T
	boot    config.Bootstrap
	estate  *sessionEstate
	chart   *seatChart
	handler http.Handler
}

// tokenBoot is a Tier A holding two tokens, one of which the identity
// directory binds to a seat, under a ceiling that withholds one grant the
// first token declares.
func tokenBoot(t *testing.T) config.Bootstrap {
	t.Helper()
	b := bootstrapFor(t)
	b.API.Auth.MaxGrants = slices.DeleteFunc(slices.Clone(iam.AllGrants),
		func(g iam.Grant) bool { return g == iam.GrantSandboxRun })
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: opsValue, Grants: []iam.Grant{
			iam.GrantStateRead, iam.GrantPeopleManage, iam.GrantSandboxRun}},
		{ID: "lead", Token: boundValue, Grants: []iam.Grant{iam.GrantWorkWrite},
			Colleague: iam.ColleagueWrite},
	}
	return b
}

func newExchangeRig(t *testing.T) *exchangeRig {
	t.Helper()
	r := &exchangeRig{
		t: t, boot: tokenBoot(t),
		estate: newSessionEstate(),
		chart: &seatChart{seats: map[string]session.Seat{
			boundSeat: {Handle: boundSeat, Kind: session.SeatKindHuman,
				Unit: "platform"},
		}},
	}
	r.estate.bindings[iam.TokenLogin("lead")] = session.PersonRow{
		Found: true, Stage: iam.StageActive, Login: iam.TokenLogin("lead"),
		Seat: boundSeat, SeatAt: 1,
	}
	r.rebuild(r.boot)
	return r
}

// rebuild stands the guard and the surface up over a Tier A, keeping the
// estate — which is what a node restarted on an edited config file is.
func (r *exchangeRig) rebuild(b config.Bootstrap) {
	r.t.Helper()
	r.boot = b
	audit := &recordingAudit{}
	arm, err := auth.NewSessions(auth.SessionsDeps{
		Signer: fixtureSigner(r.t), Directory: r.estate, Applier: r.estate,
		Chart: r.chart, External: b.API.ExternalBase(), Audit: audit,
		Now: func() time.Time { return clock },
	})
	if err != nil {
		r.t.Fatalf("NewSessions: %v", err)
	}
	guard := auth.New(&b).BindSeats(auth.SeatBindings{
		Directory: r.estate, Chart: r.chart,
	}).WithSessions(arm).WithAudit(audit)
	mux := http.NewServeMux()
	buildWith(r.t, b, nil, func(o *authapi.Options) {
		o.Writer, o.Sessions = r.estate, r.estate
	}).Routes(mux)
	mux.HandleFunc("GET /probe", func(w http.ResponseWriter, req *http.Request) {
		p, how := iam.From(req.Context())
		if how != iam.Resolved {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"login": p.Login, "kind": p.Kind, "seat": p.Seat,
			"grants": p.Grants, "colleague": p.Colleague,
			"fresh": p.Fresh(time.Now()),
		})
	})
	r.handler = guard.Middleware(mux)
}

// exchange posts a bearer to the exchange route and answers the cookie.
func (r *exchangeRig) exchange(value string) (*http.Cookie, int) {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
	req.Header.Set("Authorization", "Bearer "+value)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Value != "" {
			return c, rec.Code
		}
	}
	return nil, rec.Code
}

// probe presents a cookie and answers who the guard said it is.
func (r *exchangeRig) probe(cookie *http.Cookie) (map[string]any, *httptest.ResponseRecorder) {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out, rec
}

func TestAnExchangedTierATokenIsASessionThatWorks(t *testing.T) {
	t.Parallel()
	r := newExchangeRig(t)
	cookie, status := r.exchange(opsValue)
	if status != http.StatusOK || cookie == nil {
		t.Fatalf("the exchange answered %d with cookie %v", status, cookie)
	}
	who, rec := r.probe(cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("the exchanged session answered %d (%s) — it validated "+
			"against a directory row the token does not have", rec.Code,
			rec.Body.String())
	}
	if who["login"] != iam.TokenLogin("ops") || who["kind"] != string(iam.KindMachine) {
		t.Errorf("the session acts as %v/%v, want the token itself",
			who["login"], who["kind"])
	}
	// THE ENTRY'S GRANTS CUT TO THE CEILING, on this request: the token
	// declares sandbox:run and this node's ceiling withholds it.
	got, _ := json.Marshal(who["grants"])
	if string(got) != `["state:read","people:manage"]` {
		t.Errorf("the session carries grants %s, want the entry's cut to "+
			"the ceiling", got)
	}
	// STEPPED UP BY CONSTRUCTION, as the bearer is.
	if who["fresh"] != true {
		t.Error("the exchanged session is not fresh, so break-glass through " +
			"a browser could reach no sensitive surface")
	}
}

// REMOVING THE ENTRY ENDS THE SESSION, on the next request, with the cookie
// cleared — the token's own revocation path, which the session has to share.
func TestATokenRemovedFromTheConfigEndsItsSession(t *testing.T) {
	t.Parallel()
	r := newExchangeRig(t)
	cookie, status := r.exchange(opsValue)
	if status != http.StatusOK || cookie == nil {
		t.Fatalf("the exchange answered %d", status)
	}
	withoutOps := r.boot
	withoutOps.API.Auth.Tokens = slices.DeleteFunc(
		slices.Clone(r.boot.API.Auth.Tokens),
		func(tok config.APIToken) bool { return tok.ID == "ops" })
	r.rebuild(withoutOps)

	// A NODE THAT HAS NOT APPLIED THE START RECORD refuses it too: the
	// absent-row grace is for a record not yet applied, and a
	// configuration entry is never late.
	r.estate.setApplied(0)
	_, rec := r.probe(cookie)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a session exchanged from a token no longer in the config "+
			"answered %d, want 401", rec.Code)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the ended session's cookie was not cleared, so the browser " +
			"re-presents a value that can never work again")
	}
}

// A BOUND TOKEN'S EXCHANGE ACTS AS ITS SEAT, as its bearer does.
func TestABoundTokensExchangeActsAsItsSeat(t *testing.T) {
	t.Parallel()
	r := newExchangeRig(t)
	cookie, status := r.exchange(boundValue)
	if status != http.StatusOK || cookie == nil {
		t.Fatalf("a token the directory binds to a seat was refused the "+
			"exchange (%d)", status)
	}
	who, rec := r.probe(cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("the bound token's session answered %d: %s", rec.Code,
			rec.Body.String())
	}
	if who["seat"] != boundSeat || who["kind"] != string(iam.KindPerson) {
		t.Errorf("the bound token's session acts as %v (%v), want seat %s as "+
			"a person", who["seat"], who["kind"], boundSeat)
	}
}

// ONLY A PRESENTED TOKEN IS EXCHANGED: a session has no value to exchange.
func TestASessionIsNotExchangedForAnother(t *testing.T) {
	t.Parallel()
	r := newExchangeRig(t)
	cookie, _ := r.exchange(opsValue)
	req := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a session presented to the exchange answered %d, want 400",
			rec.Code)
	}
}

// SIGNING OUT EVERYWHERE FROM A TOKEN'S SESSION ENDS EVERY SESSION THAT TOKEN
// OPENED — its subject is the token's login, so that is where the epoch moves.
func TestSigningOutEverywhereEndsATokensSessions(t *testing.T) {
	t.Parallel()
	r := newExchangeRig(t)
	first, _ := r.exchange(opsValue)
	second, _ := r.exchange(opsValue)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout/all", nil)
	req.AddCookie(first)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign out everywhere answered %d: %s", rec.Code,
			rec.Body.String())
	}
	if _, after := r.probe(second); after.Code != http.StatusUnauthorized {
		t.Errorf("the token's other session answered %d after signing out "+
			"everywhere, want 401", after.Code)
	}
	// AND A FRESH EXCHANGE AFTER IT WORKS, because the session opens at the
	// epoch the sign-out moved to.
	third, _ := r.exchange(opsValue)
	if _, rec := r.probe(third); rec.Code != http.StatusOK {
		t.Errorf("an exchange after signing out everywhere answered %d",
			rec.Code)
	}
}

// THE SESSION ROUTE SAYS WHEN THE SESSION ENDS.
//
// `expires_at` was declared and never filled, so every answer said the session
// ended in the year 1. Mutation: drop the expiry read and it is absent.
func TestTheSessionRouteReadsAnExchangedSession(t *testing.T) {
	t.Parallel()
	r := newExchangeRig(t)
	cookie, _ := r.exchange(opsValue)
	req := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/session answered %d: %s", rec.Code,
			rec.Body.String())
	}
	var got struct {
		Login              string    `json:"login"`
		ExpiresAt          time.Time `json:"expires_at"`
		StepUpDue          bool      `json:"step_up_due"`
		SensitiveStepUpDue bool      `json:"sensitive_step_up_due"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Login != iam.TokenLogin("ops") {
		t.Errorf("login %q", got.Login)
	}
	if want := clock.Add(time.Hour).Truncate(time.Second); !got.ExpiresAt.Equal(want) {
		t.Errorf("expires_at %s, want the exchange's one-hour deadline %s",
			got.ExpiresAt, want)
	}
	if got.StepUpDue || got.SensitiveStepUpDue {
		t.Errorf("a session stepped up by construction reports a step-up due "+
			"(ordinary %v, sensitive %v) — the break-glass credential must "+
			"reach the sensitive gestures too", got.StepUpDue,
			got.SensitiveStepUpDue)
	}
}

// A STEP-UP IS DUE THE MOMENT THE PRINCIPAL'S PROOF GOES STALE.
//
// A principal carries the instant its proof goes STALE, and the route used to
// read that as the instant the proof was GIVEN and add the window again — so a
// person half a window past stale was told nothing was due. Mutation: restore
// that arithmetic and this case reads not due.
func TestAStepUpIsDueOnceTheProofIsStale(t *testing.T) {
	t.Parallel()
	window := bootstrapFor(t).API.Auth.Session.StepUp()
	mux := http.NewServeMux()
	surface(t).Routes(mux)
	for _, tc := range []struct {
		name         string
		stale        time.Time
		sensitive    time.Time
		due, sensDue bool
	}{
		{"half a window past stale", clock.Add(-window / 2),
			clock.Add(-window / 2), true, true},
		{"half a window before stale", clock.Add(window / 2),
			clock.Add(window / 2), false, false},
		// THE TWO WINDOWS ARE TWO ANSWERS: a proof inside the hour and
		// outside the quarter is fresh for one and due for the other.
		{"inside step_up, outside step_up_sensitive", clock.Add(window / 2),
			clock.Add(-time.Minute), false, true},
	} {
		req := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
		req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
			ID: uuid.Must(uuid.NewV7()), Login: "jane.doe", Kind: iam.KindPerson,
			Stage: iam.StageActive, ReauthAt: tc.stale,
			SensitiveReauthAt: tc.sensitive,
		}))
		req.Header.Set("Authorization", "Bearer whatever-the-guard-resolved")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var got struct {
			StepUpDue          bool      `json:"step_up_due"`
			SensitiveStepUpDue bool      `json:"sensitive_step_up_due"`
			SensitiveReauthAt  time.Time `json:"sensitive_reauth_at"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got.StepUpDue != tc.due || got.SensitiveStepUpDue != tc.sensDue {
			t.Errorf("%s: step_up_due %v sensitive %v, want %v and %v", tc.name,
				got.StepUpDue, got.SensitiveStepUpDue, tc.due, tc.sensDue)
		}
		if !got.SensitiveReauthAt.Equal(tc.sensitive) {
			t.Errorf("%s: sensitive_reauth_at %s, want the principal's %s",
				tc.name, got.SensitiveReauthAt, tc.sensitive)
		}
	}
}

// --- the estate --------------------------------------------------------- //

// sessionEstate is the identity estate as the exchange writes it and the guard
// reads it: sessions, revocation epochs and token bindings, and nobody enrolled.
type sessionEstate struct {
	stubWriter

	mu       sync.Mutex
	applied  uint64
	seq      uint64
	sessions map[string]sessionRow
	epochs   map[string]uint64
	bindings map[string]session.PersonRow
}

type sessionRow struct {
	person string
	epoch  uint64
	ended  bool
}

func newSessionEstate() *sessionEstate {
	return &sessionEstate{
		applied:  1 << 40,
		sessions: map[string]sessionRow{},
		epochs:   map[string]uint64{},
		bindings: map[string]session.PersonRow{},
	}
}

func (e *sessionEstate) setApplied(at uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applied = at
}

// AwaitApplied answers at once: this estate's applied position moves only when
// a case moves it, so a wait either has already arrived or never will.
func (e *sessionEstate) AwaitApplied(_ context.Context, position uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.applied >= position {
		return nil
	}
	return context.DeadlineExceeded
}

func (e *sessionEstate) OpenSession(_ context.Context, in iamdomain.SessionStart) (
	iamdomain.SessionOpened, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	epoch := e.epochs[in.Person]
	e.sessions[in.Lineage] = sessionRow{person: in.Person, epoch: epoch}
	return iamdomain.SessionOpened{
		Result: applied(statelog.Position{Stream: "CREWLET_IAM_LOG", Seq: e.seq}),
		Epoch:  epoch,
	}, nil
}

func (e *sessionEstate) Revoke(_ context.Context, person, _, _ string) (
	statelog.Result, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	e.epochs[person]++
	return applied(statelog.Position{}), nil
}

func (e *sessionEstate) Resolve(_ context.Context, lineage, person string) (
	session.Identity, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	out := session.Identity{Applied: e.applied}
	if row, ok := e.sessions[lineage]; ok && row.person == person {
		out.Session = session.SessionRow{Found: true, Ended: row.ended,
			Epoch: row.epoch}
	}
	// NOBODY IS ENROLLED: a token's subject has no person row, and the
	// epoch is read whether or not one exists.
	out.Person = session.PersonRow{Epoch: e.epochs[person]}
	return out, nil
}

func (e *sessionEstate) BoundSeat(_ context.Context, login string) (
	session.PersonRow, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bindings[login], nil
}

// seatChart is a chart view holding a fixed set of seats.
type seatChart struct{ seats map[string]session.Seat }

func (c *seatChart) Seat(_ context.Context, ref string) (session.Seat, bool, error) {
	seat, ok := c.seats[ref]
	return seat, ok, nil
}

func (c *seatChart) Position(context.Context) (uint64, time.Duration, error) {
	return 1 << 40, 0, nil
}
