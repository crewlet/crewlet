package mcpbridge_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolsession/toolsessiontest"
)

// --- a fleet of bridges ----------------------------------------------------

// fleetOf is bridges joined over one broker and one lease table, every one
// mounted on a route of its own and minting the FIRST member's address as the
// fleet's — a box dials that node whichever node holds its run's seat.
type fleetOf struct {
	broker  *memory.Broker
	leases  *coordmemory.Backend
	members []*member
	// leases held per owner, so a seat can be handed from one to another.
	held map[string]*coord.Lease
}

// member is one node of a fleetOf.
type member struct {
	bridge *mcpbridge.Bridge
	server *httptest.Server
	node   string
	owner  string
	// leave takes the member's bridge out of the fleet, as its process
	// stopping does.
	leave queue.Unsubscribe
}

// newFleet starts n members. holderWait bounds how long a call waits for a
// holder.
func newFleet(t *testing.T, n int, holderWait time.Duration) *fleetOf {
	t.Helper()
	f := &fleetOf{broker: memory.NewBroker(), leases: coordmemory.New(), held: map[string]*coord.Lease{}}
	servers := make([]*httptest.Server, n)
	muxes := make([]*http.ServeMux, n)
	for i := range n {
		muxes[i] = http.NewServeMux()
		servers[i] = httptest.NewServer(muxes[i])
		t.Cleanup(servers[i].Close)
	}
	for i := range n {
		bridge := bridgeFor(t, mcpbridge.Options{
			Material: runtoken.OneKey("k", "test-key"), BaseURL: servers[0].URL,
		})
		muxes[i].Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
		muxes[i].Handle(mcpbridge.ServerPattern, bridge.Handler())
		node := "node-" + string(rune('a'+i))
		m := &member{bridge: bridge, server: servers[i], node: node}
		f.join(t, m, holderWait)
		f.members = append(f.members, m)
	}
	return f
}

// join makes m's bridge one of the fleet's, under a new incarnation of its
// node.
func (f *fleetOf) join(t *testing.T, m *member, holderWait time.Duration) {
	t.Helper()
	q := f.broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.owner = m.node + ":" + uuid.NewString()
	leave, err := m.bridge.Join(t.Context(), mcpbridge.FleetOptions{
		Fabric: q, Leases: f.leases, Node: m.node, Owner: m.owner,
		Keyring: statelog.OneKey("k", "fleet-key"), HolderWait: holderWait,
	})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	m.leave = leave
	t.Cleanup(func() { _ = leave(context.Background()) })
}

// seat is the id of the seat every fixture session acts as.
func seat() uuid.UUID { return uuid.MustParse(seatTurn().AgentID()) }

// hold gives the seat's lease to m, taking it from whoever held it.
func (f *fleetOf) hold(t *testing.T, m *member) {
	t.Helper()
	resource := coord.SeatResource(seat())
	for owner, lease := range f.held {
		if _, err := f.leases.Release(t.Context(), resource, owner, lease.Epoch); err != nil {
			t.Fatalf("Release: %v", err)
		}
		delete(f.held, owner)
	}
	lease, _, err := f.leases.TryAcquire(t.Context(), resource,
		coord.AcquireOptions{Owner: m.owner, TTL: time.Minute})
	if err != nil || lease == nil {
		t.Fatalf("acquire the seat for %s: %v", m.owner, err)
	}
	f.held[m.owner] = lease
}

// takeOver is m taking the seat and its running job: the lease, the job's
// session opened again, and the seat marked taken — in a recovery's order.
func (f *fleetOf) takeOver(t *testing.T, m *member, s *mcpbridge.Session) string {
	t.Helper()
	f.hold(t, m)
	url := m.bridge.Open(t.Context(), s)
	if url == "" {
		t.Fatal("the job's session was not opened")
	}
	m.bridge.SeatReady(seat())
	return url
}

// dialWith connects a box's MCP client to url.
func dialWith(t *testing.T, url string, relisted *atomic.Int32) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, req *mcp.ToolListChangedRequest) {
			if relisted != nil {
				relisted.Add(1)
			}
			_, _ = req.Session.ListTools(ctx, nil)
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
	return sess
}

// --- the contract, relayed -------------------------------------------------

