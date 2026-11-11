package toolsession_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

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

// A CALL'S REQUEST IS OVER ONLY ONCE ITS ANSWER IS WRITTEN. A dispatcher that
// answers a call while its tool runs on — the bridge, as the opening a call was
// made on ends — waits for that answer to be out before it closes the session,
// whose close writes no answer once it has begun; what it waits on is the
// request's end ([toolsession.Call.Over]). That end comes after the answer's
// write by the SDK's own order (go-sdk v1.8.0 cancels a request's context once
// its response's write has returned), which is its behaviour rather than its
// promise, so it is held here: with the answer held between the handler's
// return and the SDK's write, by a middleware between the two, the request is
// not over; once the client has the answer, it is.
//
// Mutation: hand the dispatcher the beat's own end as Over (`going.unread`,
// which the handler's return ends), and the request reads as over while its
// answer is still held short of its write.
func TestACallsRequestIsOverOnlyOnceItsAnswerIsWritten(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := &overDispatcher{overs: make(chan (<-chan struct{}), 1)}
		s := toolsession.NewServer("crewlet", "a seat's tools", "", d)
		// KEPT GOING, as the bridge's are, so the beat a call's answer is kept
		// going with ends as the handler returns: the end Over must not be.
		s.KeepAnswersGoing(cadence)
		holding, held := make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(held) })
		s.MCP().AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				res, err := next(ctx, method, req)
				if method == "tools/call" {
					close(holding)
					<-held
				}
				return res, err
			}
		})
		serverEnd, clientEnd := mcp.NewInMemoryTransports()
		ss, err := s.MCP().Connect(context.WithoutCancel(t.Context()), serverEnd, nil)
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

		answered := callInBackground(context.WithoutCancel(t.Context()), cs, "now", "now-1")
		<-holding
		over := <-d.overs
		if over == nil {
			t.Fatal("a call a request carries was handed no Over")
		}
		if closedNow(over) {
			t.Fatal("the request is over while its answer is held short of its write")
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
