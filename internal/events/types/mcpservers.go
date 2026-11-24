package types

import (
	"slices"
	"strings"

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
	// MCPServerSetTools sets the levels of a server's tools, or with
	// [MCPServerActed.DryRun] checks a change to them.
	MCPServerSetTools MCPServerAction = "set_tools"
	// MCPServerSignIn starts an OAuth sign-in to a server, and
	// MCPServerSignOut gives its grant up (revoked at the vendor where it
	// offers that).
	MCPServerSignIn  MCPServerAction = "sign_in"
	MCPServerSignOut MCPServerAction = "sign_out"
	// MCPServerApprove answers a call an agent's run made to one of the
	// server's tools at Needs approval ([MCPServerActed.Answer]).
	MCPServerApprove MCPServerAction = "approve"
	// MCPServerReviewChanges decides changes the server made to its tools
	// since their levels were decided: the tools it accepted
	// ([MCPServerActed.Accepted]) and kept blocked ([MCPServerActed.Blocked]).
	MCPServerReviewChanges MCPServerAction = "review_changes"
	// MCPServerAutoAcceptChanges is a node accepting the server's tool
	// changes as listed, for its `tools.auto_accept_changes`, under the
	// node's name and with what each moved ([MCPServerActed.Changes]).
	MCPServerAutoAcceptChanges MCPServerAction = "auto_accept_changes"
)

// MCPServerActions is every action, for a reader's copy.
var MCPServerActions = []MCPServerAction{MCPServerRestart, MCPServerCheck, MCPServerProbe, MCPServerStoreValue,
	MCPServerSetTools, MCPServerSignIn, MCPServerSignOut, MCPServerApprove, MCPServerReviewChanges,
	MCPServerAutoAcceptChanges}

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
	// DryRun is a change to the server's tool levels checked and not
	// written; the revision a written one stored names its levels.
	DryRun bool `json:"dry_run,omitempty"`
	// Approval names the held call an approval answered, by its id, Tool
	// the tool it called, and Answer what the person answered: `once`,
	// `always` or `deny`. NEVER ITS ARGUMENTS, which stay sealed on the
	// run's row.
	Approval string `json:"approval,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Answer   string `json:"answer,omitempty"`

	// Accepted and Blocked are the tools a review decided, by the server's
	// own name for each: the changes accepted, and the ones kept blocked.
	// A tool's name only, never its definition, which is the server's.
	Accepted []string `json:"accepted,omitempty"`
	Blocked  []string `json:"blocked,omitempty"`
	// Changes is what each change accepted automatically moved, in
	// Accepted's order, since nobody reviewed it. Empty for a person's review.
	Changes []MCPToolChangeRecord `json:"changes,omitempty"`
	// Outcome is `applied` for a gesture that did what it was asked,
	// `refused` for one the route turned down for a stated reason (its
	// code in Refusal), and `failed` for one that broke without one.
	Outcome AuditOutcome `json:"outcome"`
	Refusal string       `json:"refusal,omitempty"`
	Failed  bool         `json:"failed,omitempty"`
}

// MCPToolChangeRecord is one change to a tool as it was accepted unreviewed:
// what it did, the parts it moved, and the definition before and after.
type MCPToolChangeRecord struct {
	Tool string `json:"tool"`
	// Kind is `added`, `changed` or `removed`; Parts the parts a changed
	// definition moved (`title`, `description`, `schema`, `annotations`).
	Kind  string   `json:"kind"`
	Parts []string `json:"parts,omitempty"`
	// Loosened is a change whose declared hints claim less risk than before.
	Loosened bool `json:"loosened"`
	// Before is the definition accepted until then, nil for an added tool;
	// After the one accepted now, nil for a removed tool.
	Before *MCPToolWords `json:"before,omitempty"`
	After  *MCPToolWords `json:"after,omitempty"`
}

// MCPToolWords is one definition of a tool as a change record keeps it: its
// digest, its words as a listing bounds them, and the hints its server
// declared. Its schemas are named by the digest alone.
type MCPToolWords struct {
	Digest      string `json:"digest,omitempty"`
	Title       string `json:"title,omitempty" untrusted:"name"`
	Description string `json:"description,omitempty" untrusted:"fence"`
	// The four hints, each `yes`, `no` or `unknown`.
	ReadOnly    string `json:"read_only,omitempty"`
	Destructive string `json:"destructive,omitempty"`
	Idempotent  string `json:"idempotent,omitempty"`
	OpenWorld   string `json:"open_world,omitempty"`
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
	case MCPServerSetTools:
		text = "set the tool levels of MCP server " + e.Server
		if e.DryRun {
			text = "checked a change to the tool levels of MCP server " + e.Server
		}
	case MCPServerSignIn:
		text = "started a sign-in to MCP server " + e.Server
	case MCPServerSignOut:
		text = "signed out of MCP server " + e.Server
	case MCPServerReviewChanges:
		text = "reviewed changed tools of MCP server " + e.Server
		var parts []string
		if len(e.Accepted) > 0 {
			parts = append(parts, "accepted "+strings.Join(e.Accepted, ", "))
		}
		if len(e.Blocked) > 0 {
			parts = append(parts, "kept "+strings.Join(e.Blocked, ", ")+" blocked")
		}
		if len(parts) > 0 {
			text += " (" + strings.Join(parts, "; ") + ")"
		}
	case MCPServerAutoAcceptChanges:
		text = "accepted changed tools of MCP server " + e.Server + " automatically"
		if len(e.Changes) > 0 {
			moved := make([]string, 0, len(e.Changes))
			for _, c := range e.Changes {
				moved = append(moved, c.summary())
			}
			text += " (" + strings.Join(moved, "; ") + ")"
		}
	case MCPServerApprove:
		switch e.Answer {
		case "once":
			text = "allowed a call to " + e.Tool + " of MCP server " + e.Server + " once"
		case "always":
			text = "allowed a call to " + e.Tool + " of MCP server " + e.Server + ", and every call after it"
		case "deny":
			text = "denied a call to " + e.Tool + " of MCP server " + e.Server
		default:
			text = "answered a held call of MCP server " + e.Server
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

// summary says what one change moved, as an audit line lists it.
func (c MCPToolChangeRecord) summary() string {
	what := c.Kind
	if len(c.Parts) > 0 {
		what = strings.Join(c.Parts, ", ")
	}
	if c.Loosened {
		what += ", now claiming less risk"
	}
	return c.Tool + ": " + what
}

// NewMCPServerActed builds the audit record for one server gesture, with
// [MCPServerActed.Failed] derived from the outcome.
func NewMCPServerActed(e MCPServerActed) MCPServerActed {
	e.Failed = e.Outcome.failed()
	return e
}
