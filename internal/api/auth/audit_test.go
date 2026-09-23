package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// trail is the REAL audit trail over a publisher that keeps what it was
// handed, so these cases exercise the coalescing a node actually runs rather
// than a fake's idea of it.
type trail struct {
	*authevents.Trail
	mu       sync.Mutex
	events   []*events.Event
	failures map[string]int
	at       time.Time
}

func (tr *trail) Publish(_ context.Context, _ string, ev *events.Event) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.events = append(tr.events, ev)
	return nil
}

func (tr *trail) Add(name string, n uint64, attrs metrics.Attrs) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if name == metrics.AuthAttemptsFailed {
		tr.failures[attrs["method"]] += int(n)
	}
}

func (tr *trail) now() time.Time {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.at
}

func (tr *trail) advance(d time.Duration) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.at = tr.at.Add(d)
}

func (tr *trail) published(eventType string) []*events.Event {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []*events.Event
	for _, ev := range tr.events {
		if ev.Type == eventType {
			out = append(out, ev)
		}
	}
	return out
}

func (tr *trail) failed(method types.FailureMethod) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.failures[string(method)]
}

func newAuditTrail(t *testing.T) *trail {
	t.Helper()
	tr := &trail{failures: map[string]int{},
		at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	built, err := authevents.New(authevents.Options{
		Publisher: tr, Counter: tr, Node: "node-a", Now: tr.now,
	})
	if err != nil {
		t.Fatalf("build the trail: %v", err)
	}
	tr.Trail = built
	return tr
}

// tierA is a guard holding the break-glass token, reporting to tr.
func tierA(t *testing.T, tr *trail, everyUse bool) *auth.Guard {
	t.Helper()
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []config.APIToken{{
		ID: "break-glass", Token: "the-break-glass-token-value",
		Grants: []iam.Grant{iam.GrantStateRead}, AuditEveryUse: everyUse,
	}}
	return auth.New(&b).WithAudit(tr)
}

// answering is a handler that answers with a fixed status.
func answering(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})
}

func call(g *auth.Guard, h http.Handler, path, bearer string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	g.Middleware(h).ServeHTTP(rec, req)
	return rec.Code
}

// A REFUSED BEARER IS COUNTED AND NEVER PUBLISHED.
//
// Whoever holds a wrong value decides how many of these there are, so a row
// per refusal would hand the size of the node estate to anybody who can reach
// the port. Mutation: publish an event from the refusal arm and the second
// assertion fails; drop the count and the first does.
func TestARefusedBearerIsCountedAndNeverPublished(t *testing.T) {
	t.Parallel()
	tr := newAuditTrail(t)
	g := tierA(t, tr, false)
	for range 25 {
		if got := call(g, answering(http.StatusOK), "/agents", "not-the-token"); got != http.StatusUnauthorized {
			t.Fatalf("a wrong bearer answered %d", got)
		}
	}
	if got := tr.failed(types.FailBearer); got != 25 {
		t.Errorf("counted %d bearer failures, want 25", got)
	}
	tr.mu.Lock()
	published := len(tr.events)
	tr.mu.Unlock()
	if published != 0 {
		t.Errorf("%d events published on the request path for refused "+
			"bearers; the only row they may become is the coalesced "+
			"iam_login_failures the engine's loop writes", published)
	}
}

