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
// Many people share one address, and a bearer names nobody until compared, so
// the only possible curve is keyed on the address; one guess every
// twenty-five seconds would hold every valid bearer there at 429. A bearer is
// protected by its value's length; a guess costs one more on its source's
// count in the audit trail's per-minute failure row.
//
// Mutation: put a source-keyed curve back in front of the comparison and the
// spray below is answered 429, and so is the valid token after it.
func TestAStrangerAtTheAddressCannotCloseTheWayBackIn(t *testing.T) {
	t.Parallel()
	tr := newAuditTrail(t)
	g := tierA(t, tr)

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
	// SEEN, NEVER SLOWED: every refusal is a failed attempt, counted on the
	// per-minute failure row under its source.
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
// Pipelines and multiplexed clients put many requests from one address on the
// wire at once; a bearer still being resolved must not count as a refused one.
//
// Each request is held inside its RESOLUTION (the identity directory's read of
// the seat binding) until all have arrived, so they are genuinely resolved
// together; one the guard held or turned away never arrives, and the wait ends
// at the timeout below.
//
// Mutation: count a bearer against its address until its resolution settles,
// and the requests past the eleventh wait or answer 429.
func TestValidBearersInFlightTogetherAreNeverHeld(t *testing.T) {
	t.Parallel()
	const inFlight = 32
	directory := &barrierBinding{
		arrived: make(chan struct{}, inFlight), release: make(chan struct{}),
	}
	g := tierA(t, newAuditTrail(t)).BindSeats(auth.SeatBindings{
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
// one there answered right and wrong values at different speeds (a match reads
// the identity directory, a refusal a map compare), so a guess against /health
// could be timed on routes whose refusals nothing counts. The guarded route
// beside it is the control: the same value reads the directory once.
//
// Mutation: resolve the bearer on every route, and each unguarded request
// below reads the directory.
func TestAnUnguardedRouteNeverComparesABearer(t *testing.T) {
	t.Parallel()
	directory := &countingBinding{}
	g := tierA(t, newAuditTrail(t)).BindSeats(auth.SeatBindings{
		Directory: directory,
	})
	seen := func(path string) iam.Resolution {
		var how iam.Resolution
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+goodValue)
		g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, how = iam.From(r.Context())
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(httptest.NewRecorder(), req)
		return how
	}
	for _, path := range []string{"/health", "/favicon.ico", "/static/app.js",
		auth.WebhookPrefix + "forge", auth.PathAuthLogout} {

		if how := seen(path); how != iam.Anonymous {
			t.Errorf("%s: the bearer resolved to %v, want an anonymous "+
				"request", path, how)
		}
	}
	if n := directory.reads.Load(); n != 0 {
		t.Errorf("unguarded routes read the directory %d times for a presented "+
			"bearer — a right value is answered slower than a wrong one", n)
	}
	if how := seen("/agents"); how != iam.Resolved || directory.reads.Load() != 1 {
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
