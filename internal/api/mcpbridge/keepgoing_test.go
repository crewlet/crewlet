package mcpbridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
)

// A call's answer, started at once and kept going, and how long a box waits
// for one (the package doc's "A call's answer starts at once and keeps going"
// and "How long a box waits for a call").

// dialHearing connects an MCP client to url as a harness's own client does,
// handing on every progress it is sent.
func dialHearing(t *testing.T, url string) (*mcp.ClientSession, <-chan *mcp.ProgressNotificationParams) {
	t.Helper()
	heard := make(chan *mcp.ProgressNotificationParams, 64)
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			select {
			case heard <- req.Params:
			default:
			}
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: httpxtest.Pool(t),
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, heard
}

// callWithToken makes a call to name on progress token token, and hands back
// its answer once the call returns.
func callWithToken(ctx context.Context, sess *mcp.ClientSession, name string, args any,
	token string) <-chan *mcp.CallToolResult {

	params := &mcp.CallToolParams{Name: name, Arguments: args}
	params.SetProgressToken(token)
	answered := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := sess.CallTool(ctx, params)
		if err != nil {
			res = &mcp.CallToolResult{IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "the call failed: " + err.Error()}}}
		}
		answered <- res
	}()
	return answered
}

// A BRIDGED CALL'S ANSWER STARTS BEFORE ITS TOOL RETURNS. A box's harness dials
// the bridge over streamable HTTP, whose SDK writes nothing on a call's
// response until its first event, and Claude Code gives that first byte a
// minute before it tells its model the call timed out, cancelling nothing,
// while the call runs on and its effect happens (2.1.310, measured). So every
// call the bridge runs is sent a progress on its own token the moment its tool
// starts: on the job's endpoint, while the tool is still held, and on each of
// the seat's servers beneath it alike.
//
// Mutation: build an opening's servers without keeping answers going, or the
// engine's own alone, or run a call without starting its answer, and a box is
// sent nothing before its answer.
func TestABridgedCallsAnswerStartsBeforeItsToolReturns(t *testing.T) {
	t.Parallel()
	t.Run("on the job's endpoint, while the tool is held", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		held := newHeldTool("write")
		g := newGated(t, held)
		sess, heard := dialHearing(t, g.url)
		t.Cleanup(held.free)

		answered := callWithToken(ctx, sess, "write", map[string]any{"value": "hold"}, "box-call-1")
		select {
		case <-held.entered:
		case <-ctx.Done():
			t.Fatal("the call never reached its tool")
		}
		select {
		case p := <-heard:
			if p.ProgressToken != "box-call-1" || p.Progress != 0 {
				t.Errorf("the box was sent %+v, want the first progress on its call's own token", p)
			}
		case <-ctx.Done():
			t.Fatal("nothing reached the box while its call's tool was held")
		}
		select {
		case res := <-answered:
			t.Fatalf("the call was answered %q before its tool returned", text(res))
		default:
		}
		held.free()
		if res := <-answered; res.IsError || text(res) != "write done" {
			t.Errorf("the call was answered %v %q, want what its tool did", res.IsError, text(res))
		}
	})

	t.Run("on one of the seat's servers", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		f := newServersFixture(t)
		sess, heard := dialHearing(t, mcpbridge.ServerURL(f.endpoint, "github"))
		if res := <-callWithToken(ctx, sess, "create_issue", map[string]any{}, "server-call-1"); res.IsError {
			t.Fatalf("the call on github's endpoint failed: %q", text(res))
		}
		select {
		case p := <-heard:
			if p.ProgressToken != "server-call-1" || p.Progress != 0 {
				t.Errorf("the box was sent %+v, want the first progress on its call's own token", p)
			}
		case <-ctx.Done():
			t.Fatal("a call on one of the seat's servers was sent nothing before its answer")
		}
	})
}

// wireTap records each POST a client makes and every byte it is answered, as
// the client reads it: what a harness's own transport sees, in the order it
// sees it.
type wireTap struct {
	next http.RoundTripper

	mu    sync.Mutex
	posts []*tapped
}

// tapped is one POST: what it carried, and what it was answered so far.
type tapped struct {
	request string

	mu     sync.Mutex
	answer bytes.Buffer
}

