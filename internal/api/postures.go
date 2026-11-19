package api

import (
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Postures is this app's live projection as an item's flow reads it
// (ADR-0162), for [engine.Engine.UsePostures]: every tracker read on the node,
// the dashboard's and the agents' tools alike, reads one posture.
func (a *App) Postures() worklet.Postures {
	return postures{state: a.state, company: a.company}
}

// postures names the projection's seats, held by agent id, by the handles an
// item is assigned to, from the company's chart as it is at each read.
type postures struct {
	state   *livestate.LiveState
	company func() (*config.Company, *org.Organization)
}

func (p postures) SeatPostures() worklet.Posture {
	return p.state.Posture(agentHandles(p.company))
}

// agentHandles is every agent seat in the company, agent id to handle.
func agentHandles(company func() (*config.Company, *org.Organization)) map[string]string {
	organization := agentOrganization(company)
	if organization == nil {
		return nil
	}
	out := map[string]string{}
	for role := range organization.AllRoles() {
		if id, ok := organization.AgentIDFor(role); ok {
			out[id.String()] = role.Handle()
		}
	}
	return out
}
