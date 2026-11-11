package toolsession_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/toolsession"
)

// overDispatcher offers `now`, answers it at once as a call that ran, and hands
// the case the [toolsession.Call.Over] each call came with.
type overDispatcher struct{ overs chan (<-chan struct{}) }

func (d *overDispatcher) Offered() []toolsession.Tool { return []toolsession.Tool{tool("now")} }

func (d *overDispatcher) Revision() uint64 { return 0 }

func (d *overDispatcher) Call(call toolsession.Call) toolsession.Result {
	if call.Running != nil {
		call.Running()
	}
	d.overs <- call.Over
	return toolsession.Result{Output: "now done"}
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

// answerGate holds the first response a server's end of a pipe writes once it
// is armed — the answer to a call, written by the SDK as it handles the
// call's result — until it is let go, saying so on holding as it starts.
type answerGate struct {
	armed            atomic.Bool
	holding, release chan struct{}
}

// answerGatedTransport is a server's end of a pipe whose writes pass gate.
type answerGatedTransport struct {
	mcp.Transport
	gate *answerGate
}

func (g answerGatedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := g.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return answerGatedConn{Connection: conn, gate: g.gate}, nil
}

// answerGatedConn holds the response its gate is armed for, inside its write
// ([answerGate]); every other message, a progress among them, passes.
type answerGatedConn struct {
	mcp.Connection
	gate *answerGate
}

func (c answerGatedConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if _, ok := msg.(*jsonrpc.Response); ok && c.gate.armed.CompareAndSwap(true, false) {
		close(c.gate.holding)
		<-c.gate.release
	}
	return c.Connection.Write(ctx, msg)
}

// A CALL'S REQUEST IS OVER ONLY ONCE ITS ANSWER IS WRITTEN. A dispatcher that
// answers a call while its tool runs on — the bridge, as the opening a call was
// made on ends — waits for that answer to be out before it closes the session,
// whose close writes no answer once it has begun; what it waits on is the
// request's end ([toolsession.Call.Over]). That end comes after the answer's
// write by the SDK's own order (go-sdk v1.8.0 writes a call's response, then
// cancels its request), which is its behaviour rather than its promise, so it
// is held here at the write itself: with the answer's write held on the
// transport, the SDK already writing it, the request is not over; once the
// write has returned and the client has the answer, it is.
//
// Mutation: hand the dispatcher the beat's own end as Over (`going.unread`,
// which the handler's return ends), or have the SDK cancel the request ahead
// of its response's write, and the request reads as over while its answer is
// still held in its write.
func TestACallsRequestIsOverOnlyOnceItsAnswerIsWritten(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := &overDispatcher{overs: make(chan (<-chan struct{}), 1)}
		s := toolsession.NewServer("crewlet", "a seat's tools", "", d)
		// KEPT GOING, as the bridge's are, so the beat a call's answer is kept
		// going with ends as the handler returns: the end Over must not be.
		s.KeepAnswersGoing(cadence)
		gate := &answerGate{holding: make(chan struct{}), release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(gate.release) })
		serverEnd, clientEnd := mcp.NewInMemoryTransports()
		ss, err := s.MCP().Connect(context.WithoutCancel(t.Context()),
			answerGatedTransport{Transport: serverEnd, gate: gate}, nil)
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "fake-cli", Version: "1"}, nil).
			Connect(context.WithoutCancel(t.Context()), clientEnd, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() {
			_ = cs.Close()
			_ = ss.Wait()
		})
		// LET GO FIRST, so a case that fails with the answer held leaves the
		// session nothing to wait for.
		t.Cleanup(release)

		// ARMED ONCE THE SESSION IS UP, so the gate holds the call's answer
		// and never the answer the session's start was given.
		gate.armed.Store(true)
		answered := callInBackground(context.WithoutCancel(t.Context()), cs, "now", "now-1")
		<-gate.holding
		over := <-d.overs
		// EVERYTHING ELSE THE SERVER DOES FOR THE CALL HAS HAPPENED, its write
		// held.
		synctest.Wait()
		if over == nil {
			t.Fatal("a call a request carries was handed no Over")
		}
		if closedNow(over) {
			t.Fatal("the request is over while its answer's write is held")
		}
		release()
		if res := <-answered; res.IsError || text(res) != "now done" {
			t.Fatalf("the call was answered %v %q, want its answer", res.IsError, text(res))
		}
		synctest.Wait()
		if !closedNow(over) {
			t.Error("the request is not over once the client has its answer")
		}
	})
}
