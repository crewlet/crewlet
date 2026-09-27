package auth_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/credential"
)

// curve is a bearer curve on a clock that stands still and a sleep that only
// records, so a case asserts the waits the guard DECIDED rather than timing
// real ones.
type curve struct {
	mu    sync.Mutex
	at    time.Time
	slept []time.Duration
}

func (c *curve) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *curve) sleep(_ context.Context, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
}

func (c *curve) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// on installs this curve on g.
func (c *curve) on(t *testing.T, g *auth.Guard) *auth.Guard {
	t.Helper()
	throttle, err := credential.NewThrottle(credential.ThrottleDeps{
		Now: c.now, Sleep: c.sleep,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build the curve: %v", err)
	}
	return g.WithBearerCurve(throttle)
}

func newCurve() *curve {
	return &curve{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
}

// from runs one request from remote presenting bearer, answering its status
// and its Retry-After.
func from(g *auth.Guard, method, path, bearer, remote string) (int, string) {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote + ":5100"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	g.Middleware(answering(http.StatusOK)).ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Retry-After")
}

const (
	sprayer   = "203.0.113.7"
	elsewhere = "198.51.100.20"
	goodValue = "the-break-glass-token-value"
)

// REFUSED BEARERS FROM ONE SOURCE MEET THE CURVE — THE EXCHANGE INCLUDED.
//
// A guarded route compared every bearer it was handed as fast as it arrived,
// and whether it matched was the whole answer: a guessing oracle at line rate
// on every route, `POST /auth/token` among them, while the sign-in throttle
// covered the password routes alone. Now the source's curve stands in front of
// the comparison — ten refusals free, then a wait that doubles, and a wait past
// five seconds is `429` with a `Retry-After`. A valid token from ANOTHER source
// is untouched; one from the spraying source waits with it, because it is
// admitted before it is compared, and a curve consulted after the comparison
// would let a correct guess through whatever it said.
//
// Mutation: drop the admission from the middleware and the spray is answered
// 401 for ever.
func TestRefusedBearersFromOneSourceMeetTheCurve(t *testing.T) {
	t.Parallel()
	tr := newAuditTrail(t)
	c := newCurve()
	g := c.on(t, tierA(t, tr, false))

	routes := []struct{ method, path string }{
		{http.MethodPost, "/auth/token"}, {http.MethodGet, "/agents"},
	}
	refused := 0
	status, retry := 0, ""
	for i := range credential.SourceAllowance + credential.CurveSteps {
		route := routes[i%len(routes)]
		status, retry = from(g, route.method, route.path, "guess-"+strconv.Itoa(i), sprayer)
		if status != http.StatusUnauthorized {
			break
		}
		refused++
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("%d refused bearers from one source and the next still answered "+
			"%d, want 429 — the comparison is an oracle nothing slows", refused, status)
	}
	if refused <= credential.SourceAllowance {
		t.Errorf("throttled after %d refusals, inside the allowance of %d",
			refused, credential.SourceAllowance)
	}
	if seconds, err := strconv.Atoi(retry); err != nil || seconds < 1 {
		t.Errorf("the 429 carried Retry-After %q, want the wait in whole seconds", retry)
	}
	// The exchange is judged exactly as any other guarded route.
	if got, _ := from(g, http.MethodPost, "/auth/token", "one-more", sprayer); got != http.StatusTooManyRequests {
		t.Errorf("POST /auth/token from the spraying source answered %d, want 429", got)
	}
	if got := tr.failed(types.FailBearer); got < refused {
		t.Errorf("counted %d bearer failures for %d refusals", got, refused)
	}

	// ANOTHER SOURCE IS UNTOUCHED.
	if got, _ := from(g, http.MethodPost, "/auth/token", goodValue, elsewhere); got != http.StatusOK {
		t.Errorf("a valid token from another source answered %d, want the route's 200", got)
	}
	// AND THE SPRAYING SOURCE'S OWN VALID TOKEN WAITS WITH IT.
	if got, _ := from(g, http.MethodGet, "/agents", goodValue, sprayer); got != http.StatusTooManyRequests {
		t.Errorf("a valid token from the spraying source answered %d, want 429: "+
			"it is admitted before it is compared", got)
	}
}

// A BEARER THAT MATCHED CLEARS NOTHING, not even its own source's record.
//
// A match proves the caller holds one credential, not that the other values
// from their address were theirs: a success that flushed the source would let
// anybody holding a token wipe the record of their guesses at another by
// presenting their own between them. Mutation: flush the source on a match and
// the refusal after it owes no wait.
func TestAMatchedBearerClearsNothing(t *testing.T) {
	t.Parallel()
	c := newCurve()
	g := c.on(t, tierA(t, newAuditTrail(t), false))
	for i := range credential.SourceAllowance + 1 {
		if got, _ := from(g, http.MethodGet, "/agents", "guess-"+strconv.Itoa(i), sprayer); got != http.StatusUnauthorized {
			t.Fatalf("refusal %d answered %d", i, got)
		}
	}
	if got, _ := from(g, http.MethodGet, "/agents", goodValue, sprayer); got != http.StatusOK {
		t.Fatalf("the valid token answered %d after a one-second wait, want 200", got)
	}
	before := len(c.waits())
	if got, _ := from(g, http.MethodGet, "/agents", "guess-after", sprayer); got != http.StatusUnauthorized {
		t.Fatalf("the refusal after the match answered %d", got)
	}
	waits := c.waits()
	if len(waits) != before+1 || waits[len(waits)-1] <= 0 {
		t.Errorf("the refusal after a match waited %v, want the wait the eleven "+
			"refusals before it earned — the match cleared the source", waits[before:])
	}
}

// A BEARER NO GUARDED ROUTE RELIED ON NEVER MEETS THE CURVE.
//
// The Forge relay posts every delivery to /webhooks/forge with its own
// `Authorization: Bearer <JWT>`, verified by that route and matching no Tier A
// entry. On the curve, the relay's address would be throttled by its own
// traffic, and every Jira and Confluence delivery with it. Mutation: judge
// bearers on every route and the deliveries meet 429.
func TestAnUnguardedRoutesBearerNeverMeetsTheCurve(t *testing.T) {
	t.Parallel()
	c := newCurve()
	g := c.on(t, tierA(t, newAuditTrail(t), false))
	for i := range 3 * (credential.SourceAllowance + credential.CurveSteps) {
		if got, _ := from(g, http.MethodPost, auth.WebhookPrefix+"forge",
			"relay-jwt-"+strconv.Itoa(i), sprayer); got != http.StatusOK {
			t.Fatalf("delivery %d answered %d, want the route's own 200", i, got)
		}
	}
	if waits := c.waits(); len(waits) != 0 {
		t.Errorf("the relay's deliveries were made to wait %v", waits)
	}
}

// A BEARER THIS NODE COULD NOT CHECK IS NOT A FAILURE.
//
// A machine token on a node that cannot read its directory is `503`: nobody
// refused it, and counting it would put a pipeline's address on the curve for
// an outage it did not cause — the next request after recovery made to wait,
// or refused, for this node's own trouble. Mutation: fail the attempt on the
// unknown arm and the first request after recovery waits.
func TestABearerThisNodeCouldNotCheckIsNotAFailure(t *testing.T) {
	t.Parallel()
	m := newMachineRig(t)
	m.dirErr = context.DeadlineExceeded
	c := newCurve()
	g := c.on(t, m.guard(nil))
	for i := range 2 * (credential.SourceAllowance + credential.CurveSteps) {
		if got, _ := from(g, http.MethodGet, "/agents", m.presented.Value(), sprayer); got != http.StatusServiceUnavailable {
			t.Fatalf("request %d answered %d, want 503", i, got)
		}
	}
	m.dirErr = nil
	if got, _ := from(g, http.MethodGet, "/agents", m.presented.Value(), sprayer); got != http.StatusOK {
		t.Errorf("the token after recovery answered %d, want 200", got)
	}
	if waits := c.waits(); len(waits) != 0 {
		t.Errorf("an outage put the source on the curve: waits %v", waits)
	}
}
