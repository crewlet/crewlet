package mcpbridge_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// How an opening ends while calls of it are still running (the package doc's
// "An opening ends without waiting for its calls").

// bubbled is a seat's session opened on a bridge of its own inside a bubble,
// whose box reaches the opening's own server over an in-process pipe: a
// transport a bubble's goroutines wait on durably, as none waits on a socket.
type bubbled struct {
	bridge  *mcpbridge.Bridge
	session *mcpbridge.Session
	ledger  *ledger
}

// openBubbled opens a session of the given tools, recorded on l. Called inside
// the bubble, so everything it builds is the bubble's.
func openBubbled(t *testing.T, l *ledger, extra ...tools.Callable) *bubbled {
	t.Helper()
	reg := tools.NewRegistry()
	var names []string
	for _, tool := range extra {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", tool.Name(), err)
		}
		names = append(names, tool.Name())
	}
	turn := seatTurn()
	turn.Calls = turnctx.NewCallLog()
	turn.Written = &turnctx.Written{}
	b := &bubbled{ledger: l, bridge: bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	})}
	// MOUNTED, as a node with a listener mounts it: a bridge nothing reaches
	// opens no session.
	_ = b.bridge.Handler()
	b.session = &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: tools.NewSurface("execute", reg.Snapshot(), names).ForTurn(turn), Ledger: l,
	}
	if b.bridge.Open(t.Context(), b.session) == "" {
		t.Fatal("no endpoint was minted")
	}
	return b
}

// connect connects a box to the opening's own server, its every write held by
// gate where gate is not nil.
func (b *bubbled) connect(t *testing.T, gate *writeGate) *mcp.ClientSession {
	t.Helper()
	serverEnd, clientEnd := mcp.NewInMemoryTransports()
	var server mcp.Transport = serverEnd
	if gate != nil {
		server = gatedTransport{Transport: serverEnd, gate: gate}
	}
	ss, err := mcpbridge.OpeningServers(b.session)[0].Connect(context.WithoutCancel(t.Context()), server, nil)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	box, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
		Connect(context.WithoutCancel(t.Context()), clientEnd, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = box.Close()
		_ = ss.Wait()
	})
	return box
}

// writeGate holds every write of a server's end of a pipe while it is shut, as
// a box that does not read holds an answer written to it.
type writeGate struct {
	mu   sync.Mutex
	shut chan struct{}
}

// hold shuts the gate.
func (g *writeGate) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shut = make(chan struct{})
}

// release opens it, letting every write it holds go.
func (g *writeGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut != nil {
		close(g.shut)
		g.shut = nil
	}
}

func (g *writeGate) pass() {
	g.mu.Lock()
	shut := g.shut
	g.mu.Unlock()
	if shut != nil {
		<-shut
	}
}

// gatedTransport is a server's end of a pipe whose every write passes gate.
type gatedTransport struct {
	mcp.Transport
	gate *writeGate
}

func (g gatedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := g.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return gatedConn{Connection: conn, gate: g.gate}, nil
}

// gatedConn writes once its gate lets it ([gatedTransport]).
type gatedConn struct {
	mcp.Connection
	gate *writeGate
}

func (c gatedConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	c.gate.pass()
	return c.Connection.Write(ctx, msg)
}

// callFromBox makes a call to name from box, on progress token token where it
// is not empty, and hands back its answer once the call returns — or its
// failure, as a failed answer that names it.
func callFromBox(ctx context.Context, box *mcp.ClientSession, name string, args map[string]any,
	token string) <-chan *mcp.CallToolResult {

	params := &mcp.CallToolParams{Name: name, Arguments: args}
	if token != "" {
		params.SetProgressToken(token)
	}
	answered := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := box.CallTool(ctx, params)
		if err != nil {
			res = &mcp.CallToolResult{IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "the call failed: " + err.Error()}}}
		}
		answered <- res
	}()
	return answered
}

