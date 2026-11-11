package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/tools"
)

// "TEST NOW" ON AN MCP SERVER'S PAGE, AND IN THE ADD FLOW BEFORE IT IS SAVED.
//
// A test brings up a connection of its own to a server where the server runs,
// walks its stages and lists its tools, and never calls one ([mcp.Test]).
//
// # Where it runs
//
// A SHARED server runs on every node, and each node reaches it from where it
// is: its own resolver, its own network, its own PATH. So it is tested on
// every node that holds a presence lease, each answering what it found. A
// PER-AGENT server runs as one seat, on the node holding that seat, with the
// seat's own credentials, which only that node resolves (a GitHub token is
// issued for the seat on the node that holds it, and no other node may act
// as the seat): so it is tested for one seat, on its holder alone, which is
// the node that proves it holds the seat when the request arrives, as a
// restart is ([Engine.applyMCPRestart]).
//
// # The request is an instruction, so it is signed
//
// A node asked to test a server starts a process or dials an address as the
// company, with the credentials the server is started with. So the request
// crosses a channel the fleet signs ([mcpCheckChannel], internal/signedask),
// bound to an id and a deadline, and a node answers only a request that
// verifies, names it (or names the seat it holds) and is in this build's
// wire version. Who may ask is decided where the request arrived
// (internal/api/mcpapi); a draft travels in the request, its ${VAR}s resolved
// by each answering node from its own snapshot, so no value crosses the
// broker that a saved entry would not.
//
// # A draft is tested as a write would leave it
//
// An entry not written yet is tested in the company it would join: its own
// launch and values, the seats' credentials for it, and turned on whatever
// its `enabled` says, since a test of how it runs is what was asked. It is
// never recorded: the server's latest test is of the entry the company runs.
//
// # A value is tested before it is stored
//
// A value is tested with the entry it is for, as the fleet's ACTIVE revision
// states it, sent with the request ([MCPCheckRequest.Entry]) the way a draft
// is: so a value stored straight after its entry's write is tested on nodes
// that have not applied that write yet, rather than refused by a node whose
// company does not have the server. Each key under test reads its value
// ([MCPValueTry]).
//
// # What a request carries is sealed
//
// The channel is signed and not encrypted, so the entry a request sends (a
// draft, or an entry under a value test, whose literal values the revision
// keeps sealed) and every value under test cross it sealed under the fleet's
// keyring, each bound to the request ([mcpEntryAAD], [mcpValueAAD]). Never
// recorded.
//
// # The latest test is kept, bounded
//
// A test of a saved server is recorded as the server's latest
// ([coord.MCPCheck]), every sentence redacted and bounded, at most
// [coord.MaxMCPCheckNodes] nodes, and the apply that removes the server
// removes it ([Engine.clearRemovedMCPChecks]). Its tools are the answer to the
// person who asked, never stored.

// The refusals of a test, beside the restart's ([ErrNoMCPServer],
// [ErrMCPServerOff], [ErrMCPSeatOnShared], [ErrNoAgentSeat] and
// [ErrMCPSeatNotGranted]), each naming what the person can change.
var (
	// ErrMCPSeatRequired is a per-agent server tested without naming the
	// seat to test it for: it runs as one seat, with that seat's
	// credentials, and as nobody without one.
	ErrMCPSeatRequired = errors.New("engine: a per-agent MCP server runs as one seat: name the seat to test it for")
	// ErrNoMCPCheckFabric is a node with no broker or coordination store to
	// ask the fleet through.
	ErrNoMCPCheckFabric = errors.New("engine: this node has no broker to ask the fleet to test the server")
)

// MCPCheckRequest is a test somebody asked for.
type MCPCheckRequest struct {
	// Server is the configured server's name, or the draft's.
	Server string
	// Entry is the entry to test, sent with the request so that no node needs
	// to have applied it: a draft not written yet, or for a value under test
	// the entry as the fleet's active revision states it. Nil tests the entry
	// each node's own company runs, which is a saved server's test and is
	// recorded as its latest.
	Entry *config.MCPServer
	// Draft is whether Entry is not written yet.
	Draft bool
	// Seat is the handle of the seat a per-agent server is tested for.
	Seat string
	// Values are the values tested before they are stored, each its own
	// key's; none for a test of what is stored.
	Values []MCPValueTry
	// By is who asked.
	By iam.Actor
}

// MCPValueTry is a value tested before it is stored: Key (the seat's block's
// for a seat's test) reads Variable, which Value answers ahead of the store.
type MCPValueTry struct {
	Key, Variable string
	// Value is never recorded, logged or answered.
	Value string
}

// MCPCheckResult is what a test found, every sentence redacted and bounded.
type MCPCheckResult struct {
	Server string
	// Seat is the handle the test was made for, "" for a shared server.
	Seat  string
	Draft bool
	At    time.Time
	By    string
	// Node is the node the test was asked through.
	Node string
	// OK is whether some node answered and every node that answered found
	// the server serving.
	OK bool
	// Nodes are the nodes asked, in order, each with what it found or why
	// it did not answer.
	Nodes []MCPCheckedNode
}

