package operator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// A STREAM THAT OPENS AS ITS CATALOGUE IS REPLACED IS STILL TOLD. A listen
// resolves the catalogue it is served by at the top of its request, and the
// SDK registers its subscription only later, once the request is read: held
// between the two while the node swaps and announces, it was in nobody's list
// when the announcement went out, subscribed on the catalogue just replaced,
// and heard nothing until the next change — days, perhaps — while its client
// held whatever it had listed before. Every stream is therefore told once as it
// opens, which also covers a client whose first listing raced its own stream
// and reached a node that had not rebuilt yet.
func TestAStreamOpenedAsItsCatalogueIsReplacedIsStillTold(t *testing.T) {
	t.Parallel()
	listeners := operator.NewListeners()
	work := builtin.WorkDeps{Reader: stubWorkReader{}, Actor: operator.WorkActor(nil, nil)}
	reads := newSurface(t, operator.Options{Work: work, Listeners: listeners})
	steers := newSurface(t, operator.Options{
		Work:      work,
		Steer:     builtin.SteerDeps{Asker: struct{ builtin.FleetAsker }{}, Actor: operator.WorkActor(nil, nil)},
		Listeners: listeners,
	})
	var current atomic.Pointer[operator.Server]
	current.Store(reads)
	resolved, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RESOLVED AT THE TOP, as the app resolves it for every request.
		server := current.Load()
		r = r.WithContext(auth.WithOperator(r.Context(), "ops-bot"))
		if methodOf(t, r) == "subscriptions/listen" {
			close(resolved)
			<-release
		}
		server.MCPHandler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	changed := make(chan struct{}, 16)
	client := mcp.NewClient(&mcp.Implementation{Name: "assistant", Version: "1"},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				changed <- struct{}{}
			},
		})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// CONNECTED ON ITS OWN GOROUTINE: the client sends its stream's request
	// as it connects, and that request is held below.
	connected := make(chan *mcp.ClientSession, 1)
	go func() {
		sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: srv.URL + operator.MCPPath, HTTPClient: httpxtest.Pool(t),
		}, &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
		if err != nil {
			t.Errorf("connect: %v", err)
		}
		connected <- sess
	}()
	select {
	case <-resolved:
	case <-ctx.Done():
		t.Fatal("the client never opened its stream")
	}

	// THE SWAP AND ITS ANNOUNCEMENT, while the stream is between resolving
	// the catalogue of reads and subscribing on it.
	current.Store(steers)
	listeners.Announce(t.Context())
	close(release)
	sess := <-connected
	if sess == nil {
		t.FailNow()
	}
	t.Cleanup(func() { _ = sess.Close() })

	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("a stream that subscribed on a catalogue already replaced was never " +
			"told its tools changed")
	}
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("the listing after it was told: %v", err)
	}
	if !hasTool(listed, builtin.SteerTurnTool) {
		t.Errorf("the stream was told and its listing is still the replaced catalogue, "+
			"without %s", builtin.SteerTurnTool)
	}
}

// ENDING THE LISTENERS ENDS EVERY STREAM, AND REFUSES TO HOLD A NEW ONE. A
// stream is one request held open, and a node's API listener shutting down
// gracefully waits for every request it holds: an assistant listening held
// every stop for the listener's whole grace, and was then cut with a warning
// meant for a request that was stuck. Ended, each listen answers its result
// — the SDK's word for a subscription the server ended gracefully — and the
// request completes; one opened afterwards completes at once.
func TestEndingTheListenersEndsEveryStream(t *testing.T) {
	t.Parallel()
	listeners := operator.NewListeners()
	reads := newSurface(t, operator.Options{
		Work:      builtin.WorkDeps{Reader: stubWorkReader{}, Actor: operator.WorkActor(nil, nil)},
		Listeners: listeners,
	})
	var held atomic.Int64
	completed := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithOperator(r.Context(), "ops-bot"))
		if methodOf(t, r) != "subscriptions/listen" {
			reads.MCPHandler().ServeHTTP(w, r)
			return
		}
		held.Add(1)
		defer func() {
			held.Add(-1)
			completed <- struct{}{}
		}()
		reads.MCPHandler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	listenOperator(t, srv.URL)
	if held.Load() != 1 {
		t.Fatalf("the premise: %d streams held, want the listener's one", held.Load())
	}

	listeners.End()
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("ending the listeners left the stream's request held open")
	}
	if n := listeners.Listening(); n != 0 {
		t.Errorf("the registry counts %d streams once every one has ended", n)
	}

	// AND A STREAM OPENED AFTERWARDS is acknowledged and ends at once.
	client := mcp.NewClient(&mcp.Implementation{Name: "late", Version: "1"},
		&mcp.ClientOptions{ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {}})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL + operator.MCPPath, HTTPClient: httpxtest.Pool(t),
	}, &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	if err != nil {
		t.Fatalf("connect after the end: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("a stream opened after the listeners ended was held open")
	}
}

// methodOf is the JSON-RPC method r carries, its body left readable.
func methodOf(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read the request: %v", err)
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var call struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &call)
	return call.Method
}
