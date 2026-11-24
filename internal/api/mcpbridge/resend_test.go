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
	"github.com/crewlet/crewlet/internal/sandbox"
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
func dialOver(ctx context.Context, t *testing.T, url string, client *http.Client) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
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
	callBare(ctx, dialOver(ctx, t, g.url, lost.client()), "write", args)
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached its tool")
	}
	lost.sever()
	mcpbridge.AwaitRequestsLost(g.session)

	// SENT AGAIN, as Kimi Code's reconnect does: a new session, a new id.
	again := callBare(ctx, dialOver(ctx, t, g.url, (&severable{}).client()), "write", args)
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

	box := dialOver(ctx, t, g.url, (&severable{}).client())
	first := callBare(ctx, box, "write", args)
	// THE SECOND ONLY ONCE THE FIRST RUNS, so it finds the first begun, the one
	// call it could join.
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the first call did not reach its tool")
	}
	second := callBare(ctx, box, "write", args)
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the second call did not reach its tool")
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

// lostFirst makes a call to write that its tool holds, over a connection it
// then cuts, and waits until the bridge has seen its POST dropped: the first
// call of a harness about to send it again.
func lostFirst(ctx context.Context, t *testing.T, g *gated, held *heldTool) {
	t.Helper()
	lost := &severable{}
	callBare(ctx, dialOver(ctx, t, g.url, lost.client()), "write", map[string]any{"value": "hold"})
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached its tool")
	}
	lost.sever()
	mcpbridge.AwaitRequestsLost(g.session)
}

// A CALL OF ANOTHER TOOL OR OTHER ARGUMENTS IS NEVER JOINED TO ONE LOST. Only
// the same tool with the same arguments is the call sent again: anything else
// joined would be answered with another call's result and never run. So with a
// call running whose POST the box dropped, a call to write with other
// arguments, or to another tool with the same, runs its own tool and is
// answered and recorded as its own, and the lost one is recorded as one nobody
// waited for.
//
// Mutation: drop resentLocked's tool or digest match, and the call joins.
func TestACallOfAnotherToolOrOtherArgumentsIsNeverJoinedToOneLost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, tool, value string
	}{
		{"other arguments", "write", "other"},
		{"another tool", "post", "hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			write, post := newHeldTool("write"), newHeldTool("post")
			g := newGated(t, write, post)
			t.Cleanup(write.free)
			t.Cleanup(post.free)
			joined := make(chan struct{}, 1)
			mcpbridge.OnJoin(g.bridge, func() { joined <- struct{}{} })
			lostFirst(ctx, t, g, write)

			ran := map[string]*heldTool{"write": write, "post": post}[tc.tool]
			again := callBare(ctx, dialOver(ctx, t, g.url, (&severable{}).client()), tc.tool,
				map[string]any{"value": tc.value})
			select {
			case <-ran.entered:
			case <-joined:
				t.Fatal("the call joined the one lost, and never ran its own tool")
			case <-ctx.Done():
				t.Fatal("the call neither ran nor joined")
			}
			write.free()
			post.free()
			select {
			case res := <-again:
				if res.IsError || text(res) != tc.tool+" done" {
					t.Fatalf("the call was answered %v %q, want its own tool's answer", res.IsError, text(res))
				}
			case <-ctx.Done():
				t.Fatal("the call was never answered")
			}
			g.bridge.Close("run-1")
			g.bridge.AwaitCalls(ctx)
			rows := g.ledger.rows()
			var abandoned []mcpbridge.Call
			for _, r := range rows {
				if r.Abandoned {
					abandoned = append(abandoned, r)
				}
			}
			if len(rows) != 2 || len(abandoned) != 1 || abandoned[0].Name != "write" ||
				abandoned[0].Args["value"] != "hold" {
				t.Errorf("the calls are recorded %+v, want two, the lost one alone as one nobody waited for", rows)
			}
		})
	}
}