// MCPCheckedNode is one node's test.
type MCPCheckedNode struct {
	Node     string
	Answered bool
	// Reason says why a node that did not answer did not.
	Reason string
	Report mcp.TestReport
}

// mcpCheckChannel names the signed channel a test request crosses.
const mcpCheckChannel = "mcp.check"

// mcpCheckWire is the request and reply version this build speaks; a node
// answers only a request in its own version, and an asker reads only a
// reply in it.
// Version 3 carries the entry under test and every value under test sealed
// ([mcpCheckAsk.Entry], [mcpCheckAsk.Values]), which a version 2 node would
// not open, testing something else.
const mcpCheckWire = 3

// mcpCheckAsk asks nodes to test a server.
type mcpCheckAsk struct {
	Version  int       `json:"v"`
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
	// Asker is the node that asked, which tests on its own account rather
	// than over the broker.
	Asker string `json:"asker"`
	// Nodes are the nodes that test a shared server; empty for a seat's
	// test, which only the seat's holder answers.
	Nodes  []string  `json:"nodes,omitempty"`
	Server string    `json:"server"`
	Seat   uuid.UUID `json:"seat"`
	// Entry is the entry to test (a draft, or an entry under a value test),
	// its JSON sealed for the fleet; "" for the one each node runs.
	Entry string `json:"entry,omitempty"`
	// Draft is whether Entry is not written yet: somebody's typing, which is
	// never tested under a saved server's OAuth grant.
	Draft bool `json:"draft,omitempty"`
	// Values are the values under test, each sealed for the fleet.
	Values []mcpValueAsk `json:"values,omitempty"`
}

func (a mcpCheckAsk) Binding() (string, time.Time) { return a.ID, a.Deadline }

// mcpValueAsk is a value under test as it crosses the broker, sealed.
type mcpValueAsk struct {
	Key      string `json:"key"`
	Variable string `json:"variable"`
	Sealed   string `json:"sealed"`
}

// mcpValueAAD binds a sealed value under test to its request and variable.
func mcpValueAAD(request, variable string) string {
	return "mcp_check/" + request + "/" + variable
}

// mcpEntryAAD binds a sealed entry under test to its request. A variable is
// an environment name, which never holds a slash, so no value's binding is
// an entry's.
func mcpEntryAAD(request string) string {
	return "mcp_check/" + request + "/entry/"
}

// errNoValueKeyring is a test that sends what it tests on a node with no
// keyring to seal it.
var errNoValueKeyring = errors.New("engine: this node holds no keyring to seal what it sends to be tested")

// errValuesWithoutEntry is values under test sent with no entry to test them
// with: a value is tested with the entry it is for, never with whatever a node
// happens to run under the name.
var errValuesWithoutEntry = errors.New("engine: a value under test is sent with the entry it is for")

// mcpCheckReply is one node's test.
type mcpCheckReply struct {
	Version int            `json:"v"`
	Request string         `json:"request"`
	Node    string         `json:"node"`
	Report  mcp.TestReport `json:"report"`
}

func (r mcpCheckReply) Answers() string { return r.Request }

// mcpCheckWireT is the channel's envelopes.
type mcpCheckWireT = signedask.Channel[mcpCheckAsk, mcpCheckReply]

// mcpChecks is this node's half of the channel.
type mcpChecks struct {
	wire *mcpCheckWireT
	// taken is the ids of the requests this node has answered.
	taken *signedask.Guard
	stop  queue.Unsubscribe
}

// newMCPChecks builds this node's half of the test channel over the fleet
// keyring every state-log record is signed under.
func newMCPChecks(ring recordseal.Keyring) (*mcpChecks, error) {
	wire, err := signedask.New[mcpCheckAsk, mcpCheckReply](mcpCheckChannel, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the MCP test channel's envelopes: %w", err)
	}
	return &mcpChecks{wire: wire, taken: signedask.NewGuard()}, nil
}

// The bounds of a test.
const (
	// mcpCheckSlack is what a test's budget allows past its stages: the
	// close of what it started ([mcp.Test]'s stop, ten seconds at most) and
	// the broker's round trips, with time to spare.
	mcpCheckSlack = 15 * time.Second
	// mcpCheckReplyMargin is how long before the asker's deadline an
	// answering node's test ends, so its reply arrives in time: a broker
	// round trip, many times over.
	mcpCheckReplyMargin = 2 * time.Second
	// maxMCPCheckToolBytes bounds the tools one node's reply carries, as
	// encoded: every tool the server listed (at most [mcp.MaxServerTools]),
	// their descriptions and then their titles shortened until they fit.
	// A quarter of the 8 MiB message a broker carries, so the reply's
	// envelope and the rest of the report fit beside it; 461 tools take
	// about 150 KiB with their words whole.
	maxMCPCheckToolBytes = 2 << 20
)

// mcpCheckBudget is how long a test of server may take on a node: its
// handshake and its listing, each within the server's startup budget, and
// the close.
func mcpCheckBudget(server *config.MCPServer) time.Duration {
	startup := mcpTimeout(server.StartupTimeoutSeconds)
	if startup == 0 {
		startup = mcp.DefaultStartupTimeout
	}
	return 2*startup + mcpCheckSlack
}