// closedNow reports whether ch is closed at this moment.
func closedNow(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// EVERY ENDING OF AN OPENING RETURNS WHILE A CALL OF IT IS HELD. The SDK's close
// of a session waits for every request in flight on it, and writes no answer
// once it has begun: an ending that closed first waited out a call still
// running, so the run's settle waited on it, a seat handed on kept its lease
// for as long, and the call's answer was never written, its harness waiting
// for the box's own wait to run out. So each ending — the run's close, a
// relaunch's opening of its next job, the seat leaving this node, a recovery's
// opening of the same job — answers the box's call while its tool is still
// held, in the words of how it ended and not as a failure, and returns; and the
// call's tool runs on, and is recorded once it returns — one update of the entry
// written ahead of it — as one nobody waited for.
//
// Mutation: let an ending answer no call still running (`false &&` before its
// cut), and the box is never answered while its call's tool is held; answer it
// as failed, and the box is told its call failed; take a relaunch's opening for
// a recovery's, and the box is told the other ending's words.
func TestEveryEndingOfAnOpeningReturnsWhileACallIsHeld(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, g *gated)
		want string
	}{
		{"the run's close", func(_ *testing.T, g *gated) { g.bridge.Close("run-1") },
			mcpbridge.EndedRunningNote},
		{"a relaunch's opening of the run's next job", func(t *testing.T, g *gated) {
			if g.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "run-1", Launch: "job-2",
				Handle: "dev", Role: "Engineer", Surface: g.surface, Ledger: &ledger{}}) == "" {
				t.Error("the relaunch's opening minted no endpoint")
			}
		}, mcpbridge.EndedRunningNote},
		{"the seat leaving this node", func(_ *testing.T, g *gated) { g.bridge.ReleaseSeat(seat()) },
			mcpbridge.StillRunningNote},
		{"a recovery's opening of the same job", func(t *testing.T, g *gated) {
			if g.bridge.Open(t.Context(), g.session) == "" {
				t.Error("the recovery's opening minted no endpoint")
			}
		}, mcpbridge.StillRunningNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			held := newHeldTool("write")
			g := newGated(t, held)
			sess, _ := dialHearing(t, g.url)
			t.Cleanup(held.free)

			answered := callWithToken(ctx, sess, "write", map[string]any{"value": "hold"}, "held-1")
			select {
			case <-held.entered:
			case <-ctx.Done():
				t.Fatal("the call never reached its tool")
			}
			ended := make(chan struct{})
			go func() {
				defer close(ended)
				tc.end(t, g)
			}()
			select {
			case <-ended:
			case <-ctx.Done():
				t.Fatal("the ending waited for a call still running")
			}
			select {
			case res := <-answered:
				if res.IsError || text(res) != tc.want {
					t.Errorf("the box's call was answered %v %q, want %q, and not as failed",
						res.IsError, text(res), tc.want)
				}
			case <-ctx.Done():
				t.Fatal("the box was not answered while its call's tool was held")
			}
			if rows := g.ledger.rows(); len(rows) != 0 {
				t.Fatalf("the call is recorded before its tool returned: %+v", rows)
			}

			held.free()
			g.bridge.AwaitCalls(ctx)
			rows := g.ledger.rows()
			if len(rows) != 1 || rows[0].Output != "write done" || !rows[0].Abandoned {
				t.Fatalf("the call is recorded %+v, want it once, with what its tool did, as one "+
					"nobody waited for", rows)
			}
			if trail := g.ledger.order(); !slices.Equal(trail, []string{"begin write", "record write"}) {
				t.Errorf("the ledger took %v, want the call's entry and then its one update", trail)
			}
		})
	}
}