// A RELAYED BRIDGE KEEPS THE WHOLE TOOL SESSION CONTRACT. Every case dials the
// fleet's address — one node — while the run's session lives on another, so
// every gate, every activation's re-list and every cut box crosses the relay,
// event stream included, and is held to exactly what a box dialling its
// holder directly is. The session names its caller, as the suite's surface is
// one that parks ([parking]), so it is relayed to the process that opened it;
// a seat's route to its holder is held by the cases below.
func TestTheRelayedBridgeKeepsTheToolSessionContract(t *testing.T) {
	t.Parallel()
	toolsessiontest.Run(t, func(t *testing.T, spec toolsessiontest.Spec) *toolsessiontest.Served {
		f := newFleet(t, 2, 10*time.Second)
		holder := f.members[1]
		calls := &ledger{}
		surface := contractSurface(t, spec)
		session := &mcpbridge.Session{
			RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
			Surface: surface, Caller: actingSeat{surface.Surface},
			Ledger: calls, Fence: spec.Fence,
		}
		f.hold(t, holder)
		url := holder.bridge.Open(spec.Context, session)
		if url == "" {
			t.Fatal("no endpoint was minted")
		}
		holder.bridge.SeatReady(seat())
		if !strings.HasPrefix(url, f.members[0].server.URL) {
			t.Fatalf("the endpoint %q is not the fleet's address", url)
		}
		return &toolsessiontest.Served{
			Dial: func(t *testing.T) (mcp.Transport, func()) {
				box := newBoxLine(t)
				return &mcp.StreamableClientTransport{Endpoint: url,
					HTTPClient: &http.Client{Transport: box}}, box.cut
			},
			Dispatcher: mcpbridge.Dispatcher(session),
			End:        func(*testing.T) { holder.bridge.Close(session.RunID) },
			Recorded: func(*testing.T) []toolsessiontest.Recorded {
				var out []toolsessiontest.Recorded
				for _, row := range calls.rows() {
					out = append(out, toolsessiontest.Recorded{Seq: row.Seq, Name: row.Name,
						Output: row.Output, Failed: row.Failed, Class: row.Refusal()})
				}
				return out
			},
			UnparsedArguments: toolloop.UnparsedArguments,
		}
	})
}

// --- where a call goes -----------------------------------------------------

// A RUN WHOSE SEAT MOVED IS SERVED BY ITS NEW HOLDER through the address its
// box started with. The node that launched it lets the run's session go with
// the seat, the peer that took the seat opens the job's session again, and the
// box — told its MCP session is gone, as the protocol tells any client whose
// server restarted — connects again through the same URL and reaches the peer.
func TestARunWhoseSeatMovedIsServedByItsNewHolder(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	f := newFleet(t, 2, 10*time.Second)
	launcher, peer := f.members[0], f.members[1]
	session := &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: fx.surface, Ledger: fx.ledger}
	url := f.takeOver(t, launcher, session)

	box := dialWith(t, url, nil)
	if res := callTool(t, box, "read_page", map[string]any{}); res.IsError {
		t.Fatalf("the launcher did not serve its own run: %s", text(res))
	}

	// THE SEAT MOVES: released here, taken there.
	launcher.bridge.ReleaseSeat(seat())
	if launcher.bridge.Live() != 0 {
		t.Fatalf("the launcher still holds %d sessions for a seat it released", launcher.bridge.Live())
	}
	recovered := &ledger{}
	f.takeOver(t, peer, &mcpbridge.Session{RunID: "run-1", Launch: "job-1", LastSeq: 1, Handle: "dev",
		Role: "Engineer", Surface: fx.surface, Ledger: recovered})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := box.CallTool(ctx, &mcp.CallToolParams{Name: "read_page", Arguments: map[string]any{}}); err == nil {
		t.Error("the box's MCP session survived its seat's move; it lived on the node that let it go")
	}
	again := dialWith(t, url, nil)
	if res := callTool(t, again, "post_message", map[string]any{}); res.IsError {
		t.Fatalf("the box could not reach its run's new holder: %s", text(res))
	}
	rows := recovered.rows()
	if len(rows) != 1 || rows[0].Name != "post_message" || rows[0].Seq != 2 {
		t.Errorf("the new holder recorded %+v, want the box's call as the run's second", rows)
	}
}

// EACH OF A SEAT'S MCP SERVERS IS RELAYED TO THE SEAT'S HOLDER, as the job's
// own endpoint is: a box dials every server beneath the job's endpoint at the
// fleet's address, and the node it lands on, which holds no session for the
// run, hands the call to the one that does — which serves that server.
//
// Mutation: leave [mcpbridge.ServerPattern] out of the relay's mux
// (Bridge.Join), and the holder answers the relayed call Not Found.
func TestEachOfASeatsServersIsRelayedToItsHolder(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2, 10*time.Second)
	holder := f.members[1]
	calls := &ledger{}
	url := f.takeOver(t, holder, &mcpbridge.Session{RunID: "run-1", Launch: "job-1",
		Handle: "dev", Role: "Engineer", Surface: serversSurface(t), Ledger: calls})
	if !strings.HasPrefix(url, f.members[0].server.URL) {
		t.Fatalf("the endpoint %q is not the fleet's address", url)
	}

	box := dialWith(t, mcpbridge.ServerURL(url, "github"), nil)
	if res := callTool(t, box, "create_issue", map[string]any{}); res.IsError {
		t.Fatalf("github's endpoint, relayed, refused the call: %s", text(res))
	}
	rows := calls.rows()
	if len(rows) != 1 || rows[0].Name != "create_issue" {
		t.Errorf("the holder recorded %+v, want the box's one call on github's endpoint", rows)
	}
}