// CheckMCPServer tests a server where it runs: see the file's comment. The
// refusals are the restart's ([ErrNoMCPServer], [ErrMCPServerOff],
// [ErrMCPSeatOnShared], [ErrNoAgentSeat], [ErrMCPSeatNotGranted]) and
// [ErrMCPSeatRequired]; any other error is a request this node could not make.
func (e *Engine) CheckMCPServer(ctx context.Context, req MCPCheckRequest) (MCPCheckResult, error) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return MCPCheckResult{}, fmt.Errorf("%w: %q", ErrNoMCPServer, req.Server)
	}
	server, err := checkedEntry(c, req)
	if err != nil {
		return MCPCheckResult{}, err
	}
	ask := mcpCheckAsk{Version: mcpCheckWire, ID: signedask.NewID(), Asker: e.selfNode(), Server: server.Name,
		Draft: req.Draft}
	if (req.Entry != nil || len(req.Values) > 0) && e.cipher == nil {
		return MCPCheckResult{}, errNoValueKeyring
	}
	if req.Entry != nil {
		// TESTED TURNED ON, whatever its `enabled` says: how it runs is what was asked.
		entry := *req.Entry
		entry.Enabled = config.Toggle{}
		raw, marshalErr := json.Marshal(entry)
		if marshalErr != nil {
			return MCPCheckResult{}, fmt.Errorf("engine: write the entry under test: %w", marshalErr)
		}
		sealed, sealErr := e.cipher.Encrypt(string(raw), mcpEntryAAD(ask.ID))
		if sealErr != nil {
			return MCPCheckResult{}, fmt.Errorf("engine: seal the entry under test: %w", sealErr)
		}
		ask.Entry = sealed
	}
	for _, tried := range req.Values {
		sealed, sealErr := e.cipher.Encrypt(tried.Value, mcpValueAAD(ask.ID, tried.Variable))
		if sealErr != nil {
			return MCPCheckResult{}, fmt.Errorf("engine: seal the value under test: %w", sealErr)
		}
		ask.Values = append(ask.Values, mcpValueAsk{Key: tried.Key, Variable: tried.Variable, Sealed: sealed})
	}
	seat := strings.TrimSpace(req.Seat)
	switch {
	case server.IsShared() && seat != "":
		return MCPCheckResult{}, fmt.Errorf("%w: %q", ErrMCPSeatOnShared, server.Name)
	case !server.IsShared() && seat == "":
		return MCPCheckResult{}, fmt.Errorf("%w: %q", ErrMCPSeatRequired, server.Name)
	case !server.IsShared():
		role := c.Org.AgentSeatByHandle(seat)
		if role == nil {
			return MCPCheckResult{}, fmt.Errorf("%w: %q", ErrNoAgentSeat, seat)
		}
		if _, reached := server.Reach(role); !reached {
			return MCPCheckResult{}, fmt.Errorf("%w: %q is not granted %q", ErrMCPSeatNotGranted, seat, server.Name)
		}
		id, ok := c.Org.AgentIDFor(role)
		if !ok {
			return MCPCheckResult{}, fmt.Errorf("%w: %q", ErrNoAgentSeat, seat)
		}
		ask.Seat = id
	}
	at := e.now()
	out := MCPCheckResult{Server: server.Name, Seat: seat, Draft: req.Draft, At: at, By: req.By.Name,
		Node: e.selfNode()}
	budget := mcpCheckBudget(server)
	ask.Deadline = at.Add(budget)
	if ask.Seat == uuid.Nil {
		nodes, readErr := e.liveNodes(ctx)
		if readErr != nil {
			return MCPCheckResult{}, fmt.Errorf("engine: read the nodes to test the server on: %w", readErr)
		}
		if !slices.Contains(nodes, e.selfNode()) {
			nodes = append(nodes, e.selfNode())
		}
		ask.Nodes = slices.Compact(slices.Sorted(slices.Values(nodes)))
	}
	out.Nodes, err = e.askMCPCheck(ctx, ask, budget)
	if err != nil {
		return MCPCheckResult{}, err
	}
	answered := 0
	out.OK = true
	for _, n := range out.Nodes {
		if n.Answered {
			answered++
			out.OK = out.OK && n.Report.Serving()
		}
	}
	out.OK = out.OK && answered > 0
	// The latest test is of the entry the company runs.
	if req.Entry == nil {
		e.recordMCPCheck(ctx, out, ask.Seat, req.By)
	}
	return out, nil
}

// checkedEntry is the entry req tests: the one it sends (tested turned on), or
// the saved one this node runs, refused when turned off.
func checkedEntry(c *Company, req MCPCheckRequest) (*config.MCPServer, error) {
	if req.Entry != nil {
		return req.Entry, nil
	}
	if len(req.Values) > 0 {
		return nil, errValuesWithoutEntry
	}
	i := slices.IndexFunc(c.Config.MCPServers, func(s config.MCPServer) bool { return s.Name == req.Server })
	if i < 0 {
		return nil, fmt.Errorf("%w: %q", ErrNoMCPServer, req.Server)
	}
	server := &c.Config.MCPServers[i]
	if !server.IsEnabled() {
		return nil, fmt.Errorf("%w: %q", ErrMCPServerOff, req.Server)
	}
	return server, nil
}

