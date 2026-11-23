package mcpbridge_test

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
)

// A call its harness sends again (the package doc's "A call longer than its
// harness waits"; binding.resentLocked has why).

// severable is an HTTP client whose every connection a case can cut, as a
// network failure or Node's fetch giving up a silent response cuts one.
type severable struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (s *severable) client() *http.Client {
	dialer := &net.Dialer{}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err == nil {
				s.mu.Lock()
				s.conns = append(s.conns, conn)
				s.mu.Unlock()
			}
			return conn, err
		},
	}}
}

// sever cuts every connection the client opened, sending nothing first.
func (s *severable) sever() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.conns {
		_ = conn.Close()
	}
	s.conns = nil
}

// dialOver connects a harness's MCP client to url through client.
func dialOver(t *testing.T, url string, client *http.Client) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "kimi", Version: "2.7.2"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: url, HTTPClient: client}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// callBare makes a call with no progress token, as Kimi Code makes every
// call, and hands back its answer or its failure.
func callBare(ctx context.Context, sess *mcp.ClientSession, name string, args map[string]any,
) <-chan *mcp.CallToolResult {
	answered := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			res = &mcp.CallToolResult{IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "the call failed: " + err.Error()}}}
		}
		answered <- res
	}()
	return answered
}

// A CALL SENT AGAIN AFTER ITS REQUEST WAS LOST JOINS THE CALL STILL RUNNING.
// Kimi Code, its silent response dropped at 300 seconds, sends the call again
// under an id of its own over a new connection, and once more after that
// (measured on 2.7.2): run each time, the tool took effect three times. So a
// call the box sends again, of the same tool with the same arguments, while
// the first still runs and its request is gone, runs nothing: it is answered
// with the first's own answer, and the first is recorded once, as a call its
// harness was handed.
//
// Mutation: never join (resentLocked answering nil), and the tool runs twice
// and is recorded twice.
func TestACallSentAgainAfterItsRequestWasLostJoinsTheCallStillRunning(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	held := newHeldTool("write")
	g := newGated(t, held)
	t.Cleanup(held.free)
	joined := make(chan struct{}, 1)
	mcpbridge.OnJoin(g.bridge, func() { joined <- struct{}{} })
	args := map[string]any{"value": "hold"}

	lost := &severable{}
	callBare(ctx, dialOver(t, g.url, lost.client()), "write", args)
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached its tool")
	}
	lost.sever()
	mcpbridge.AwaitRequestsLost(g.session)

	// SENT AGAIN, as Kimi Code's reconnect does: a new session, a new id.
	again := callBare(ctx, dialOver(t, g.url, (&severable{}).client()), "write", args)
	select {
	case <-joined:
	case <-held.entered:
		t.Fatal("the call sent again ran its tool a second time")
	case <-ctx.Done():
		t.Fatal("the call sent again neither joined nor ran")
	}
	held.free()
	select {
	case res := <-again:
		if res.IsError || text(res) != "write done" {
			t.Fatalf("the call sent again was answered %v %q, want the first's own answer", res.IsError, text(res))
		}
	case <-ctx.Done():
		t.Fatal("the call sent again was never answered")
	}
	select {
	case <-held.entered:
		t.Fatal("the call sent again ran its tool a second time")
	default:
	}
	g.bridge.Close("run-1")
	g.bridge.AwaitCalls(ctx)
	if rows := g.ledger.rows(); len(rows) != 1 || rows[0].Output != "write done" || rows[0].Abandoned {
		t.Errorf("the call is recorded %+v, want once, as one its harness was handed", rows)
	}
}

// A DUPLICATE WHOSE FIRST REQUEST STILL WAITS IS A CALL OF ITS OWN. A run may
// make the same call twice on purpose, two posts of one message among them:
// while a request for the first is still there to take its answer, the second
// runs, and each is recorded.
//
// Mutation: join a duplicate whatever its first's request (drop resentLocked's
// check that it is gone), and the second never runs.
func TestADuplicateWhoseFirstRequestStillWaitsIsACallOfItsOwn(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	held := newHeldTool("write")
	g := newGated(t, held)
	t.Cleanup(held.free)
	args := map[string]any{"value": "hold"}

	box := dialOver(t, g.url, (&severable{}).client())
	first := callBare(ctx, box, "write", args)
	second := callBare(ctx, box, "write", args)
	for range 2 {
		select {
		case <-held.entered:
		case <-ctx.Done():
			t.Fatal("both calls did not reach their tool")
		}
	}
	held.free()
	for _, answered := range []<-chan *mcp.CallToolResult{first, second} {
		if res := <-answered; res.IsError || text(res) != "write done" {
			t.Errorf("a call was answered %v %q, want its own answer", res.IsError, text(res))
		}
	}
	if rows := g.ledger.rows(); len(rows) != 2 {
		t.Errorf("the calls are recorded %+v, want both", rows)
	}
}
