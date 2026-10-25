package types

import "github.com/crewlet/crewlet/internal/events"

func init() {
	events.Register[OperatorToolsChanged]()
}

// OperatorToolsChanged tells every node that one node built its operator
// catalogue again — the tools `/operator/mcp` and `/operator/act` serve — so
// each tells the MCP clients listening on it to list their tools again.
//
// # Why every node has to hear it
//
// A node builds its catalogue again at its OWN apply, and every node applies
// a revision on its own clock: the activation reaches each by a nudge and a
// poll, and the first apply of a company builds the native runtime, which
// takes seconds. An assistant's listening stream is held by one node, and the
// transport keeps no session, so its next listing reaches whichever ingress
// node a load balancer picks — often a peer that has not applied yet, which
// answers the old catalogue. Told only by the node holding its stream, it kept
// that answer until the next change. Told by every node as each rebuilds, the
// last thing it hears follows the last node's rebuild, so the listing it makes
// then is the new catalogue wherever it lands.
//
// # Deliberately empty, and a nudge rather than a record
//
// The envelope's origin (`node`, stamped by the queue) names the node that
// rebuilt, which is all a hearer needs: what to list is each serving node's to
// answer at the listing, never something carried here. Losing one costs a
// listener the change until the next one is announced; it is never a
// divergence between nodes, since no node's catalogue is built from it.
type OperatorToolsChanged struct{}

// EventType is the "operator_tools_changed" wire type.
func (OperatorToolsChanged) EventType() string { return "operator_tools_changed" }

// Summary says what happened; the envelope's origin says on which node.
func (OperatorToolsChanged) Summary() string { return "Operator tools rebuilt" }