// askMCPCheck asks the nodes ask names (or, for a seat's test, the seat's
// holder) to test the server, this node directly where it is one of them, and
// answers each node asked with what it found or why it did not answer.
func (e *Engine) askMCPCheck(ctx context.Context, ask mcpCheckAsk, budget time.Duration) ([]MCPCheckedNode, error) {
	self := e.selfNode()
	if ask.Seat != uuid.Nil {
		// THE HOLDER ALONE ANSWERS: this node, where it holds the seat, and
		// otherwise the one peer that does.
		if here := e.testHere(ctx, ask); here != nil {
			return []MCPCheckedNode{{Node: self, Answered: true, Report: *here}}, nil
		}
		replies, reason, err := e.askPeersMCPCheck(ctx, ask, nil, budget)
		if err != nil {
			return nil, err
		}
		for node, reply := range replies {
			return []MCPCheckedNode{{Node: node, Answered: true, Report: reply.Report}}, nil
		}
		return []MCPCheckedNode{{Reason: "no node holding the seat answered: " + reason}}, nil
	}
	var peers []string
	for _, n := range ask.Nodes {
		if n != self {
			peers = append(peers, n)
		}
	}
	local := make(chan *mcp.TestReport, 1)
	go func() { local <- e.testHere(ctx, ask) }()
	replies := map[string]mcpCheckReply{}
	reason := ""
	if len(peers) > 0 {
		var err error
		replies, reason, err = e.askPeersMCPCheck(ctx, ask, peers, budget)
		if err != nil {
			<-local
			return nil, err
		}
	}
	here := <-local
	out := make([]MCPCheckedNode, 0, len(ask.Nodes))
	for _, node := range ask.Nodes {
		switch reply, ok := replies[node]; {
		case node == self && here != nil:
			out = append(out, MCPCheckedNode{Node: node, Answered: true, Report: *here})
		case ok:
			out = append(out, MCPCheckedNode{Node: node, Answered: true, Report: reply.Report})
		default:
			out = append(out, MCPCheckedNode{Node: node, Reason: reason})
		}
	}
	return out, nil
}

// askPeersMCPCheck asks peers (or every node, for a seat's test) over the
// signed channel, and answers each reply that verified, by node, with the
// reason the rest are named for.
func (e *Engine) askPeersMCPCheck(ctx context.Context, ask mcpCheckAsk, peers []string, budget time.Duration,
) (map[string]mcpCheckReply, string, error) {
	if e.backends == nil || e.backends.Queue == nil || e.mcpChecks == nil {
		return map[string]mcpCheckReply{}, "this node has no broker to ask its peers over", nil
	}
	sealed, err := e.mcpChecks.wire.SealRequest(ask)
	if err != nil {
		return nil, "", fmt.Errorf("engine: seal an MCP test request: %w", err)
	}
	askCtx, cancel := context.WithDeadline(ctx, ask.Deadline)
	defer cancel()
	asked := make(map[string]bool, len(peers))
	for _, p := range peers {
		asked[p] = true
	}
	self := e.selfNode()
	seatTest := ask.Seat != uuid.Nil
	out := map[string]mcpCheckReply{}
	err = e.backends.Queue.AskEach(askCtx, topics.MCPCheck, sealed, func(body []byte) bool {
		reply, why, openErr := e.mcpChecks.wire.OpenReply(body, ask.ID)
		switch {
		case openErr != nil || why != "":
			log.WarnContext(ctx, "mcp_check_reply_refused", "server", ask.Server,
				"detail", "a reply to an MCP test that did not verify was not read")
			return true
		case reply.Version != mcpCheckWire || reply.Node == self:
			return true
		case !seatTest && !asked[reply.Node]:
			return true
		}
		if _, dup := out[reply.Node]; !dup {
			out[reply.Node] = reply
		}
		// A SEAT'S TEST HAS ONE ANSWER, its holder's.
		return !seatTest && len(out) < len(peers)
	})
	if err != nil {
		if errors.Is(err, queue.ErrUnavailable) {
			return out, "this node could not reach the broker to ask", nil
		}
		return nil, "", fmt.Errorf("engine: ask the nodes to test the MCP server: %w", err)
	}
	return out, "did not answer within " + budget.String(), nil
}

// errMCPCheckNotHere is a test this node does not answer: addressed to other
// nodes, or for a seat it does not hold.
var errMCPCheckNotHere = errors.New("engine: the MCP test is for other nodes")

