package queries

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
)

// AN MCP SERVER'S TESTS, as its detail and the test routes answer them.
//
// One node's test is the same shape wherever it is read: the answer to the
// person who asked (internal/api/mcpapi) and the server's latest test on its
// detail ([MCPServerAnswer.LastCheck]) draw each node from [MCPCheckNodeView],
// so a page shows a test it just ran and the one it reads back alike.

// MCPCheckReader reads a server's latest test, as [coord.MCPChecks] keeps it.
type MCPCheckReader interface {
	LatestMCPCheck(ctx context.Context, server string) (coord.MCPCheck, bool, error)
}

// MCPCheckView is a server's latest test, as its detail answers it: who asked
// and when, the seat it was made for, and what each node found. Its tools are
// never kept; a person reads them on the answer to the test they ran.
type MCPCheckView struct {
	At time.Time `json:"at"`
	By string    `json:"by"`
	// Seat is the handle of the seat a per-agent server was tested for, ""
	// for a shared server tested on every node.
	Seat string `json:"seat"`
	// OK is whether some node answered and every node that answered found
	// the server serving.
	OK bool `json:"ok"`
	// Nodes are the nodes asked, in order. ALWAYS A LIST.
	Nodes []MCPCheckNodeView `json:"nodes"`
}

// MCPCheckNodeView is what one node's test found.
type MCPCheckNodeView struct {
	// Node is the node, "" for a seat's test no node holding the seat
	// answered.
	Node string `json:"node"`
	// Answered is whether the node answered within the test's budget, and
	// Reason why one did not.
	Answered bool   `json:"answered"`
	Reason   string `json:"reason"`
	// OK is whether it found the server serving: every stage passed or was
	// skipped. A server listing no tool it can be offered under is serving,
	// and its failure says so (`empty_tools`).
	OK bool `json:"ok"`
	// Steps are its stages in order ([mcp.StagesOf]). ALWAYS A LIST.
	Steps []MCPCheckStepView `json:"steps"`
	// Failure is why the test stopped on it, or the finding that the
	// server lists no tool it can be offered under; null where it found the
	// server serving tools. Its sentence is redacted and bounded where it
	// was read, and carries no header or variable value.
	Failure *MCPCheckFailure `json:"failure"`
	// ServerInfo is what the server said of itself in the handshake, null
	// where it never answered one.
	ServerInfo *coord.MCPServerInfo `json:"server_info"`
	// Tools counts the tools it would offer, and Refused the ones it lists
	// that no model could be handed under their names.
	Tools   int `json:"tools"`
	Refused int `json:"refused"`
	// TookMS is the node's whole test, in milliseconds.
	TookMS int64 `json:"took_ms"`
}

// MCPCheckStepView is one stage of a node's test.
type MCPCheckStepView struct {
	Stage  mcp.TestStage `json:"stage"`
	State  mcp.StepState `json:"state"`
	TookMS int64         `json:"took_ms"`
	// Detail is what the stage found, in the engine's words, or the
	// failure's sentence for the stage that failed.
	Detail string `json:"detail" untrusted:"fence"`
}

// MCPCheckFailure is why a node's test stopped: its class (empty where none
// fits) and its sentence, which carries what a server or its process said.
type MCPCheckFailure struct {
	Class mcp.FailureClass `json:"class"`
	Said  string           `json:"said" untrusted:"fence"`
}

// MCPCheckOf is a server's latest test as its detail answers it, its seat
// named by the chart's handle where organization holds it.
func MCPCheckOf(c coord.MCPCheck, organization *org.Organization) MCPCheckView {
	out := MCPCheckView{At: c.At, By: c.By, OK: c.OK, Nodes: make([]MCPCheckNodeView, 0, len(c.Nodes))}
	if c.Seat != uuid.Nil {
		out.Seat = organization.AgentHandleByID(c.Seat)
	}
	for _, n := range c.Nodes {
		view := MCPCheckNodeView{Node: n.Node, Answered: n.Answered, Reason: n.Reason,
			Steps: make([]MCPCheckStepView, 0, len(n.Steps)), Tools: n.Tools, Refused: n.Refused,
			TookMS: n.Took.Milliseconds()}
		for _, s := range n.Steps {
			view.Steps = append(view.Steps, MCPCheckStepView{Stage: mcp.TestStage(s.Stage),
				State: mcp.StepState(s.State), TookMS: s.Took.Milliseconds(), Detail: s.Detail})
		}
		if n.Failure != nil {
			view.Failure = &MCPCheckFailure{Class: mcp.FailureClass(n.Failure.Class), Said: n.Failure.Said}
		}
		if n.Answered {
			info := n.Server
			view.ServerInfo = &info
			view.OK = n.Failure == nil || mcp.FailureClass(n.Failure.Class) == mcp.FailEmptyTools
		}
		out.Nodes = append(out.Nodes, view)
	}
	return out
}

// MCPCheckNodeOf is one node's test as the routes answer it: the same view
// its record would give ([MCPCheckOf]), read off the report directly.
func MCPCheckNodeOf(node string, answered bool, reason string, r mcp.TestReport) MCPCheckNodeView {
	view := MCPCheckNodeView{Node: node, Answered: answered, Reason: reason,
		Steps: make([]MCPCheckStepView, 0, len(r.Steps))}
	if !answered {
		return view
	}
	for _, s := range r.Steps {
		view.Steps = append(view.Steps, MCPCheckStepView{Stage: s.Stage, State: s.State,
			TookMS: s.Took.Milliseconds(), Detail: s.Detail})
	}
	if r.Failure != nil {
		view.Failure = &MCPCheckFailure{Class: r.Failure.Class, Said: r.Failure.Said}
	}
	view.ServerInfo = &coord.MCPServerInfo{Name: r.Identity.Name, Title: r.Identity.Title,
		Version: r.Identity.Version, Protocol: r.Identity.Protocol}
	for _, t := range r.Tools {
		if t.Refused == "" {
			view.Tools++
		} else {
			view.Refused++
		}
	}
	view.OK = r.Serving()
	view.TookMS = r.Took.Milliseconds()
	return view
}