// AN OPENING'S END ANSWERS ITS RUNNING CALLS BEFORE IT CLOSES THEIR SESSIONS.
// The SDK writes no answer on a session whose close has begun, so the end
// waits for its answer to a call still running to be out before it closes the
// box's session: held between the call's handler and the SDK's write, the
// answer still reaches the box, then the session is closed and the end
// returns. (That the call's beat is stopped before that answer, as before any,
// is the tool session's, held by its
// TestARunningCallsAnswerWaitsForAProgressAlreadyOnItsWay.)
//
// Mutation: close the sessions without waiting for the answers (`true ||` before
// the end's wait), and the SDK refuses the answer, the box's call failing on a
// closed session.
func TestAnOpeningsEndAnswersItsRunningCallsBeforeItClosesTheirSessions(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		held := newHeldTool("write")
		b := openBubbled(t, &ledger{}, held)
		holding, withheld := make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(withheld) })
		mcpbridge.OpeningServers(b.session)[0].AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				res, err := next(ctx, method, req)
				if method == "tools/call" {
					close(holding)
					<-withheld
				}
				return res, err
			}
		})
		box := b.connect(t, nil)
		t.Cleanup(held.free)
		t.Cleanup(release)

		answered := callFromBox(context.WithoutCancel(t.Context()), box, "write", map[string]any{"value": "hold"}, "write-1")
		<-held.entered
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			b.bridge.Close("run-1")
		}()
		// THE END'S ANSWER HAS LEFT THE CALL'S HANDLER, held short of the
		// SDK's write.
		<-holding
		synctest.Wait()
		if closedNow(ended) {
			t.Fatal("the end returned with its answer to the box unwritten")
		}
		release()
		if res := <-answered; res.IsError || text(res) != mcpbridge.EndedRunningNote {
			t.Fatalf("the box was answered %v %q, want the end's answer %q", res.IsError, text(res),
				mcpbridge.EndedRunningNote)
		}
		synctest.Wait()
		if !closedNow(ended) {
			t.Error("the end did not return once its answer was out")
		}
		if n := b.session.Connections(); n != 0 {
			t.Errorf("%d of the box's sessions are open after the end", n)
		}

		held.free()
		b.bridge.AwaitCalls(t.Context())
		if rows := b.ledger.rows(); len(rows) != 1 || !rows[0].Abandoned {
			t.Errorf("the call is recorded %+v, want it once, as one nobody waited for", rows)
		}
	})
}

// AN OPENING'S END IS HELD NO LONGER THAN ITS BOUND BY A BOX THAT DOES NOT READ.
// An answer the end hands its box is out once the box reads it, and a box that
// does not read must not hold an ending: the end waits for it [EndAnswerWait],
// then returns with the box's session left open behind it, and that session is
// closed once the box has read its answer.
//
// Mutation: wait past the bound (an hour added to the end's timer), and the end
// is still waiting once the bound has passed.
func TestAnOpeningsEndIsHeldNoLongerThanItsBoundByABoxThatDoesNotRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		held := newHeldTool("write")
		b := openBubbled(t, &ledger{}, held)
		gate := &writeGate{}
		box := b.connect(t, gate)
		t.Cleanup(held.free)
		t.Cleanup(gate.release)

		answered := callFromBox(context.WithoutCancel(t.Context()), box, "write", map[string]any{"value": "hold"}, "")
		<-held.entered
		// THE BOX STOPS READING.
		gate.hold()
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			b.bridge.Close("run-1")
		}()
		synctest.Wait()
		if closedNow(ended) {
			t.Fatal("the end returned before its bound, its answer to the box unwritten")
		}
		time.Sleep(mcpbridge.EndAnswerWait)
		synctest.Wait()
		if !closedNow(ended) {
			t.Fatal("the end waited past its bound for a box that does not read")
		}
		if n := b.session.Connections(); n == 0 {
			t.Error("the box's session was closed with its answer still on its way")
		}

		// THE BOX READS AGAIN, and its answer is the end's.
		gate.release()
		if res := <-answered; res.IsError || text(res) != mcpbridge.EndedRunningNote {
			t.Errorf("the box, reading again, was answered %v %q, want %q", res.IsError, text(res),
				mcpbridge.EndedRunningNote)
		}
		mcpbridge.AwaitClosing(b.bridge)
		if n := b.session.Connections(); n != 0 {
			t.Errorf("%d of the box's sessions are open once its answer was out", n)
		}
		held.free()
		b.bridge.AwaitCalls(t.Context())
	})
}