// answerMCPCheck is the serve handler: one test request, answered only where
// the fleet signed it and it names this node (or a seat this node holds).
func (e *Engine) answerMCPCheck(ctx context.Context, raw []byte) ([]byte, error) {
	admitted := e.mcpChecks.wire.Admit(raw, e.mcpChecks.taken, time.Now())
	switch admitted.Disposition {
	case signedask.Admitted:
	case signedask.Tampered:
		log.ErrorContext(ctx, "mcp_check_refused", "disposition", string(admitted.Disposition),
			"detail", admitted.Why+"; nothing was tested")
		return nil, errors.New("engine: an MCP test the fleet did not sign")
	default:
		return nil, fmt.Errorf("engine: an MCP test not to be taken: %s", admitted.Why)
	}
	ask := admitted.Request
	self := e.selfNode()
	if ask.Version != mcpCheckWire || ask.Asker == self ||
		(ask.Seat == uuid.Nil && !slices.Contains(ask.Nodes, self)) {
		return nil, errMCPCheckNotHere
	}
	// ITS ANSWER HAS TO REACH THE ASKER BEFORE THE DEADLINE, so the test
	// ends a reply's round trip before it.
	testCtx, cancel := context.WithDeadline(ctx, ask.Deadline.Add(-mcpCheckReplyMargin))
	defer cancel()
	report := e.testHere(testCtx, ask)
	if report == nil {
		return nil, errMCPCheckNotHere
	}
	return e.mcpChecks.wire.SealReply(mcpCheckReply{Version: mcpCheckWire, Request: ask.ID,
		Node: e.selfNode(), Report: *report})
}

// testHere tests the server ask names on this node, nil where this node does
// not answer it: a seat's test on a node that cannot prove it holds the seat.
// The report is bounded and redacted, as every node's is before it leaves.
//
// ONE TEST OF AN ENTRY AT A TIME ON A NODE ([mcpTestKey]): a test holds a
// process or a connection for up to its budget, so a second asked while the
// first runs is answered at once that one is running, rather than starting
// another beside it. A person asking again gets the first one's answer by
// asking once it has come.
func (e *Engine) testHere(ctx context.Context, ask mcpCheckAsk) *mcp.TestReport {
	c := e.Company()
	if ask.Seat != uuid.Nil {
		if s := e.seatChildrenOf(ask.Seat); s == nil || !s.held() {
			// THE SEAT'S HOLDER ALONE: no other node may act as the seat.
			return nil
		}
	}
	key := mcpTestKey(ask)
	if _, running := e.mcpTesting.LoadOrStore(key, struct{}{}); running {
		report := boundedReport(mcp.Unstarted(mcp.Spec{Name: ask.Server},
			"a test of this server is already running on this node: ask again once it has answered"), nil)
		return &report
	}
	defer e.mcpTesting.Delete(key)
	var report mcp.TestReport
	sent, tried, openErr := e.openAsk(ask)
	switch server := testedEntry(c, ask, sent); {
	case openErr != nil:
		report = mcp.Unstarted(mcp.Spec{Name: ask.Server},
			"this node could not open what it was sent to test: "+openErr.Error())
	case server == nil:
		report = mcp.Unstarted(mcp.Spec{Name: ask.Server},
			"this node runs no company that declares the server; it may not have applied the latest revision yet")
	case ask.Seat == uuid.Nil && !ask.Draft && server.PersonSignIn():
		// A SAVED SERVER SIGNED IN TO WITH OAUTH is tested under its grant,
		// as it runs, a value under test included (its entry sent as the
		// fleet's active revision states it). A draft never is, whatever its
		// name: the grant's token goes to the address it was issued for, and
		// a draft's address is somebody's typing.
		report = e.testServer(ctx, server, nil, nil, nil, nil, tried, e.testBearer(ctx, server.Name))
	case ask.Seat == uuid.Nil:
		report = e.testServer(ctx, server, nil, nil, nil, nil, tried, nil)
	default:
		role := c.Org.AgentSeatByID(ask.Seat)
		if role == nil {
			return nil
		}
		// THE SEAT'S BLOCK FOR THE ENTRY UNDER TEST, a draft's included: its
		// save binds the seat's own credential to it, and a draft is decided
		// as that save, so it is tested with what the save will hand the seat.
		block := c.Config.SeatToolEnv(server, role.Handle())
		env, token := e.seatResolver(ctx, c, role, toolTokenLife)
		report = e.testServer(ctx, server, role, block, env,
			&seatMint{kinds: e.mintedFor(c, role.Handle()), token: token}, tried, nil)
	}
	report = boundedReport(report, e.epochTools())
	return &report
}

// openAsk opens what ask sends to be tested: the entry (nil for the one this
// node runs) and every value under test (none for none).
func (e *Engine) openAsk(ask mcpCheckAsk) (*config.MCPServer, []MCPValueTry, error) {
	if ask.Entry == "" && len(ask.Values) == 0 {
		return nil, nil, nil
	}
	if e.cipher == nil {
		return nil, nil, errNoValueKeyring
	}
	var entry *config.MCPServer
	if ask.Entry != "" {
		raw, err := e.cipher.Decrypt(ask.Entry, mcpEntryAAD(ask.ID))
		if err != nil {
			return nil, nil, fmt.Errorf("the fleet's keyring does not open the entry here: %w", err)
		}
		entry = &config.MCPServer{}
		if err = json.Unmarshal([]byte(raw), entry); err != nil {
			return nil, nil, fmt.Errorf("the entry it was sent is not one this build reads: %w", err)
		}
	}
	tried := make([]MCPValueTry, 0, len(ask.Values))
	for _, v := range ask.Values {
		value, err := e.cipher.Decrypt(v.Sealed, mcpValueAAD(ask.ID, v.Variable))
		if err != nil {
			return nil, nil, fmt.Errorf("the fleet's keyring does not open a value under test here: %w", err)
		}
		tried = append(tried, MCPValueTry{Key: v.Key, Variable: v.Variable, Value: value})
	}
	return entry, tried, nil
}