func (w *wireTap) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.Body == nil {
		return w.next.RoundTrip(r)
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	out := r.Clone(r.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	res, err := w.next.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	rec := &tapped{request: string(body)}
	w.mu.Lock()
	w.posts = append(w.posts, rec)
	w.mu.Unlock()
	res.Body = &tappedBody{ReadCloser: res.Body, into: rec}
	return res, nil
}

// tappedBody copies what a client reads of an answer into its record.
type tappedBody struct {
	io.ReadCloser
	into *tapped
}

func (b *tappedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.into.mu.Lock()
	b.into.answer.Write(p[:n])
	b.into.mu.Unlock()
	return n, err
}

// answerTo is what the one POST whose body holds marker was answered, as far
// as the client has read it.
func (w *wireTap) answerTo(t *testing.T, marker string) string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	var found []*tapped
	for _, p := range w.posts {
		if strings.Contains(p.request, marker) {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d POSTs carried %q, want one", len(found), marker)
	}
	found[0].mu.Lock()
	defer found[0].mu.Unlock()
	return found[0].answer.String()
}

// A CALL THE BRIDGE ANSWERS WITHOUT RUNNING GETS NO PROGRESS. The bridge
// answers some calls at once, never running them — one whose arguments did
// not parse, one made once the run's tools have closed — and nothing about
// such a call needs keeping going: its answer comes before any harness could
// give up on it. Read off the wire, in the order the box reads it: a call that
// runs, on the same session, starts its answer with a progress.
//
// Mutation: start a call's answer before the bridge's own refusals, and each
// refused call's answer is preceded by a progress.
func TestACallTheBridgeAnswersWithoutRunningGetsNoProgress(t *testing.T) {
	t.Parallel()
	g := newGated(t)
	tap := &wireTap{next: httpxtest.Pool(t).Transport}
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: g.url, HTTPClient: &http.Client{Transport: tap},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	if res := <-callWithToken(ctx, sess, "read_page", json.RawMessage(`["unparsed-arguments"]`),
		"unparsed-1"); !res.IsError {
		t.Fatalf("a call whose arguments did not parse was answered %q, as one that ran", text(res))
	}
	if res := <-callWithToken(ctx, sess, "read_page", map[string]any{"text": "ran-call"}, "ran-1"); res.IsError {
		t.Fatalf("a call that ran failed: %q", text(res))
	}
	g.closeFence(errors.New("this node no longer holds the seat"))
	if res := <-callWithToken(ctx, sess, "read_page", map[string]any{"text": "closed-call"},
		"closed-1"); !res.IsError {
		t.Fatalf("a call on closed tools was answered %q, as one that ran", text(res))
	}

	for _, tc := range []struct {
		marker, call string
		progress     int
	}{
		{"unparsed-arguments", "a call whose arguments did not parse", 0},
		{"ran-call", "a call that ran", 1},
		{"closed-call", "a call made once the run's tools had closed", 0},
	} {
		answer := tap.answerTo(t, tc.marker)
		if n := strings.Count(answer, "notifications/progress"); n != tc.progress {
			t.Errorf("%s was sent %d progress before its answer, want %d:\n%s", tc.call, n, tc.progress, answer)
		}
	}
}

// boundedTool is a tool that declares the longest one call of it runs, as a
// seat's MCP server's tools and `delegate` do.
type boundedTool struct {
	stubTool
	bound time.Duration
}

func (b *boundedTool) CallBound() time.Duration { return b.bound }

// boundedSurface is a seat's surface offering read_page alone, whose universe
// also holds a tool bound at each of bounds: offered or not, a tool is one a
// run can activate as it goes, after its box's settings were written.
func boundedSurface(t *testing.T, bounds ...time.Duration) *tools.Surface {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(&stubTool{name: "read_page"}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(read_page): %v", err)
	}
	for i, bound := range bounds {
		name := fmt.Sprintf("bounded_%d", i)
		if err := reg.Register(&boundedTool{stubTool: stubTool{name: name}, bound: bound},
			tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	return tools.NewSurface("execute", reg.Snapshot(), []string{"read_page"}).ForTurn(seatTurn())
}

// A BOX WAITS AS LONG AS THE BRIDGE CAN TAKE TO ANSWER A CALL. Kept going, a
// call keeps its harness waiting for as long as it runs, so the box is told
// how long that can be: the holder's wait where the bridge joined a fleet,
// the longest bound the surface's tools declare — never under the engine's
// one-call ceiling, and counting a tool not offered yet — and the bridge's own
// margin around a call.
//
// Mutation: ignore the bounds the tools declare, or leave the holder's wait
// out, and a box gives up a call the bridge could still answer.
func TestABoxWaitsAsLongAsTheBridgeCanTakeToAnswer(t *testing.T) {
	t.Parallel()
	alone := bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key")})
	floor, margin := crewletmcp.DefaultRequestTimeout, mcpbridge.CallMargin
	for _, tc := range []struct {
		name    string
		surface mcpbridge.Surface
		want    time.Duration
	}{
		{"a seat whose tools declare no bound", boundedSurface(t), floor + margin},
		{"a seat holding a tool bound past the floor", boundedSurface(t, 10*time.Second, 2*time.Hour),
			2*time.Hour + margin},
		{"a seat whose tools are bound under the floor", boundedSurface(t, 10*time.Second), floor + margin},
		{"a surface that is not a seat's", &probeSurface{}, floor + margin},
	} {
		if got := alone.CallTimeout(tc.surface); got != tc.want {
			t.Errorf("%s: a box waits %s for a call, want %s", tc.name, got, tc.want)
		}
	}

	// AND THE HOLDER'S WAIT, on a bridge that joined its fleet: a call may
	// wait that long before it reaches the node that runs it.
	const holderWait = 90 * time.Second
	joined := newFleet(t, 1, holderWait).members[0].bridge
	if got, want := joined.CallTimeout(boundedSurface(t, 2*time.Hour)), holderWait+2*time.Hour+margin; got != want {
		t.Errorf("a box of a bridge that joined its fleet waits %s for a call, want %s", got, want)
	}
}