// A CREDENTIAL AN UNGUARDED ROUTE CARRIES IS NOT A FAILED SIGN-IN.
//
// The Forge relay posts every Jira and Confluence delivery to /webhooks/forge
// with its own `Authorization: Bearer <JWT>`, which that route verifies against
// the relay's keys and which no Tier A entry will ever match. Counted at the
// resolution, each delivery was a bearer failure — a DIFFERENT value each
// time, so the relay's address climbed toward a spray's distinct-name count
// every minute. The same holds for a browser loading the sign-in page with a
// cookie signed under a retired key, and for an open socket re-checking its
// credential: none of them is somebody's attempt to authenticate here.
//
// Mutation: count the refusal inside Resolve (where it was) and every arm
// below counts one; count it before the middleware's Unguarded check and the
// first two do.
func TestACredentialNoGuardReliedOnIsNotAFailedAttempt(t *testing.T) {
	t.Parallel()
	tr := newAuditTrail(t)
	g := tierA(t, tr, false)
	const forgeJWT = "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJmb3JnZSJ9.c2lnbmF0dXJl"
	for range 3 {
		if got := call(g, answering(http.StatusOK), auth.WebhookPrefix+"forge",
			forgeJWT); got != http.StatusOK {
			t.Fatalf("the forge delivery answered %d, want the route's own 200", got)
		}
	}
	if got := tr.failed(types.FailBearer); got != 0 {
		t.Errorf("three Forge deliveries counted %d bearer failures, want 0: "+
			"the route verifies its own signature and the guard relied on "+
			"nothing", got)
	}

	// A cookie that is not a bearer of this format, on the sign-in page.
	rig := newSignedIn(t)
	rig.cookie = "v2.nonsense"
	gs := rig.withAudit(tr)
	rig.call(gs, http.MethodGet, auth.PathAuthConfig, rig.withCookie)
	if got := tr.failed(types.FailBearer); got != 0 {
		t.Errorf("a stale cookie on the sign-in page counted %d failures, want 0", got)
	}

	// A socket re-checking the credential it was opened with.
	req := httptest.NewRequest(http.MethodGet, auth.SocketPath+"?token=not-the-token", nil)
	resolved, _ := g.Resolve(httptest.NewRecorder(), req)
	if _, how := iam.From(resolved.Context()); how != iam.Anonymous {
		t.Fatalf("a wrong token resolved as %v, want anonymous", how)
	}
	if got := tr.failed(types.FailBearer); got != 0 {
		t.Errorf("a resolution outside the middleware counted %d failures, want 0", got)
	}

	// THE CONTROL: the same wrong bearer on a guarded route IS the attempt.
	if got := call(g, answering(http.StatusOK), auth.SocketPath, "not-the-token"); got != http.StatusUnauthorized {
		t.Fatalf("a wrong bearer on the socket answered %d", got)
	}
	if got := tr.failed(types.FailBearer); got != 1 {
		t.Errorf("a wrong bearer on a guarded route counted %d, want 1", got)
	}
}

// A TIER A TOKEN'S USE IS ONE ROW AN HOUR, unless its entry asks for every use.
//
// Mutation: record per request regardless of the entry and the hourly case
// counts five; coalesce regardless and the every-use case counts one.
func TestATierATokensUseIsOneRowAnHourUnlessItsEntrySaysOtherwise(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		everyUse bool
		want     int
	}{
		{"coalesced by default", false, 1},
		{"every use where the entry says audit_every_use", true, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newAuditTrail(t)
			g := tierA(t, tr, tc.everyUse)
			for range 5 {
				call(g, answering(http.StatusOK), "/secrets/github-token",
					"the-break-glass-token-value")
			}
			uses := tr.published("iam_token_first_use")
			if len(uses) != tc.want {
				t.Fatalf("%d use rows for five requests, want %d", len(uses), tc.want)
			}
			row, _ := events.DataAs[*types.IAMTokenFirstUse](uses[0])
			if row.Token != "break-glass" || row.Route != "/secrets" ||
				row.EveryUse != tc.everyUse {
				t.Errorf("use row = %+v: want the token's id, the route's CLASS "+
					"(never the path, which names a secret) and every_use %v",
					row, tc.everyUse)
			}
			if !tc.everyUse {
				tr.advance(auth.TokenUseWindow)
				call(g, answering(http.StatusOK), "/iam/people", "the-break-glass-token-value")
				if got := len(tr.published("iam_token_first_use")); got != 2 {
					t.Errorf("after the window, %d rows, want the next hour's first", got)
				}
			}
		})
	}
}