// mcpTestKey names what a test tests on a node: the kind of entry, its name
// and the seat.
func mcpTestKey(ask mcpCheckAsk) string {
	kind := "saved"
	switch {
	case len(ask.Values) > 0:
		kind = "value"
	case ask.Entry != "":
		kind = "draft"
	}
	return kind + "/" + ask.Server + "/" + ask.Seat.String()
}

// testedEntry is the entry ask tests as this node's company holds it, nil
// where it holds none.
func testedEntry(c *Company, ask mcpCheckAsk, sent *config.MCPServer) *config.MCPServer {
	if sent != nil {
		return sent
	}
	if c == nil || c.Config == nil {
		return nil
	}
	i := slices.IndexFunc(c.Config.MCPServers, func(s config.MCPServer) bool { return s.Name == ask.Server })
	if i < 0 {
		return nil
	}
	return &c.Config.MCPServers[i]
}

// testServer tests server on this node, as a seat when seat is set (with
// block, the seat's values for it), with the values a start of it would be
// given: the same resolution ([resolveServer]), the same spec ([serverSpec])
// and so the same address rule and the same launch ([mcp.Test]); with tried,
// each of its keys reads its value. bearer is the grant's token source of a saved server a person signed
// in to with OAuth, nil for any other, and is sent only where the test dials
// the origin the saved server is dialled at. A service identity signs in for
// the test itself.
func (e *Engine) testServer(ctx context.Context, server *config.MCPServer, seat *org.Role, block map[string]string,
	env *config.Resolver, mint *seatMint, tried []MCPValueTry, bearer mcp.BearerSource) mcp.TestReport {

	instance, handle := server.Name, ""
	if seat != nil {
		instance, handle = mcp.InstanceName(server.Name, seat.Name), seat.Handle()
	}
	if env == nil {
		env = e.resolver()
	}
	if len(tried) > 0 {
		server, block = withTried(server, block, seat != nil, tried)
		values := make(map[string]string, len(tried))
		for _, t := range tried {
			values[t.Variable] = t.Value
		}
		env = env.Trying(values)
	}
	unstarted := mcp.Spec{Name: instance, Transport: mcp.TransportKind(server.Transport),
		Command: server.Command, URL: server.URL}
	values, missing := resolveServer(server, handle, instance, block, env, mint)
	if missing != nil {
		return mcp.Unstarted(unstarted, missing.Error())
	}
	spec, err := serverSpec(server, instance, values)
	if err != nil {
		return mcp.Unstarted(unstarted, err.Error())
	}
	// THE GRANT'S TOKEN GOES WHERE THE SERVER RUNS AND NOWHERE ELSE: the
	// origin of the entry this node runs, which the grant was checked against
	// ([Engine.grantMoved]). A value under test that moves the server's
	// address, or an entry sent with the test (the fleet's active revision's,
	// ahead of this node's apply) that names another, takes it to an origin
	// the grant was not issued for, which is tested without it.
	if bearer != nil && mcp.SameOrigin(values.url, e.grantedAddress(server.Name)) {
		spec.Bearer = bearer
	}
	// A SERVICE IDENTITY SIGNS IN FOR ITS TEST, saved or a draft, with what
	// the test resolves, a value under test included: its own client, for
	// the address under test, so no token goes anywhere it was not issued
	// for. A source of its own, which asks the vendor now rather than
	// answering with a token this node already holds, and holds nothing the
	// running server reads.
	if server.OAuth.ServiceIdentity() {
		spec.Bearer = &oauthSource{engine: e, server: instance, service: e.newServiceIdentity(server, env)}
	}
	ctx, cancel := context.WithTimeout(ctx, mcpCheckBudget(server))
	defer cancel()
	return mcp.Test(ctx, spec, nil)
}

// grantedAddress is the address of the entry called name this node runs, the
// one its OAuth grant is checked against, resolved; "" where it runs none.
func (e *Engine) grantedAddress(name string) string {
	entry, err := e.oauthServer(name)
	if err != nil {
		return ""
	}
	return e.resolver().Value(entry.URL)
}

// withTried points each tried key at its variable in copies: the OAuth
// client's secret for [config.MCPValueClientSecret], the seat's block for a
// seat's test, the server's own map otherwise. A key spelled another way in
// the map (`authorization` for `Authorization`) gives way to it.
func withTried(server *config.MCPServer, block map[string]string, forSeat bool,
	tried []MCPValueTry) (*config.MCPServer, map[string]string) {

	copied := *server
	keyed := make([]MCPValueTry, 0, len(tried))
	for _, t := range tried {
		// THE OAUTH CLIENT'S SECRET is the server's own, whichever seat asks,
		// and is sent to its authorization server alone.
		if t.Key == config.MCPValueClientSecret && copied.OAuth != nil {
			signIn := *copied.OAuth
			signIn.ClientSecret = "${" + t.Variable + "}"
			copied.OAuth = &signIn
			continue
		}
		keyed = append(keyed, t)
	}
	if len(keyed) == 0 {
		return &copied, block
	}
	point := func(values map[string]string) map[string]string {
		under := make(map[string]bool, len(keyed))
		for _, t := range keyed {
			under[server.ValueKey(t.Key)] = true
		}
		out := make(map[string]string, len(values)+len(keyed))
		for key, value := range values {
			if !under[server.ValueKey(key)] {
				out[key] = value
			}
		}
		for _, t := range keyed {
			out[t.Key] = "${" + t.Variable + "}"
		}
		return out
	}
	if forSeat {
		return &copied, point(block)
	}
	if copied.Kind() == config.TransportHTTP {
		copied.Headers = point(server.Headers)
	} else {
		copied.Env = point(server.Env)
	}
	return &copied, block
}