// A CALL THAT LANDS WHILE ITS SEAT IS BEING TAKEN WAITS FOR THE TAKER, rather
// than being told its live run is over: a node that holds the seat's lease and
// has not yet opened the run's session again says nothing yet about the run.
func TestACallWaitsForAHolderStillTakingTheSeat(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	f := newFleet(t, 2, 10*time.Second)
	entry, holder := f.members[0], f.members[1]
	session := &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: fx.surface, Ledger: fx.ledger}
	url := f.takeOver(t, entry, session)
	entry.bridge.ReleaseSeat(seat())

	// THE TAKER HOLDS THE LEASE and has not recovered the seat yet.
	f.hold(t, holder)
	go func() {
		time.Sleep(300 * time.Millisecond)
		holder.bridge.Open(context.Background(), &mcpbridge.Session{RunID: "run-1", Launch: "job-1",
			Handle: "dev", Role: "Engineer", Surface: fx.surface, Ledger: fx.ledger})
		holder.bridge.SeatReady(seat())
	}()
	if res := callTool(t, dialWith(t, url, nil), "read_page", map[string]any{}); res.IsError {
		t.Fatalf("a call during the takeover was refused: %s", text(res))
	}
}

// A RUN THAT IS OVER IS REFUSED WHEREVER ITS BOX'S CALL LANDS: the node holding
// its seat has finished taking it and holds no session for its job, which is
// what an ended run looks like.
func TestARunOverOnItsHolderIsRefusedThroughAnyNode(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	f := newFleet(t, 2, 10*time.Second)
	holder := f.members[1]
	url := f.takeOver(t, holder, &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev",
		Role: "Engineer", Surface: fx.surface, Ledger: fx.ledger})
	holder.bridge.Close("run-1")

	res, err := httpxtest.Pool(t).Post(url, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a call for an ended run answered %d through another node, want 401", res.StatusCode)
	}
}

// A CALL NO HOLDER ANSWERS IS TOLD THE BRIDGE IS UNAVAILABLE once it has waited
// its bound — never that its run is over, which nobody said — and when to try
// again.
func TestACallNoHolderAnswersIsUnavailable(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	f := newFleet(t, 2, 500*time.Millisecond)
	url := f.takeOver(t, f.members[1], &mcpbridge.Session{RunID: "run-1", Launch: "job-1",
		Handle: "dev", Role: "Engineer", Surface: fx.surface, Ledger: fx.ledger})
	// THE HOLDER IS GONE: its lease released and nobody has taken the seat.
	resource := coord.SeatResource(seat())
	lease := f.held[f.members[1].owner]
	if _, err := f.leases.Release(t.Context(), resource, f.members[1].owner, lease.Epoch); err != nil {
		t.Fatalf("Release: %v", err)
	}

	began := time.Now()
	res, err := httpxtest.Pool(t).Post(url, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" {
		t.Errorf("a call with no holder answered %d (Retry-After %q), want 503 with one",
			res.StatusCode, res.Header.Get("Retry-After"))
	}
	if took := time.Since(began); took < 300*time.Millisecond {
		t.Errorf("a call with no holder was answered after %s, before it waited for one", took)
	}
}

// A RELEASED SEAT TAKES ITS SESSIONS WITH IT: the run goes on under the seat's
// next holder, and an opening left behind would keep the seat's live surface —
// its servers and their credentials — in a process that no longer speaks for
// it.
func TestReleasingASeatEndsItsSessionsHere(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	url := fx.open(t)
	box := dial(t, url)
	if res := callTool(t, box, "read_page", map[string]any{}); res.IsError {
		t.Fatalf("the run did not dispatch: %s", text(res))
	}
	other := uuid.New()
	fx.bridge.ReleaseSeat(other)
	if fx.bridge.Live() != 1 {
		t.Fatal("releasing another seat ended this one's session")
	}
	fx.bridge.ReleaseSeat(seat())
	if fx.bridge.Live() != 0 || fx.session.Connections() != 0 {
		t.Errorf("after the seat's release %d sessions and %d box connections remain",
			fx.bridge.Live(), fx.session.Connections())
	}
}