// A CALL WHOSE TOOL RETURNED BEFORE ITS OPENING ENDED IS ANSWERED WITH ITS OWN
// RESULT. A call's own answer follows its record, so an end can come while a
// call whose tool has returned is still being recorded: that call's answer is
// its own, which the end waits for before it closes the box's session, and the
// call is recorded as one its harness was handed — never both answered by the
// end and recorded as answered, nor the reverse.
//
// Mutation: never let a call's record take its own answer (claim nothing), and
// the end answers it as still running, and returns while its record is still
// being made.
func TestACallWhoseToolReturnedBeforeItsOpeningEndedIsAnsweredWithItsOwnResult(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		l := &ledger{hold: make(chan struct{}), holding: make(chan struct{}, 1)}
		record := sync.OnceFunc(func() { close(l.hold) })
		b := openBubbled(t, l, &stubTool{name: "write"})
		box := b.connect(t, nil)
		t.Cleanup(record)

		answered := callFromBox(context.WithoutCancel(t.Context()), box, "write", map[string]any{}, "write-1")
		// ITS TOOL HAS RETURNED, and its record is being made.
		<-l.holding
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			b.bridge.Close("run-1")
		}()
		synctest.Wait()
		if closedNow(ended) {
			t.Fatal("the end returned before the answer of a call whose tool had returned was out")
		}
		record()
		if res := <-answered; res.IsError || text(res) != "write ok" {
			t.Fatalf("the call was answered %v %q, want its own result", res.IsError, text(res))
		}
		synctest.Wait()
		if !closedNow(ended) {
			t.Error("the end did not return once that answer was out")
		}
		if rows := l.rows(); len(rows) != 1 || rows[0].Abandoned {
			t.Errorf("the call is recorded %+v, want it once, as one its harness was handed", rows)
		}
	})
}

// answerHold is one answer held between its call's handler and the SDK's
// write of it ([holdAnswers]).
type answerHold struct {
	holding  chan struct{}
	said     sync.Once
	released chan struct{}
	release  func()
}

// holdAnswers holds, on the opening's own server, every answer to a call
// whose text is one of texts, between the call's handler and the SDK's write
// of it: each says so on its hold's holding, the first time, and waits until
// that hold is let go. Added before the box connects; a case lets every hold
// go in a cleanup it registers once the box is connected, so that one runs
// before the box's session is closed and waited for ([letGo]).
func holdAnswers(b *bubbled, texts ...string) map[string]*answerHold {
	holds := map[string]*answerHold{}
	for _, said := range texts {
		h := &answerHold{holding: make(chan struct{}), released: make(chan struct{})}
		h.release = sync.OnceFunc(func() { close(h.released) })
		holds[said] = h
	}
	mcpbridge.OpeningServers(b.session)[0].AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if answered, ok := res.(*mcp.CallToolResult); ok && method == "tools/call" {
				if h := holds[text(answered)]; h != nil {
					h.said.Do(func() { close(h.holding) })
					<-h.released
				}
			}
			return res, err
		}
	})
	return holds
}

// letGo lets every hold go ([holdAnswers]).
func letGo(holds map[string]*answerHold) {
	for _, h := range holds {
		h.release()
	}
}