// boundedReport is report as it leaves this node: every sentence passed
// through the redaction pass and cut to [coord.MaxMCPErrorBytes], the
// server's identity cut to [coord.MaxMCPServerInfoBytes] with its
// instructions left out, every tool the server listed with its names and words
// cut and all of them within [maxMCPCheckToolBytes] ([toolsWithin]), and each
// tool whose name a built-in of surface holds refused for it, since built-ins
// are filed first. EVERY TOOL: the first listing of a server is accepted as
// the person adding it was shown it, so a test shows them all.
//
// NO INSTRUCTIONS: they are the server's words for a model, and a test's
// answer is read by people and, on approval, by the assistant; the server's
// detail carries them to the configuration's reader.
func boundedReport(report mcp.TestReport, surface *tools.Registry) mcp.TestReport {
	out := report
	out.Steps = make([]mcp.TestStep, len(report.Steps))
	for i, step := range report.Steps {
		step.Detail = boundedSaid(step.Detail)
		out.Steps[i] = step
	}
	if report.Failure != nil {
		failure := *report.Failure
		failure.Said = boundedSaid(failure.Said)
		out.Failure = &failure
	}
	out.Identity = mcp.Identity{Name: infoField(report.Identity.Name), Title: infoField(report.Identity.Title),
		Version: infoField(report.Identity.Version), Protocol: infoField(report.Identity.Protocol)}
	out.Tools = make([]mcp.TestedTool, 0, len(report.Tools))
	for _, t := range report.Tools {
		t.Name = textcut.Bytes(t.Name, coord.MaxMCPToolNameBytes)
		t.Raw = textcut.Bytes(t.Raw, coord.MaxMCPToolNameBytes)
		if t.Refused == "" && surface != nil {
			if entry, filed := surface.Lookup(t.Name); filed {
				if _, fromMCP := entry.FromMCP(); !fromMCP {
					t.Refused = mcp.NameBuiltin
				}
			}
		}
		out.Tools = append(out.Tools, t)
	}
	out.Tools = toolsWithin(out.Tools, maxMCPCheckToolBytes)
	return out
}

// toolsWithin is tools with their words cut until they encode within budget
// bytes: every title and description to its bound first, then every
// description to half as many bytes again until they fit, then every title,
// each cut marked. Every tool stays.
func toolsWithin(tools []mcp.TestedTool, budget int) []mcp.TestedTool {
	description, title := coord.MaxMCPToolDescriptionBytes, coord.MaxMCPToolTitleBytes
	for {
		out := slices.Clone(tools)
		for i := range out {
			out[i].Title = textcut.Bytes(out[i].Title, coord.MaxMCPToolTitleBytes)
			out[i].Description = textcut.Bytes(out[i].Description, coord.MaxMCPToolDescriptionBytes)
			if description < coord.MaxMCPToolDescriptionBytes {
				out[i].Description = textcut.Within(out[i].Description, description)
			}
			if title < coord.MaxMCPToolTitleBytes {
				out[i].Title = textcut.Within(out[i].Title, title)
			}
		}
		raw, err := json.Marshal(out)
		if err != nil || len(raw) <= budget || title == 0 {
			return out
		}
		if description > 0 {
			description /= 2
		} else {
			title /= 2
		}
	}
}

// infoField is one field of a server's identity as a reader on another node
// is handed it.
func infoField(s string) string { return textcut.Bytes(s, coord.MaxMCPServerInfoBytes) }

// recordMCPCheck keeps a saved server's test as its latest ([coord.MCPCheck]),
// bounded to [coord.MaxMCPCheckNodes] nodes. A test the store could not
// record is still the answer to the person who asked; the page shows the
// previous one until the next test.
func (e *Engine) recordMCPCheck(ctx context.Context, result MCPCheckResult, seat uuid.UUID, by iam.Actor) {
	if e.backends == nil || e.backends.Fleet == nil {
		return
	}
	record := coord.MCPCheck{Server: result.Server, Seat: seat, At: result.At, By: by.Name, ByKind: by.Kind,
		OperatorID: by.OperatorID, OK: result.OK}
	for _, n := range result.Nodes[:min(len(result.Nodes), coord.MaxMCPCheckNodes)] {
		record.Nodes = append(record.Nodes, checkedNodeRecord(n))
	}
	if _, err := e.backends.Fleet.PutMCPCheck(ctx, record); err != nil {
		log.WarnContext(ctx, "mcp_check_not_recorded", "server", result.Server, "error", err,
			"detail", "the test was answered; the server's page shows its previous test until the next one")
	}
}

