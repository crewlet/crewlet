package coord

import (
	"slices"
	"time"

	"github.com/google/uuid"
)

// AN MCP INSTANCE, as the node running it holds it now.
//
// # Who has to agree on it
//
// Nobody: it is each node's own memory of the children it runs (a shared
// server's one child, each held seat's own), read where it lives when a person
// opens a server's page, as a turn's detail is (ADR-0021). Neither stored nor
// on the heartbeat: the heartbeat's per-server row ([MCPServerStatus]) carries
// the bounded counts every reader needs on every beat, and a row per child
// would put a company's seat count times its per-agent servers on a lease
// re-sent every [ReconcileInterval] to answer a question asked a few times a
// minute. So this type is a WIRE shape between nodes (internal/engine's
// mcpinstances.go asks and answers it), declared here beside the row it
// details because both the engine and the API read it.

// MCPInstanceState is one instance's condition on its node.
type MCPInstanceState string

const (
	// MCPInstanceRunning is an instance a live session serves.
	MCPInstanceRunning MCPInstanceState = "running"
	// MCPInstanceFailed is one that is not serving: a start that failed, a
	// session that ended and is being restarted, a configuration that did
	// not resolve, or a credential nothing will supply until a person acts.
	MCPInstanceFailed MCPInstanceState = "failed"
	// MCPInstanceWaiting is one held back, unstarted, for a credential a
	// provisioning pass mints for its seat and has not sealed yet: nothing is
	// wrong, as [MCPServerStatus.Waiting] says.
	MCPInstanceWaiting MCPInstanceState = "waiting"
)

// MCPInstanceStates is every state, for the gate holding a reader's copy.
var MCPInstanceStates = []MCPInstanceState{MCPInstanceRunning, MCPInstanceFailed, MCPInstanceWaiting}

// Valid reports whether s is a state this build sends.
func (s MCPInstanceState) Valid() bool { return slices.Contains(MCPInstanceStates, s) }

// MCPInstance is one instance of one MCP server on one node.
type MCPInstance struct {
	// Server is the configured server's name, never an instance name.
	Server string `json:"server"`
	// Seat is the agent id of the seat the instance runs for (ADR-0013),
	// uuid.Nil for a shared server's one child: an id rather than a handle
	// for [MCPServerStatus.ErrorSeat]'s reason.
	Seat uuid.UUID `json:"seat"`
	// Node is the node running it.
	Node  string           `json:"node"`
	State MCPInstanceState `json:"state"`
	// Since is when the instance's state began: when the session serving it
	// started, when it stopped serving or first failed to start, or when it
	// was first held back.
	Since time.Time `json:"since"`
	// Restarts is how many times the node brought it back since it was
	// started from its spec ([MCPServerStatus.Restarts], for one instance).
	Restarts int `json:"restarts"`
	// Tools is how many tools it contributes.
	Tools int `json:"tools"`
	// Failure is why a failed instance is not serving, nil otherwise; Wait
	// what a waiting one waits for, "" otherwise. Each bounded to
	// [MaxMCPErrorBytes].
	Failure *MCPInstanceFailure `json:"failure,omitempty"`
	Wait    string              `json:"wait,omitempty"`
	// LastFailure is the instance's latest failure, kept after it came back:
	// nil when it has had none since it was started from its spec.
	LastFailure *MCPInstanceFailure `json:"last_failure,omitempty"`
	// LastAnswer is when the instance last answered: a start that listed its
	// tools, or a tool call that came back. Zero before either.
	LastAnswer time.Time `json:"last_answer"`
}

// MCPInstanceFailure is one failure of an instance: its class in
// internal/mcp's FailureClass words ("" where none fits), the engine's
// sentence (bounded to [MaxMCPErrorBytes]; it carries what a server or its
// process said, which is somebody else's text), and when.
type MCPInstanceFailure struct {
	Class string    `json:"class"`
	Said  string    `json:"said"`
	At    time.Time `json:"at"`
}

// MaxMCPInstances bounds how many instances of one server one node answers
// for: TWO HUNDRED, the first by seat id, past the seats any one node of a
// company holds in practice, and short enough that the answer (each instance
// at most two bounded sentences, about 1.2 KiB) stays well under the broker's
// one-megabyte message. The count travels whole beside them.
const MaxMCPInstances = 200

// MCPInstanceRead is every instance of one server the nodes asked hold, and
// what each of them said.
type MCPInstanceRead struct {
	// Instances are the instances, by node then seat id.
	Instances []MCPInstance
	// Nodes is one entry per node asked, in id order.
	Nodes []MCPInstanceNode
}

// MCPInstanceNode is what one node asked for its instances said.
type MCPInstanceNode struct {
	Node string
	// Answered is whether the node answered in time; Reason says why not
	// where it did not.
	Answered bool
	Reason   string
	// Count is how many instances of the server the node holds, never fewer
	// than it listed ([MaxMCPInstances]).
	Count int
}