// AN OPENING HOLDS A CALL UNTIL ITS ANSWER IS OUT, WHATEVER IT ADMITS AFTER.
// The opening lets go of a call it admitted only once the call's answer is
// decided and out, which it reads as it admits the next: a box's parallel
// batch admits a long call beside quick ones, and an end that found the long
// one let go would answer it nothing and wait out its tool on the session's
// close, while one that found a call whose own answer was still on its way let
// go would close the session under that answer, which its box would then never
// read. So, with a later call of the opening admitted and returned after it, a
// call still running is answered by the end at once, the end returning; and a
// call whose own answer is held short of the SDK's write is waited for, its
// box reading that answer and not a failure.
//
// Mutation: let go of every held call at each admission (`true ||` before the
// opening's test of a held call's answer), and the end neither answers the
// call still running nor waits for the answer on its way; let go of a call
// whose answer is decided without asking whether it is out (`true ||` before
// that question), and the end closes the session under the second.
func TestAnOpeningHoldsACallUntilItsAnswerIsOutWhateverItAdmitsAfter(t *testing.T) {
	t.Parallel()
	t.Run("a call still running", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			held := newHeldTool("write")
			b := openBubbled(t, &ledger{}, held)
			box := b.connect(t, nil)
			t.Cleanup(held.free)

			first := callFromBox(context.WithoutCancel(t.Context()), box, "write",
				map[string]any{"value": "hold"}, "")
			<-held.entered
			// A LATER CALL OF THE OPENING is admitted, and returns.
			if res := <-callFromBox(context.WithoutCancel(t.Context()), box, "write",
				map[string]any{"value": "go"}, ""); res.IsError || text(res) != "write done" {
				t.Fatalf("the later call was answered %v %q, want its own result", res.IsError, text(res))
			}
			ended := make(chan struct{})
			go func() {
				defer close(ended)
				b.bridge.Close("run-1")
			}()
			synctest.Wait()
			select {
			case res := <-first:
				if res.IsError || text(res) != mcpbridge.EndedRunningNote {
					t.Errorf("the call still running was answered %v %q as its opening ended, want %q",
						res.IsError, text(res), mcpbridge.EndedRunningNote)
				}
			default:
				t.Error("the call still running was not answered as its opening ended, a later call " +
					"of the opening having been admitted")
			}
			if !closedNow(ended) {
				t.Error("the end waited for the call still running")
			}
			held.free()
			<-ended
			b.bridge.AwaitCalls(t.Context())
		})
	})
	t.Run("a call whose own answer is on its way", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			b := openBubbled(t, &ledger{}, &stubTool{name: "write"}, &stubTool{name: "read"})
			holds := holdAnswers(b, "write ok")
			box := b.connect(t, nil)
			t.Cleanup(func() { letGo(holds) })

			first := callFromBox(context.WithoutCancel(t.Context()), box, "write", map[string]any{}, "")
			// ITS TOOL RETURNED, it is recorded, and its own answer is held
			// short of the SDK's write.
			<-holds["write ok"].holding
			if res := <-callFromBox(context.WithoutCancel(t.Context()), box, "read",
				map[string]any{}, ""); res.IsError || text(res) != "read ok" {
				t.Fatalf("the later call was answered %v %q, want its own result", res.IsError, text(res))
			}
			synctest.Wait()
			ended := make(chan struct{})
			go func() {
				defer close(ended)
				b.bridge.Close("run-1")
			}()
			synctest.Wait()
			if closedNow(ended) {
				t.Fatal("the end returned with the first call's own answer unwritten")
			}
			holds["write ok"].release()
			if res := <-first; res.IsError || text(res) != "write ok" {
				t.Errorf("the call whose answer was on its way as its opening ended was answered %v %q, "+
					"want its own result", res.IsError, text(res))
			}
			synctest.Wait()
			if !closedNow(ended) {
				t.Error("the end did not return once that answer was out")
			}
		})
	})
}

// AN OPENING'S END WAITS FOR THE ANSWER OF A CALL IT REFUSED BEFORE IT CLOSES.
// A call that reaches an opening after its end runs nothing and is told what
// the end means (below), which its box reads only where that answer is
// written before the opening's sessions close: the SDK writes no answer on a
// session whose close has begun, and the box's call would fail on the closed
// session instead. So the end holds every answer the opening gives without
// running a call, as it holds a running call's: with the end waiting for its
// answer to a call still running, a call that reaches the opening then is
// refused, its refusal held short of the SDK's write, and the end does not
// close the session until that answer too is out, which the box reads.
//
// Mutation: hold no refusal for the end to wait for (`false &&` before the
// opening's hold of one), and the end closes the session under the refusal,
// the box's call failing on the closed session.
func TestAnOpeningsEndWaitsForTheAnswerOfACallItRefusedBeforeItCloses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		held := newHeldTool("write")
		b := openBubbled(t, &ledger{}, held, &stubTool{name: "read"})
		holds := holdAnswers(b, mcpbridge.EndedRunningNote, mcpbridge.EndedNote)
		box := b.connect(t, nil)
		t.Cleanup(held.free)
		t.Cleanup(func() { letGo(holds) })

		running := callFromBox(context.WithoutCancel(t.Context()), box, "write",
			map[string]any{"value": "hold"}, "")
		<-held.entered
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			b.bridge.Close("run-1")
		}()
		// THE END'S ANSWER TO THE CALL STILL RUNNING is held short of its
		// write, so the end waits; and a call reaches the opening then.
		<-holds[mcpbridge.EndedRunningNote].holding
		refused := callFromBox(context.WithoutCancel(t.Context()), box, "read", map[string]any{}, "")
		<-holds[mcpbridge.EndedNote].holding
		holds[mcpbridge.EndedRunningNote].release()
		if res := <-running; res.IsError || text(res) != mcpbridge.EndedRunningNote {
			t.Fatalf("the call still running was answered %v %q, want %q", res.IsError, text(res),
				mcpbridge.EndedRunningNote)
		}
		synctest.Wait()
		if closedNow(ended) {
			t.Fatal("the end returned with its refusal of a later call unwritten")
		}
		holds[mcpbridge.EndedNote].release()
		if res := <-refused; !res.IsError || text(res) != mcpbridge.EndedNote {
			t.Errorf("the call that reached the opening after its end was answered %v %q, want %q, failed",
				res.IsError, text(res), mcpbridge.EndedNote)
		}
		synctest.Wait()
		if !closedNow(ended) {
			t.Error("the end did not return once every answer it gave was out")
		}
		held.free()
		b.bridge.AwaitCalls(t.Context())
	})
}