// A TIER A TOKEN A ROUTE REFUSES IS AN OVERREACH, and a route that served it is
// not.
//
// Mutation: record the overreach before the handler has answered and the
// served case records one too.
func TestATierATokenRefusedByARouteIsAnOverreach(t *testing.T) {
	t.Parallel()
	tr := newAuditTrail(t)
	g := tierA(t, tr, false)
	call(g, answering(http.StatusOK), "/agents", "the-break-glass-token-value")
	if got := len(tr.published("iam_token_overreach")); got != 0 {
		t.Fatalf("a served request recorded %d overreach rows", got)
	}
	for range 3 {
		call(g, answering(http.StatusForbidden), "/secrets", "the-break-glass-token-value")
	}
	over := tr.published("iam_token_overreach")
	if len(over) != 1 {
		t.Fatalf("%d overreach rows for three refusals in one hour, want 1", len(over))
	}
	row, _ := events.DataAs[*types.IAMTokenOverreach](over[0])
	if row.Status != http.StatusForbidden || row.Route != "/secrets" {
		t.Errorf("overreach row = %+v", row)
	}
	// A 404 is a route that does not exist, which no grant would have
	// opened: not an overreach.
	tr.advance(auth.TokenUseWindow)
	call(g, answering(http.StatusNotFound), "/nowhere", "the-break-glass-token-value")
	if got := len(tr.published("iam_token_overreach")); got != 1 {
		t.Errorf("a 404 was recorded as an overreach (%d rows)", got)
	}
}

// A SOCKET RE-CHECKING ITS CREDENTIAL IS NOT A USE.
//
// An open socket re-runs the guard's resolution once a minute; a per-request
// audit that counted it would write a row a minute for every open tab.
// Mutation: record the use inside Resolve and this case records one.
func TestASocketRevalidationIsNotAUse(t *testing.T) {
	t.Parallel()
	tr := newAuditTrail(t)
	g := tierA(t, tr, true)
	req := httptest.NewRequest(http.MethodGet, auth.SocketPath+"?token=the-break-glass-token-value", nil)
	resolved, _ := g.Resolve(httptest.NewRecorder(), req)
	if _, how := iam.From(resolved.Context()); how != iam.Resolved {
		t.Fatalf("the token did not resolve (%v)", how)
	}
	if got := len(tr.published("iam_token_first_use")); got != 0 {
		t.Errorf("a resolution outside the middleware recorded %d uses", got)
	}
}

// A FORGED COOKIE IS A BEARER FAILURE; A COOKIE WHOSE SESSION MERELY ENDED IS
// NOT, AND ITS END IS SAID ONCE.
//
// A tab left open over a weekend presents a cookie that verifies and whose
// session is over: counting that as a failure would page somebody for every
// such tab. What it IS, when a deadline ended it, is the one way a session
// ends that no record states — so this is where the idle and absolute ends
// are announced, once per lineage per node.
//
// Mutation: count every refused cookie as a failure and the ended case counts
// one; drop the once-per-lineage key and the replay publishes three.
func TestAForgedCookieIsCountedAndADeadlineEndIsSaidOnce(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	tr := newAuditTrail(t)
	g := rig.withAudit(tr)

	rig.cookie = "v2.nonsense"
	rig.call(g, http.MethodGet, "/agents", rig.withCookie)
	if got := tr.failed(types.FailBearer); got != 1 {
		t.Errorf("a forged cookie counted %d bearer failures, want 1", got)
	}

	// The rig's own cookie, validated nine hours on: past the eight-hour
	// absolute deadline it was minted with.
	rig2 := newSignedIn(t)
	rig2.at = rig2.at.Add(9 * time.Hour)
	rig2.signerAt(rig2.at)
	g2 := rig2.withAudit(tr)
	for range 3 {
		rig2.call(g2, http.MethodGet, "/agents", rig2.withCookie)
	}
	if got := tr.failed(types.FailBearer); got != 1 {
		t.Errorf("an expired cookie was counted as a failure (%d in all)", got)
	}
	ended := tr.published("iam_session_ended")
	if len(ended) != 1 {
		t.Fatalf("%d session-ended rows for three presentations of one expired "+
			"cookie, want 1", len(ended))
	}
	row, _ := events.DataAs[*types.IAMSessionEnded](ended[0])
	if row.Reason != types.EndAbsolute || row.Person != sessionPerson || row.Lineage == "" {
		t.Errorf("ended row = %+v, want the absolute deadline naming the person and session", row)
	}
}