// A CALL WHOSE POST WAS DROPPED AND NEVER SENT AGAIN IS RECORDED AS NOBODY'S.
// The box never read its answer, so the record says nobody waited for it
// (Abandoned), which hands its result to the job as it lands: recorded as one
// its harness was handed, the result reaches nobody.
//
// Mutation: drop conclude's check of the POST it came on, and the call is
// recorded as handed to its harness.
func TestACallWhosePostWasDroppedAndNeverSentAgainIsRecordedAsNobodys(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	held := newHeldTool("write")
	hold, holding := make(chan struct{}), make(chan struct{}, 1)
	g := newGatedWith(t, nil, func(s *mcpbridge.Session) {
		l := s.Ledger.(*ledger)
		l.hold, l.holding = hold, holding
	}, held)
	t.Cleanup(held.free)
	releaseRecord := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(releaseRecord)
	lostFirst(ctx, t, g, held)

	held.free()
	// ITS RECORD REACHED, so who answers it is decided, before the run ends,
	// whose end would answer a call still running.
	select {
	case <-holding:
	case <-ctx.Done():
		t.Fatal("the call was never recorded")
	}
	releaseRecord()
	g.bridge.Close("run-1")
	g.bridge.AwaitCalls(ctx)
	if rows := g.ledger.rows(); len(rows) != 1 || rows[0].Output != "write done" || !rows[0].Abandoned {
		t.Errorf("the call is recorded %+v, want once, with what it did, as one nobody waited for", rows)
	}
}

// A CALL SENT AGAIN BEFORE ITS FIRST BEGAN IS A CALL OF ITS OWN. Only a call
// whose entry is on the run's record and whose tool runs is joined: one still
// being written may yet be refused, and nothing would answer its joiner.
//
// Mutation: drop resentLocked's check that the first began, and the call joins.
func TestACallSentAgainBeforeItsFirstBeganIsACallOfItsOwn(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	held := newHeldTool("write")
	beginHold, beginHolding := make(chan struct{}), make(chan struct{}, 2)
	g := newGatedWith(t, nil, func(s *mcpbridge.Session) {
		l := s.Ledger.(*ledger)
		l.beginHold, l.beginHolding = beginHold, beginHolding
	}, held)
	t.Cleanup(held.free)
	releaseBegins := sync.OnceFunc(func() { close(beginHold) })
	t.Cleanup(releaseBegins)
	joined := make(chan struct{}, 1)
	mcpbridge.OnJoin(g.bridge, func() { joined <- struct{}{} })
	args := map[string]any{"value": "hold"}

	lost := &severable{}
	callBare(ctx, dialOver(ctx, t, g.url, lost.client()), "write", args)
	select {
	case <-beginHolding:
	case <-ctx.Done():
		t.Fatal("the call's entry was never written")
	}
	lost.sever()
	mcpbridge.AwaitRequestsLost(g.session)

	again := callBare(ctx, dialOver(ctx, t, g.url, (&severable{}).client()), "write", args)
	select {
	case <-beginHolding:
	case <-joined:
		t.Fatal("the call sent again joined a call not yet begun")
	case <-ctx.Done():
		t.Fatal("the call sent again neither joined nor ran")
	}
	releaseBegins()
	for range 2 {
		select {
		case <-held.entered:
		case <-ctx.Done():
			t.Fatal("both calls did not reach their tool")
		}
	}
	held.free()
	if res := <-again; res.IsError || text(res) != "write done" {
		t.Errorf("the call sent again was answered %v %q, want its own answer", res.IsError, text(res))
	}
	g.bridge.Close("run-1")
	g.bridge.AwaitCalls(ctx)
	if rows := g.ledger.rows(); len(rows) != 2 {
		t.Errorf("the calls are recorded %+v, want both", rows)
	}
}

// shortCeiling is a ceiling whose park clock runs one second
// ([toolloop.ParkBefore]), for a case over real connections, which no bubble
// can hold.
const shortCeiling = 6 * time.Second