// A CALL REACHING AN OPENING AFTER ITS END IS TOLD WHAT THE END MEANS. Nothing
// runs it and nothing records it, as ever; but where the run goes on — the seat
// left this node, or a recovery opened its job again — the box is told the
// session it reached was closing and to call again, which reaches the opening
// that serves it now, and never that its run has ended, which would send a live
// run's model to end its turn.
//
// Mutation: answer every such call as the run's end (`endedNote` whatever the
// opening's end), and a box that goes on is told to end its turn.
func TestACallReachingAnOpeningAfterItsEndIsToldWhatTheEndMeans(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, g *gated)
		want string
	}{
		{"the run's close", func(_ *testing.T, g *gated) { g.bridge.Close("run-1") }, mcpbridge.EndedNote},
		{"a relaunch's opening of the run's next job", func(t *testing.T, g *gated) {
			g.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "run-1", Launch: "job-2",
				Handle: "dev", Role: "Engineer", Surface: g.surface, Ledger: &ledger{}})
		}, mcpbridge.EndedNote},
		{"the seat leaving this node", func(_ *testing.T, g *gated) { g.bridge.ReleaseSeat(seat()) },
			mcpbridge.ClosingNote},
		{"a recovery's opening of the same job", func(t *testing.T, g *gated) {
			g.bridge.Open(t.Context(), g.session)
		}, mcpbridge.ClosingNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newGated(t)
			ended := mcpbridge.Dispatcher(g.session)
			tc.end(t, g)
			res := ended.Call(toolsession.Call{Name: "read_page", Arguments: map[string]any{}})
			if !res.Failed || res.Output != tc.want {
				t.Errorf("a call reaching the opening after its end was answered %v %q, want %q, failed",
					res.Failed, res.Output, tc.want)
			}
			if rows := g.ledger.rows(); len(rows) != 0 {
				t.Errorf("the call is recorded %+v, though nothing ran it", rows)
			}
			if n := len(g.byName["read_page"].seen()); n != 0 {
				t.Errorf("the tool ran %d times for a call that reached an ended opening", n)
			}
		})
	}
}

