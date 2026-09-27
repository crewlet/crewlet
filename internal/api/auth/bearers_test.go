package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

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
	goodValue = "the-break-glass-token-value"
)

// A STRANGER AT THE ADDRESS CANNOT CLOSE THE WAY BACK IN.
//
// Many people share one address — an office, a VPN's egress, and on a
// deployment whose proxy is not in `api.trusted_proxies` the whole internet —
// and a bearer names nobody until it is compared, so the only curve that could
// stand in front of the comparison is one keyed on that address. There was
// one, and a stranger refusing one guessed value every twenty-five seconds held
// every valid bearer at the address at 429 on every guarded route, the
// break-glass Tier A token and `POST /auth/token` among them. What protects a
// bearer is its value's length; what a guess costs the guesser is a line in the
// audit trail's failure tally.
//
// Mutation: put a source-keyed curve back in front of the comparison and the
// spray below is answered 429, and so is the valid token after it.
func TestAStrangerAtTheAddressCannotCloseTheWayBackIn(t *testing.T) {
	t.Parallel()
	tr := newAuditTrail(t)
	g := tierA(t, tr, false)

	routes := []struct{ method, path string }{
		{http.MethodPost, "/auth/token"}, {http.MethodGet, "/agents"},
	}
	const spray = 48
	for i := range spray {
		route := routes[i%len(routes)]
		status, retry := from(g, route.method, route.path, "guess-"+strconv.Itoa(i), sprayer)
		if status != http.StatusUnauthorized || retry != "" {
			t.Fatalf("guess %d answered %d (Retry-After %q), want the plain 401 "+
				"— a refusal decided on the address is one the address's other "+
				"callers pay for", i, status, retry)
		}
	}
	// SEEN, NEVER SLOWED: every refusal is a failed attempt in the tally.
	if got := tr.failed(types.FailBearer); got != spray {
		t.Errorf("counted %d bearer failures for %d refusals", got, spray)
	}
	for _, route := range routes {
		if got, retry := from(g, route.method, route.path, goodValue, sprayer); got != http.StatusOK {
			t.Errorf("the valid token on %s %s from the spraying address answered "+
				"%d (Retry-After %q), want the route's 200", route.method, route.path,
				got, retry)
		}
	}
}

// VALID BEARERS IN FLIGHT TOGETHER ARE NEVER HELD OR REFUSED.
//
// A pipeline fanning out, an HTTP/2 client multiplexing, `xargs -P` and the
// operator's assistant all put many requests from one address on the wire at
// once, every one of them carrying the right value. When a bearer still being
// resolved counted as a refused one on its address, the twelfth such request
// waited a second, the thirteenth three and the fourteenth and every one after
// it answered 429 — with nothing refused at all.
//
// Each request is held inside its RESOLUTION — the identity directory's read
// of the token's seat binding, which is I/O on a real node — until all of them
// have arrived there, so they are genuinely being resolved together; a request
// the guard held or turned away before resolving never arrives, and the wait
// for it ends at the timeout below.
//
// Mutation: count a bearer against its address until its resolution settles,
// and the requests past the eleventh wait or answer 429.
func TestValidBearersInFlightTogetherAreNeverHeld(t *testing.T) {
	t.Parallel()
	const inFlight = 32
	directory := &barrierBinding{
		arrived: make(chan struct{}, inFlight), release: make(chan struct{}),
	}
	g := tierA(t, newAuditTrail(t), false).BindSeats(auth.SeatBindings{
		Directory: directory,
	})

	statuses := make([]int, inFlight)
	var wg sync.WaitGroup
	for i := range inFlight {
		wg.Go(func() {
			statuses[i], _ = from(g, http.MethodGet, "/agents", goodValue, sprayer)
		})
	}
	reached := 0
	deadline := time.After(5 * time.Second)
wait:
	for reached < inFlight {
		select {
		case <-directory.arrived:
			reached++
		case <-deadline:
			break wait
		}
	}
	close(directory.release)
	wg.Wait()

	if reached != inFlight {
		t.Errorf("%d of %d valid requests were being resolved together — the "+
			"guard held or refused the rest before comparing them", reached, inFlight)
	}
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Errorf("valid request %d answered %d, want 200", i, status)
		}
	}
}

// barrierBinding is a seat-binding directory whose every read waits for the
// test to release it, reporting each arrival first. It binds nobody.
type barrierBinding struct {
	arrived chan struct{}
	release chan struct{}
}

func (b *barrierBinding) BoundSeat(context.Context, string) (session.PersonRow, error) {
	b.arrived <- struct{}{}
	<-b.release
	return session.PersonRow{}, nil
}

// AN UNGUARDED ROUTE NEVER COMPARES A BEARER, AND NEVER READS FOR ONE.
//
// Nothing unguarded acts on the principal a bearer resolves to, and comparing
// one there anyway answered a right value and a wrong one at different speeds:
// a matching Tier A token reads the identity directory for its seat binding
// before it answers, a refused one returns after a map compare — so a guess
// against /health or a static asset was timed at line rate, on routes whose
// refusals nothing counts. The request is anonymous there, marked as having
// presented a bearer, and the guarded route beside it is the control: the same
// value reads the directory once and resolves.
//
// Mutation: resolve the bearer on every route, and each unguarded request
// below reads the directory.
func TestAnUnguardedRouteNeverComparesABearer(t *testing.T) {
	t.Parallel()
	directory := &countingBinding{}
	g := tierA(t, newAuditTrail(t), false).BindSeats(auth.SeatBindings{
		Directory: directory,
	})
	seen := func(path string) (iam.Resolution, bool) {
		var how iam.Resolution
		var presented bool
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+goodValue)
		g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, how = iam.From(r.Context())
			presented = auth.PresentedBearer(r.Context())
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(httptest.NewRecorder(), req)
		return how, presented
	}
	for _, path := range []string{"/health", "/favicon.ico", "/static/app.js",
		auth.WebhookPrefix + "forge", auth.PathAuthOIDCStart} {

		how, presented := seen(path)
		if how != iam.Anonymous || !presented {
			t.Errorf("%s: the bearer resolved to %v (presented %v), want an "+
				"anonymous request marked as presenting one", path, how, presented)
		}
	}
	if n := directory.reads.Load(); n != 0 {
		t.Errorf("unguarded routes read the directory %d times for a presented "+
			"bearer — a right value is answered slower than a wrong one", n)
	}
	if how, _ := seen("/agents"); how != iam.Resolved || directory.reads.Load() != 1 {
		t.Errorf("the guarded route resolved the token to %v after %d directory "+
			"reads, want resolved after one", how, directory.reads.Load())
	}
}

// countingBinding is a seat-binding directory that binds nobody and counts
// how often it is asked.
type countingBinding struct{ reads atomic.Int32 }

func (c *countingBinding) BoundSeat(context.Context, string) (session.PersonRow, error) {
	c.reads.Add(1)
	return session.PersonRow{}, nil
}
