package types

import (
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

// Organization lifecycle: the org coming up and going down, and seats being
// filled, emptied, moved or redefined under it.

func init() {
	events.Register(OrgStarted{})
	events.Register(OrgStopped{})
	events.Register(AgentSpawned{})
	events.Register(AgentTerminated{})
	events.Register(AgentReassigned{})
	events.Register(RoleUpdated{})
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

func (OrgStarted) EventType() string { return "org_started" }

func (e OrgStarted) SummaryFor(actor string) string {
	return "Organization '" + orgName(e.OrgName, actor) + "' started"
}

// OrgStopped marks an organization shutting down.
type OrgStopped struct {
	OrgName string `json:"org_name"`
}

func (OrgStopped) EventType() string { return "org_stopped" }

func (e OrgStopped) SummaryFor(actor string) string {
	return "Organization '" + orgName(e.OrgName, actor) + "' stopped"
}

// AgentSpawned marks a seat being filled by a live agent instance.
type AgentSpawned struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
}

func (AgentSpawned) EventType() string { return "agent_spawned" }

func (e AgentSpawned) Role() string    { return e.RoleName }
func (e AgentSpawned) AgentID() string { return e.Agent }

func (e AgentSpawned) SummaryFor(actor string) string { return lead(actor, "joined the organization") }

// AgentTerminated marks a seat's instance being torn down.
type AgentTerminated struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	Reason   string `json:"reason"`
}

func (AgentTerminated) EventType() string { return "agent_terminated" }

func (e AgentTerminated) Role() string    { return e.RoleName }
func (e AgentTerminated) AgentID() string { return e.Agent }

func (e AgentTerminated) SummaryFor(actor string) string {
	line := lead(actor, "was terminated")
	if e.Reason != "" {
		return line + ": " + e.Reason
	}
	return line
}

// AgentReassigned marks a seat moving in the hierarchy.
type AgentReassigned struct {
	Agent      string `json:"agent_id"`
	OldRole    string `json:"old_role"`
	NewRole    string `json:"new_role"`
	OldManager string `json:"old_manager"`
	NewManager string `json:"new_manager"`
}

func (AgentReassigned) EventType() string { return "agent_reassigned" }

func (e AgentReassigned) AgentID() string { return e.Agent }

func (e AgentReassigned) Summary() string {
	return "Agent reassigned from " + e.OldRole + " to " + e.NewRole
}

// RoleUpdated fires when an existing role's definition changes during a config
// reload — the seat stays, what it is changes.
type RoleUpdated struct {
	RoleName      string   `json:"role"`
	Agent         string   `json:"agent_id"`
	ChangedFields []string `json:"changed_fields,omitempty"`
}

func (RoleUpdated) EventType() string { return "role_updated" }

func (e RoleUpdated) Role() string    { return e.RoleName }
func (e RoleUpdated) AgentID() string { return e.Agent }

// SummaryFor is possessive ("Engineer's role updated"), so it spells the actor
// into the line itself rather than going through lead.
func (e RoleUpdated) SummaryFor(actor string) string {
	changed := ""
	if len(e.ChangedFields) > 0 {
		changed = " (" + strings.Join(e.ChangedFields, ", ") + ")"
	}
	if actor == "" {
		return "Role updated" + changed
	}
	return actor + "'s role updated" + changed
}
