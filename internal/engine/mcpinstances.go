package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/tools"
)

// EACH MCP INSTANCE'S HEALTH, read where it lives.
//
// A node knows its own children (the shared servers' and each held seat's)
// and nothing of a peer's, and that is where it stays: the heartbeat carries
// each server's bounded counts per node ([coord.MCPServerStatus]), and a
// server's page asks the nodes that hold its instances for the rest when a
// person opens it, as a turn's detail is read (ADR-0021). Nothing is stored
// and nothing rides the heartbeat that a seat count multiplies.
//
// # One reading, two readers
//
// The heartbeat's row and the page's instances are read off the same
// [mcpReading] of each instance, so a server the row calls failing on a node
// is one whose instances on that node read failed, with the same reason.
//
// # The scatter
//
// The node serving the question reads its own instances directly and asks
// every other node named over the broker's ephemeral request and reply
// ([topics.ObserveMCPInstances]), within [mcpInstanceReadBudget]; a node that
// does not answer in time is named in the answer as not having answered, never
// read as one that holds nothing. A read of a node's own memory, which no
// node carries out as an instruction, so it is not a signed channel; a reply
// names the request it answers, so a stray reply is never read as this one's.

// mcpInstanceReadBudget bounds how long a read of one server's instances waits
// for the nodes it asks: the fleet's history read budget
// ([eventfan.FleetReadBudget], two seconds), because it is the same kind of
// read (a person's screen waiting on every node's own memory) and a node that
// has not answered by then is one the screen says so about, rather than one it
// waits on.
const mcpInstanceReadBudget = eventfan.FleetReadBudget

// mcpInstancesWire is the request and reply shape this build speaks; a node
// answers only a request in its own version, and an asker reads only a reply
// in it. Evolution is a new version, since a field a peer did not send would
// read as a zero.
const mcpInstancesWire = 1

// mcpInstancesRequest asks the nodes named for their instances of one server.
type mcpInstancesRequest struct {
	Version int      `json:"v"`
	ID      string   `json:"id"`
	Server  string   `json:"server"`
	Nodes   []string `json:"nodes"`
}

// mcpInstancesReply is one node's answer: its instances of the server, at most
// [coord.MaxMCPInstances] of them, and how many it holds.
type mcpInstancesReply struct {
	Version   int                 `json:"v"`
	ID        string              `json:"id"`
	Node      string              `json:"node"`
	Instances []coord.MCPInstance `json:"instances"`
	Count     int                 `json:"count"`
}

// mcpReading is one instance as this node holds it at the moment it is read:
// its start's outcome, judged against what its bridge says now.
type mcpReading struct {
	server   string
	seat     uuid.UUID
	state    coord.MCPInstanceState
	since    time.Time
	restarts int
	tools    int
	// unlisted are the entry's tool_annotations keys this instance's
	// listing names no tool for.
	unlisted []string
	// refused and shadowed are what its tools lost to their names, read only
	// where a bridge still supervises it.
	refused  []mcp.RefusedTool
	shadowed []mcp.ShadowedTool
	// failure is why a failed instance is not serving, and class its class;
	// wait is what a waiting one waits for.
	failure error
	class   mcp.FailureClass
	wait    *credentialMissing
	// last is the latest failure, kept after the instance came back, and
	// answered when it last answered.
	last     *mcp.Failure
	answered time.Time
}

