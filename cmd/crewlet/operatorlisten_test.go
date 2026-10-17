package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// AN ASSISTANT LISTENING IS TOLD WHEN AN APPLY CHANGES ITS TOOLS, with no
// request of its own or anybody else's in between.
//
// The node builds the operator's catalogue again when what it is built from
// moves — here, the company applied after boot bringing the native tracker,
// then a rename, which titles the MCP server — and the MCP transport keeps no
// session, so the one thing that can reach a connected assistant between its
// requests is the stream it opened at connect (`subscriptions/listen`, the
// 2026-07-28 protocol). That stream stays on the catalogue that answered it:
// the boot catalogue, offering the fleet's own gestures and nothing else. The
// server advertises `tools.listChanged`, and nothing announced a change, so
// the assistant kept offering those four tools until somebody reconnected it.
// And the catalogue was built again only when a request happened to find the
// facts moved, so an assistant left alone was not told even once a swap did
// announce.
//
// Twice, because the stream is still parked on the BOOT catalogue at the
// rename: a node that announced only on the catalogue it had just replaced
// told this assistant of the first change and of nothing after it.
func TestAListeningAssistantIsToldWhenAnApplyChangesItsTools(t *testing.T) {
	t.Parallel()
	n := bootUnconfigured(t, "a-test-token")
	sess, changed := n.listen(t)
	if listed := toolNames(t, sess); slices.Contains(listed, builtin.CreateWorkItemTool) {
		t.Fatalf("the premise: a node with no company offers %v", listed)
	}

	n.reconcile()
	if err := n.settle(n.put(companyYAML)); err != nil {
		t.Fatalf("the company: %v", err)
	}
	told(t, changed, "the company applied after boot")
	if listed := toolNames(t, sess); !slices.Contains(listed, builtin.CreateWorkItemTool) {
		t.Errorf("after the company applied after boot the assistant is offered %v, "+
			"want %s among it", listed, builtin.CreateWorkItemTool)
	}

	if err := n.settle(n.put(strings.Replace(companyYAML, "name: Acme",
		"name: Acme Holdings", 1))); err != nil {
		t.Fatalf("the rename: %v", err)
	}
	told(t, changed, "the rename")
}

// A PEER'S REBUILD REACHES AN ASSISTANT LISTENING HERE, through the engine's
// own queue. An assistant's listing may reach any ingress node, so a node that
// told its listeners only of its own rebuilds left one that listed from a peer
// before the peer applied holding the peer's old tools: every node's rebuild
// is announced to the fleet (`operator_tools_changed`) and each node tells its
// own listeners of it. The two-node rule is held in internal/api over a shared
// broker; this holds that `crewlet run` wires it to the queue its peers
// publish on.
func TestAPeersRebuildReachesAnAssistantListeningHere(t *testing.T) {
	t.Parallel()
	n := bootUnconfigured(t, "a-test-token")
	_, changed := n.listen(t)

	ev := events.New(types.OperatorToolsChanged{}, events.TraceContext{})
	// ANOTHER NODE'S, which a node's own client would otherwise stamp as
	// this one's.
	ev.Node, ev.Source = "a-peer", "a-peer"
	if err := n.e.Backends().Queue.Publish(t.Context(),
		topics.Event(ev.Type), ev); err != nil {
		t.Fatalf("publish a peer's rebuild: %v", err)
	}
	told(t, changed, "a peer rebuilt its operator tools")
}

// A LISTENING ASSISTANT DOES NOT HOLD THE LISTENER'S SHUTDOWN. Its stream is
// one request held open, and the API listener's graceful shutdown waits for
// every request: an assistant listening held every stop of its node for the
// whole shutdown grace, which then cut it and logged api_shutdown_failed — on
// every restart of a node an assistant was connected to, burying the warning
// that means a request was stuck. The listener ends every stream as its
// shutdown begins.
func TestAListeningAssistantDoesNotHoldTheListenersShutdown(t *testing.T) {
	t.Parallel()
	n := bootUnconfigured(t, "a-test-token")
	n.listen(t)

	ctx, cancel := context.WithTimeout(t.Context(), apiShutdownGrace/2)
	defer cancel()
	if err := n.surface.server.server.Shutdown(ctx); err != nil {
		t.Errorf("the API listener's shutdown with an assistant listening: %v — "+
			"the stream held it", err)
	}
}

// listen connects an assistant to the node's /operator/mcp as the MCP client
// this repository ships does when it is handed a tool-list handler — at the
// 2026-07-28 protocol, opening a `subscriptions/listen` stream at connect —
// and answers the session and a channel that receives once per notification
// that the tools changed. It returns once the stream has been told the once a
// stream is told as it opens, so nothing a case does next can race the
// subscription and the channel starts quiet.
func (n *unconfiguredNode) listen(t *testing.T) (*mcp.ClientSession, <-chan struct{}) {
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
		Endpoint:   n.base + operator.MCPPath,
		HTTPClient: &http.Client{Transport: bearer{token: n.token, base: n.client.Transport}},
	}, &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("the assistant's stream was never told the tools changed as it opened")
	}
	return sess, changed
}

// told waits for one notification that the tools changed, failing the case
// where none arrives.
func told(t *testing.T, changed <-chan struct{}, after string) {
	t.Helper()
	select {
	case <-changed:
	case <-time.After(15 * time.Second):
		t.Fatalf("after %s the listening assistant was never told its tools changed", after)
	}
}

// toolNames is what the node offers the session now.
func toolNames(t *testing.T, sess *mcp.ClientSession) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list the tools: %v", err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// bearer sends every request with the operator's token, as an assistant
// configured with one does.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}