// AN ENDING IS ANNOUNCED ONCE, FROM THE FACT THAT ENDED IT.
//
// The deadlines are decided before any row is read, so a session a RECORD
// ended is refused on its deadline too once its cookie outlives it: an
// administrator revokes somebody, and their other browser presents the cookie
// the next day. The record already announced that ending; a second row naming
// `absolute` would name the wrong cause for a session over a day earlier.
//
// Mutation: announce the deadline without asking the rows and every record
// case below publishes one; announce it when the rows cannot say and the
// stalled case does.
func TestADeadlineEndsOnlyASessionNoRecordEnded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		shape func(*session.Identity)
	}{
		{"the row says it ended", func(id *session.Identity) { id.Session.Ended = true }},
		{"the person's epoch moved", func(id *session.Identity) { id.Person.Epoch = 4 }},
		{"the company's generation moved", func(id *session.Identity) { id.Generation = 2 }},
		{"the person was suspended", func(id *session.Identity) { id.Person.Stage = iam.StageSuspended }},
		{"the sweep collected the row", func(id *session.Identity) { id.Session = session.SessionRow{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSignedIn(t)
			tc.shape(&rig.dir.identity)
			rig.at = rig.at.Add(9 * time.Hour)
			rig.signerAt(rig.at)
			tr := newAuditTrail(t)
			g := rig.withAudit(tr)
			for range 2 {
				if got := rig.call(g, http.MethodGet, "/agents", rig.withCookie); got.status != http.StatusUnauthorized {
					t.Fatalf("an expired cookie answered %d", got.status)
				}
			}
			if ended := tr.published("iam_session_ended"); len(ended) != 0 {
				row, _ := events.DataAs[*types.IAMSessionEnded](ended[0])
				t.Errorf("a session a record had already ended was announced "+
					"again as %q", row.Reason)
			}
		})
	}

	// A NODE THAT CANNOT SAY ANNOUNCES NOTHING, and asks again next time.
	rig := newSignedIn(t)
	rig.at = rig.at.Add(9 * time.Hour)
	rig.signerAt(rig.at)
	tr := newAuditTrail(t)
	g := rig.withAudit(tr)
	rig.dir.err = errors.New("the replicated estate is not open")
	rig.call(g, http.MethodGet, "/agents", rig.withCookie)
	if got := len(tr.published("iam_session_ended")); got != 0 {
		t.Fatalf("a node that could not read the rows announced %d endings", got)
	}
	rig.dir.err = nil
	rig.call(g, http.MethodGet, "/agents", rig.withCookie)
	rig.call(g, http.MethodGet, "/agents", rig.withCookie)
	ended := tr.published("iam_session_ended")
	if len(ended) != 1 {
		t.Fatalf("%d endings once the rows could be read, want the one the "+
			"stalled read handed back", len(ended))
	}
	row, _ := events.DataAs[*types.IAMSessionEnded](ended[0])
	if row.Reason != types.EndAbsolute {
		t.Errorf("reason %q, want absolute", row.Reason)
	}
}

