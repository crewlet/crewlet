package mcpbridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// A call's answer, started at once and kept going (the package doc's "A call's
// answer starts at once and keeps going").

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