// AN OPENING WHOSE FENCE HAS CLOSED DEFERS TO THE SEAT'S HOLDER: the node that
// launched the run still has its opening — the seat left before it let the
// opening go — and a call that lands there is answered by the node now holding
// the seat, never refused by the one that lost it.
func TestAFencedOpeningDefersToTheSeatsHolder(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	f := newFleet(t, 2, 10*time.Second)
	launcher, peer := f.members[0], f.members[1]
	var moved atomic.Bool
	url := f.takeOver(t, launcher, &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev",
		Role: "Engineer", Surface: fx.surface, Ledger: fx.ledger,
		Fence: func() error {
			if moved.Load() {
				return errors.New("this node no longer holds the seat")
			}
			return nil
		}})

	moved.Store(true)
	recovered := &ledger{}
	f.takeOver(t, peer, &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev",
		Role: "Engineer", Surface: fx.surface, Ledger: recovered})
	if res := callTool(t, dialWith(t, url, nil), "read_page", map[string]any{}); res.IsError {
		t.Fatalf("the call was refused by the node that lost the seat: %s", text(res))
	}
	if rows := recovered.rows(); len(rows) != 1 {
		t.Errorf("the seat's holder recorded %d calls, want the box's one", len(rows))
	}
}

// --- a caller's own session ------------------------------------------------

// openDeep opens a deep-work-shaped session on m: a caller of its own, and so
// no seat whose lease could name its holder.
func openDeep(t *testing.T, m *member, probe *probeSurface, loopback bool) string {
	t.Helper()
	url := m.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "deep-1", Launch: "job-1",
		Surface: probe, Caller: nobody{bound: true}, Loopback: loopback})
	if url == "" {
		t.Fatal("the session was not opened")
	}
	return url
}

// postPing sends a box's JSON-RPC ping to url and answers its status.
func postPing(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := httpxtest.Pool(t).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// A CALLER'S OWN SESSION IS SERVED BY THE PROCESS THAT OPENED IT, wherever its
// box's call lands: it has no seat whose lease could name a holder, so its
// token names its opener and the node the call lands on relays it straight
// there — decided as the session's caller, never as the request. Once the
// opener ends it, the run is over through every node.
func TestACallersOwnSessionIsServedByItsOpenerThroughAnyNode(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2, 10*time.Second)
	probe := &probeSurface{}
	opener := f.members[1]
	url := openDeep(t, opener, probe, false)
	if !strings.HasPrefix(url, f.members[0].server.URL) {
		t.Fatalf("the session's address %s is not the fleet's", url)
	}
	if got := text(callTool(t, dialWith(t, url, nil), "probe", nil)); got != string(iam.Unknown) {
		t.Errorf("the relayed call was decided as %q, want its caller's %q", got, iam.Unknown)
	}
	if n := probe.calls.Load(); n != 1 {
		t.Errorf("the opener's surface took %d calls, want the box's one", n)
	}
	opener.bridge.Close("deep-1")
	if got := postPing(t, url); got != http.StatusUnauthorized {
		t.Errorf("a call for a closed session answered %d through another node, want 401", got)
	}
}

// A CALLER'S OWN SESSION ENDS WITH THE PROCESS THAT OPENED IT: nothing takes it
// over, so once its node runs a later incarnation, a call for it is refused at
// once rather than waiting for a holder that will never come.
func TestACallersOwnSessionEndsWithItsOpener(t *testing.T) {
	t.Parallel()
	const wait = 10 * time.Second
	f := newFleet(t, 2, wait)
	opener := f.members[1]
	url := openDeep(t, opener, &probeSurface{}, false)
	if err := opener.leave(t.Context()); err != nil {
		t.Fatalf("leave: %v", err)
	}
	f.join(t, &member{bridge: bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: f.members[0].server.URL,
	}), node: opener.node}, wait)

	began := time.Now()
	if got := postPing(t, url); got != http.StatusUnauthorized {
		t.Errorf("a call for a session whose opener restarted answered %d, want 401", got)
	}
	if took := time.Since(began); took >= wait {
		t.Errorf("the refusal took %s, the whole wait for a holder", took)
	}
}

// A LOOPBACK SESSION IS NEVER RELAYED: its token is worth something from its
// opener's own loopback alone, and a call that lands on another node is
// refused there rather than carried to it — though it arrived from that
// node's loopback, which is what the address the entry carries would say.
func TestALoopbackSessionIsNeverRelayed(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2, 10*time.Second)
	url := openDeep(t, f.members[1], &probeSurface{}, true)
	if got := postPing(t, url); got != http.StatusUnauthorized {
		t.Errorf("a loopback session's call through another node answered %d, want 401", got)
	}
}