// checkedNodeRecord is one node's test as the coordination store keeps it:
// its stages, its failure and its counts, never its tools.
func checkedNodeRecord(n MCPCheckedNode) coord.MCPCheckNode {
	node := n.Node
	if node == "" {
		node = "unanswered"
	}
	out := coord.MCPCheckNode{Node: node, Answered: n.Answered, Reason: boundedSaid(n.Reason)}
	if !n.Answered {
		return out
	}
	r := n.Report
	for _, step := range r.Steps {
		out.Steps = append(out.Steps, coord.MCPCheckStep{Stage: string(step.Stage), State: string(step.State),
			Took: step.Took, Detail: boundedSaid(step.Detail)})
	}
	if r.Failure != nil {
		out.Failure = &coord.MCPInstanceFailure{Class: string(r.Failure.Class), Said: boundedSaid(r.Failure.Said),
			At: r.Failure.At}
	}
	out.Server = coord.MCPServerInfo{Name: infoField(r.Identity.Name), Title: infoField(r.Identity.Title),
		Version: infoField(r.Identity.Version), Protocol: infoField(r.Identity.Protocol)}
	for _, t := range r.Tools {
		if t.Refused == "" {
			out.Tools++
		} else {
			out.Refused++
		}
	}
	out.Took = r.Took
	return out
}

// LatestMCPCheck reads a server's latest test, false where it has none.
func (e *Engine) LatestMCPCheck(ctx context.Context, server string) (coord.MCPCheck, bool, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return coord.MCPCheck{}, false, nil
	}
	return e.backends.Fleet.MCPCheck(ctx, server)
}

// clearRemovedMCPChecks deletes the latest test of every server the next
// revision removed: a test goes with what it tested, as a restart does
// ([Engine.clearRemovedMCPRestarts]).
func (e *Engine) clearRemovedMCPChecks(ctx context.Context, next *Company) {
	if next == nil || next.Config == nil || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	if err := e.fleetCompany(ctx, next); err != nil {
		log.DebugContext(ctx, "mcp_checks_not_swept", "reason", err.Error(),
			"detail", "a removed server's test is cleared by the company the fleet is pointed "+
				"at; this one is not it yet")
		return
	}
	checks, err := e.backends.Fleet.ListMCPChecks(ctx)
	if err != nil {
		log.WarnContext(ctx, "mcp_checks_not_swept", "error", err,
			"detail", "a removed server's latest test stays until the next apply clears it")
		return
	}
	for _, check := range checks {
		if slices.ContainsFunc(next.Config.MCPServers, func(s config.MCPServer) bool { return s.Name == check.Server }) {
			continue
		}
		if _, err := e.backends.Fleet.DeleteMCPCheck(ctx, check.Server, check.Version); err != nil {
			log.WarnContext(ctx, "mcp_check_not_cleared", "server", check.Server, "error", err)
		}
	}
}

// startMCPChecks makes this node an answerer of tests of the servers it runs,
// until stopMCPChecks. EVERY NODE WITH AN ENGINE SERVES, data or not, as the
// instance reads do: every node runs the shared servers and the seats it holds.
func (e *Engine) startMCPChecks(ctx context.Context) {
	if e.mcpChecks == nil || e.backends == nil || e.backends.Queue == nil {
		return
	}
	stop, err := e.backends.Queue.Serve(ctx, topics.MCPCheck, queue.ConcurrentAnswers, e.answerMCPCheck)
	if err != nil {
		log.WarnContext(ctx, "mcp_check_answerer_not_registered", "error", err,
			"detail", "a test asked through another node reads this node as not answering until "+
				"the node restarts; everything else works")
		return
	}
	e.mcpChecks.stop = stop
}

// stopMCPChecks withdraws this node as an answerer, before its servers stop.
func (e *Engine) stopMCPChecks(ctx context.Context) {
	if e.mcpChecks == nil || e.mcpChecks.stop == nil {
		return
	}
	if err := e.mcpChecks.stop(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "mcp_check_answerer_not_withdrawn", "error", err)
	}
	e.mcpChecks.stop = nil
}

// CheckMCPAddress checks the address raw before a server is added at it, on
// this node ([mcp.CheckAddress]): an address the configuration's rule refuses
// is refused there, before anything is dialled, as a write would refuse it.
func (e *Engine) CheckMCPAddress(ctx context.Context, raw string) mcp.AddressCheck {
	if problem := config.MCPAddressProblem(raw); problem != "" {
		return mcp.AddressCheck{Test: boundedReport(mcp.Unstarted(
			mcp.Spec{Name: "address-check", Transport: mcp.TransportHTTP, URL: raw},
			"the address "+redact.URL(raw)+" "+problem), nil)}
	}
	check := mcp.CheckAddress(ctx, raw, nil)
	check.Test = boundedReport(check.Test, e.epochTools())
	if check.OAuth != nil && check.OAuth.Failure != nil {
		failure := *check.OAuth.Failure
		failure.Said = boundedSaid(failure.Said)
		check.OAuth.Failure = &failure
	}
	return check
}
