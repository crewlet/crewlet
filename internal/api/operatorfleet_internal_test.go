package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/queue"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
)

// AN ASSISTANT IS TOLD AFTER THE LAST NODE'S REBUILD, wherever its listing
// lands. Two ingress nodes on one broker, with no affinity: the assistant's
// listening stream is held by node A and every other request it makes reaches
// node B. A revision reaches A first, A rebuilds and tells its listener, and
// the listener lists again at once — on B, which has not applied it, so it is
// listed the old tools. When B rebuilds, its own listeners are all it could
// tell, and this assistant's stream is on A: it kept the old tools until the
// next change anywhere. So every node's rebuild is told to the fleet, and A,
// hearing B's, tells its listener again, after which the listing — still on B
// — is the new catalogue.
func TestAListeningAssistantIsToldAfterTheLastNodesRebuild(t *testing.T) {
	t.Parallel()
	broker := queuememory.NewBroker()
	a, b := fleetNode(t, broker, "node-a"), fleetNode(t, broker, "node-b")
	sess, changed := listenAcross(t, a.url, b.url)
	if offers(t, sess, builtin.AnswerRunTool) {
		t.Fatalf("the premise: a fleet that has applied nothing offers %s",
			builtin.AnswerRunTool)
	}

	a.moving.move(operatorBasis{knowledge: true})
	a.surface.Refresh(t.Context())
	toldOnce(t, changed, "node A rebuilt")
	if offers(t, sess, builtin.AnswerRunTool) {
		t.Fatalf("the premise: a listing that reached node B, which has not applied "+
			"the revision, offers %s", builtin.AnswerRunTool)
	}

	b.moving.move(operatorBasis{knowledge: true})
	b.surface.Refresh(t.Context())
	toldOnce(t, changed, "node B, which the assistant's listings reach, rebuilt")
	if !offers(t, sess, builtin.AnswerRunTool) {
		t.Errorf("after the last node rebuilt the assistant was listed the old "+
			"catalogue, without %s", builtin.AnswerRunTool)
	}
}

// A NODE TELLS NOBODY OF ITS OWN REBUILD TWICE: the nudge it publishes for its
// peers reaches its own subscription too, and a node that heard itself would
// tell its listeners a second time — one listing more per listener per swap,
// across every listener of every node.
func TestANodeDoesNotHearItsOwnRebuild(t *testing.T) {
	t.Parallel()
	broker := queuememory.NewBroker()
	a := fleetNode(t, broker, "node-a")
	_, changed := listenAcross(t, a.url, a.url)

	a.moving.move(operatorBasis{knowledge: true})
	a.surface.Refresh(t.Context())
	toldOnce(t, changed, "the node rebuilt")
	// The nudge is published in the background; the memory broker hands it to
	// every subscription before Publish returns.
	a.surface.fleet.telling.Wait()
	select {
	case <-changed:
		t.Error("the listener was told of one rebuild twice: the node heard its " +
			"own nudge")
	case <-time.After(200 * time.Millisecond):
	}
}

// fleetMember is one ingress node of a test fleet: an operator surface over
// facts the case moves, joined to the fleet's broker under its own id, and
// served over HTTP the way the app serves it.
type fleetMember struct {
	moving  *movingOperators
	surface *EngineOperators
	url     string
}

// fleetNode starts one ingress node named id on broker.
func fleetNode(t *testing.T, broker *queuememory.Broker, id string) fleetMember {
	t.Helper()
	m := &movingOperators{}
	o := m.surface(t)
	client := broker.Client(queuememory.Contract(queue.WithNode(id)))
	if err := client.Start(t.Context()); err != nil {
		t.Fatalf("start %s's queue: %v", id, err)
	}
	o.join(t.Context(), client, id)
	if o.fleet.detach == nil {
		t.Fatalf("%s did not join the fleet's announcements", id)
	}
	route := operatorRoute(o, (*operator.Server).MCPHandler)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The operator the app's guard puts on every request it admits.
		route.ServeHTTP(w, r.WithContext(auth.WithOperator(r.Context(), "ops-bot")))
	}))
	t.Cleanup(func() {
		o.EndListens()
		srv.Close()
		o.Close(context.Background())
		_ = client.Stop(context.Background())
	})
	return fleetMember{moving: m, surface: o, url: srv.URL}
}

// listenAcross connects an assistant whose listening stream is served at
// listen and every other request at serve — two ingress nodes behind a load
// balancer with no affinity — at the 2026-07-28 protocol, as the MCP client
// this repository ships connects when handed a tool-list handler. It returns
// once the stream is acknowledged and told the once it is told as it opens,
// so a case starts from a quiet channel that receives once per notification.
func listenAcross(t *testing.T, listen, serve string) (*mcp.ClientSession, <-chan struct{}) {
	t.Helper()
	changed := make(chan struct{}, 16)
	client := mcp.NewClient(&mcp.Implementation{Name: "assistant", Version: "1"},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				changed <- struct{}{}
			},
		})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: serve + operator.MCPPath,
		HTTPClient: &http.Client{Transport: routeListens{
			listen: listen, base: httpxtest.Pool(t).Transport,
		}},
	}, &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	toldOnce(t, changed, "the stream opened")
	return sess, changed
}

// routeListens sends a `subscriptions/listen` to one node and every other
// request on to the address it was made for.
type routeListens struct {
	listen string
	base   http.RoundTripper
}

func (r routeListens) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	var call struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &call)
	req = req.Clone(req.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	if call.Method == "subscriptions/listen" {
		target, err := req.URL.Parse(r.listen + operator.MCPPath)
		if err != nil {
			return nil, err
		}
		req.URL, req.Host = target, target.Host
	}
	return r.base.RoundTrip(req)
}

// toldOnce waits for one notification that the tools changed.
func toldOnce(t *testing.T, changed <-chan struct{}, after string) {
	t.Helper()
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatalf("after %s the listening assistant was never told its tools changed", after)
	}
}

// offers reports whether the assistant is offered tool now.
func offers(t *testing.T, sess *mcp.ClientSession, tool string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list the tools: %v", err)
	}
	return slices.ContainsFunc(listed.Tools, func(listed *mcp.Tool) bool {
		return listed.Name == tool
	})
}
