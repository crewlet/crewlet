package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN ASSISTANT IS TOLD AFTER THE LAST NODE'S MOVE, wherever its listing lands.
// Two ingress nodes on one broker, with no affinity: the assistant's listening
// stream is held by node A and every other request it makes reaches node B. A
// revision reaches A first, A's catalogue moves and it tells its listener, and
// the listener lists again at once — on B, which has not applied it, so it is
// listed the old tools. When B moves, its own listeners are all it could tell,
// and this assistant's stream is on A: it kept the old tools until the next
// change anywhere. So every node's move is told to the fleet, and A, hearing
// B's, tells its listener again, after which the listing — still on B — is the
// new catalogue.
//
// Mutation: drop the Announce from hear and the last assertion goes red.
func TestAListeningAssistantIsToldAfterTheLastNodesMove(t *testing.T) {
	t.Parallel()
	broker := queuememory.NewBroker()
	a, b := fleetNode(t, broker, "node-a"), fleetNode(t, broker, "node-b")
	sess, changed := listenAcross(t, a.url, b.url)
	if offers(t, sess, builtin.GetPageTool) {
		t.Fatalf("the premise: a fleet that has applied nothing offers %s",
			builtin.GetPageTool)
	}

	a.applyPages()
	a.listens.Moved(t.Context())
	toldOnce(t, changed, "node A moved")
	if offers(t, sess, builtin.GetPageTool) {
		t.Fatalf("the premise: a listing that reached node B, which has not applied "+
			"the revision, offers %s", builtin.GetPageTool)
	}

	b.applyPages()
	b.listens.Moved(t.Context())
	toldOnce(t, changed, "node B, which the assistant's listings reach, moved")
	if !offers(t, sess, builtin.GetPageTool) {
		t.Errorf("after the last node moved the assistant was listed the old "+
			"catalogue, without %s", builtin.GetPageTool)
	}
}

// A NODE TELLS NOBODY OF ITS OWN MOVE TWICE: the nudge it publishes for its
// peers reaches its own subscription too, and a node that heard itself would
// tell its listeners a second time — one listing more per listener per move,
// across every listener of every node. And a move that changes nothing a call
// is served — an apply that touched no fact the catalogue is built from — tells
// nobody at all.
func TestANodeDoesNotHearItsOwnMove(t *testing.T) {
	t.Parallel()
	broker := queuememory.NewBroker()
	a := fleetNode(t, broker, "node-a")
	_, changed := listenAcross(t, a.url, a.url)

	a.listens.Moved(t.Context())
	a.listens.fleet.telling.Wait()
	select {
	case <-changed:
		t.Fatal("an apply that moved no tool told the listener its tools changed")
	case <-time.After(200 * time.Millisecond):
	}

	a.applyPages()
	a.listens.Moved(t.Context())
	toldOnce(t, changed, "the node moved")
	// The nudge is published in the background; the memory broker hands it to
	// every subscription before Publish returns.
	a.listens.fleet.telling.Wait()
	select {
	case <-changed:
		t.Error("the listener was told of one move twice: the node heard its " +
			"own nudge")
	case <-time.After(200 * time.Millisecond):
	}
}

// fleetMember is one ingress node of a test fleet: an operator surface over
// halves the case moves, joined to the fleet's broker under its own id, and
// served over HTTP as the app serves it.
type fleetMember struct {
	halves  *atomic.Pointer[operator.Halves]
	listens *OperatorListens
	url     string
}

// applyPages moves the node's halves to a company that also keeps a knowledge
// base, as an apply would.
func (m fleetMember) applyPages() {
	next := *m.halves.Load()
	next.Pages = builtin.PageDeps{Reader: struct{ builtin.PageReader }{}}
	m.halves.Store(&next)
}

// listenerSeatControls is opts with the seat controls [operator.New] requires
// and no case here calls: listing a catalogue answers no run, pauses no seat
// and steers no turn, so each is a value nobody reaches, and the steer channel
// signs under a key no node of any case holds.
func listenerSeatControls(opts operator.Options) operator.Options {
	wire, err := signedask.New[steer.Request, steer.Reply](steer.Channel,
		statelog.OneKey("k1", "the listener cases' seat-control key material"))
	if err != nil {
		panic(err)
	}
	opts.Runs = builtin.RunDeps{Desk: struct{ builtin.RunDesk }{}}
	opts.Pauses = builtin.SeatPauseDeps{Pauses: struct{ builtin.SeatPauseStore }{},
		Announce: struct{ builtin.Announcer }{}, Org: func() *org.Organization { return nil }}
	opts.Steer = builtin.SteerDeps{Asker: struct{ builtin.FleetAsker }{}, Wire: wire,
		Runs: struct{ builtin.RunSteers }{}}
	return opts
}

// fleetNode starts one ingress node named id on broker.
func fleetNode(t *testing.T, broker *queuememory.Broker, id string) fleetMember {
	t.Helper()
	halves := &atomic.Pointer[operator.Halves]{}
	halves.Store(&operator.Halves{
		// A READER THAT IS NEVER CALLED: listing a catalogue calls no tool.
		Work: builtin.WorkDeps{Reader: struct{ builtin.WorkReader }{}},
	})
	listeners := operator.NewListeners()
	server, err := operator.New(listenerSeatControls(operator.Options{
		Halves:    func() (operator.Halves, bool) { return *halves.Load(), true },
		Authorize: builtin.Decide(authz.NoChart{}),
		Audit:     discardAudit{},
		Listeners: listeners,
	}))
	if err != nil {
		t.Fatalf("operator.New: %v", err)
	}
	client := broker.Client(queuememory.Contract(queue.WithNode(id)))
	if err := client.Start(t.Context()); err != nil {
		t.Fatalf("start %s's queue: %v", id, err)
	}
	listens := NewOperatorListens(server, listeners, client, id)
	listens.Join(t.Context())
	if listens.fleet.detach == nil {
		t.Fatalf("%s did not join the fleet's announcements", id)
	}
	ops := iam.Principal{
		ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("token:ops-bot")), Login: "token:ops-bot",
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: iam.Apps,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The principal the app's guard puts on every request it admits.
		server.MCPHandler().ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), ops)))
	}))
	t.Cleanup(func() {
		listens.End()
		srv.Close()
		listens.Close(context.Background())
		_ = client.Stop(context.Background())
	})
	return fleetMember{halves: halves, listens: listens, url: srv.URL}
}

// discardAudit publishes nothing: these cases call no tool.
type discardAudit struct{}

func (discardAudit) Publish(context.Context, string, *events.Event) error { return nil }

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
