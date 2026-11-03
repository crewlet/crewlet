package mcpbridge_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// told is what a session's [mcpbridge.Session.Started] was told, and what the
// run had called by then.
type told struct {
	mu   sync.Mutex
	said []bool
	// calledBefore is how many calls the run's ledger held as it was told.
	calledBefore []int
}

// hook is a [mcpbridge.Session.Started] that records what it is told against
// the calls calls holds at that moment.
func (t *told) hook(calls *ledger) func(context.Context, bool) {
	return func(_ context.Context, fellBack bool) {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.said = append(t.said, fellBack)
		t.calledBefore = append(t.calledBefore, len(calls.rows()))
	}
}

func (t *told) words() ([]bool, []int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]bool(nil), t.said...), append([]int(nil), t.calledBefore...)
}

// postPingTo posts an MCP ping to url, answering the status.
func postPingTo(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	res, err := client.Post(url, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

// THE TOKEN A JOB'S BOX DIALS SAYS WHICH SESSION IT STARTED. A launch that
// continues a session hands the box two tokens of the job: the one Open
// answers, for that session, and the job's other, for a session of its own its
// script starts in that one's place. Both reach the one opening; the first
// request on either tells the session which the box started — before that
// request is served, so before the run's first call — and a later request on
// the other token is refused, since the box started one session and holds that
// token no longer. Told once, whichever request comes first.
//
// Mutation: tell the session nothing, tell it after the first call is served,
// read every token as the session the launch chose, or serve the other token
// after the box has said, and a case goes red.
func TestTheTokenABoxDialsSaysWhichSessionItStarted(t *testing.T) {
	t.Parallel()
	for name, fellBack := range map[string]bool{
		"the session its launch chose": false,
		"a session of its own":         true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			heard := &told{}
			f.session.Started = heard.hook(f.ledger)
			chosen := f.open(t)
			other := f.bridge.FreshEndpoint(f.session)
			if chosen == "" || other == "" || other == chosen {
				t.Fatalf("the job's tokens are %q and %q, want two of them", chosen, other)
			}
			dialled, refused := chosen, other
			if fellBack {
				dialled, refused = other, chosen
			}
			box := dial(t, dialled)
			if res := callTool(t, box, "read_page", map[string]any{}); res.IsError {
				t.Fatalf("the box's call failed: %s", text(res))
			}
			_ = callTool(t, box, "post_message", map[string]any{})
			said, calledBefore := heard.words()
			if len(said) != 1 || said[0] != fellBack || calledBefore[0] != 0 {
				t.Fatalf("the session was told %v with %v calls made before, want once, %v, before any call",
					said, calledBefore, fellBack)
			}
			if status := postPingTo(t, f.server.Client(), refused); status != http.StatusUnauthorized {
				t.Errorf("the job's other token answered %d once the box had said, want 401", status)
			}
			if said, _ := heard.words(); len(said) != 1 {
				t.Errorf("the session was told %d times, want once", len(said))
			}
		})
	}
}

// A BOX CANNOT SAY A SESSION ITS TOKEN DOES NOT NAME. Which session a box
// started is in what the fleet signs, so a token whose subject was rewritten to
// name the other session — what the run's model could try with the token it
// holds — is no token at all, and tells the session nothing.
//
// Mutation: read the session off anything but the signed subject, and the
// rewritten token is served as a session of the box's own.
func TestABoxCannotSayASessionItsTokenDoesNotName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	heard := &told{}
	f.session.Started = heard.hook(f.ledger)
	chosen := f.open(t)
	cut := strings.LastIndex(chosen, mcpbridge.PathPrefix) + len(mcpbridge.PathPrefix)
	parts := strings.Split(chosen[cut:], ".")
	if len(parts) != 5 || !strings.Contains(parts[2], "~") {
		t.Fatalf("the token %q is not one this case can rewrite", chosen[cut:])
	}
	parts[2] = strings.Replace(parts[2], "~", "_fresh~", 1)
	forged := chosen[:cut] + strings.Join(parts, ".")
	if status := postPingTo(t, f.server.Client(), forged); status != http.StatusUnauthorized {
		t.Errorf("a token rewritten to name the other session answered %d, want 401", status)
	}
	if said, _ := heard.words(); len(said) != 0 {
		t.Errorf("the session was told %v by a token the fleet never signed", said)
	}
}