// A REPLAYED COOKIE REVOKES ONCE, however often it is presented.
//
// The replay is refused and nothing stops its holder presenting it again, and
// each presentation used to publish another revocation — a write to the
// identity log paced by the holder of a cookie this node had already turned
// away. Mutation: revoke on every presentation and the count is three.
func TestAReplayedCookieRevokesOnceHoweverOftenItIsPresented(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	tr := newAuditTrail(t)
	rig.cookie = rig.aheadOfTheClock(t)
	g := rig.withAudit(tr)
	for range 3 {
		if got := rig.call(g, http.MethodGet, "/agents", rig.withCookie); got.status != http.StatusUnauthorized {
			t.Fatalf("a replayed cookie answered %d", got.status)
		}
	}
	if ended := rig.ended.all(); len(ended) != 1 {
		t.Errorf("revoked %d times for three presentations, want once", len(ended))
	}
	// AT THE BEARER'S EPOCH, which is what makes the revocation itself
	// land once however many nodes ask: it moves the epoch only while it is
	// still at the one the replayed cookie was minted at.
	rig.ended.mu.Lock()
	epochs := append([]uint64(nil), rig.ended.epochs...)
	rig.ended.mu.Unlock()
	if len(epochs) != 1 || epochs[0] != 3 {
		t.Errorf("revoked through epochs %v, want the replayed bearer's 3", epochs)
	}
	reuse := tr.published("iam_session_reuse_detected")
	if len(reuse) != 1 {
		t.Fatalf("%d reuse rows, want 1", len(reuse))
	}
	row, _ := events.DataAs[*types.IAMSessionReuseDetected](reuse[0])
	if row.Person != sessionPerson || row.Rotation == 0 {
		t.Errorf("reuse row = %+v, want the person and the index the replay carried", row)
	}
}

// THE SESSION ARM IS REFUSED WITHOUT A TRAIL, because the revocation above
// hangs on the trail's decision.
func TestTheSessionArmNeedsATrail(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	_, err := auth.NewSessions(auth.SessionsDeps{
		Signer: rig.signer, Directory: rig.dir, Chart: rig.chart,
	})
	if err == nil {
		t.Fatal("a session arm with no audit trail was built")
	}
}

// A ROUTE CLASS IS THE SURFACE, NEVER THE PATH.
func TestRouteClassIsTheFirstSegment(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"/secrets/github-token": "/secrets",
		"/iam/people/0192":      "/iam",
		"/agents":               "/agents",
		"/":                     "/",
		"":                      "/",
	} {
		if got := auth.RouteClass(path); got != want {
			t.Errorf("RouteClass(%q) = %q, want %q", path, got, want)
		}
	}
	if slices.Contains([]string{auth.RouteClass("/work/items/ENG-12")}, "/work/items/ENG-12") {
		t.Error("a route class carried an item key")
	}
}

// withAudit is this rig's guard, its session arm reporting to tr.
func (s *signedIn) withAudit(tr *trail) *auth.Guard {
	s.t.Helper()
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{{ID: "ci", Token: "a-tier-a-token"}}
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.ExternalURL = "http://127.0.0.1:8080"
	arm, err := auth.NewSessions(auth.SessionsDeps{
		Signer: s.signer, Directory: s.dir, Chart: s.chart,
		External: b.API.ExternalBase(),
		OnReuse:  s.ended.record,
		Audit:    tr,
		Now:      func() time.Time { return s.at },
	})
	if err != nil {
		s.t.Fatalf("build the session arm: %v", err)
	}
	return auth.New(&b).WithSessions(arm).WithAudit(tr)
}

// signerAt rebuilds this rig's signer on a clock at the given instant, which
// is how a case moves the validating node's clock past a deadline.
func (s *signedIn) signerAt(at time.Time) {
	s.t.Helper()
	signer, err := session.New(session.Options{
		Material: sessionKeyring(), RotateAfter: time.Hour,
		Now: func() time.Time { return at },
	})
	if err != nil {
		s.t.Fatalf("build a signer: %v", err)
	}
	s.signer = signer
}

// aheadOfTheClock mints this rig's session on a signer four windows ahead of
// the rig's own clock — the node-whose-clock-ran-fast case, and the one
// positive evidence of a replay rotate.go recognises.
func (s *signedIn) aheadOfTheClock(t *testing.T) string {
	t.Helper()
	future, err := session.New(session.Options{
		Material: sessionKeyring(), RotateAfter: time.Hour,
		Now: func() time.Time { return s.at.Add(4 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("build a future-clocked signer: %v", err)
	}
	cookie, err := future.Mint(session.Mint{
		Lineage: lineageAt(t, s.at), Person: sessionPerson, Epoch: 3,
		Generation: 1, StartPosition: sessionStart,
		AbsoluteExpiresAt: s.at.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return cookie
}