// mcpReadings is every instance this node holds, sorted by server then seat,
// each AS IT IS: an instance its bridge still supervises is read off the
// bridge's own record, so a child that died since its start reads failed,
// with what took it down, until it is back.
//
// A seat's child held back for a credential a provisioning pass has not sealed
// yet is WAITING, and JUDGED AGAIN AT EVERY READ against what the integration
// minting it last recorded ([credentialMissing.judged]): see [Engine.mcpStatus].
func (e *Engine) mcpReadings(ctx context.Context) []mcpReading {
	// The surface each instance is filed into: the epoch's registry for a
	// shared server, the seat's own for a seat's child.
	epoch := e.epochTools()
	e.mcpMu.Lock()
	all := make([]filedOutcome, 0, len(e.mcpShared))
	for _, o := range e.mcpShared {
		all = append(all, filedOutcome{mcpOutcome: o, surface: epoch})
	}
	for handle, outcomes := range e.mcpSeats {
		for _, o := range outcomes {
			all = append(all, filedOutcome{mcpOutcome: o, surface: e.seatTools[handle]})
		}
	}
	e.mcpMu.Unlock()
	slices.SortFunc(all, func(a, b filedOutcome) int {
		return cmp.Or(cmp.Compare(a.server, b.server), cmp.Compare(a.seat.String(), b.seat.String()))
	})
	var withheld []*credentialMissing
	for _, o := range all {
		var missing *credentialMissing
		if errors.As(o.err, &missing) {
			withheld = append(withheld, missing)
		}
	}
	states := e.mintingStates(ctx, withheld)
	out := make([]mcpReading, 0, len(all))
	for _, o := range all {
		r := mcpReading{server: o.server, seat: o.seat, state: coord.MCPInstanceRunning,
			since: o.since, tools: o.tools, unlisted: o.unlisted}
		failure := o.err
		if health, ok := mcpHealth(o.mcpOutcome); ok {
			// THE LISTING NOW, like its tools: a restart can come back
			// listing a different set, so the keys it leaves out are read
			// off the same record. And so are the tools it lost to their
			// names, which a server that is down keeps from its last
			// listing, as it keeps its tools.
			r.tools, r.unlisted, failure = health.Tools, o.bridge.UnlistedOverrides(o.instance), nil
			r.refused, r.shadowed = unfiled(o.mcpOutcome, o.surface)
			r.restarts, r.since, r.last, r.answered = health.Restarts, health.Since, health.LastFailure, health.LastAnswer
			if !health.Serving {
				failure, r.class = errors.New(health.Down), health.Class
			}
		} else if failure != nil {
			// A START NO BRIDGE KEPT is its outcome's whole record: a
			// seat's child whose first start failed, or a configuration
			// that did not resolve into a spec.
			r.class = mcp.ClassOf(failure)
			r.last = &mcp.Failure{Class: r.class, Said: failure.Error(), At: o.since}
		}
		var missing *credentialMissing
		if errors.As(failure, &missing) {
			missing = missing.judged(states)
			if missing.waiting() {
				r.state, r.wait, r.last = coord.MCPInstanceWaiting, missing, nil
				out = append(out, r)
				continue
			}
			failure = missing
		}
		if failure != nil {
			r.state, r.failure = coord.MCPInstanceFailed, failure
		}
		out = append(out, r)
	}
	return out
}

// epochTools is the current epoch's registry, nil before one is published.
func (e *Engine) epochTools() *tools.Registry {
	if c := e.Company(); c != nil {
		return c.Tools
	}
	return nil
}

// instance is the reading as a peer is answered it: every sentence bounded to
// [coord.MaxMCPErrorBytes] and passed through the redaction pass first, since
// a failure carries what a server or its process said, which can hold a
// credential it never meant to print.
func (r mcpReading) instance(node string) coord.MCPInstance {
	out := coord.MCPInstance{
		Server: r.server, Seat: r.seat, Node: node, State: r.state, Since: r.since,
		Restarts: r.restarts, Tools: r.tools, LastAnswer: r.answered,
	}
	if r.failure != nil {
		out.Failure = &coord.MCPInstanceFailure{Class: string(r.class), Said: boundedSaid(r.failure.Error()),
			At: r.since}
	}
	if r.wait != nil {
		out.Wait = boundedSaid(r.wait.Error())
	}
	if r.last != nil {
		out.LastFailure = &coord.MCPInstanceFailure{Class: string(r.last.Class),
			Said: boundedSaid(r.last.Said), At: r.last.At}
	}
	return out
}

// boundedSaid is a sentence about a server as a reader on another node is
// handed it: redacted, then cut to [coord.MaxMCPErrorBytes].
func boundedSaid(s string) string { return textcut.Within(redact.Secrets(s), coord.MaxMCPErrorBytes) }

// localMCPInstances is this node's instances of server, the first
// [coord.MaxMCPInstances] by seat id, and how many it holds.
func (e *Engine) localMCPInstances(ctx context.Context, server string) ([]coord.MCPInstance, int) {
	node := e.selfNode()
	out := []coord.MCPInstance{}
	count := 0
	for _, r := range e.mcpReadings(ctx) {
		if r.server != server {
			continue
		}
		count++
		if len(out) < coord.MaxMCPInstances {
			out = append(out, r.instance(node))
		}
	}
	return out, count
}

// MCPInstances reads every instance of server the nodes named hold: this
// node's own directly, every other node's over the broker within
// [mcpInstanceReadBudget]. Each node asked is named in the answer, with
// whether it answered; a node that did not is never read as one holding
// nothing. An error is a request this node could not make at all.
func (e *Engine) MCPInstances(ctx context.Context, server string, nodes []string) (coord.MCPInstanceRead, error) {
	self := e.selfNode()
	nodes = slices.Compact(slices.Sorted(slices.Values(nodes)))
	out := coord.MCPInstanceRead{Instances: []coord.MCPInstance{}, Nodes: make([]coord.MCPInstanceNode, 0, len(nodes))}
	var peers []string
	for _, node := range nodes {
		if node != self {
			peers = append(peers, node)
		}
	}
	replies := map[string]mcpInstancesReply{}
	reason := ""
	if len(peers) > 0 {
		var err error
		replies, reason, err = e.askMCPInstances(ctx, server, peers)
		if err != nil {
			return coord.MCPInstanceRead{}, err
		}
	}
	for _, node := range nodes {
		if node == self {
			instances, count := e.localMCPInstances(ctx, server)
			out.Instances = append(out.Instances, instances...)
			out.Nodes = append(out.Nodes, coord.MCPInstanceNode{Node: node, Answered: true, Count: count})
			continue
		}
		reply, answered := replies[node]
		if !answered {
			out.Nodes = append(out.Nodes, coord.MCPInstanceNode{Node: node, Reason: reason})
			continue
		}
		out.Instances = append(out.Instances, reply.Instances...)
		out.Nodes = append(out.Nodes, coord.MCPInstanceNode{Node: node, Answered: true,
			Count: max(reply.Count, len(reply.Instances))})
	}
	return out, nil
}