// A RELAYED REQUEST TELLS THE SESSION TOO. A box dials the fleet's address,
// and the node it lands on relays the request to the seat's holder: that is as
// much the run's first request as one that reached the holder's own route, so
// the holder tells the session which session the box started before it serves
// the relayed request — the MCP endpoint's as much as the notes route's.
//
// Mutation: leave the relay's MCP routes serving the MCP handler alone, and the
// relayed run's first call is served before the session is told.
func TestARelayedRequestTellsTheSessionWhichSessionTheBoxStarted(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	f := newFleet(t, 2, 10*time.Second)
	holder := f.members[1]
	heard := &told{}
	session := &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: fx.surface, Ledger: fx.ledger, Started: heard.hook(fx.ledger)}
	f.takeOver(t, holder, session)
	other := holder.bridge.FreshEndpoint(session)
	if !strings.HasPrefix(other, f.members[0].server.URL) {
		t.Fatalf("the job's other token %q is not at the fleet's address", other)
	}
	box := dialWith(t, other, nil)
	if res := callTool(t, box, "read_page", map[string]any{}); res.IsError {
		t.Fatalf("the relayed call failed: %s", text(res))
	}
	if said, calledBefore := heard.words(); len(said) != 1 || !said[0] || calledBefore[0] != 0 {
		t.Errorf("the holder told the session %v with %v calls made before, want once that the box "+
			"started its own, before any call", said, calledBefore)
	}
}

// A JOB'S ID CANNOT CARRY THE MARK OF ITS OTHER TOKEN: "_" opens it, so a job
// named with one would read as the other session's, and is never opened.
//
// Mutation: let a job's id hold "_", and its opening's token reads as a session
// its box never said it started.
func TestAJobsIDCannotCarryTheMarkOfItsOtherToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.session.Launch = "job_fresh"
	if url := f.open(t); url != "" {
		t.Errorf("a job named %q was opened at %q, want refused", f.session.Launch, url)
	}
}

// A REQUEST THAT ARRIVES WHILE THE SESSION IS BEING TOLD WAITS FOR IT. The first
// request on either token tells the session which session the box started
// before it is served, and a request on the same opening that comes in
// meanwhile — the harness's own MCP client racing a notes poll or a skill's
// report — must not be served against the guard the telling is about to put
// right: it waits until the session has been told, and only then reaches its
// tool.
//
// Mutation: let the first request go before the session is told — record the
// opening's word and unlock, then tell it — and the racing call reaches the
// ledger while the session is still being told.
func TestARequestArrivingWhileTheSessionIsToldWaitsForIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	let := sync.OnceFunc(func() { close(release) })
	t.Cleanup(let)
	var first sync.Once
	f.session.Started = func(context.Context, bool) {
		first.Do(func() { close(entered) })
		<-release
	}
	chosen := f.open(t)
	client := f.server.Client()
	pinged := make(chan struct{})
	go func() {
		defer close(pinged)
		res, err := client.Post(chosen, "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err == nil {
			_ = res.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first request never told the session which session its box started")
	}

	// THE RACING CALL, on the same opening, while the session is being told.
	pool := httpxtest.Pool(t)
	called := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		box, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: chosen, HTTPClient: pool}, nil)
		if err != nil {
			called <- err
			return
		}
		defer func() { _ = box.Close() }()
		_, err = box.CallTool(ctx, &mcp.CallToolParams{Name: "read_page", Arguments: map[string]any{}})
		called <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if n := len(f.ledger.rows()); n != 0 {
		t.Fatalf("%d calls reached the ledger while the session was still being told", n)
	}

	let()
	if err := <-called; err != nil {
		t.Fatalf("the racing call, once the session was told: %v", err)
	}
	<-pinged
	if n := len(f.ledger.rows()); n != 1 {
		t.Errorf("%d calls reached the ledger once the session was told, want the racing one", n)
	}
}
