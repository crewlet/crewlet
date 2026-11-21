package types

import "github.com/crewlet/crewlet/internal/events"

func init() {
	events.Register[OperatorToolsChanged]()
}

// OperatorToolsChanged tells every node that one node built its operator
// catalogue again (the tools `/operator/mcp` and `/operator/act` serve), so
// each tells its listening MCP clients to list again.
//
// Every node must hear it: each applies a revision on its own clock. A stream
// is held by one node and the transport keeps no session, so the next listing
// reaches whichever ingress node the load balancer picks, often a peer that has
// not applied yet. Told by every node as each rebuilds, the last thing a
// listener hears follows the last rebuild. Deliberately empty, a nudge rather
// than a record: the envelope's origin (`node`) names the rebuilder, and losing
// one never diverges nodes, since no catalogue is built from it.
type OperatorToolsChanged struct{}

// EventType is the "operator_tools_changed" wire type.
func (OperatorToolsChanged) EventType() string { return "operator_tools_changed" }

// Summary says what happened; the envelope's origin says on which node.
func (OperatorToolsChanged) Summary() string { return "Operator tools rebuilt" }