// A BRIDGE AWAITS EVERY CALL IT RUNS UNTIL IT IS RECORDED. A call its opening's
// end answered as still running runs on, the bridge's, under the run's
// context, and is recorded once its tool returns; a caller that must not tear
// down what such a call writes through — a node that has given its seats back
// and is stopping — asks the bridge to wait for it. It returns at once where no
// such call runs, a call on an opening still serving not being one; it returns
// once each call whose opening had ended is recorded; and it returns once its
// context ends, the call running on.
//
// Mutation: wait for no call (`false &&` before the bridge's test of a call's
// opening), and the bridge answers that its calls are done while one still
// runs.
func TestABridgeAwaitsEveryCallItRunsUntilItIsRecorded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		held := newHeldTool("delegate")
		b := openBubbled(t, &ledger{}, held)
		t.Cleanup(held.free)
		// NOTHING RUNS.
		b.bridge.AwaitCalls(t.Context())

		opened := mcpbridge.Dispatcher(b.session)
		answered := make(chan toolsession.Result, 1)
		go func() {
			answered <- opened.Call(toolsession.Call{Name: "delegate",
				Arguments: map[string]any{"value": "hold"}, Abandoned: make(chan struct{})})
		}()
		<-held.entered
		// A CALL ON AN OPENING STILL SERVING is not waited for.
		b.bridge.AwaitCalls(t.Context())

		b.bridge.Close("run-1")
		if res := <-answered; res.Failed || res.Output != mcpbridge.EndedRunningNote {
			t.Fatalf("the call was answered %v %q as its opening ended, want %q", res.Failed, res.Output,
				mcpbridge.EndedRunningNote)
		}
		// ITS CONTEXT ENDS THE WAIT, the call running on.
		ctx, cancel := context.WithCancel(t.Context())
		cut := make(chan struct{})
		go func() {
			defer close(cut)
			b.bridge.AwaitCalls(ctx)
		}()
		synctest.Wait()
		if closedNow(cut) {
			t.Fatal("the bridge answered that its calls were done while one still ran")
		}
		cancel()
		synctest.Wait()
		if !closedNow(cut) {
			t.Fatal("the bridge went on waiting once its caller's context ended")
		}

		awaited := make(chan struct{})
		go func() {
			defer close(awaited)
			b.bridge.AwaitCalls(t.Context())
		}()
		synctest.Wait()
		if closedNow(awaited) {
			t.Fatal("the bridge answered that its calls were done while one still ran")
		}
		held.free()
		synctest.Wait()
		if !closedNow(awaited) {
			t.Fatal("the bridge did not answer once the call was recorded")
		}
		if rows := b.ledger.rows(); len(rows) != 1 || !rows[0].Abandoned {
			t.Errorf("the call is recorded %+v, want it once, as one nobody waited for", rows)
		}
	})
}

// A CALL RETURNING AFTER ITS OPENING ENDED STOPS NOTHING. The fence after a
// call closes an opening's tools where its seat moved while the call ran, and
// says so: every later call is refused, and the run's harness told to end its
// turn. A seat leaving this node leaves the node's grants before its release
// ends the opening, and that release answers a call still running and lets it
// run on, so such a call returns to a closed fence on an opening that has
// ended — whose end already refuses every later call in its own words, and
// tells a box that goes on to call again. So the call is recorded, as one
// nobody waited for, and nothing is stopped or said of the run: no warning
// that its harness is told to end its turn, which is false of a run that goes
// on with the seat's next holder.
//
// NOT PARALLEL: it installs the process's log sink, which only a case running
// alone may do.
//
// Mutation: stop an opening whatever its end (`false &&` before the stop's
// test of it), and the run is said to be stopped as its call returns.
func TestACallReturningAfterItsOpeningEndedStopsNothing(t *testing.T) {
	logs := &syncBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatText, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	held := newHeldTool("delegate")
	g := newGated(t, held)
	t.Cleanup(held.free)
	opened := mcpbridge.Dispatcher(g.session)

	answered := make(chan toolsession.Result, 1)
	go func() {
		answered <- opened.Call(toolsession.Call{Name: "delegate",
			Arguments: map[string]any{"value": "hold"}, Abandoned: make(chan struct{})})
	}()
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached its tool")
	}
	// THE SEAT LEAVES THIS NODE: out of the node's grants first, and then
	// its release ends the opening.
	g.closeFence(errors.New("this node no longer holds the seat"))
	g.bridge.ReleaseSeat(seat())
	select {
	case res := <-answered:
		if res.Failed || res.Output != mcpbridge.StillRunningNote {
			t.Fatalf("the call still running as its seat left was answered %v %q, want %q",
				res.Failed, res.Output, mcpbridge.StillRunningNote)
		}
	case <-ctx.Done():
		t.Fatal("the call still running as its seat left was not answered")
	}
	held.free()
	g.bridge.AwaitCalls(ctx)

	if rows := g.ledger.rows(); len(rows) != 1 || rows[0].Output != "delegate done" || !rows[0].Abandoned {
		t.Errorf("the call is recorded %+v, want it once, with what its tool did, as one nobody "+
			"waited for", rows)
	}
	said := logs.String()
	if !strings.Contains(said, "mcp_bridge_answered_at_end") {
		t.Fatalf("the log heard nothing of the opening's end, so it cannot say what was not said:\n%s", said)
	}
	if strings.Contains(said, "mcp_bridge_run_stopped") {
		t.Errorf("a call returning after its opening ended stopped the run:\n%s", said)
	}
}