// askMCPInstances asks peers for their instances of server, and answers each
// one that replied in time, by node, with the reason the rest are named for.
func (e *Engine) askMCPInstances(ctx context.Context, server string, peers []string,
) (map[string]mcpInstancesReply, string, error) {
	if e.backends == nil || e.backends.Queue == nil {
		return nil, "this node has no broker to ask its peers over", nil
	}
	id := signedask.NewID()
	raw, err := json.Marshal(mcpInstancesRequest{Version: mcpInstancesWire, ID: id, Server: server, Nodes: peers})
	if err != nil {
		return nil, "", fmt.Errorf("engine: encode an MCP instance read: %w", err)
	}
	askCtx, cancel := context.WithTimeout(ctx, mcpInstanceReadBudget)
	defer cancel()
	asked := make(map[string]bool, len(peers))
	for _, p := range peers {
		asked[p] = true
	}
	out := make(map[string]mcpInstancesReply, len(peers))
	err = e.backends.Queue.AskEach(askCtx, topics.ObserveMCPInstances, raw, func(body []byte) bool {
		var reply mcpInstancesReply
		if json.Unmarshal(body, &reply) != nil || reply.Version != mcpInstancesWire || reply.ID != id ||
			!asked[reply.Node] {
			return true
		}
		if _, dup := out[reply.Node]; !dup {
			// ITS OWN INSTANCES ONLY: a reply naming instances of another
			// server, or another node's, is not this read's answer.
			reply.Instances = slices.DeleteFunc(reply.Instances, func(in coord.MCPInstance) bool {
				return in.Server != server || in.Node != reply.Node || !in.State.Valid()
			})
			out[reply.Node] = reply
		}
		return len(out) < len(peers)
	})
	if err != nil {
		if errors.Is(err, queue.ErrUnavailable) {
			return map[string]mcpInstancesReply{}, "this node could not reach the broker to ask", nil
		}
		return nil, "", fmt.Errorf("engine: ask the nodes for their MCP instances: %w", err)
	}
	return out, "did not answer within " + mcpInstanceReadBudget.String(), nil
}

// startMCPInstances makes this node an answerer for reads of its MCP
// instances, until stopMCPInstances.
//
// EVERY NODE WITH AN ENGINE SERVES, data or not: a node holding seats runs
// their children wherever it is, and only it can say how they are.
func (e *Engine) startMCPInstances(ctx context.Context) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	stop, err := e.backends.Queue.Serve(ctx, topics.ObserveMCPInstances, e.answerMCPInstances)
	if err != nil {
		log.WarnContext(ctx, "mcp_instance_answerer_not_registered", "error", err,
			"detail", "a server's page reads this node's instances as not answering until "+
				"the node restarts; everything else works")
		return
	}
	e.stopMCPInstanceServe = stop
}

// stopMCPInstances withdraws this node as an answerer, before its servers
// stop, so a read arriving during the teardown is declined rather than
// answered from bridges being stopped.
func (e *Engine) stopMCPInstances(ctx context.Context) {
	if e.stopMCPInstanceServe == nil {
		return
	}
	if err := e.stopMCPInstanceServe(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "mcp_instance_answerer_not_withdrawn", "error", err)
	}
	e.stopMCPInstanceServe = nil
}

// errMCPInstancesNotAsked is a read addressed to other nodes: this one answers
// nothing for it.
var errMCPInstancesNotAsked = errors.New("engine: the MCP instance read names other nodes")

// answerMCPInstances answers one read of this node's instances, or nothing
// for a read that does not name this node or that this build cannot read.
func (e *Engine) answerMCPInstances(ctx context.Context, raw []byte) ([]byte, error) {
	var req mcpInstancesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("engine: an MCP instance read this build cannot read: %w", err)
	}
	self := e.selfNode()
	if req.Version != mcpInstancesWire || req.Server == "" || !slices.Contains(req.Nodes, self) {
		return nil, errMCPInstancesNotAsked
	}
	instances, count := e.localMCPInstances(ctx, req.Server)
	return json.Marshal(mcpInstancesReply{Version: mcpInstancesWire, ID: req.ID, Node: self,
		Instances: instances, Count: count})
}
