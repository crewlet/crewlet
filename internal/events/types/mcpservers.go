package types

import (
	"slices"

	"github.com/crewlet/crewlet/internal/events"
)

func init() {
	events.Register[MCPServerActed]()
}

// MCPServerAction is which gesture on an MCP server an [MCPServerActed]
// records: the verbs of the server routes (internal/api/mcpapi), each its own
// route.
type MCPServerAction string

// The MCP server gestures.
const (
	// MCPServerRestart asks every node, or the node holding one seat, to
	// restart a server now.
	MCPServerRestart MCPServerAction = "restart"
	// MCPServerCheck tests a server where it runs: the entry the company
	// runs, or with [MCPServerActed.Draft] an entry not written yet.
	MCPServerCheck MCPServerAction = "check"
	// MCPServerProbe checks an address before a server is added at it.
	MCPServerProbe MCPServerAction = "probe"
	// MCPServerStoreValue stores a server's header or variable, write-only.
	MCPServerStoreValue MCPServerAction = "store_value"
)

// MCPServerActions is every action, for a reader's copy.
var MCPServerActions = []MCPServerAction{MCPServerRestart, MCPServerCheck, MCPServerProbe, MCPServerStoreValue}

// Valid reports whether a is an action this build records.
func (a MCPServerAction) Valid() bool { return slices.Contains(MCPServerActions, a) }

// MCPServerActed is one call to an MCP server's gesture route
// (`/mcp-servers/{name}/...`): who made it, which gesture on which server, and
// what became of it, refused ones included.
//
// ITS OWN EVENT, as a model's gestures are ([ModelActed]) and for their
// reason: these are REST verbs that change runtime state and leave no record
// anywhere else, and a refused one leaves nothing at all.
//
// NAMES, NEVER A VALUE: a gesture names the server and, for one instance, the
// seat, and nothing it would have been started with.
//
// THE NODE IS THE ENVELOPE'S, as a model gesture's is.
type MCPServerActed struct {
	// ActorName, ActorKind and OperatorID are who acted, in the three
	// halves of [OperatorActed.ActorName].
	ActorName  string `json:"actor"`
	ActorKind  string `json:"actor_kind"`
	OperatorID string `json:"operator_id"`

	Action MCPServerAction `json:"action"`
	// Server is the configured server's name, and Seat the handle of the
	// seat whose instance the gesture named, "" for every instance.
	Server string `json:"mcp_server"`
	Seat   string `json:"seat,omitempty"`
	// Draft is whether a check tested an entry not written yet, under the
	// name the draft gives it.
	Draft bool `json:"draft,omitempty"`
	// Address is the address an address check dialled, its credentials
	// withheld (redact.URL): a dial the engine made at a person's word is
	// one the trail names.
	Address string `json:"address,omitempty"`
	// Key and Variable name a stored value's key and secret, never the value.
	Key      string `json:"key,omitempty"`
	Variable string `json:"variable,omitempty"`
	// Outcome is `applied` for a gesture that did what it was asked,
	// `refused` for one the route turned down for a stated reason (its
	// code in Refusal), and `failed` for one that broke without one.
	Outcome AuditOutcome `json:"outcome"`
	Refusal string       `json:"refusal,omitempty"`
	Failed  bool         `json:"failed,omitempty"`
}

// EventType is the "mcp_server_acted" wire type.
func (MCPServerActed) EventType() string { return "mcp_server_acted" }

// Actor is who acted on the server.
func (e MCPServerActed) Actor() string { return e.ActorName }

// SummaryFor says what was done to which server and how it ended.
func (e MCPServerActed) SummaryFor(actor string) string {
	var text string
	switch e.Action {
	case MCPServerRestart:
		text = "restarted MCP server " + e.Server
	case MCPServerCheck:
		text = "tested MCP server " + e.Server
		if e.Draft {
			text = "tested a draft of MCP server " + e.Server
		}
	case MCPServerProbe:
		text = "checked the MCP server address " + e.Address
	case MCPServerStoreValue:
		text = "stored " + e.Key + " of MCP server " + e.Server
		if e.Variable != "" {
			text += " as " + e.Variable
		}
	default:
		text = string(e.Action) + " MCP server " + e.Server
	}
	if e.Seat != "" {
		text += " for seat " + e.Seat
	}
	if e.Outcome != AuditApplied {
		text += ": " + outcomeWords(e.Outcome, e.Refusal)
	}
	return lead(actor, text)
}

// NewMCPServerActed builds the audit record for one server gesture, with
// [MCPServerActed.Failed] derived from the outcome.
func NewMCPServerActed(e MCPServerActed) MCPServerActed {
	e.Failed = e.Outcome.failed()
	return e
}