// A CALL SENT AGAIN AFTER ITS PARK WENT TO A DROPPED POST JOINS IT. The park's
// answer (end your turn, you will be resumed) written onto a POST the box had
// dropped is never read, and Kimi Code sends the call again: run again, its
// tool takes effect twice while the job is parked on the first. So it joins
// the first and is answered with that park, and the tool runs once, recorded
// once, as the park's to tell.
//
// Mutation: join only a call whose answer is undecided (resentLocked without
// parkUnread), and the tool runs a second time.
func TestACallSentAgainAfterItsParkWentToADroppedPostJoinsIt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	held := newHeldTool("write")
	parking, release := make(chan struct{}, 1), make(chan struct{})
	g := newGatedWith(t, nil, func(s *mcpbridge.Session) {
		s.CallCeiling = shortCeiling
		s.Park = func(context.Context, sandbox.CallWait) (sandbox.AskOutcome, error) {
			parking <- struct{}{}
			<-release
			return sandbox.AskRecorded, nil
		}
	}, held)
	t.Cleanup(held.free)
	releasePark := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releasePark)
	joined := make(chan struct{}, 1)
	mcpbridge.OnJoin(g.bridge, func() { joined <- struct{}{} })
	args := map[string]any{"value": "hold"}

	lost := &severable{}
	callBare(ctx, dialOver(ctx, t, g.url, lost.client()), "write", args)
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached its tool")
	}
	// PARKED, ITS ANSWER NOT YET WRITTEN, AS THE BOX DROPS ITS POST: the park's
	// answer goes to a request nobody reads.
	select {
	case <-parking:
	case <-ctx.Done():
		t.Fatal("the call was never parked")
	}
	lost.sever()
	mcpbridge.AwaitRequestsLost(g.session)
	releasePark()

	again := callBare(ctx, dialOver(ctx, t, g.url, (&severable{}).client()), "write", args)
	select {
	case <-joined:
	case <-held.entered:
		t.Fatal("the call sent again ran its tool a second time")
	case <-ctx.Done():
		t.Fatal("the call sent again neither joined nor ran")
	}
	select {
	case res := <-again:
		if res.IsError || text(res) != mcpbridge.ParkedNote {
			t.Fatalf("the call sent again was answered %v %q, want the park's, %q", res.IsError, text(res),
				mcpbridge.ParkedNote)
		}
	case <-ctx.Done():
		t.Fatal("the call sent again was never answered")
	}
	held.free()
	g.bridge.Close("run-1")
	g.bridge.AwaitCalls(ctx)
	select {
	case <-held.entered:
		t.Fatal("the call sent again ran its tool a second time")
	default:
	}
	if rows := g.ledger.rows(); len(rows) != 1 || rows[0].Output != "write done" || !rows[0].Abandoned {
		t.Errorf("the call is recorded %+v, want once, as the park's, one nobody waited for", rows)
	}
}

// A CALL SENT AGAIN ONCE THE BOX READ ITS PARK IS A CALL OF ITS OWN. A box
// told the call still runs knows it, so the same call made again is the run's
// on purpose: it runs its tool, and each is recorded.
//
// Mutation: join a parked call whatever request it went out on (parkUnread
// reading c.request.gone(), which the answer's own write ends), and the call
// joins.
func TestACallSentAgainOnceTheBoxReadItsParkIsACallOfItsOwn(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	held := newHeldTool("write")
	g := newGatedWith(t, nil, func(s *mcpbridge.Session) {
		s.CallCeiling = shortCeiling
		s.Park = func(context.Context, sandbox.CallWait) (sandbox.AskOutcome, error) {
			return sandbox.AskRecorded, nil
		}
	}, held)
	t.Cleanup(held.free)
	joined := make(chan struct{}, 1)
	mcpbridge.OnJoin(g.bridge, func() { joined <- struct{}{} })
	args := map[string]any{"value": "hold"}

	box := dialOver(ctx, t, g.url, (&severable{}).client())
	first := callBare(ctx, box, "write", args)
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached its tool")
	}
	select {
	case res := <-first:
		if res.IsError || text(res) != mcpbridge.ParkedNote {
			t.Fatalf("the call was answered %v %q, want the park's", res.IsError, text(res))
		}
	case <-ctx.Done():
		t.Fatal("the call was never parked")
	}

	callBare(ctx, box, "write", args)
	select {
	case <-held.entered:
	case <-joined:
		t.Fatal("the call made again joined the one whose park the box read")
	case <-ctx.Done():
		t.Fatal("the call made again neither ran nor joined")
	}
	held.free()
	g.bridge.Close("run-1")
	g.bridge.AwaitCalls(ctx)
	if rows := g.ledger.rows(); len(rows) != 2 {
		t.Errorf("the calls are recorded %+v, want both", rows)
	}
}
