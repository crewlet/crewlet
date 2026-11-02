package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// AN MCP CHECK is what the latest test of an MCP server found: on every node
// that runs it, or on the node holding the one seat it was tested for ("Test
// now" on a server's page).
//
// # Who has to agree on it
//
// The whole company, now. A test is asked through one node and answered by
// the nodes that run the server, and its result is shown on the server's page
// to every administrator, whichever node serves them. So it is one record per
// server in the coordination store, the CURRENT VALUE ONLY: a later test
// replaces it, and the apply that removes the server deletes it.
//
// # Bounded
//
// A record names at most [MaxMCPCheckNodes] nodes, each with its stages and
// every sentence cut to [MaxMCPErrorBytes]: what a test listed (the tools, with
// their schemas) is the answer to the person who asked, never stored. The
// writer bounds it ([MCPCheck.Validate] refuses one it did not).
//
// # The later test wins
//
// Two people testing one server at once want to know how it is now, and the
// later stamp answers that: a write keeps whichever test is LATER
// ([MCPChecks.PutMCPCheck]).

// MCPCheck is one server's latest test, as the coordination store holds it.
type MCPCheck struct {
	// Server is the configured server's name.
	Server string `json:"server"`

	// Seat is the agent id of the seat the test was made for (a per-agent
	// server, tested on the node holding the seat), or uuid.Nil for a
	// shared server tested on every node.
	Seat uuid.UUID `json:"seat"`

	// At is when the test was asked for.
	At time.Time `json:"at"`

	// By, ByKind and OperatorID are who asked, as [ModelRetry] records them.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// OK is whether the test found the server serving: some node answered,
	// and every node that answered found it serving.
	OK bool `json:"ok"`

	// Nodes are the nodes asked, each with what it found or why it did not
	// answer, at most [MaxMCPCheckNodes].
	Nodes []MCPCheckNode `json:"nodes"`

	// Version is the store's version of the record as it was read. Opaque,
	// as [ModelRetry.Version] is. Not on the wire.
	Version uint64 `json:"-"`
}

// MCPCheckNode is what one node's test found.
type MCPCheckNode struct {
	Node string `json:"node"`
	// Answered is whether the node answered within the test's budget;
	// Reason says why one did not.
	Answered bool   `json:"answered"`
	Reason   string `json:"reason,omitempty"`
	// Steps are its stages, in order.
	Steps []MCPCheckStep `json:"steps,omitempty"`
	// Failure is why the test stopped on it, or the finding that the server
	// lists no tool it can be offered under; nil where it found the server
	// serving tools.
	Failure *MCPInstanceFailure `json:"failure,omitempty"`
	// Server is what the server said of itself in the handshake, each field
	// cut to [MaxMCPServerInfoBytes]; empty where it gave none.
	Server MCPServerInfo `json:"server_info"`
	// Tools counts the tools it would offer, and Refused the ones it lists
	// that no model could be handed under their names.
	Tools   int `json:"tools"`
	Refused int `json:"refused"`
	// Took is the node's whole test.
	Took time.Duration `json:"took"`
}

// MCPCheckStep is one stage of a node's test: its name and state as
// internal/mcp words them, how long it took, and what it found (the failure's
// sentence for the stage that failed).
type MCPCheckStep struct {
	Stage  string        `json:"stage"`
	State  string        `json:"state"`
	Took   time.Duration `json:"took"`
	Detail string        `json:"detail,omitempty"`
}

// MaxMCPCheckNodes bounds the nodes one check names. REASONED: a fleet runs a
// handful of nodes, and the largest the docs describe is a dozen; a record
// naming more than this many is not one a server's page shows whole, and the
// test answer names them all to the person who asked.
const MaxMCPCheckNodes = 32

// maxMCPCheckSteps bounds one node's stages: the most a transport walks.
const maxMCPCheckSteps = 7

// Validate reports why a check cannot be written.
func (c MCPCheck) Validate() error {
	switch {
	case c.Server == "" || strings.ContainsAny(c.Server, "/@"):
		return errors.New("coord: an MCP check needs the configured name of the server it tested")
	case c.At.IsZero():
		return errors.New("coord: an MCP check needs the instant it was asked for")
	case c.By == "":
		return errors.New("coord: an MCP check needs who asked for it")
	case !c.ByKind.Valid():
		return errors.New("coord: an MCP check needs what sort of author asked for it " +
			"(iam.ActorFor's kind): agent, human, operator or system")
	case len(c.Nodes) > MaxMCPCheckNodes:
		return fmt.Errorf("coord: an MCP check names %d nodes, more than the %d a record holds",
			len(c.Nodes), MaxMCPCheckNodes)
	}
	for _, n := range c.Nodes {
		if err := n.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (n MCPCheckNode) validate() error {
	switch {
	case n.Node == "":
		return errors.New("coord: an MCP check's node needs its id")
	case len(n.Steps) > maxMCPCheckSteps:
		return fmt.Errorf("coord: an MCP check's node %s names %d stages, more than any test walks",
			n.Node, len(n.Steps))
	case len(n.Reason) > MaxMCPErrorBytes,
		n.Failure != nil && len(n.Failure.Said) > MaxMCPErrorBytes:
		return fmt.Errorf("coord: an MCP check's node %s carries a sentence over %d bytes",
			n.Node, MaxMCPErrorBytes)
	case len(n.Server.Name) > MaxMCPServerInfoBytes, len(n.Server.Title) > MaxMCPServerInfoBytes,
		len(n.Server.Version) > MaxMCPServerInfoBytes, len(n.Server.Protocol) > MaxMCPServerInfoBytes:
		return fmt.Errorf("coord: an MCP check's node %s carries a server identity over %d bytes",
			n.Node, MaxMCPServerInfoBytes)
	}
	for _, s := range n.Steps {
		if len(s.Detail) > MaxMCPErrorBytes {
			return fmt.Errorf("coord: an MCP check's node %s carries a stage's sentence over %d bytes",
				n.Node, MaxMCPErrorBytes)
		}
	}
	return nil
}

// MCPCheckClass is the key class a check is filed under in the positions
// register.
const MCPCheckClass = "mcp_check"

// MCPCheckKey is a server's check's key in the register.
func MCPCheckKey(server string) string { return DocumentKey(MCPCheckClass, server) }

// MCPCheckServerOf recovers the server from a key of the check class,
// reporting false for a key of another class or one this grammar did not
// write.
func MCPCheckServerOf(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != MCPCheckClass || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}

// MCPChecks is the fleet's record of each server's latest test.
//
// Every read RAISES rather than answering empty when the store cannot be
// read: "never tested" and "could not read" are two facts, and a page that
// showed the first for the second would hide a test somebody just made.
type MCPChecks interface {
	// MCPCheck reads one server's latest check.
	MCPCheck(ctx context.Context, server string) (MCPCheck, bool, error)

	// ListMCPChecks returns every server's latest check, ordered by server.
	ListMCPChecks(ctx context.Context) ([]MCPCheck, error)

	// PutMCPCheck records a check, keeping whichever of it and the stored one
	// is LATER, and returns the record the server holds after the call.
	PutMCPCheck(ctx context.Context, c MCPCheck) (MCPCheck, error)

	// DeleteMCPCheck removes a server's check at a version, reporting
	// whether that version still held. A version of zero never holds.
	DeleteMCPCheck(ctx context.Context, server string, version uint64) (bool, error)
}
