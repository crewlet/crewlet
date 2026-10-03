package auth_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// trail is the REAL audit trail over a publisher that keeps what it was
// handed, so these cases exercise the counting a node actually runs rather
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
func tierA(t *testing.T, tr *trail) *auth.Guard {
	t.Helper()
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []config.APIToken{{
		ID: "break-glass", Token: "the-break-glass-token-value",
		Grants: []iam.Grant{iam.GrantStateRead},
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
	g := tierA(t, tr)
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
	g := tierA(t, tr)
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
	req := httptest.NewRequest(http.MethodGet, auth.SocketPath, nil)
	req.Header.Set("Authorization", "Bearer not-the-token")
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

// A TIER A TOKEN'S USE IS NO ROW, AND NEITHER IS ITS OVERREACH.
//
// Everything a token writes already names it — the author and operator id on
// every record — so a row for its first use in an hour said again what those
// say, and took a remembered set to say it once. And a route refusing it is a
// WARN log line ([TestATierATokenRefusedByARouteIsAWarnLine]), never a row.
//
// Mutation: publish anything for a token's use or its overreach and the trail
// holds a row.
func TestATierATokensUseIsNoRow(t *testing.T) {
	t.Parallel()
	tr := newAuditTrail(t)
	g := tierA(t, tr)
	for range 5 {
		if got := call(g, answering(http.StatusOK), "/secrets/github-token",
			"the-break-glass-token-value"); got != http.StatusOK {
			t.Fatalf("the token answered %d", got)
		}
	}
	call(g, answering(http.StatusForbidden), "/iam/people", "the-break-glass-token-value")
	tr.mu.Lock()
	published := len(tr.events)
	tr.mu.Unlock()
	if published != 0 {
		t.Errorf("a token's use and its overreach published %d rows, want none",
			published)
	}
}

// A TIER A TOKEN A ROUTE REFUSES IS A WARN LINE, and a route that served it or
// does not exist is not.
//
// Something holding the token reaching past what it was pinned for is the
// question a break-glass credential's owner most wants answered, and a WARN
// line is what their log alerting already reads. NOT PARALLEL: it installs
// this process's log sink for its duration, which only a case running alone
// may do.
//
// Mutation: log before the handler has answered and the served case logs one
// too; log on any refusal status and the 404 does.
func TestATierATokenRefusedByARouteIsAWarnLine(t *testing.T) {
	var out syncBuffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })

	tr := newAuditTrail(t)
	g := tierA(t, tr)
	overreaches := func() []string {
		var lines []string
		for line := range strings.Lines(out.String()) {
			if strings.Contains(line, "api_auth_token_overreach") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	call(g, answering(http.StatusOK), "/agents", "the-break-glass-token-value")
	if got := overreaches(); len(got) != 0 {
		t.Fatalf("a served request logged an overreach: %q", got)
	}
	call(g, answering(http.StatusForbidden), "/secrets", "the-break-glass-token-value")
	got := overreaches()
	if len(got) != 1 || !strings.Contains(got[0], "level=WARN") ||
		!strings.Contains(got[0], "token=break-glass") ||
		!strings.Contains(got[0], "status=403") ||
		!strings.Contains(got[0], "route=/secrets") {
		t.Fatalf("a refused token logged %q, want one WARN line naming the "+
			"token, the route and the status", got)
	}
	// A 404 is a route that does not exist, which no grant would have
	// opened: not an overreach.
	call(g, answering(http.StatusNotFound), "/nowhere", "the-break-glass-token-value")
	if got := overreaches(); len(got) != 1 {
		t.Errorf("a 404 was logged as an overreach: %q", got)
	}
}

// syncBuffer is a log sink a case reads while the guard writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A FORGED COOKIE IS A BEARER FAILURE; AN EXPIRED ONE IS NEITHER A FAILURE NOR
// A ROW.
//
// A tab left open over a weekend presents a cookie that verifies and whose
// session is over: counting that as a failure would page somebody for every
// such tab. Nor is its expiry announced — nobody authored it, and every ending
// somebody did author was announced by whoever wrote the record — so it is
// refused, its cookie cleared, and nothing is published.
//
// Mutation: count every refused cookie as a failure and the expired case
// counts three; announce an expiry and the trail holds a row.
func TestAForgedCookieIsCountedAndAnExpiredOneIsNeither(t *testing.T) {
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
		if got := rig2.call(g2, http.MethodGet, "/agents", rig2.withCookie); got.status != http.StatusUnauthorized {
			t.Fatalf("an expired cookie answered %d", got.status)
		}
	}
	if got := tr.failed(types.FailBearer); got != 1 {
		t.Errorf("an expired cookie was counted as a failure (%d in all)", got)
	}
	tr.mu.Lock()
	published := len(tr.events)
	tr.mu.Unlock()
	if published != 0 {
		t.Errorf("an expired cookie published %d rows, want none: an expiry "+
			"nobody authored is no row", published)
	}
}

// A COOKIE FROM A NODE WHOSE CLOCK RAN AHEAD ANNOUNCES NOTHING.
//
// It used to publish `iam_session_reuse_detected` at WARN and ask for the
// person's revocation epoch to be bumped — a theft alarm naming them, over two
// hosts' clocks disagreeing. Mutation: announce a replay for a bearer ahead of
// the clock and the trail holds a row.
func TestACookieFromAFastNodeAnnouncesNothing(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	tr := newAuditTrail(t)
	rig.cookie = rig.aheadOfTheClock(t)
	g := rig.withAudit(tr)
	for range 3 {
		if got := rig.call(g, http.MethodGet, "/agents", rig.withCookie); got.status != http.StatusOK {
			t.Fatalf("a cookie from a fast node answered %d", got.status)
		}
	}
	tr.mu.Lock()
	published := len(tr.events)
	tr.mu.Unlock()
	if published != 0 {
		t.Errorf("serving a cookie from a fast node published %d rows, want "+
			"none", published)
	}
}

// withAudit is this rig's guard, its session arm reporting to tr.
func (s *signedIn) withAudit(tr *trail) *auth.Guard {
	s.t.Helper()
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{{ID: "ci", Token: "a-tier-a-token"}}
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.ExternalURL = "http://127.0.0.1:8000"
	arm, err := auth.NewSessions(auth.SessionsDeps{
		Signer: s.signer, Directory: s.dir, Applier: s.dir, Chart: s.chart,
		External: b.API.ExternalBase(),
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
		Material: sessionKeyring(),
		Now:      func() time.Time { return at },
	})
	if err != nil {
		s.t.Fatalf("build a signer: %v", err)
	}
	s.signer = signer
}

// aheadOfTheClock mints this rig's session on a signer four hours ahead of
// the rig's own clock — the node-whose-clock-ran-fast case — with a lineage
// minted on that clock, as the node would mint it.
func (s *signedIn) aheadOfTheClock(t *testing.T) string {
	t.Helper()
	future, err := session.New(session.Options{
		Material: sessionKeyring(),
		Now:      func() time.Time { return s.at.Add(4 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("build a future-clocked signer: %v", err)
	}
	cookie, err := future.Mint(session.Mint{
		Lineage: lineageAt(t, s.at.Add(4*time.Hour)), Person: sessionPerson, Epoch: 3,
		Generation: 1, StartPosition: sessionStart,
		AbsoluteExpiresAt: s.at.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return cookie
}
