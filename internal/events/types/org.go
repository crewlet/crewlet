package types

import "github.com/crewlet/crewlet/internal/events"

// Two lifecycles, on different clocks: this NODE coming up and going down
// under a company, and a SEAT arriving on or leaving this node.
//
// ONE ORG PAIR PER NODE, and the envelope's source names which — the same
// shape [ConfigRevisionApplied] has, and for the same reason: starting and
// stopping is something a PROCESS does, and a fleet of three that reported one
// start between them would be hiding two of them.
//
// THE SEAT PAIR IS LIVE-ONLY (see [events.LiveOnly]). Placement moves seats on
// every rebalance, so a durable row per claim would fill the audit log with a
// fact about scheduling rather than about the company. What reads them is the
// live seat state, where "is this seat running, and where" is the question —
// and where `terminated` was a state nothing could reach, so a seat that went
// away kept showing whatever it last did.
//
// # Why there is no seat REDEFINITION here
//
// There were two more — reassigned, redefined — and neither had a publisher.
// The roster change they were about is a CONFIG REVISION, which the engine
// stores whole and serves a diff of: a precise record where these were a lossy
// paraphrase of one, published N times on a fleet that all apply it.

func init() {
	events.Register[OrgStarted]()
	events.Register[OrgStopped]()
	events.Register[AgentSpawned]()
	events.Register[AgentTerminated]()
}

// orgName is the organization's own name, falling back to the actor — which
// for these two events is the publisher, since neither carries a role or an
// agent id for the chain to prefer.
func orgName(name, actor string) string {
	if name != "" {
		return name
	}
	return actor
}

// OrgStarted marks an organization coming up.
type OrgStarted struct {
	OrgName string `json:"org_name"`
}

// EventType is the "org_started" wire type.
func (OrgStarted) EventType() string { return "org_started" }

// SummaryFor names the organization rather than the actor: at start-up no seat
// exists yet to attribute the line to.
func (e OrgStarted) SummaryFor(actor string) string {
	return "Organization '" + orgName(e.OrgName, actor) + "' started"
}

// OrgStopped marks an organization shutting down.
type OrgStopped struct {
	OrgName string `json:"org_name"`
}

// EventType is the "org_stopped" wire type.
func (OrgStopped) EventType() string { return "org_stopped" }

// SummaryFor names the organization rather than the actor, mirroring
// OrgStarted so a shutdown reads as the counterpart of the start-up line.
func (e OrgStopped) SummaryFor(actor string) string {
	return "Organization '" + orgName(e.OrgName, actor) + "' stopped"
}

// AgentSpawned marks a seat starting to run on this node.
type AgentSpawned struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
}

// EventType is the "agent_spawned" wire type.
func (AgentSpawned) EventType() string { return "agent_spawned" }

// Role is the seat that was filled.
func (e AgentSpawned) Role() string { return e.RoleName }

// AgentID is the handle now holding the seat.
func (e AgentSpawned) AgentID() string { return e.Agent }

// SummaryFor leads with the seat, which the actor chain resolves from Role.
func (e AgentSpawned) SummaryFor(actor string) string { return lead(actor, "joined the organization") }

// AgentTerminated marks a seat leaving this node.
type AgentTerminated struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	Reason   string `json:"reason"`
}

// EventType is the "agent_terminated" wire type.
func (AgentTerminated) EventType() string { return "agent_terminated" }

// Role is the seat being emptied.
func (e AgentTerminated) Role() string { return e.RoleName }

// AgentID is the handle giving the seat up.
func (e AgentTerminated) AgentID() string { return e.Agent }

// SummaryFor appends the reason when one was given: a seat released because a
// node is draining and one shed under capacity read identically without it.
func (e AgentTerminated) SummaryFor(actor string) string {
	line := lead(actor, "was terminated")
	if e.Reason != "" {
		return line + ": " + e.Reason
	}
	return line
}
